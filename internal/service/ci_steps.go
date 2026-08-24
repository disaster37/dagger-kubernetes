package service

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// Bounds for log-chunk batching in the CI event stream (see ADR-024). Chunks
// are split by owner and capped by line count and byte size so a single span's
// log volume cannot blow out the NDJSON stream.
const (
	ciLogChunkLines = 100
	ciLogChunkBytes = 8192
)

// ciMaxAbsoluteDepth is the hard upper bound on the tree depth the builder
// walks, regardless of the configured clamp (0 = unlimited): it bounds the
// DFS recursion so a hostile snapshot cannot exhaust the wrapper's stack
// (CWE-674/CWE-400).
const ciMaxAbsoluteDepth = 1024

// maxMarkKeys bounds the number of already-emitted log identities we retain at
// the watermark timestamp. Two separate log records can share a nanosecond
// timestamp; retaining only a count would silently drop the second record's
// lines on the next poll. This cap bounds memory while making the watermark
// drop-not-duplicate for any realistic log volume.
var maxMarkKeys = 4096

// logMarkKey identifies a single emitted log record at the watermark timestamp
// so equal-timestamp re-polls neither drop nor duplicate lines.
type logMarkKey struct {
	spanID string
	line   string
}

// StepEventBuilder diffs successive tree+log snapshots into an ordered,
// idempotent normalized CI event stream. It is safe for concurrent use.
//
// The emitted event stream is a depth-first traversal: node_started, then its
// children (each with their own started/logs/finished), then the node's own
// log_chunks, then node_finished. This ordering is what lets a sequential
// renderer (Jenkins scripted pipeline) open and close nested stage() blocks
// without reordering, and guarantees a node's logs always precede its
// node_finished event.
type StepEventBuilder struct {
	mu           sync.Mutex
	maxDepth     int
	seen         map[string]*domain.StepNode // last emitted state per node id
	order        []string                    // node ids in first-emitted order
	logMark      time.Time                   // log watermark (exclusive)
	markKeys     map[logMarkKey]struct{}     // emitted records at logMark
	traceID      string                      // remembered for Finalize
	seq          int64
	pipelineDone bool
}

// NewStepEventBuilder returns a builder that clamps the tree at maxDepth
// (nodes deeper than maxDepth are folded into their deepest emitted ancestor;
// their logs are attributed to that ancestor). maxDepth <= 0 means unlimited,
// hard-capped at ciMaxAbsoluteDepth for stack safety.
func NewStepEventBuilder(maxDepth int) *StepEventBuilder {
	return &StepEventBuilder{
		maxDepth: maxDepth,
		seen:     make(map[string]*domain.StepNode),
	}
}

// LogMark returns the builder's current log watermark (exclusive): any log
// entry at or before this timestamp has already been emitted. The poller uses
// it as the QueryTraceLogs start bound to avoid re-fetching old lines.
func (b *StepEventBuilder) LogMark() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logMark
}

// Advance diffs the current snapshot (trace tree + logs) against the previous
// one and returns the ordered events to emit, updating internal state.
// It is idempotent: an unchanged snapshot returns an empty slice.
// A nil trace returns a wrapped error; a tree with no resolvable root emits
// nothing yet (the run is still ingesting).
func (b *StepEventBuilder) Advance(trace *domain.TraceInfo, logs []domain.LogEntry) ([]domain.CIEvent, error) {
	if trace == nil {
		return nil, fmt.Errorf("advance ci step snapshot: nil trace")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.seen == nil {
		b.seen = make(map[string]*domain.StepNode)
	}

	flat, attribution := flattenTrace(trace.RootSpan, b.maxDepth)
	if len(flat) == 0 {
		// No resolvable root yet: the run is still ingesting. Emit nothing
		// (node events, log chunks and pipeline_done are all deferred).
		return nil, nil
	}

	if b.traceID == "" {
		b.traceID = trace.TraceID
	}
	rootID := flat[0].node.SpanID

	var events []domain.CIEvent
	emit := func(e domain.CIEvent) {
		b.seq++
		e.Seq = b.seq
		if e.Timestamp.IsZero() {
			e.Timestamp = time.Now()
		}
		if e.TraceID == "" {
			e.TraceID = b.traceID
		}
		events = append(events, e)
	}

	// Resolve + group logs first so each node's own log_chunks can be emitted
	// immediately before its node_finished (and after its children close).
	chunks := b.prepareLogChunks(logs, attribution, rootID)

	b.emitTree(flat, attribution, rootID, chunks, emit)

	// Advance the watermark only after the entries have been emitted, and only
	// forward (never backwards). Besides strictly-newer records, the watermark
	// update also folds late arrivals at the current watermark into the dedupe
	// set so they are emitted exactly once.
	if mark, keys, ok := b.advanceWatermark(logs); ok {
		b.logMark = mark
		b.markKeys = keys
	}

	if !b.pipelineDone {
		if status, ok := terminalStatus(trace); ok {
			done := domain.CIEvent{Type: domain.CIEventPipelineDone, Status: status}
			if status == "failed" {
				done.Error = failureReason(trace.RootSpan)
			}
			emit(done)
			b.pipelineDone = true
		}
	}

	return events, nil
}

// prepareLogChunks sorts the log entries chronologically, drops already-emitted
// ones (watermark + equal-timestamp identity dedupe), and groups the remainder
// into bounded LogChunks keyed by owner node id. Ownership resolution folds
// unknown/deep span ids into root / the deepest emitted ancestor.
func (b *StepEventBuilder) prepareLogChunks(logs []domain.LogEntry, attribution map[string]string, rootID string) map[string][]*domain.LogChunk {
	if len(logs) == 0 {
		return nil
	}

	sorted := append([]domain.LogEntry(nil), logs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	byOwner := make(map[string][]*domain.LogChunk)
	for _, entry := range sorted {
		if entry.Timestamp.Before(b.logMark) {
			continue
		}
		if entry.Timestamp.Equal(b.logMark) {
			if _, dup := b.markKeys[logMarkKey{spanID: entry.SpanID, line: entry.Line}]; dup {
				continue
			}
		}
		owner := resolveLogOwner(entry.SpanID, attribution, rootID)
		chunks := byOwner[owner]
		if len(chunks) == 0 || shouldSplitChunk(chunks[len(chunks)-1], entry.Line) {
			chunks = append(chunks, &domain.LogChunk{NodeID: owner})
		}
		last := chunks[len(chunks)-1]
		last.Lines = append(last.Lines, entry.Line)
		last.Timestamp = entry.Timestamp
		byOwner[owner] = chunks
	}
	return byOwner
}

// advanceWatermark computes the next log watermark and the set of emitted log
// identities at that watermark, from the (unsorted) entries in this snapshot.
// It returns ok=false when the watermark must not move (no entries, or every
// entry older than the current watermark). When the snapshot's newest entry
// equals the current watermark, the identities of the entries at that
// timestamp are folded into the dedupe set: without that, a late-arriving
// record sharing the watermark timestamp would be re-emitted on every
// subsequent poll (unbounded duplicate output, CWE-400).
func (b *StepEventBuilder) advanceWatermark(logs []domain.LogEntry) (time.Time, map[logMarkKey]struct{}, bool) {
	if len(logs) == 0 {
		return time.Time{}, nil, false
	}
	var maxTS time.Time
	for _, e := range logs {
		if e.Timestamp.After(maxTS) {
			maxTS = e.Timestamp
		}
	}
	if maxTS.Before(b.logMark) {
		// Every entry is older than the watermark (e.g. a stale re-poll).
		return time.Time{}, nil, false
	}

	keys := make(map[logMarkKey]struct{}, len(b.markKeys)+4)
	// Carry the previously emitted identities forward so only the new records
	// are added to the dedupe set, never re-adding (and never re-dropping) the
	// old ones.
	for k := range b.markKeys {
		keys[k] = struct{}{}
	}
	for _, e := range logs {
		if e.Timestamp.Equal(maxTS) {
			keys[logMarkKey{spanID: e.SpanID, line: e.Line}] = struct{}{}
		}
	}
	trimMarkKeys(keys)
	return maxTS, keys, true
}

// trimMarkKeys bounds the dedupe set: beyond the cap we accept the (harmless)
// possibility of re-emitting a line rather than risk dropping a new one.
func trimMarkKeys(keys map[logMarkKey]struct{}) {
	if len(keys) <= maxMarkKeys {
		return
	}
	for k := range keys {
		if len(keys) <= maxMarkKeys {
			break
		}
		delete(keys, k)
	}
}

// emitTree walks the flattened tree in depth-first pre-order and emits, for
// each node: node_started, then (recursively) its children, then its own
// log_chunks, then node_finished when it reached a terminal state. This keeps
// parent stages open while children and logs render, and closes them last.
func (b *StepEventBuilder) emitTree(flat []flatNode, attribution map[string]string, rootID string, chunks map[string][]*domain.LogChunk, emit func(domain.CIEvent)) {
	// The flat list is in depth-first pre-order. Build an index mapping each
	// node to its children (by flat-list index) and emit the subtree in
	// depth-first order — node_started, children, own logs, node_finished — so
	// a parent's stage stays open across its whole subtree and closes last.
	children := make(map[int][]int, len(flat))
	var stack []int
	for idx, fn := range flat {
		for len(stack) > 0 && flat[stack[len(stack)-1]].depth >= fn.depth {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			parent := stack[len(stack)-1]
			children[parent] = append(children[parent], idx)
		}
		stack = append(stack, idx)
	}

	var visit func(idx int)
	visit = func(idx int) {
		fn := flat[idx]
		b.emitNodeStarted(fn, attribution, rootID, emit)
		for _, c := range children[idx] {
			visit(c)
		}
		for _, ch := range chunks[fn.node.SpanID] {
			emit(domain.CIEvent{Type: domain.CIEventLogChunk, Log: ch})
		}
		b.emitNodeFinished(fn, emit)
	}
	if len(flat) > 0 {
		visit(0)
	}
}

// emitNodeStarted emits node_started for a node seen for the first time and
// records its emitted running state (pending -> running).
func (b *StepEventBuilder) emitNodeStarted(fn flatNode, attribution map[string]string, rootID string, emit func(domain.CIEvent)) {
	node := fn.node
	parentID := node.ParentSpanID
	if fn.depth == 0 {
		parentID = ""
	} else if _, ok := attribution[parentID]; !ok {
		// Orphan: parent absent from this snapshot -> re-parent to root.
		parentID = rootID
	}

	step := &domain.StepNode{
		ID:        node.SpanID,
		ParentID:  parentID,
		Name:      normalizeStepName(node.Name, node.SpanID),
		State:     stepStateForStatus(node.Status),
		Depth:     fn.depth,
		StartedAt: node.StartTime,
	}
	if node.Duration > 0 {
		step.FinishedAt = node.StartTime.Add(node.Duration)
	}

	if _, ok := b.seen[node.SpanID]; !ok {
		started := *step
		started.State = domain.StepStateRunning
		emit(domain.CIEvent{Type: domain.CIEventNodeStarted, Node: &started})
		b.seen[node.SpanID] = &started
		b.order = append(b.order, node.SpanID)
	}
}

// emitNodeFinished emits node_finished when a previously-running node reached a
// terminal state in this snapshot.
func (b *StepEventBuilder) emitNodeFinished(fn flatNode, emit func(domain.CIEvent)) {
	node := fn.node
	state := stepStateForStatus(node.Status)
	terminal := state == domain.StepStateSucceeded || state == domain.StepStateFailed

	prev, ok := b.seen[node.SpanID]
	if !ok || prev.State != domain.StepStateRunning || !terminal {
		return
	}

	finished := &domain.StepNode{
		ID:        node.SpanID,
		ParentID:  prev.ParentID,
		Name:      prev.Name,
		State:     state,
		Depth:     prev.Depth,
		StartedAt: prev.StartedAt,
	}
	if node.Duration > 0 {
		finished.FinishedAt = node.StartTime.Add(node.Duration)
	}
	errMsg := ""
	if state == domain.StepStateFailed {
		errMsg = failureReason(node)
	}
	emit(domain.CIEvent{Type: domain.CIEventNodeFinished, Node: finished, Error: errMsg})
	b.seen[node.SpanID] = finished
}

// Finalize emits the terminal events a snapshot stream can never produce on its
// own: a node_finished for every node still running (in child-before-parent
// order) and a single pipeline_done carrying the authoritative build status.
// It is idempotent (returns nothing after the first call or after a
// snapshot-derived pipeline_done) and must be called after the Dagger command
// exits so the consumer always sees a terminal event — even when the trace
// never indexed, the root never resolved, or the engine failed before printing
// a trace id. errMsg is surfaced on the pipeline_done event when the pipeline
// failed.
func (b *StepEventBuilder) Finalize(status string, errMsg string) []domain.CIEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.pipelineDone {
		return nil
	}
	b.pipelineDone = true

	if status != "failed" && status != "canceled" {
		status = "success"
	}

	var events []domain.CIEvent
	emit := func(e domain.CIEvent) {
		b.seq++
		e.Seq = b.seq
		if e.Timestamp.IsZero() {
			e.Timestamp = time.Now()
		}
		if e.TraceID == "" {
			e.TraceID = b.traceID
		}
		events = append(events, e)
	}

	// Close running nodes in reverse-first-emitted order so descendants close
	// before their ancestors.
	terminal := domain.StepStateSucceeded
	if status != "success" {
		terminal = domain.StepStateFailed
	}
	for i := len(b.order) - 1; i >= 0; i-- {
		id := b.order[i]
		if n, ok := b.seen[id]; ok && n.State == domain.StepStateRunning {
			finished := *n
			finished.State = terminal
			emit(domain.CIEvent{Type: domain.CIEventNodeFinished, Node: &finished})
			b.seen[id] = &finished
		}
	}

	done := domain.CIEvent{Type: domain.CIEventPipelineDone, Status: status, Error: errMsg}
	emit(done)
	return events
}

// resolveLogOwner maps a span id to its owning step node; falls back to rootID
// for unknown or empty span ids.
func resolveLogOwner(spanID string, attribution map[string]string, rootID string) string {
	if spanID == "" {
		return rootID
	}
	if owner, ok := attribution[spanID]; ok {
		return owner
	}
	return rootID
}

// shouldSplitChunk reports whether adding entryLine to cur would exceed the
// configured line or byte cap.
func shouldSplitChunk(cur *domain.LogChunk, entryLine string) bool {
	if len(cur.Lines) >= ciLogChunkLines {
		return true
	}
	if len(cur.Lines) > 0 && chunkBytes(cur)+len(entryLine) > ciLogChunkBytes {
		return true
	}
	return false
}

// chunkBytes returns the total byte size of a chunk's buffered lines.
func chunkBytes(c *domain.LogChunk) int {
	n := 0
	for _, l := range c.Lines {
		n += len(l)
	}
	return n
}

// flatNode pairs a span node with its DFS depth in the reconstructed tree.
type flatNode struct {
	node  *domain.SpanNode
	depth int
}

// flattenTrace walks the reconstructed span tree DFS pre-order, dedupes by
// span id (first wins), and clamps depth at maxDepth (<= 0 = unlimited,
// hard-capped at ciMaxAbsoluteDepth). Nodes deeper than the clamp are not
// emitted; they are recorded in the returned attribution map so their logs
// fold into their deepest emitted ancestor.
func flattenTrace(root *domain.SpanNode, maxDepth int) ([]flatNode, map[string]string) {
	if root == nil || root.SpanID == "" {
		return nil, nil
	}
	if maxDepth <= 0 || maxDepth > ciMaxAbsoluteDepth {
		maxDepth = ciMaxAbsoluteDepth
	}

	seen := make(map[string]bool)
	attribution := make(map[string]string)
	var flat []flatNode

	var walk func(n *domain.SpanNode, depth int, owner string)
	walk = func(n *domain.SpanNode, depth int, owner string) {
		if n == nil {
			return
		}
		if n.SpanID == "" {
			// A node with no id cannot be attributed or emitted; skip it but
			// keep walking its children at the same depth/owner.
			for _, c := range n.Children {
				walk(c, depth, owner)
			}
			return
		}
		if seen[n.SpanID] {
			return
		}
		seen[n.SpanID] = true

		if maxDepth > 0 && depth > maxDepth {
			attribution[n.SpanID] = owner
			return
		}

		flat = append(flat, flatNode{node: n, depth: depth})
		attribution[n.SpanID] = n.SpanID
		for _, c := range n.Children {
			walk(c, depth+1, n.SpanID)
		}
	}

	walk(root, 0, "")
	return flat, attribution
}

// stepStateForStatus maps a SpanNode status string to a StepState:
// ""|"running" -> running; "success" -> succeeded; "failed" -> failed.
func stepStateForStatus(status string) domain.StepState {
	switch status {
	case "success":
		return domain.StepStateSucceeded
	case "failed":
		return domain.StepStateFailed
	default:
		return domain.StepStateRunning
	}
}

// terminalStatus reports whether the trace has reached a terminal state and, if
// so, the pipeline-level status string ("success"|"failed"|"canceled"). The
// root span status wins; a terminal trace.Status is the fallback (and the only
// source of "canceled").
func terminalStatus(trace *domain.TraceInfo) (string, bool) {
	if trace.RootSpan != nil {
		switch trace.RootSpan.Status {
		case "success":
			return "success", true
		case "failed":
			return "failed", true
		}
	}
	switch trace.Status {
	case "success", "failed":
		return trace.Status, true
	case "canceled":
		return "canceled", true
	}
	return "", false
}

// failureReason surfaces a best-effort human-readable error from a span's
// attributes (the reconstructor does not preserve OTLP status messages, so
// these attribute keys are the only available carrier).
func failureReason(n *domain.SpanNode) string {
	if n == nil {
		return ""
	}
	for _, k := range []string{"error", "error.message", "status.message", "failure_reason"} {
		if v := n.Attributes[k]; v != "" {
			return v
		}
	}
	return ""
}

// normalizeStepName sanitizes a span name for use as a CI step name: control
// characters are stripped, whitespace runs collapse to a single space, and an
// empty result falls back to a short, stable id-derived name. Jenkins-specific
// sanitization (length cap + collision disambiguation) is done by the shared
// library's normalizeStageName.
func normalizeStepName(name, id string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name != "" {
		return name
	}
	if id == "" {
		return "step"
	}
	if len(id) >= 8 {
		return fmt.Sprintf("step-%s", id[:8])
	}
	return fmt.Sprintf("step-%s", id)
}
