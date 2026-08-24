package repository

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestSupervisorTraceClientGetTrace(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/traces/abcdef0123456789" {
			t.Errorf("path = %q, want /api/v1/traces/abcdef0123456789", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(domain.TraceInfo{TraceID: "abcdef0123456789", Status: "success"})
	}))
	defer srv.Close()

	c := NewSupervisorTraceClient(srv.URL, "tok", time.Second)
	trace, err := c.GetTrace("abcdef0123456789")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if trace.TraceID != "abcdef0123456789" || trace.Status != "success" {
		t.Fatalf("trace = %+v", trace)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want Bearer tok", gotAuth)
	}
}

func TestSupervisorTraceClientGetTraceNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	c := NewSupervisorTraceClient(srv.URL, "tok", time.Second)
	_, err := c.GetTrace("abcdef0123456789")
	if err == nil {
		t.Fatal("GetTrace = nil error, want wrapped error")
	}
	if !strings.Contains(err.Error(), "get trace abcdef0123456789") || !strings.Contains(err.Error(), "server returned 404") {
		t.Fatalf("err = %q", err)
	}
}

func TestSupervisorTraceClientGetTraceInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewSupervisorTraceClient(srv.URL, "tok", time.Second)
	_, err := c.GetTrace("abcdef0123456789")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %q, want decode error", err)
	}
}

func TestSupervisorTraceClientQueryTraceLogs(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"trace_id": "abcdef0123456789",
			"entries": []domain.LogEntry{
				{Timestamp: time.Unix(100, 0), Line: "hello", SpanID: "s1"},
			},
		})
	}))
	defer srv.Close()

	c := NewSupervisorTraceClient(srv.URL, "tok", time.Second)
	start := time.Unix(90, 0)
	end := time.Unix(110, 0)
	entries, err := c.QueryTraceLogs("abcdef0123456789", start, end, 500)
	if err != nil {
		t.Fatalf("QueryTraceLogs: %v", err)
	}
	if len(entries) != 1 || entries[0].Line != "hello" || entries[0].SpanID != "s1" {
		t.Fatalf("entries = %+v", entries)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	// url.Values.Encode sorts keys and percent-encodes; integer nanos are
	// emitted verbatim, so the encoded query contains the raw bounds.
	for _, want := range []string{"start=90000000000", "end=110000000000", "limit=500"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query = %q, want containing %q", gotQuery, want)
		}
	}
}

func TestSupervisorTraceClientQueryTraceLogsOmitsZeroParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": []domain.LogEntry{}})
	}))
	defer srv.Close()

	c := NewSupervisorTraceClient(srv.URL, "tok", time.Second)
	if _, err := c.QueryTraceLogs("abcdef0123456789", time.Time{}, time.Time{}, 0); err != nil {
		t.Fatalf("QueryTraceLogs: %v", err)
	}
	if gotQuery != "" {
		t.Fatalf("query = %q, want empty", gotQuery)
	}
}

func TestSupervisorTraceClientQueryTraceLogsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewSupervisorTraceClient(srv.URL, "tok", time.Second)
	_, err := c.QueryTraceLogs("abcdef0123456789", time.Time{}, time.Time{}, 0)
	if err == nil || !strings.Contains(err.Error(), "query trace logs abcdef0123456789") || !strings.Contains(err.Error(), "server returned 500") {
		t.Fatalf("err = %q", err)
	}
}

func TestSupervisorTraceClientTrimsBaseURLSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(domain.TraceInfo{TraceID: "abcdef0123456789"})
	}))
	defer srv.Close()

	c := NewSupervisorTraceClient(srv.URL+"/", "tok", time.Second)
	if _, err := c.GetTrace("abcdef0123456789"); err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if gotPath != "/api/v1/traces/abcdef0123456789" {
		t.Fatalf("path = %q, want /api/v1/traces/abcdef0123456789", gotPath)
	}
}

func TestSupervisorTraceClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	c := NewSupervisorTraceClient(srv.URL, "tok", 20*time.Millisecond)
	_, err := c.GetTrace("abcdef0123456789")
	if err == nil {
		t.Fatal("GetTrace = nil error, want timeout error")
	}
	if !strings.Contains(err.Error(), "get trace abcdef0123456789") {
		t.Fatalf("err = %q", err)
	}
}

func TestSupervisorTraceClientBuildRequestError(t *testing.T) {
	c := NewSupervisorTraceClient("://bad", "tok", time.Second)
	_, err := c.GetTrace("abcdef0123456789")
	if err == nil {
		t.Fatal("GetTrace = nil error, want build-request error")
	}
	if !strings.Contains(err.Error(), "build request") {
		t.Fatalf("err = %q", err)
	}
}

// TestSupervisorTraceClientRejectsInvalidTraceID proves path-tampering inputs
// (traversal, query strings, oversized hex) are rejected before any URL is
// built or a request is sent (CWE-20/CWE-918).
func TestSupervisorTraceClientRejectsInvalidTraceID(t *testing.T) {
	c := NewSupervisorTraceClient("http://127.0.0.1:1", "tok", time.Second)
	for _, bad := range []string{"", "../etc/passwd", "abc?x=1", "not-hex", "short", strings.Repeat("a", 129)} {
		if _, err := c.GetTrace(bad); err == nil || !strings.Contains(err.Error(), "invalid trace id") {
			t.Fatalf("GetTrace(%q) err = %v, want invalid trace id", bad, err)
		}
		if _, err := c.QueryTraceLogs(bad, time.Time{}, time.Time{}, 0); err == nil || !strings.Contains(err.Error(), "invalid trace id") {
			t.Fatalf("QueryTraceLogs(%q) err = %v, want invalid trace id", bad, err)
		}
	}
}

// TestSupervisorTraceClientResponseSizeCap proves an oversized response body
// is truncated and fails to decode instead of exhausting memory (CWE-400).
func TestSupervisorTraceClientResponseSizeCap(t *testing.T) {
	old := maxSupervisorResponseBytes
	maxSupervisorResponseBytes = 8
	defer func() { maxSupervisorResponseBytes = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"trace_id":"abcdef0123456789","status":"success"}`))
	}))
	defer srv.Close()

	c := NewSupervisorTraceClient(srv.URL, "tok", time.Second)
	_, err := c.GetTrace("abcdef0123456789")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want decode error from truncated body", err)
	}
}
