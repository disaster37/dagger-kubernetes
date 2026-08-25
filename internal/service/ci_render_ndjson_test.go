package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type flushBuffer struct {
	bytes.Buffer
	flushed int
}

func (f *flushBuffer) Flush() error {
	f.flushed++
	return nil
}

func TestNDJSONEventSinkEmitsOneJSONObjectPerLine(t *testing.T) {
	var buf flushBuffer
	sink := NewNDJSONEventSink(&buf)

	e := domain.CIEvent{
		Seq:       1,
		Type:      domain.CIEventNodeStarted,
		TraceID:   "trace-1",
		Timestamp: time.Unix(100, 0),
		Node: &domain.StepNode{
			ID: "r", Name: "build", State: domain.StepStateRunning, Depth: 0,
		},
	}
	if err := sink.Emit(&e); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("output = %q, want exactly 1 line", buf.String())
	}

	var got domain.CIEvent
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}
	if got.Seq != 1 || got.Type != domain.CIEventNodeStarted || got.Node.ID != "r" || got.Node.State != domain.StepStateRunning {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Emitting a second event appends a second line.
	e2 := domain.CIEvent{Seq: 2, Type: domain.CIEventPipelineDone, TraceID: "trace-1", Timestamp: time.Unix(101, 0), Status: "success"}
	if err := sink.Emit(&e2); err != nil {
		t.Fatalf("Emit 2: %v", err)
	}
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Fatalf("output = %q, want 2 lines", buf.String())
	}
}

func TestNDJSONEventSinkFlushForwardsToFlusher(t *testing.T) {
	var buf flushBuffer
	sink := NewNDJSONEventSink(&buf)

	if buf.flushed != 0 {
		t.Fatalf("flushed = %d before emit", buf.flushed)
	}
	if err := sink.Emit(&domain.CIEvent{Type: domain.CIEventPipelineDone, Status: "success"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if buf.flushed != 1 {
		t.Fatalf("flushed = %d after emit, want 1", buf.flushed)
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if buf.flushed != 2 {
		t.Fatalf("flushed = %d after flush, want 2", buf.flushed)
	}
}

func TestNDJSONEventSinkNoFlusher(t *testing.T) {
	// A plain bytes.Buffer does not implement Flush(); Emit/Flush must be no-ops
	// for the flush step and still write the JSON line.
	var buf bytes.Buffer
	sink := NewNDJSONEventSink(&buf)
	if err := sink.Emit(&domain.CIEvent{Type: domain.CIEventPipelineDone, Status: "success"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("output = %q, want trailing newline", buf.String())
	}
}

func TestNDJSONEventSinkExactSerialization(t *testing.T) {
	var buf bytes.Buffer
	sink := NewNDJSONEventSink(&buf)
	ts := time.Unix(1700000000, 0).UTC()

	e := domain.CIEvent{
		Seq:       3,
		Type:      domain.CIEventLogChunk,
		TraceID:   "abcdef",
		Timestamp: ts,
		Log: &domain.LogChunk{
			NodeID:    "n1",
			Timestamp: ts,
			Lines:     []string{"a", "b"},
		},
	}
	if err := sink.Emit(&e); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Empty-able fields are omitted in the wire format.
	if strings.Contains(buf.String(), "\"node\"") || strings.Contains(buf.String(), "\"status\"") || strings.Contains(buf.String(), "\"error\"") {
		t.Fatalf("output = %q, want no node/status/error keys", buf.String())
	}
	sc := bufio.NewScanner(&buf)
	if !sc.Scan() {
		t.Fatal("no line written")
	}
	var m map[string]any
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "log_chunk" || m["seq"] != float64(3) {
		t.Fatalf("m = %v", m)
	}
	log, ok := m["log"].(map[string]any)
	if !ok {
		t.Fatalf("log field = %v", m["log"])
	}
	lines, _ := log["lines"].([]any)
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want 2", lines)
	}
}

type failWrite struct{}

func (failWrite) Write([]byte) (int, error) { return 0, errors.New("write boom") }

type failFlush struct{ bytes.Buffer }

func (f *failFlush) Flush() error { return errors.New("flush boom") }

func TestNDJSONEventSinkEncodeError(t *testing.T) {
	sink := NewNDJSONEventSink(failWrite{})
	err := sink.Emit(&domain.CIEvent{Type: domain.CIEventPipelineDone, Status: "success"})
	if err == nil || !strings.Contains(err.Error(), "encode ci event") {
		t.Fatalf("Emit err = %q, want encode error", err)
	}
}

func TestNDJSONEventSinkFlushError(t *testing.T) {
	var fw failFlush
	sink := NewNDJSONEventSink(&fw)

	// Emit's flush step fails and wraps the error.
	err := sink.Emit(&domain.CIEvent{Type: domain.CIEventPipelineDone, Status: "success"})
	if err == nil || !strings.Contains(err.Error(), "flush ci event") {
		t.Fatalf("Emit err = %q, want flush error", err)
	}

	// Flush directly fails too.
	if err := sink.Flush(); err == nil || !strings.Contains(err.Error(), "flush ci event stream") {
		t.Fatalf("Flush err = %q, want flush error", err)
	}
}
