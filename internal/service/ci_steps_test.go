package service

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func span(id, parent, name, status string, attrs map[string]string, children ...*domain.SpanNode) *domain.SpanNode {
	return &domain.SpanNode{
		SpanID:       id,
		ParentSpanID: parent,
		Name:         name,
		Status:       status,
		Attributes:   attrs,
		Children:     children,
	}
}

func tr(root *domain.SpanNode, status string) *domain.TraceInfo {
	return &domain.TraceInfo{TraceID: "trace-1", RootSpan: root, Status: status}
}

func logEntry(ts time.Time, spanID, line string) domain.LogEntry {
	return domain.LogEntry{Timestamp: ts, SpanID: spanID, Line: line}
}

// summarize flattens events into a compact, order-sensitive string form for
// assertions.
func summarize(events []domain.CIEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		switch e.Type {
		case domain.CIEventNodeStarted:
			out = append(out, fmt.Sprintf("started:%s:%s:%s:%d", e.Node.ID, e.Node.State, e.Node.ParentID, e.Node.Depth))
		case domain.CIEventNodeFinished:
			out = append(out, fmt.Sprintf("finished:%s:%s:%s", e.Node.ID, e.Node.State, e.Error))
		case domain.CIEventLogChunk:
			out = append(out, fmt.Sprintf("log:%s:%v", e.Log.NodeID, e.Log.Lines))
		case domain.CIEventPipelineDone:
			out = append(out, fmt.Sprintf("done:%s:%s", e.Status, e.Error))
		default:
			out = append(out, fmt.Sprintf("unknown:%s", e.Type))
		}
	}
	return out
}

func assertEvents(t *testing.T, got []domain.CIEvent, want []string) {
	t.Helper()
	if g := summarize(got); !reflect.DeepEqual(g, want) {
		t.Fatalf("events = %v, want %v", g, want)
	}
}

func TestAdvanceNilTrace(t *testing.T) {
	b := NewStepEventBuilder(0)
	_, err := b.Advance(nil, nil)
	if err == nil {
		t.Fatal("Advance(nil) = nil error, want wrapped error")
	}
	if !strings.Contains(err.Error(), "nil trace") {
		t.Fatalf("err = %q, want containing %q", err, "nil trace")
	}
}

func TestAdvanceNoRootEmitsNothing(t *testing.T) {
	b := NewStepEventBuilder(0)
	events, err := b.Advance(tr(nil, "running"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want empty", summarize(events))
	}
}

func TestAdvanceUninitializedBuilder(t *testing.T) {
	// A zero-value builder (seen nil) must self-initialize.
	var b StepEventBuilder
	root := span("r", "", "build", "running", nil)
	events, err := b.Advance(tr(root, "running"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{"started:r:running::0"})
}

func TestAdvanceNodeStartedThenFinished(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)
	root.StartTime = time.Unix(100, 0)

	events, err := b.Advance(tr(root, "running"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{"started:r:running::0"})

	// Unchanged snapshot -> idempotent (empty).
	events, err = b.Advance(tr(root, "running"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want empty", summarize(events))
	}

	root.Status = "success"
	root.Duration = 3 * time.Second
	events, err = b.Advance(tr(root, "success"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{"finished:r:succeeded:", "done:success:"})

	// pipeline_done emitted at most once.
	events, err = b.Advance(tr(root, "success"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want empty", summarize(events))
	}
}

func TestAdvanceFailedCarriesError(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "failed", map[string]string{"error": "boom"})

	events, err := b.Advance(tr(root, "failed"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{
		"started:r:running::0",
		"finished:r:failed:boom",
		"done:failed:boom",
	})
}

func TestAdvanceDuplicateSpanIDsDedupe(t *testing.T) {
	b := NewStepEventBuilder(0)
	dup := span("dup", "r", "op", "running", nil)
	root := span("r", "", "build", "running", nil, dup, dup) // same child twice

	events, err := b.Advance(tr(root, "running"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{
		"started:r:running::0",
		"started:dup:running:r:1",
	})
}

func TestAdvanceOrphanReparentedToRoot(t *testing.T) {
	b := NewStepEventBuilder(0)
	orphan := span("o", "missing", "orphan", "running", nil)
	root := span("r", "", "build", "running", nil, orphan)

	events, err := b.Advance(tr(root, "running"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// The orphan's ParentID ("missing") is absent -> re-parented to root.
	assertEvents(t, events, []string{
		"started:r:running::0",
		"started:o:running:r:1",
	})
}

func TestAdvanceDepthClampFoldsNodesAndLogs(t *testing.T) {
	b := NewStepEventBuilder(1)
	grand := span("g", "c", "deep", "running", nil)
	child := span("c", "r", "mid", "running", nil, grand)
	root := span("r", "", "build", "running", nil, child)

	ts := time.Unix(100, 0)
	logs := []domain.LogEntry{logEntry(ts, "g", "deep log")}

	events, err := b.Advance(tr(root, "running"), logs)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// grand is folded; its log is attributed to its deepest emitted ancestor c.
	assertEvents(t, events, []string{
		"started:r:running::0",
		"started:c:running:r:1",
		"log:c:[deep log]",
	})
}

func TestAdvanceEmptyIDChildSkipped(t *testing.T) {
	b := NewStepEventBuilder(0)
	grand := span("g", "r", "grand", "running", nil)
	empty := span("", "r", "", "running", nil, grand)
	root := span("r", "", "build", "running", nil, empty)

	events, err := b.Advance(tr(root, "running"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{
		"started:r:running::0",
		"started:g:running:r:1", // empty-id child skipped; grandchild emitted at depth 1
	})
}

func TestAdvanceNilChildSkipped(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil, nil) // one nil child

	events, err := b.Advance(tr(root, "running"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{"started:r:running::0"})
}

func TestAdvanceLogWatermark(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)

	ts1 := time.Unix(100, 0)
	events, err := b.Advance(tr(root, "running"), []domain.LogEntry{logEntry(ts1, "r", "line1")})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{
		"started:r:running::0",
		"log:r:[line1]",
	})

	// Same logs again -> dropped by the watermark.
	events, err = b.Advance(tr(root, "running"), []domain.LogEntry{logEntry(ts1, "r", "line1")})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want empty", summarize(events))
	}

	// A record sharing the watermark timestamp but with new content is emitted
	// (it is a distinct entry, not a duplicate); a strictly-older timestamp is
	// still dropped.
	ts2 := ts1.Add(time.Second)
	events, err = b.Advance(tr(root, "running"), []domain.LogEntry{logEntry(ts1, "r", "old"), logEntry(ts1.Add(-time.Second), "r", "ancient"), logEntry(ts2, "r", "line2")})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{"log:r:[old line2]"})
}

func TestAdvanceUnknownLogAttributedToRoot(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)
	ts := time.Unix(100, 0)

	// A log whose span id is not in the tree (node not yet created) is
	// attributed to root.
	events, err := b.Advance(tr(root, "running"), []domain.LogEntry{logEntry(ts, "unknown", "mystery")})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{
		"started:r:running::0",
		"log:r:[mystery]",
	})
}

func TestAdvanceLogsSplitByOwner(t *testing.T) {
	b := NewStepEventBuilder(0)
	c1 := span("c1", "r", "one", "running", nil)
	c2 := span("c2", "r", "two", "running", nil)
	root := span("r", "", "build", "running", nil, c1, c2)

	ts := time.Unix(100, 0)
	logs := []domain.LogEntry{
		logEntry(ts, "c1", "a"),
		logEntry(ts.Add(time.Second), "c1", "b"),
		logEntry(ts.Add(2*time.Second), "c2", "c"),
	}

	events, err := b.Advance(tr(root, "running"), logs)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// Depth-first order: each child's logs follow its node_started and precede
	// the next sibling (so a nested renderer can keep stages open correctly).
	assertEvents(t, events, []string{
		"started:r:running::0",
		"started:c1:running:r:1",
		"log:c1:[a b]",
		"started:c2:running:r:1",
		"log:c2:[c]",
	})
}

func TestAdvanceLogChunkLineCap(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)

	var logs []domain.LogEntry
	for i := 0; i < 250; i++ {
		logs = append(logs, logEntry(time.Unix(100, 0).Add(time.Duration(i)*time.Second), "r", "x"))
	}

	events, err := b.Advance(tr(root, "running"), logs)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// 250 lines -> chunks of 100/100/50.
	got := summarize(events)
	if len(got) != 4 { // node_started + 3 chunks
		t.Fatalf("events = %v, want node_started + 3 chunks", got)
	}
	if got[1] != "log:r:[x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x x]" {
		t.Fatalf("first chunk = %q", got[1])
	}
}

func TestAdvanceLogChunkByteCap(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)

	var logs []domain.LogEntry
	line := strings.Repeat("y", 100) // 100 bytes/line -> byte cap (8192) hits before line cap (100)
	for i := 0; i < 90; i++ {
		logs = append(logs, logEntry(time.Unix(100, 0).Add(time.Duration(i)*time.Second), "r", line))
	}

	events, err := b.Advance(tr(root, "running"), logs)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	got := summarize(events)
	if len(got) < 3 {
		t.Fatalf("events = %d entries, want node_started + multiple chunks", len(got))
	}
	if got[0] != "started:r:running::0" {
		t.Fatalf("events[0] = %q", got[0])
	}
	total := 0
	for _, g := range got[1:] {
		parts := strings.SplitN(g, ":", 3)
		if parts[0] != "log" {
			t.Fatalf("unexpected event %q", g)
		}
		total += len(strings.Split(strings.Trim(parts[2], "[]"), " "))
	}
	if total != 90 {
		t.Fatalf("total log lines = %d, want 90", total)
	}
}

func TestAdvancePipelineDoneCanceled(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)

	events, err := b.Advance(tr(root, "canceled"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{
		"started:r:running::0",
		"done:canceled:",
	})
}

func TestAdvancePipelineDoneFromTraceStatus(t *testing.T) {
	b := NewStepEventBuilder(0)
	// Root still "running" but trace.Status is terminal "failed".
	root := span("r", "", "build", "running", nil)

	events, err := b.Advance(tr(root, "failed"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{
		"started:r:running::0",
		"done:failed:",
	})
}

func TestLogMarkAccessor(t *testing.T) {
	b := NewStepEventBuilder(0)
	if m := b.LogMark(); !m.IsZero() {
		t.Fatalf("LogMark = %v, want zero", m)
	}
	root := span("r", "", "build", "running", nil)
	ts := time.Unix(100, 0)
	if _, err := b.Advance(tr(root, "running"), []domain.LogEntry{logEntry(ts, "r", "l")}); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if m := b.LogMark(); !m.Equal(ts) {
		t.Fatalf("LogMark = %v, want %v", m, ts)
	}
}

func TestAdvanceNestedFinishOrderAndLogsBeforeFinish(t *testing.T) {
	b := NewStepEventBuilder(0)
	testNode := span("c-test", "r", "test", "failed", map[string]string{"error": "boom"})
	lint := span("c-lint", "r", "lint", "success", nil)
	root := span("r", "", "build", "success", nil, lint, testNode)

	ts := time.Unix(100, 0)
	logs := []domain.LogEntry{
		logEntry(ts, "c-test", "t-line"),
		logEntry(ts.Add(time.Second), "c-lint", "l-line"),
		logEntry(ts.Add(2*time.Second), "r", "r-line"),
	}

	events, err := b.Advance(tr(root, "success"), logs)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// Depth-first: a child's logs precede its own node_finished and the next
	// sibling; the root closes only after every descendant.
	assertEvents(t, events, []string{
		"started:r:running::0",
		"started:c-lint:running:r:1",
		"log:c-lint:[l-line]",
		"finished:c-lint:succeeded:",
		"started:c-test:running:r:1",
		"log:c-test:[t-line]",
		"finished:c-test:failed:boom",
		"log:r:[r-line]",
		"finished:r:succeeded:",
		"done:success:",
	})
}

func TestAdvanceEqualTimestampDoesNotDropOrDuplicate(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)

	ts := time.Unix(100, 0)
	// Two distinct records share the same nanosecond timestamp. Emitting both
	// must not advance the watermark past the second one's identity.
	events, err := b.Advance(tr(root, "running"), []domain.LogEntry{
		logEntry(ts, "r", "a"),
		logEntry(ts, "r", "b"),
	})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{
		"started:r:running::0",
		"log:r:[a b]",
	})

	// Re-poll with both records (as the supervisor always returns the full
	// window) -> both dropped, no duplicates.
	events, err = b.Advance(tr(root, "running"), []domain.LogEntry{
		logEntry(ts, "r", "a"),
		logEntry(ts, "r", "b"),
	})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want empty (no duplicates)", summarize(events))
	}

	// A new record at the same timestamp is NOT dropped: it is the only one
	// beyond the watermark identity set.
	events, err = b.Advance(tr(root, "running"), []domain.LogEntry{
		logEntry(ts, "r", "a"),
		logEntry(ts, "r", "b"),
		logEntry(ts, "r", "c"),
	})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{"log:r:[c]"})

	// Re-poll with the same three records: the late arrival "c" must NOT be
	// re-emitted on every subsequent poll (regression: the watermark used to
	// ignore equal-timestamp late arrivals, re-emitting them until a newer
	// record arrived — unbounded duplicate output, CWE-400).
	events, err = b.Advance(tr(root, "running"), []domain.LogEntry{
		logEntry(ts, "r", "a"),
		logEntry(ts, "r", "b"),
		logEntry(ts, "r", "c"),
	})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want empty (late arrival not re-emitted)", summarize(events))
	}
}

// TestAdvanceStaleLogsDoNotRegress proves a poll carrying only entries older
// than the watermark emits nothing and never moves the watermark backwards.
func TestAdvanceStaleLogsDoNotRegress(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)
	ts := time.Unix(100, 0)

	if _, err := b.Advance(tr(root, "running"), []domain.LogEntry{logEntry(ts, "r", "l1")}); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	events, err := b.Advance(tr(root, "running"), []domain.LogEntry{logEntry(ts.Add(-time.Second), "r", "old")})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want empty", summarize(events))
	}
	if m := b.LogMark(); !m.Equal(ts) {
		t.Fatalf("LogMark = %v, want %v (not regressed)", m, ts)
	}
}

// TestAdvanceZeroTimestampLogsEmittedOnce covers the degenerate case where all
// entries carry the zero timestamp: they must be emitted once and deduped on
// re-poll, not re-emitted forever.
func TestAdvanceZeroTimestampLogsEmittedOnce(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)
	logs := []domain.LogEntry{logEntry(time.Time{}, "r", "z")}

	events, err := b.Advance(tr(root, "running"), logs)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{"started:r:running::0", "log:r:[z]"})

	events, err = b.Advance(tr(root, "running"), logs)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want empty (zero-timestamp log re-emitted)", summarize(events))
	}
}

// TestAdvanceAbsoluteDepthCap proves the unlimited clamp (0) is still bounded
// by ciMaxAbsoluteDepth: a pathologically deep snapshot folds instead of
// exhausting the wrapper's stack (CWE-674/CWE-400).
func TestAdvanceAbsoluteDepthCap(t *testing.T) {
	b := NewStepEventBuilder(0) // unlimited
	// Chain d0 -> d1 -> ... -> d1025 (depth 0..1025).
	node := span("d1025", "d1024", "deep", "running", nil)
	for i := 1024; i >= 1; i-- {
		node = span(fmt.Sprintf("d%d", i), fmt.Sprintf("d%d", i-1), "deep", "running", nil, node)
	}
	root := span("d0", "", "build", "running", nil, node)

	events, err := b.Advance(tr(root, "running"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// Depths 0..ciMaxAbsoluteDepth are emitted; d1025 folds into d1024.
	if len(events) != ciMaxAbsoluteDepth+1 {
		t.Fatalf("events = %d, want %d node_started", len(events), ciMaxAbsoluteDepth+1)
	}
	if got := events[len(events)-1].Node.ID; got != "d1024" {
		t.Fatalf("deepest emitted node = %q, want d1024", got)
	}
}

func TestAdvanceGrandchildDepthAndOrder(t *testing.T) {
	b := NewStepEventBuilder(0)
	grand := span("g", "c", "grand", "success", nil)
	child := span("c", "r", "child", "success", nil, grand)
	root := span("r", "", "build", "success", nil, child)

	events, err := b.Advance(tr(root, "success"), nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// Depth-first: grandchild closes before its parent, which closes before root.
	assertEvents(t, events, []string{
		"started:r:running::0",
		"started:c:running:r:1",
		"started:g:running:c:2",
		"finished:g:succeeded:",
		"finished:c:succeeded:",
		"finished:r:succeeded:",
		"done:success:",
	})
}

func TestAdvanceEqualTimestampDedupeSetBounded(t *testing.T) {
	orig := maxMarkKeys
	maxMarkKeys = 2
	defer func() { maxMarkKeys = orig }()

	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)
	ts := time.Unix(100, 0)

	// Three distinct records at one timestamp exceed the cap: the dedupe set is
	// trimmed to the cap and emission still succeeds (no dropped lines here).
	events, err := b.Advance(tr(root, "running"), []domain.LogEntry{
		logEntry(ts, "r", "a"), logEntry(ts, "r", "b"), logEntry(ts, "r", "c"),
	})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertEvents(t, events, []string{"started:r:running::0", "log:r:[a b c]"})

	// The dedupe set stays bounded even after repeated re-polls.
	for i := 0; i < 5; i++ {
		if _, err := b.Advance(tr(root, "running"), []domain.LogEntry{
			logEntry(ts, "r", "a"), logEntry(ts, "r", "b"), logEntry(ts, "r", "c"),
		}); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		b.mu.Lock()
		if len(b.markKeys) > maxMarkKeys {
			b.mu.Unlock()
			t.Fatalf("markKeys grew to %d, want <= %d", len(b.markKeys), maxMarkKeys)
		}
		b.mu.Unlock()
	}
}

func TestFinalizeClosesRunningNodesAndEmitsDone(t *testing.T) {
	b := NewStepEventBuilder(0)
	child := span("c", "r", "child", "running", nil)
	root := span("r", "", "build", "running", nil, child)

	if _, err := b.Advance(tr(root, "running"), nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	events := b.Finalize("failed", "boom")
	assertEvents(t, events, []string{
		"finished:c:failed:", // child closes before its parent
		"finished:r:failed:", // root closes last
		"done:failed:boom",
	})

	// Idempotent: a second Finalize emits nothing.
	if got := b.Finalize("failed", "boom"); len(got) != 0 {
		t.Fatalf("second Finalize = %v, want empty", summarize(got))
	}
}

func TestFinalizeNeverEmitsAfterSnapshotDone(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "success", nil)

	if _, err := b.Advance(tr(root, "success"), nil); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := b.Finalize("failed", "boom"); len(got) != 0 {
		t.Fatalf("Finalize after snapshot pipeline_done = %v, want empty", summarize(got))
	}
}

func TestFinalizeNoRootStillEmitsDone(t *testing.T) {
	b := NewStepEventBuilder(0)
	// No trace ever ingested: Finalize must still produce a terminal event so
	// the consumer cannot wait forever.
	events := b.Finalize("success", "")
	assertEvents(t, events, []string{"done:success:"})
}

func TestAdvanceConcurrent(t *testing.T) {
	b := NewStepEventBuilder(0)
	root := span("r", "", "build", "running", nil)
	trace := tr(root, "running")

	const n = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	var seqs []int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev, err := b.Advance(trace, []domain.LogEntry{logEntry(time.Now(), "r", "x")})
			if err != nil {
				t.Errorf("Advance: %v", err)
				return
			}
			mu.Lock()
			for _, e := range ev {
				seqs = append(seqs, e.Seq)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seq not monotonic: %v", seqs)
		}
	}
}

func TestNormalizeStepName(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "  build   step ", id: "abcdefgh", want: "build step"},
		{name: "", id: "abcdefgh1234", want: "step-abcdefgh"},
		{name: "", id: "ab", want: "step-ab"},
		{name: "", id: "", want: "step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeStepName(tt.name, tt.id); got != tt.want {
				t.Fatalf("normalizeStepName(%q, %q) = %q, want %q", tt.name, tt.id, got, tt.want)
			}
		})
	}
}

func TestStepStateForStatus(t *testing.T) {
	tests := []struct {
		status string
		want   domain.StepState
	}{
		{status: "success", want: domain.StepStateSucceeded},
		{status: "failed", want: domain.StepStateFailed},
		{status: "running", want: domain.StepStateRunning},
		{status: "", want: domain.StepStateRunning},
		{status: "bogus", want: domain.StepStateRunning},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := stepStateForStatus(tt.status); got != tt.want {
				t.Fatalf("stepStateForStatus(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestTerminalStatus(t *testing.T) {
	tests := []struct {
		name   string
		trace  *domain.TraceInfo
		status string
		ok     bool
	}{
		{name: "root success", trace: tr(span("r", "", "b", "success", nil), "running"), status: "success", ok: true},
		{name: "root failed", trace: tr(span("r", "", "b", "failed", nil), "running"), status: "failed", ok: true},
		{name: "trace success", trace: tr(span("r", "", "b", "running", nil), "success"), status: "success", ok: true},
		{name: "trace failed", trace: tr(span("r", "", "b", "running", nil), "failed"), status: "failed", ok: true},
		{name: "trace canceled", trace: tr(span("r", "", "b", "running", nil), "canceled"), status: "canceled", ok: true},
		{name: "no root no status", trace: tr(nil, "running"), status: "", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, ok := terminalStatus(tt.trace)
			if status != tt.status || ok != tt.ok {
				t.Fatalf("terminalStatus = (%q, %v), want (%q, %v)", status, ok, tt.status, tt.ok)
			}
		})
	}
}

func TestFailureReason(t *testing.T) {
	if r := failureReason(nil); r != "" {
		t.Fatalf("failureReason(nil) = %q, want empty", r)
	}
	if r := failureReason(span("r", "", "b", "failed", map[string]string{"other": "x"})); r != "" {
		t.Fatalf("failureReason = %q, want empty", r)
	}
	if r := failureReason(span("r", "", "b", "failed", map[string]string{"error.message": "exploded"})); r != "exploded" {
		t.Fatalf("failureReason = %q, want exploded", r)
	}
}

func TestResolveLogOwner(t *testing.T) {
	tests := []struct {
		name        string
		spanID      string
		attribution map[string]string
		rootID      string
		want        string
	}{
		{name: "empty span id -> root", spanID: "", attribution: map[string]string{"r": "r"}, rootID: "r", want: "r"},
		{name: "known span -> own owner", spanID: "c1", attribution: map[string]string{"r": "r", "c1": "c1"}, rootID: "r", want: "c1"},
		{name: "unknown span -> root", spanID: "missing", attribution: map[string]string{"r": "r"}, rootID: "r", want: "r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveLogOwner(tt.spanID, tt.attribution, tt.rootID); got != tt.want {
				t.Fatalf("resolveLogOwner(%q, %v, %q) = %q, want %q", tt.spanID, tt.attribution, tt.rootID, got, tt.want)
			}
		})
	}
}
