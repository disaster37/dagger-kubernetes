package service

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestPlaintextEventSinkNodeStarted(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)

	e := domain.CIEvent{
		Seq:     1,
		Type:    domain.CIEventNodeStarted,
		TraceID: "trace-1",
		Node: &domain.StepNode{
			ID: "r", Name: "build", State: domain.StepStateRunning, Depth: 0,
		},
	}
	if err := sink.Emit(&e); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	want := "\u25b6 build"
	if got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPlaintextEventSinkNodeFinished(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)

	e := domain.CIEvent{
		Seq:     2,
		Type:    domain.CIEventNodeFinished,
		TraceID: "trace-1",
		Node: &domain.StepNode{
			ID: "r", Name: "build", State: domain.StepStateSucceeded, Depth: 0,
		},
	}
	if err := sink.Emit(&e); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	want := "\u25c0 build (succeeded)"
	if got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPlaintextEventSinkNodeFinishedWithError(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)

	e := domain.CIEvent{
		Seq:     3,
		Type:    domain.CIEventNodeFinished,
		TraceID: "trace-1",
		Error:   "unit test failed",
		Node: &domain.StepNode{
			ID: "r", Name: "test", State: domain.StepStateFailed, Depth: 1,
		},
	}
	if err := sink.Emit(&e); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if !strings.Contains(got, "test") || !strings.Contains(got, "failed") || !strings.Contains(got, "unit test failed") {
		t.Fatalf("output = %q, want test (failed): unit test failed", got)
	}
}

func TestPlaintextEventSinkLogChunk(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)

	ts := time.Unix(1700000000, 0).UTC()
	e := domain.CIEvent{
		Seq:       4,
		Type:      domain.CIEventLogChunk,
		TraceID:   "trace-1",
		Timestamp: ts,
		Log: &domain.LogChunk{
			NodeID:    "n1",
			Timestamp: ts,
			Lines:     []string{"Building...", "Compiling..."},
		},
	}
	if err := sink.Emit(&e); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("output = %q, want 2 lines", buf.String())
	}
	if lines[0] != "Building..." || lines[1] != "Compiling..." {
		t.Fatalf("lines = %v, want [Building... Compiling...]", lines)
	}
}

func TestPlaintextEventSinkPipelineDone(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)

	e := domain.CIEvent{
		Seq:     5,
		Type:    domain.CIEventPipelineDone,
		TraceID: "trace-1",
		Status:  "success",
	}
	if err := sink.Emit(&e); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	want := "Pipeline: success"
	if got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPlaintextEventSinkPipelineDoneWithError(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)

	e := domain.CIEvent{
		Seq:     6,
		Type:    domain.CIEventPipelineDone,
		TraceID: "trace-1",
		Status:  "failed",
		Error:   "exit status 1",
	}
	if err := sink.Emit(&e); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if !strings.Contains(got, "Pipeline: failed") || !strings.Contains(got, "exit status 1") {
		t.Fatalf("output = %q, want Pipeline: failed (exit status 1)", got)
	}
}

func TestPlaintextEventSinkMultipleEvents(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)

	events := []domain.CIEvent{
		{Seq: 1, Type: domain.CIEventNodeStarted, TraceID: "t", Node: &domain.StepNode{ID: "a", Name: "build", State: domain.StepStateRunning}},
		{Seq: 2, Type: domain.CIEventLogChunk, TraceID: "t", Log: &domain.LogChunk{NodeID: "a", Lines: []string{"hello"}}},
		{Seq: 3, Type: domain.CIEventNodeFinished, TraceID: "t", Node: &domain.StepNode{ID: "a", Name: "build", State: domain.StepStateSucceeded}},
		{Seq: 4, Type: domain.CIEventPipelineDone, TraceID: "t", Status: "success"},
	}
	for i := range events {
		if err := sink.Emit(&events[i]); err != nil {
			t.Fatalf("Emit[%d]: %v", i, err)
		}
	}

	out := buf.String()
	if !strings.Contains(out, "\u25b6 build") {
		t.Fatalf("missing node_started: %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("missing log line: %q", out)
	}
	if !strings.Contains(out, "\u25c0 build (succeeded)") {
		t.Fatalf("missing node_finished: %q", out)
	}
	if !strings.Contains(out, "Pipeline: success") {
		t.Fatalf("missing pipeline_done: %q", out)
	}
}

func TestPlaintextEventSinkFlushForwards(t *testing.T) {
	var buf flushBuffer
	sink := NewPlaintextEventSink(&buf)

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

func TestPlaintextEventSinkNoFlusher(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)
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

func TestPlaintextEventSinkNilNode(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)

	if err := sink.Emit(&domain.CIEvent{Type: domain.CIEventNodeStarted, TraceID: "t"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("output = %q, want empty for nil node", buf.String())
	}
}

func TestPlaintextEventSinkNilLog(t *testing.T) {
	var buf bytes.Buffer
	sink := NewPlaintextEventSink(&buf)

	if err := sink.Emit(&domain.CIEvent{Type: domain.CIEventLogChunk, TraceID: "t"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("output = %q, want empty for nil log", buf.String())
	}
}

func TestPlaintextEventSinkWriteError(t *testing.T) {
	sink := NewPlaintextEventSink(failWrite{})
	err := sink.Emit(&domain.CIEvent{Type: domain.CIEventPipelineDone, Status: "success"})
	if err == nil || !strings.Contains(err.Error(), "write plaintext event") {
		t.Fatalf("Emit err = %q, want write error", err)
	}
}

func TestPlaintextEventSinkFlushError(t *testing.T) {
	var fw failFlush
	sink := NewPlaintextEventSink(&fw)

	err := sink.Emit(&domain.CIEvent{Type: domain.CIEventPipelineDone, Status: "success"})
	if err == nil || !strings.Contains(err.Error(), "flush plaintext event") {
		t.Fatalf("Emit err = %q, want flush error", err)
	}

	if err := sink.Flush(); err == nil || !strings.Contains(err.Error(), "flush plaintext event stream") {
		t.Fatalf("Flush err = %q, want flush error", err)
	}
}
