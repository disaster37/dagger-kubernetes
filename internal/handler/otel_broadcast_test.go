package handler

import (
	"bytes"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/disaster/dagger-kubernetes/internal/repository"
)

const testTraceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// mockConn is a minimal network.Conn whose write path is captured into an
// in-memory buffer so tests can observe SSE bytes without a real socket.
type mockConn struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	mallocs [][]byte
}

func (m *mockConn) Read([]byte) (int, error) { return 0, io.EOF }
func (m *mockConn) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(b)
}
func (m *mockConn) Close() error                        { return nil }
func (m *mockConn) LocalAddr() net.Addr                 { return mockAddr("local") }
func (m *mockConn) RemoteAddr() net.Addr                { return mockAddr("remote") }
func (m *mockConn) SetDeadline(time.Time) error         { return nil }
func (m *mockConn) SetReadDeadline(time.Time) error     { return nil }
func (m *mockConn) SetWriteDeadline(time.Time) error    { return nil }
func (m *mockConn) Peek(int) ([]byte, error)            { return nil, io.EOF }
func (m *mockConn) Skip(int) error                      { return nil }
func (m *mockConn) Release() error                      { return nil }
func (m *mockConn) Len() int                            { return 0 }
func (m *mockConn) ReadByte() (byte, error)             { return 0, io.EOF }
func (m *mockConn) ReadBinary(int) ([]byte, error)      { return nil, io.EOF }
func (m *mockConn) SetReadTimeout(time.Duration) error  { return nil }
func (m *mockConn) SetWriteTimeout(time.Duration) error { return nil }

func (m *mockConn) Malloc(n int) ([]byte, error) {
	b := make([]byte, n)
	m.mu.Lock()
	m.mallocs = append(m.mallocs, b)
	m.mu.Unlock()
	return b, nil
}

func (m *mockConn) WriteBinary(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(b)
}

// Flush copies every Malloc'd slice (which the caller wrote into in place)
// into the capture buffer, mirroring the netpoll ring-buffer flush.
func (m *mockConn) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.mallocs {
		m.buf.Write(b)
	}
	m.mallocs = m.mallocs[:0]
	return nil
}

func (m *mockConn) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.String()
}

type mockAddr string

func (a mockAddr) Network() string { return string(a) }
func (a mockAddr) String() string  { return string(a) }

func decodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

func otlpTraceBody(t *testing.T, traceIDHex string) []byte {
	t.Helper()
	rs := &tracepb.ResourceSpans{
		ScopeSpans: []*tracepb.ScopeSpans{
			{Spans: []*tracepb.Span{{TraceId: decodeHex(t, traceIDHex), SpanId: decodeHex(t, "1111111111111111")}}},
		},
	}
	rsBytes, err := proto.Marshal(rs)
	if err != nil {
		t.Fatalf("marshal resource spans: %v", err)
	}
	body := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(body, rsBytes)
}

func otlpLogsBody(t *testing.T, traceIDHex string) []byte {
	t.Helper()
	rl := &logspb.ResourceLogs{
		ScopeLogs: []*logspb.ScopeLogs{
			{LogRecords: []*logspb.LogRecord{{TraceId: decodeHex(t, traceIDHex)}}},
		},
	}
	rlBytes, err := proto.Marshal(rl)
	if err != nil {
		t.Fatalf("marshal resource logs: %v", err)
	}
	body := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(body, rlBytes)
}

// subscribeCaptureClient registers a live SSE client backed by a mockConn so
// tests can assert on the bytes writePump emits.
func subscribeCaptureClient(t *testing.T, hub *repository.LiveHub, traceID string) *mockConn {
	t.Helper()
	conn := &mockConn{}
	c := app.NewContext(0)
	c.SetConn(conn)
	client := repository.NewLiveClient(c, traceID)
	hub.Subscribe(traceID, client)
	t.Cleanup(func() { hub.Unsubscribe(traceID, client) })
	return conn
}

func waitForString(t *testing.T, conn *mockConn, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(conn.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in SSE output; got: %q", want, conn.String())
}

func TestBroadcastOTelUpdateTraces(t *testing.T) {
	s, _ := newTestServer(t)
	conn := subscribeCaptureClient(t, s.liveHub, testTraceID)

	s.broadcastOTelUpdate("traces", otlpTraceBody(t, testTraceID))
	waitForString(t, conn, `"type":"trace_update"`, 2*time.Second)
}

func TestBroadcastOTelUpdateLogs(t *testing.T) {
	s, _ := newTestServer(t)
	conn := subscribeCaptureClient(t, s.liveHub, testTraceID)

	s.broadcastOTelUpdate("logs", otlpLogsBody(t, testTraceID))
	waitForString(t, conn, `"type":"logs_update"`, 2*time.Second)
}

func TestBroadcastOTelUpdateMetricsNoop(t *testing.T) {
	s, _ := newTestServer(t)
	conn := subscribeCaptureClient(t, s.liveHub, testTraceID)

	s.broadcastOTelUpdate("metrics", otlpTraceBody(t, testTraceID))

	// Give any (incorrect) write a moment to appear.
	time.Sleep(50 * time.Millisecond)
	if got := conn.String(); strings.Contains(got, "update") {
		t.Fatalf("metrics ingest must not broadcast, got: %q", got)
	}
}

func TestBroadcastOTelUpdateNilHub(t *testing.T) {
	s, _ := newTestServer(t)
	s.liveHub = nil
	// Must not panic.
	s.broadcastOTelUpdate("traces", otlpTraceBody(t, testTraceID))
	s.broadcastOTelUpdate("logs", otlpLogsBody(t, testTraceID))
}

func TestHandleOTelBroadcastsToLiveSubscriber(t *testing.T) {
	s, bearer := newTestServer(t)

	// Point the OTLP proxy at a local collector stub so handleOTel runs to
	// completion (broadcast is independent of proxy success, but this keeps
	// the request from erroring).
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	s.otelProxy = s.newHertzProxy(collector.URL, nil, "collector")

	e := route.NewEngine(config.NewOptions(nil))
	e.POST("/v1/traces", s.handleOTel("traces"))
	e.POST("/v1/logs", s.handleOTel("logs"))

	traceConn := subscribeCaptureClient(t, s.liveHub, testTraceID)

	traceBody := otlpTraceBody(t, testTraceID)
	resp := ut.PerformRequest(e, "POST", "/v1/traces", &ut.Body{
		Body: bytes.NewReader(traceBody),
		Len:  len(traceBody),
	}, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200 from traces ingest, got %d", resp.Result().StatusCode())
	}
	waitForString(t, traceConn, `"type":"trace_update"`, 2*time.Second)

	logsConn := subscribeCaptureClient(t, s.liveHub, testTraceID)

	logsBody := otlpLogsBody(t, testTraceID)
	resp = ut.PerformRequest(e, "POST", "/v1/logs", &ut.Body{
		Body: bytes.NewReader(logsBody),
		Len:  len(logsBody),
	}, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200 from logs ingest, got %d", resp.Result().StatusCode())
	}
	waitForString(t, logsConn, `"type":"logs_update"`, 2*time.Second)
}
