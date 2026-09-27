package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TestHandleTracesListLiveAuth proves the list-level SSE route enforces auth:
// an unauthenticated request is rejected with 401 before any subscription.
func TestHandleTracesListLiveAuth(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/live", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.Result().StatusCode())
	}
}

// TestHandleTracesListLiveStreams drives the handler directly with a capture
// conn: the subscription lands on domain.PipelinesTopic, broadcasts reach the
// connection, and cancelling the context tears the subscription down (the
// handler returns and later broadcasts never reach the conn again).
func TestHandleTracesListLiveStreams(t *testing.T) {
	env := newTestEnv(t)
	s := env.server
	bearer := env.loginAsAdmin(t)

	conn := &mockConn{}
	c := app.NewContext(0)
	c.SetConn(conn)
	c.Request.Header.Set("Authorization", bearer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleTracesListLive(ctx, c)
	}()

	// Broadcast until the subscriber is registered and the event lands (the
	// subscribe happens asynchronously relative to this loop).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(conn.String(), `"type":"pipelines_update"`) {
		s.liveHub.Broadcast(domain.PipelinesTopic, map[string]string{"type": "pipelines_update"})
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(conn.String(), `"type":"pipelines_update"`) {
		t.Fatalf("timed out waiting for pipelines_update; got: %q", conn.String())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	// Unsubscribe ran before the handler returned; after writePump drains the
	// queued events and exits, further broadcasts must not reach the conn.
	time.Sleep(200 * time.Millisecond)
	before := conn.String()
	s.liveHub.Broadcast(domain.PipelinesTopic, map[string]string{"type": "pipelines_update"})
	time.Sleep(100 * time.Millisecond)
	if after := conn.String(); after != before {
		t.Fatalf("subscription leaked after handler return: %q -> %q", before, after)
	}
}
