package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// recordedPruneRequest captures one request the fake engine received.
type recordedPruneRequest struct {
	method string
	path   string
	query  string
	meta   engineClientMetadata
}

// fakeEngine emulates the engine's /query session surface for pruner tests.
type fakeEngine struct {
	mu       sync.Mutex
	requests []recordedPruneRequest
	status   int
	body     string
	location string // when set, answers with a Location header (redirect tests)
}

func (fe *fakeEngine) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(body, &payload)

	var meta engineClientMetadata
	if decoded, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Dagger-Client-Metadata")); err == nil {
		_ = json.Unmarshal(decoded, &meta)
	}

	fe.mu.Lock()
	fe.requests = append(fe.requests, recordedPruneRequest{
		method: r.Method,
		path:   r.URL.Path,
		query:  payload.Query,
		meta:   meta,
	})
	status, respBody, location := fe.status, fe.body, fe.location
	fe.mu.Unlock()

	if location != "" {
		w.Header().Set("Location", location)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(respBody))
}

func (fe *fakeEngine) lastRequest(t *testing.T) recordedPruneRequest {
	t.Helper()
	fe.mu.Lock()
	defer fe.mu.Unlock()
	if len(fe.requests) == 0 {
		t.Fatal("no request recorded")
	}
	return fe.requests[len(fe.requests)-1]
}

// newFakeEngine starts an httptest server and returns a pruner pointed at it
// (white-box: the unexported port field is overridden).
func newFakeEngine(t *testing.T, status int, body string) (*fakeEngine, *SessionEnginePruner) {
	t.Helper()
	fe := &fakeEngine{status: status, body: body}
	ts := httptest.NewServer(http.HandlerFunc(fe.handle))
	t.Cleanup(ts.Close)

	_, portStr, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	p := NewSessionEnginePruner()
	p.port = port
	return fe, p
}

func TestPruneSendsCorrectQueryAndMetadata(t *testing.T) {
	fe, p := newFakeEngine(t, http.StatusOK, `{"data":{"engine":{"localCache":{"prune":null}}}}`)

	if err := p.PruneLocalCache(context.Background(), "127.0.0.1", "v0.19.0"); err != nil {
		t.Fatalf("PruneLocalCache: %v", err)
	}

	req := fe.lastRequest(t)
	if req.method != http.MethodPost || req.path != "/query" {
		t.Fatalf("request = %s %s, want POST /query", req.method, req.path)
	}
	for _, want := range []string{"engine", "localCache", "prune", "useDefaultPolicy: false"} {
		if !strings.Contains(req.query, want) {
			t.Fatalf("query %q missing %q", req.query, want)
		}
	}
	if req.meta.ClientVersion != "v0.19.0" {
		t.Fatalf("client_version = %q, want v0.19.0", req.meta.ClientVersion)
	}
	if req.meta.ClientHostname != "supervisor" {
		t.Fatalf("client_hostname = %q, want supervisor", req.meta.ClientHostname)
	}
	ids := []string{req.meta.ClientID, req.meta.SessionID, req.meta.ClientSecretToken, req.meta.ClientStableID}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			t.Fatal("metadata id is empty")
		}
		if seen[id] {
			t.Fatalf("metadata id %q is not distinct", id)
		}
		seen[id] = true
	}
}

func TestPruneHappyPath(t *testing.T) {
	_, p := newFakeEngine(t, http.StatusOK, `{"data":{"engine":{"localCache":{"prune":null}}}}`)

	if err := p.PruneLocalCache(context.Background(), "127.0.0.1", "v0.19.0"); err != nil {
		t.Fatalf("PruneLocalCache: %v", err)
	}
}

func TestPruneGraphQLErrorSurfaced(t *testing.T) {
	_, p := newFakeEngine(t, http.StatusOK, `{"errors":[{"message":"field not found"}]}`)

	err := p.PruneLocalCache(context.Background(), "127.0.0.1", "v0.19.0")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "field not found") || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error = %v, want GraphQL message + pod IP", err)
	}
}

func TestPruneNon200Surfaced(t *testing.T) {
	_, p := newFakeEngine(t, http.StatusInternalServerError, "incompatible client version")

	err := p.PruneLocalCache(context.Background(), "127.0.0.1", "v0.19.0")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "incompatible client version") {
		t.Fatalf("error = %v, want status + body", err)
	}
}

func TestPruneNon200TruncatesBody(t *testing.T) {
	_, p := newFakeEngine(t, http.StatusInternalServerError, strings.Repeat("x", maxPruneErrorBody*2))

	err := p.PruneLocalCache(context.Background(), "127.0.0.1", "v0.19.0")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want truncated marker", err)
	}
	if len(err.Error()) > maxPruneErrorBody+256 {
		t.Fatalf("error length = %d, want bounded", len(err.Error()))
	}
}

func TestPruneRedirectNotFollowed(t *testing.T) {
	// The engine answers directly; the pruner must surface a 3xx as a per-pod
	// error instead of chasing the redirect (CWE-601).
	fe, p := newFakeEngine(t, http.StatusFound, "")
	fe.location = "/query"

	err := p.PruneLocalCache(context.Background(), "127.0.0.1", "v0.19.0")
	if err == nil {
		t.Fatal("expected error for 3xx response")
	}
	if !strings.Contains(err.Error(), "unexpected status 302") {
		t.Fatalf("error = %v, want unexpected status 302 (redirect must not be followed)", err)
	}
}

func TestPruneDialErrorSurfaced(t *testing.T) {
	// Bind then close to obtain a port nothing listens on. This is a unit test
	// (not an integration test), so the probe-then-close race is acceptable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	p := NewSessionEnginePruner()
	p.port = port

	err = p.PruneLocalCache(context.Background(), "127.0.0.1", "v0.19.0")
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error = %v, want pod IP", err)
	}
}

func TestPruneClientVersionParameterized(t *testing.T) {
	fe, p := newFakeEngine(t, http.StatusOK, `{"data":{"engine":{"localCache":{"prune":null}}}}`)

	for _, version := range []string{"v0.19.0", "v0.21.3"} {
		if err := p.PruneLocalCache(context.Background(), "127.0.0.1", version); err != nil {
			t.Fatalf("PruneLocalCache(%s): %v", version, err)
		}
	}

	fe.mu.Lock()
	defer fe.mu.Unlock()
	if len(fe.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(fe.requests))
	}
	if fe.requests[0].meta.ClientVersion != "v0.19.0" || fe.requests[1].meta.ClientVersion != "v0.21.3" {
		t.Fatalf("client versions = %q, %q, want v0.19.0, v0.21.3",
			fe.requests[0].meta.ClientVersion, fe.requests[1].meta.ClientVersion)
	}
}
