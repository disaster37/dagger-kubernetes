package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// stubLogRepo is a configurable domain.LogRepository for handler tests.
type stubLogRepo struct {
	page    domain.LogSearchPage
	err     error
	lastReq domain.LogSearchRequest
	calls   int
}

func (s *stubLogRepo) QueryTraceLogs(string, time.Time, time.Time, int) ([]domain.LogEntry, error) {
	return nil, nil
}

//nolint:gocritic // signature is fixed by domain.LogRepository.
func (s *stubLogRepo) SearchTraceLogs(_ context.Context, _ string, req domain.LogSearchRequest) (domain.LogSearchPage, error) {
	s.calls++
	s.lastReq = req
	return s.page, s.err
}

func (s *stubLogRepo) DeleteTraceLogs(context.Context, string) error { return nil }

// newSearchEngine registers the trace-search route on a bare engine.
func newSearchEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/api/v1/traces/:traceID/search", s.handleTracesSearch)
	return e
}

func searchTraceTree() *domain.TraceInfo {
	child := &domain.SpanNode{SpanID: "child", Name: "child", Status: "success", Children: []*domain.SpanNode{}}
	root := &domain.SpanNode{SpanID: "root", Name: "root", Status: "success", Children: []*domain.SpanNode{child}}
	return &domain.TraceInfo{TraceID: "trace-1", RootSpan: root, Status: "success"}
}

func TestHandleTracesSearch(t *testing.T) {
	env := newTestEnv(t)
	bearer := env.loginAsAdmin(t)

	tests := []struct {
		name       string
		query      string
		trace      *domain.TraceInfo
		traceErr   error
		repoPage   domain.LogSearchPage
		repoErr    error
		wantCode   int
		wantCalls  int
		wantSpanID []string
		wantLimit  int
		wantNext   int64
	}{
		{
			name:      "whole trace passthrough",
			query:     "?q=error",
			trace:     searchTraceTree(),
			repoPage:  domain.LogSearchPage{Entries: []domain.LogEntry{{Line: "an error"}}, Next: 42},
			wantCode:  http.StatusOK,
			wantCalls: 1,
			wantLimit: defaultTraceSearchLimit,
			wantNext:  42,
		},
		{
			name:       "scoped span resolves descendants",
			query:      "?span_id=root",
			trace:      searchTraceTree(),
			repoPage:   domain.LogSearchPage{},
			wantCode:   http.StatusOK,
			wantCalls:  1,
			wantSpanID: nil, // root focus = whole trace
		},
		{
			name:       "scoped child span",
			query:      "?span_id=child",
			trace:      searchTraceTree(),
			repoPage:   domain.LogSearchPage{},
			wantCode:   http.StatusOK,
			wantCalls:  1,
			wantSpanID: []string{"child"},
		},
		{
			name:      "unknown span",
			query:     "?span_id=nope",
			trace:     searchTraceTree(),
			wantCode:  http.StatusNotFound,
			wantCalls: 0,
		},
		{
			name:      "trace not found",
			query:     "?span_id=child",
			traceErr:  domain.ErrNotFound,
			wantCode:  http.StatusNotFound,
			wantCalls: 0,
		},
		{
			name:      "invalid mode",
			query:     "?mode=fuzzy",
			trace:     searchTraceTree(),
			wantCode:  http.StatusBadRequest,
			wantCalls: 0,
		},
		{
			name:      "invalid cursor",
			query:     "?cursor=abc",
			trace:     searchTraceTree(),
			wantCode:  http.StatusBadRequest,
			wantCalls: 0,
		},
		{
			name:      "negative cursor",
			query:     "?cursor=-1",
			trace:     searchTraceTree(),
			wantCode:  http.StatusBadRequest,
			wantCalls: 0,
		},
		{
			name:      "invalid regex",
			query:     "?mode=regex&q=%28%5B",
			trace:     searchTraceTree(),
			repoErr:   fmt.Errorf("%w: bad", domain.ErrInvalidRegex),
			wantCode:  http.StatusBadRequest,
			wantCalls: 1,
		},
		{
			name:      "backend error",
			query:     "?q=x",
			trace:     searchTraceTree(),
			repoErr:   errors.New("loki down"),
			wantCode:  http.StatusBadGateway,
			wantCalls: 1,
		},
		{
			name:      "limit clamp",
			query:     "?limit=99999",
			trace:     searchTraceTree(),
			repoPage:  domain.LogSearchPage{},
			wantCode:  http.StatusOK,
			wantCalls: 1,
			wantLimit: maxTraceSearchLimit,
		},
		{
			name:      "limit default on garbage",
			query:     "?limit=abc",
			trace:     searchTraceTree(),
			repoPage:  domain.LogSearchPage{},
			wantCode:  http.StatusOK,
			wantCalls: 1,
			wantLimit: defaultTraceSearchLimit,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubLogRepo{page: tc.repoPage, err: tc.repoErr}
			env.server.logs = repo
			env.server.traces = &stubTraceRepo{trace: tc.trace, err: tc.traceErr}
			e := newSearchEngine(env.server)

			resp := ut.PerformRequest(e, "GET", "/api/v1/traces/trace-1/search"+tc.query, nil,
				ut.Header{Key: "Authorization", Value: bearer})
			if got := resp.Result().StatusCode(); got != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", got, tc.wantCode, resp.Result().Body())
			}
			if repo.calls != tc.wantCalls {
				t.Fatalf("repo calls = %d, want %d", repo.calls, tc.wantCalls)
			}
			if tc.wantCalls == 0 {
				return
			}
			if tc.wantLimit != 0 && repo.lastReq.Limit != tc.wantLimit {
				t.Fatalf("limit = %d, want %d", repo.lastReq.Limit, tc.wantLimit)
			}
			if len(repo.lastReq.SpanIDs) != len(tc.wantSpanID) {
				t.Fatalf("spanIDs = %v, want %v", repo.lastReq.SpanIDs, tc.wantSpanID)
			}
			for i := range repo.lastReq.SpanIDs {
				if repo.lastReq.SpanIDs[i] != tc.wantSpanID[i] {
					t.Fatalf("spanIDs = %v, want %v", repo.lastReq.SpanIDs, tc.wantSpanID)
				}
			}
			if tc.wantCode != http.StatusOK {
				return
			}
			var page domain.LogSearchPage
			if err := json.Unmarshal(resp.Result().Body(), &page); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if page.Next != tc.wantNext {
				t.Fatalf("next = %d, want %d", page.Next, tc.wantNext)
			}
		})
	}
}

func TestHandleTracesSearchAuthGate(t *testing.T) {
	env := newTestEnv(t)
	env.server.logs = &stubLogRepo{}
	env.server.traces = &stubTraceRepo{trace: searchTraceTree()}
	e := newSearchEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/trace-1/search", nil)
	if got := resp.Result().StatusCode(); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got)
	}
}

func TestClampSearchLimit(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{"", defaultTraceSearchLimit},
		{"abc", defaultTraceSearchLimit},
		{"0", defaultTraceSearchLimit},
		{"-5", defaultTraceSearchLimit},
		{"10", 10},
		{"2000", maxTraceSearchLimit},
		{"99999", maxTraceSearchLimit},
	}
	for _, tc := range tests {
		if got := clampSearchLimit(tc.raw); got != tc.want {
			t.Fatalf("clampSearchLimit(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestParseSearchCursor(t *testing.T) {
	if got, err := parseSearchCursor(""); err != nil || got != 0 {
		t.Fatalf("empty cursor = (%d, %v), want (0, nil)", got, err)
	}
	if got, err := parseSearchCursor("123"); err != nil || got != 123 {
		t.Fatalf("cursor = (%d, %v), want (123, nil)", got, err)
	}
	if _, err := parseSearchCursor("abc"); err == nil {
		t.Fatal("expected error for non-integer cursor")
	}
	if _, err := parseSearchCursor("-1"); err == nil {
		t.Fatal("expected error for negative cursor")
	}
}
