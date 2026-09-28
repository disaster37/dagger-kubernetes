package repository

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/network"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// captureConn is a minimal network.Conn whose write path is captured into an
// in-memory buffer so tests can observe SSE bytes without a real socket.
type captureConn struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	mallocs [][]byte
}

func (c *captureConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *captureConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(b)
}
func (c *captureConn) Close() error                    { return nil }
func (c *captureConn) LocalAddr() net.Addr             { return captureAddr("local") }
func (c *captureConn) RemoteAddr() net.Addr            { return captureAddr("remote") }
func (c *captureConn) SetDeadline(time.Time) error     { return nil }
func (c *captureConn) SetReadDeadline(time.Time) error { return nil }

func (c *captureConn) SetWriteDeadline(time.Time) error { return nil }
func (c *captureConn) Peek(int) ([]byte, error)         { return nil, io.EOF }
func (c *captureConn) Skip(int) error                   { return nil }
func (c *captureConn) Release() error                   { return nil }
func (c *captureConn) Len() int                         { return 0 }
func (c *captureConn) ReadByte() (byte, error)          { return 0, io.EOF }
func (c *captureConn) ReadBinary(int) ([]byte, error)   { return nil, io.EOF }
func (c *captureConn) SetReadTimeout(time.Duration) error {
	return nil
}
func (c *captureConn) SetWriteTimeout(time.Duration) error { return nil }

func (c *captureConn) Malloc(n int) ([]byte, error) {
	b := make([]byte, n)
	c.mu.Lock()
	c.mallocs = append(c.mallocs, b)
	c.mu.Unlock()
	return b, nil
}

func (c *captureConn) WriteBinary(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(b)
}

// Flush copies every Malloc'd slice (which the caller wrote into in place)
// into the capture buffer, mirroring the netpoll ring-buffer flush.
func (c *captureConn) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.mallocs {
		c.buf.Write(b)
	}
	c.mallocs = c.mallocs[:0]
	return nil
}

func (c *captureConn) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type captureAddr string

func (a captureAddr) Network() string { return string(a) }
func (a captureAddr) String() string  { return string(a) }

// failingFlushConn fails the write path so writePump's error branches are
// covered (a client that is gone mid-write).
type failingFlushConn struct{ captureConn }

func (c *failingFlushConn) Flush() error { return errors.New("write failed") }

// newCaptureClient subscribes a live client backed by a captureConn on key and
// returns the conn; the client is unsubscribed on cleanup.
func newCaptureClient(t *testing.T, hub *LiveHub, key string) *captureConn {
	t.Helper()
	conn := &captureConn{}
	newTestLiveClient(t, hub, key, conn)
	return conn
}

// newTestLiveClient builds a LiveClient bound to conn and subscribes it; the
// client is unsubscribed when the test ends.
func newTestLiveClient(t *testing.T, hub *LiveHub, key string, conn network.Conn) *LiveClient {
	t.Helper()
	c := app.NewContext(0)
	c.SetConn(conn)
	client := NewLiveClient(c, key)
	hub.Subscribe(key, client)
	t.Cleanup(func() { hub.Unsubscribe(key, client) })
	return client
}

func waitForSSE(t *testing.T, conn *captureConn, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(conn.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in SSE output; got: %q", want, conn.String())
}

// TestLiveHubPipelinesTopicStreams proves the pipelines-overview sentinel key
// fans out through the same Subscribe/Broadcast/writePump path as per-trace
// keys and the SSE bytes reach the subscriber's connection.
func TestLiveHubPipelinesTopicStreams(t *testing.T) {
	hub := NewLiveHub()
	conn := newCaptureClient(t, hub, domain.PipelinesTopic)

	hub.Broadcast(domain.PipelinesTopic, map[string]string{"type": "pipelines_update"})

	waitForSSE(t, conn, `{"type":"pipelines_update"}`)
}

// TestLiveHubBroadcastFullBufferSkips proves Broadcast never blocks on a
// subscriber whose 256-slot Send buffer is full (slow-consumer skip).
func TestLiveHubBroadcastFullBufferSkips(t *testing.T) {
	hub := NewLiveHub()
	client := &LiveClient{
		TraceID: domain.PipelinesTopic,
		Send:    make(chan []byte, 256),
		done:    make(chan struct{}),
	}
	hub.mu.Lock()
	hub.clients[domain.PipelinesTopic] = map[*LiveClient]bool{client: true}
	hub.mu.Unlock()
	for i := 0; i < cap(client.Send); i++ {
		client.Send <- []byte("queued")
	}

	done := make(chan struct{})
	go func() {
		hub.Broadcast(domain.PipelinesTopic, map[string]string{"type": "pipelines_update"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast blocked on a full Send buffer")
	}
	if got := len(client.Send); got != cap(client.Send) {
		t.Fatalf("Send length = %d, want %d (event dropped, not queued)", got, cap(client.Send))
	}
}

// TestLiveHubBroadcastMarshalError covers the JSON-marshalling failure path:
// an unmarshalable event is dropped without reaching subscribers.
func TestLiveHubBroadcastMarshalError(t *testing.T) {
	hub := NewLiveHub()
	conn := newCaptureClient(t, hub, domain.PipelinesTopic)

	hub.Broadcast(domain.PipelinesTopic, make(chan int))

	// Only keep-alives may appear; the unmarshalable event must be dropped.
	time.Sleep(50 * time.Millisecond)
	if got := conn.String(); strings.Contains(got, "pipelines_update") {
		t.Fatalf("marshal-failed event must not be written, got: %q", got)
	}
}

// TestLiveHubUnsubscribeClosesAndRemoves proves Unsubscribe deletes the topic
// map entry, closes Send so writePump exits and closes done, and is safe to
// call twice (the second call must not double-close).
func TestLiveHubUnsubscribeClosesAndRemoves(t *testing.T) {
	hub := NewLiveHub()
	conn := &captureConn{}
	client := newTestLiveClient(t, hub, domain.PipelinesTopic, conn)
	hub.Broadcast(domain.PipelinesTopic, map[string]string{"type": "pipelines_update"})
	waitForSSE(t, conn, "pipelines_update")

	hub.mu.RLock()
	_, subscribed := hub.clients[domain.PipelinesTopic]
	hub.mu.RUnlock()
	if !subscribed {
		t.Fatal("expected the pipelines topic entry while subscribed")
	}

	hub.Unsubscribe(domain.PipelinesTopic, client)

	hub.mu.RLock()
	_, remaining := hub.clients[domain.PipelinesTopic]
	hub.mu.RUnlock()
	if remaining {
		t.Fatal("expected the topic map entry to be removed on last unsubscribe")
	}

	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("writePump did not exit after Unsubscribe")
	}

	// Idempotent: the entry is gone, so no second close of Send.
	hub.Unsubscribe(domain.PipelinesTopic, client)
}

// TestLiveHubKeepAliveOnTick proves writePump emits the keep-alive comment on
// the ticker cadence (keeps nginx proxy_read_timeout happy for idle streams).
func TestLiveHubKeepAliveOnTick(t *testing.T) {
	hub := NewLiveHub()
	hub.keepAlive = 10 * time.Millisecond
	conn := newCaptureClient(t, hub, domain.PipelinesTopic)

	waitForSSE(t, conn, ":keep-alive")
}

// TestLiveHubWritePumpExitsOnEventWriteError proves a connection that starts
// failing mid-stream tears the writePump down (done closed) instead of
// spinning.
func TestLiveHubWritePumpExitsOnEventWriteError(t *testing.T) {
	hub := NewLiveHub()
	client := newTestLiveClient(t, hub, domain.PipelinesTopic, &failingFlushConn{})

	hub.Broadcast(domain.PipelinesTopic, map[string]string{"type": "pipelines_update"})

	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("writePump did not exit after a write error")
	}
}

// TestLiveHubWritePumpExitsOnKeepAliveWriteError covers the ticker branch's
// write-error path.
func TestLiveHubWritePumpExitsOnKeepAliveWriteError(t *testing.T) {
	hub := NewLiveHub()
	hub.keepAlive = 10 * time.Millisecond
	client := newTestLiveClient(t, hub, domain.PipelinesTopic, &failingFlushConn{})

	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("writePump did not exit after a keep-alive write error")
	}
}
