package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const searchTraceID = "401ccb197124a8ff2028720fcb5eaa06"

// lokiSearchServer serves a fixed set of (timestamp, span_id, line) records
// from /loki/api/v1/query_range, honouring start/end/limit so pagination can be
// exercised. Records are returned in the order given (the client must sort).
func lokiSearchServer(t *testing.T, records []searchRecord) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		start, _ := parseInt64(q.Get("start"))
		end, _ := parseInt64(q.Get("end"))
		limit, _ := parseInt64(q.Get("limit"))

		type stream struct {
			spanID string
			values [][]string
		}
		var streams []stream
		index := map[string]int{}
		for _, rec := range records {
			if rec.ts < start || rec.ts > end {
				continue
			}
			idx, ok := index[rec.spanID]
			if !ok {
				idx = len(streams)
				index[rec.spanID] = idx
				streams = append(streams, stream{spanID: rec.spanID})
			}
			streams[idx].values = append(streams[idx].values, []string{fmt.Sprintf("%d", rec.ts), rec.line})
		}

		var result strings.Builder
		result.WriteString(`{"data":{"result":[`)
		written := 0
		for i, st := range streams {
			if written >= int(limit) {
				break
			}
			if i > 0 {
				result.WriteString(",")
			}
			result.WriteString(`{"stream":{"trace_id":"` + searchTraceID + `","span_id":"` + st.spanID + `"},"values":[`)
			for j, v := range st.values {
				if written >= int(limit) {
					break
				}
				if j > 0 {
					result.WriteString(",")
				}
				fmt.Fprintf(&result, `["%s",%q]`, v[0], v[1])
				written++
			}
			result.WriteString(`]}`)
		}
		result.WriteString(`]}}`)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(result.String()))
	}))
}

type searchRecord struct {
	ts     int64
	spanID string
	line   string
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func TestMatchLogLine(t *testing.T) {
	re := regexp.MustCompile(`err(or)?`)
	tests := []struct {
		name  string
		line  string
		query string
		mode  domain.LogSearchMode
		re    *regexp.Regexp
		want  bool
	}{
		{"empty query matches", "anything", "", domain.LogSearchContains, nil, true},
		{"contains hit", "hello world", "world", domain.LogSearchContains, nil, true},
		{"contains miss", "hello world", "planet", domain.LogSearchContains, nil, false},
		{"contains case sensitive", "Hello", "hello", domain.LogSearchContains, nil, false},
		{"regex hit", "an error occurred", `err(or)?`, domain.LogSearchRegex, re, true},
		{"regex miss", "all good", `err(or)?`, domain.LogSearchRegex, re, false},
		{"regex nil pattern", "an error", "err", domain.LogSearchRegex, nil, false},
		{"unicode contains", "héllo wörld", "wörld", domain.LogSearchContains, nil, true},
		{"unicode regex", "héllo wörld", `w.rld`, domain.LogSearchRegex, regexp.MustCompile(`w.rld`), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchLogLine(tc.line, tc.query, tc.mode, tc.re); got != tc.want {
				t.Fatalf("matchLogLine(%q, %q, %q) = %v, want %v", tc.line, tc.query, tc.mode, got, tc.want)
			}
		})
	}
}

func TestSearchTraceLogsFiltersAndSorts(t *testing.T) {
	base := time.Now().Add(-time.Hour).UnixNano()
	records := []searchRecord{
		{ts: base + 300, spanID: "span-b", line: "third error"},
		{ts: base + 100, spanID: "span-a", line: "first ok"},
		{ts: base + 200, spanID: "span-a", line: "second error"},
	}
	srv := lokiSearchServer(t, records)
	defer srv.Close()

	client := NewLogsClient(srv.URL)
	page, err := client.SearchTraceLogs(context.Background(), searchTraceID, domain.LogSearchRequest{
		Query: "error",
		Mode:  domain.LogSearchContains,
		Start: time.Unix(0, base),
		End:   time.Unix(0, base+1000),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("SearchTraceLogs: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(page.Entries))
	}
	if page.Entries[0].Line != "second error" || page.Entries[1].Line != "third error" {
		t.Fatalf("entries not ascending: %+v", page.Entries)
	}
	if page.Next != 0 {
		t.Fatalf("next = %d, want 0 (exhausted)", page.Next)
	}
}

func TestSearchTraceLogsSpanFilter(t *testing.T) {
	base := time.Now().Add(-time.Hour).UnixNano()
	records := []searchRecord{
		{ts: base + 100, spanID: "span-a", line: "a line"},
		{ts: base + 200, spanID: "span-b", line: "b line"},
	}
	srv := lokiSearchServer(t, records)
	defer srv.Close()

	client := NewLogsClient(srv.URL)
	page, err := client.SearchTraceLogs(context.Background(), searchTraceID, domain.LogSearchRequest{
		SpanIDs: []string{"span-b"},
		Start:   time.Unix(0, base),
		End:     time.Unix(0, base+1000),
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("SearchTraceLogs: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].SpanID != "span-b" {
		t.Fatalf("entries = %+v, want only span-b", page.Entries)
	}
}

func TestSearchTraceLogsPagination(t *testing.T) {
	base := time.Now().Add(-time.Hour).UnixNano()
	records := []searchRecord{
		{ts: base + 100, spanID: "span-a", line: "one"},
		{ts: base + 200, spanID: "span-a", line: "two"},
		{ts: base + 300, spanID: "span-a", line: "three"},
	}
	srv := lokiSearchServer(t, records)
	defer srv.Close()

	client := NewLogsClient(srv.URL)
	req := domain.LogSearchRequest{
		Start: time.Unix(0, base),
		End:   time.Unix(0, base+1000),
		Limit: 1,
	}

	first, err := client.SearchTraceLogs(context.Background(), searchTraceID, req)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Entries) != 1 || first.Entries[0].Line != "one" {
		t.Fatalf("first page = %+v, want [one]", first.Entries)
	}
	if first.Next == 0 {
		t.Fatal("first page next = 0, want a cursor")
	}

	req.Cursor = first.Next
	second, err := client.SearchTraceLogs(context.Background(), searchTraceID, req)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Entries) != 1 || second.Entries[0].Line != "two" {
		t.Fatalf("second page = %+v, want [two]", second.Entries)
	}
	if second.Next == 0 {
		t.Fatal("second page next = 0, want a cursor")
	}

	req.Cursor = second.Next
	third, err := client.SearchTraceLogs(context.Background(), searchTraceID, req)
	if err != nil {
		t.Fatalf("third page: %v", err)
	}
	if len(third.Entries) != 1 || third.Entries[0].Line != "three" {
		t.Fatalf("third page = %+v, want [three]", third.Entries)
	}
	if third.Next == 0 {
		t.Fatal("third page next = 0, want a cursor (the pager cannot know the stream ended)")
	}

	// A follow-up query past the last entry returns an empty page and no cursor.
	req.Cursor = third.Next
	fourth, err := client.SearchTraceLogs(context.Background(), searchTraceID, req)
	if err != nil {
		t.Fatalf("fourth page: %v", err)
	}
	if len(fourth.Entries) != 0 {
		t.Fatalf("fourth page = %+v, want empty", fourth.Entries)
	}
	if fourth.Next != 0 {
		t.Fatalf("fourth page next = %d, want 0 (exhausted)", fourth.Next)
	}
}

func TestSearchTraceLogsLimitCap(t *testing.T) {
	base := time.Now().Add(-time.Hour).UnixNano()
	var records []searchRecord
	for i := 0; i < 5; i++ {
		records = append(records, searchRecord{ts: base + int64(i*100), spanID: "span-a", line: fmt.Sprintf("line-%d", i)})
	}
	srv := lokiSearchServer(t, records)
	defer srv.Close()

	client := NewLogsClient(srv.URL)
	page, err := client.SearchTraceLogs(context.Background(), searchTraceID, domain.LogSearchRequest{
		Start: time.Unix(0, base),
		End:   time.Unix(0, base+1000),
		Limit: 2,
	})
	if err != nil {
		t.Fatalf("SearchTraceLogs: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(page.Entries))
	}
	if page.Next == 0 {
		t.Fatal("next = 0, want a cursor for the remaining entries")
	}
}

func TestSearchTraceLogsEmptyResult(t *testing.T) {
	base := time.Now().Add(-time.Hour).UnixNano()
	srv := lokiSearchServer(t, nil)
	defer srv.Close()

	client := NewLogsClient(srv.URL)
	page, err := client.SearchTraceLogs(context.Background(), searchTraceID, domain.LogSearchRequest{
		Start: time.Unix(0, base),
		End:   time.Unix(0, base+1000),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("SearchTraceLogs: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(page.Entries))
	}
	if page.Next != 0 {
		t.Fatalf("next = %d, want 0", page.Next)
	}
}

func TestSearchTraceLogsInvalidRegex(t *testing.T) {
	client := NewLogsClient("http://loki:3100")
	_, err := client.SearchTraceLogs(context.Background(), searchTraceID, domain.LogSearchRequest{
		Query: "([",
		Mode:  domain.LogSearchRegex,
		Start: time.Now().Add(-time.Hour),
		End:   time.Now(),
		Limit: 10,
	})
	if !errors.Is(err, domain.ErrInvalidRegex) {
		t.Fatalf("err = %v, want ErrInvalidRegex", err)
	}
}

func TestSearchTraceLogsInvalidTraceID(t *testing.T) {
	client := NewLogsClient("http://loki:3100")
	_, err := client.SearchTraceLogs(context.Background(), "not-hex!", domain.LogSearchRequest{})
	if err == nil || !strings.Contains(err.Error(), "invalid trace ID") {
		t.Fatalf("err = %v, want invalid trace ID", err)
	}
}

func TestSearchTraceLogsUnconfigured(t *testing.T) {
	client := NewLogsClient("")
	_, err := client.SearchTraceLogs(context.Background(), searchTraceID, domain.LogSearchRequest{})
	if err == nil || !strings.Contains(err.Error(), "loki URL not configured") {
		t.Fatalf("err = %v, want loki URL not configured", err)
	}
}

func TestSearchTraceLogsLokiError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewLogsClient(srv.URL)
	_, err := client.SearchTraceLogs(context.Background(), searchTraceID, domain.LogSearchRequest{
		Start: time.Now().Add(-time.Hour),
		End:   time.Now(),
		Limit: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want status 500", err)
	}
}
