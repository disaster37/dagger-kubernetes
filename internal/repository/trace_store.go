package repository

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

var hexTraceID = regexp.MustCompile(`^[a-fA-F0-9]{16,}$`)

type SpanTreeReconstructor struct {
	tempoURL   string
	httpClient *http.Client
}

var _ domain.TraceRepository = (*SpanTreeReconstructor)(nil)

func NewSpanTreeReconstructor(tempoURL string) *SpanTreeReconstructor {
	return &SpanTreeReconstructor{
		tempoURL: tempoURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (r *SpanTreeReconstructor) GetTrace(traceID string) (*domain.TraceInfo, error) {
	if !hexTraceID.MatchString(traceID) {
		return nil, fmt.Errorf("invalid trace ID format")
	}
	resp, err := r.httpClient.Get(fmt.Sprintf("%s/api/traces/%s", r.tempoURL, traceID))
	if err != nil {
		return nil, fmt.Errorf("query tempo trace: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var raw map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode tempo response: %w", err)
	}

	return r.reconstruct(traceID, raw), nil
}

func (r *SpanTreeReconstructor) reconstruct(traceID string, raw map[string]interface{}) *domain.TraceInfo {
	spans := extractSpans(raw)

	// The Dagger CLI emits each span twice with the same span ID: once when the
	// operation starts (no end time, no status) and once when it finishes (end
	// time + status code). Merge duplicates by span ID before linking so the
	// tree contains a single node per operation.
	nodes := make(map[string]*domain.SpanNode)
	order := make([]*domain.SpanNode, 0, len(spans))
	for _, s := range spans {
		if existing, ok := nodes[s.SpanID]; ok {
			mergeSpanNode(existing, s)
		} else {
			nodes[s.SpanID] = s
			order = append(order, s)
		}
	}

	// Link children to their parents, collecting any spans whose parent is
	// absent (the root, plus any orphan whose parent Tempo has not indexed
	// yet). A naive "last parentless span wins" is fragile: while a trace is
	// still being ingested, Tempo can return a child span before its parent,
	// and a trace whose root record is missing would otherwise pick an
	// arbitrary span as root.
	var parentless []*domain.SpanNode
	for _, s := range order {
		parent, ok := nodes[s.ParentSpanID]
		if !ok {
			parentless = append(parentless, s)
		} else {
			parent.Children = append(parent.Children, s)
		}
	}

	// Prefer the earliest-starting parentless span (the true root starts
	// first); tie-break in favour of the span that parents the most others.
	root := pickRoot(parentless)
	if root == nil && len(order) > 0 {
		// Every span claims a parent that is present (cycle or missing root
		// record): fall back to the earliest-starting span so the tree still
		// renders instead of showing an empty "no steps" state.
		root = pickRoot(order)
	}

	info := &domain.TraceInfo{
		TraceID:  traceID,
		RootSpan: root,
		Status:   "running",
	}

	if root != nil {
		info.StartTime = root.StartTime
		if v, ok := root.Attributes["dagger.io/engine.version"]; ok {
			info.Version = v
		}
		if v, ok := root.Attributes["dagger.io/ci"]; ok {
			info.CIProvider = v
		}
		if v, ok := root.Attributes["dagger.io/ci.repo"]; ok {
			info.CIRepo = v
		}
	}

	info.Status = traceStatus(root)
	info.DurationMS = traceDurationMS(order)
	info.Duration = time.Duration(info.DurationMS) * time.Millisecond

	return info
}

// pickRoot selects the most likely root span from a list of candidates: the
// span with the earliest start time, breaking ties in favour of the span that
// is the parent of the most other spans (the root typically has the largest
// fan-out and no parent of its own).
func pickRoot(candidates []*domain.SpanNode) *domain.SpanNode {
	var root *domain.SpanNode
	for _, s := range candidates {
		if root == nil {
			root = s
			continue
		}
		// A span with an unknown start time loses to one with a known start.
		if s.StartTime.IsZero() {
			continue
		}
		if root.StartTime.IsZero() || s.StartTime.Before(root.StartTime) {
			root = s
			continue
		}
		if s.StartTime.Equal(root.StartTime) && len(s.Children) > len(root.Children) {
			root = s
		}
	}
	return root
}

// mergeSpanNode folds a duplicate span (same span ID) into dst. The "finish"
// record carries the end time, status code and additional attributes, so those
// win; the "start" record supplies the start time when the finish record
// omits it.
func mergeSpanNode(dst, src *domain.SpanNode) {
	if dst.StartTime.IsZero() && !src.StartTime.IsZero() {
		dst.StartTime = src.StartTime
	}
	if dst.Duration == 0 && src.Duration != 0 {
		dst.Duration = src.Duration
		dst.DurationMS = src.DurationMS
	}
	if dst.Status == "running" && src.Status != "running" {
		dst.Status = src.Status
	}
	if dst.Name == "" && src.Name != "" {
		dst.Name = src.Name
	}
	if dst.TraceID == "" && src.TraceID != "" {
		dst.TraceID = src.TraceID
	}
	if dst.ParentSpanID == "" && src.ParentSpanID != "" {
		// Tempo can return one duplicate record with the parent absent (or
		// empty) and another with it present; keep whichever is non-empty so
		// the span links into the tree instead of being mistaken for a root.
		dst.ParentSpanID = src.ParentSpanID
	}
	for k, v := range src.Attributes {
		if _, ok := dst.Attributes[k]; !ok {
			dst.Attributes[k] = v
		}
	}
}

// traceStatus derives the overall trace status from the root span. The root
// span represents the whole run; until it carries a final status the pipeline
// is still in flight, so we report "running". Child spans can finish (and even
// fail) while sibling steps are still executing, so deriving the status from
// any single child would surface a final state prematurely.
func traceStatus(root *domain.SpanNode) string {
	if root == nil {
		return "running"
	}
	switch root.Status {
	case "success", "failed":
		return root.Status
	default:
		return "running"
	}
}

// traceDurationMS computes the trace duration as the span of time between the
// earliest span start and the latest span end. This is more robust than using
// the root span alone, whose finish record is often absent in Tempo.
func traceDurationMS(spans []*domain.SpanNode) int64 {
	var minStart, maxEnd int64
	for _, s := range spans {
		if s.StartTime.IsZero() {
			continue
		}
		ns := s.StartTime.UnixNano()
		if minStart == 0 || ns < minStart {
			minStart = ns
		}
		if s.Duration > 0 {
			if endNS := ns + s.Duration.Nanoseconds(); endNS > maxEnd {
				maxEnd = endNS
			}
		}
	}
	if maxEnd <= minStart {
		return 0
	}
	return (maxEnd - minStart) / int64(time.Millisecond)
}

func extractSpans(raw map[string]interface{}) []*domain.SpanNode {
	var spans []*domain.SpanNode
	for _, batch := range asSlice(raw["batches"]) {
		batchMap, ok := batch.(map[string]interface{})
		if !ok {
			continue
		}
		for _, scopeSpan := range asSlice(batchMap["scopeSpans"]) {
			scopeMap, ok := scopeSpan.(map[string]interface{})
			if !ok {
				continue
			}
			for _, span := range asSlice(scopeMap["spans"]) {
				spanMap, ok := span.(map[string]interface{})
				if !ok {
					continue
				}
				if node := mapToSpanNode(spanMap); node != nil {
					spans = append(spans, node)
				}
			}
		}
	}
	return spans
}

// asSlice coerces a decoded JSON value to a slice; anything else yields an
// empty slice so callers can range over it unconditionally.
func asSlice(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}

// mapStatus maps an OTLP span status code to the internal status string.
// Tempo returns the code as a string ("STATUS_CODE_OK", "STATUS_CODE_ERROR",
// "STATUS_CODE_UNSET"); numeric codes (0/1/2) are also accepted for
// robustness against other Tempo response shapes.
func mapStatus(status map[string]interface{}) string {
	switch code := status["code"].(type) {
	case string:
		switch code {
		case "STATUS_CODE_OK":
			return "success"
		case "STATUS_CODE_ERROR":
			return "failed"
		case "STATUS_CODE_UNSET":
			return "running"
		}
	case float64:
		switch code {
		case 0:
			return "running"
		case 1:
			return "success"
		case 2:
			return "failed"
		}
	}
	return "running"
}

func mapToSpanNode(m map[string]interface{}) *domain.SpanNode {
	name, _ := m["name"].(string)
	spanID, _ := m["spanId"].(string)
	parentSpanID, _ := m["parentSpanId"].(string)
	traceID, _ := m["traceId"].(string)

	if spanID == "" {
		return nil
	}

	node := &domain.SpanNode{
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
		TraceID:      traceID,
		Name:         name,
		Status:       "running",
		Attributes:   make(map[string]string),
		Children:     []*domain.SpanNode{},
	}

	if status, ok := m["status"].(map[string]interface{}); ok {
		node.Status = mapStatus(status)
	}

	if startTimeUnix, ok := m["startTimeUnixNano"].(string); ok {
		ns, err := strconv.ParseInt(startTimeUnix, 10, 64)
		if err == nil {
			node.StartTime = time.Unix(0, ns)
		}
	}
	if endTimeUnix, ok := m["endTimeUnixNano"].(string); ok {
		ns, err := strconv.ParseInt(endTimeUnix, 10, 64)
		if err == nil {
			endTime := time.Unix(0, ns)
			node.Duration = endTime.Sub(node.StartTime)
			node.DurationMS = node.Duration.Milliseconds()
		}
	}

	if attrs, ok := m["attributes"].([]interface{}); ok {
		for _, a := range attrs {
			attrMap, ok := a.(map[string]interface{})
			if !ok {
				continue
			}
			key, _ := attrMap["key"].(string)
			if v, ok := attrMap["value"].(map[string]interface{}); ok {
				if sv, ok := v["stringValue"].(string); ok {
					node.Attributes[key] = sv
				} else if bv, ok := v["boolValue"].(bool); ok {
					// Dagger UI hints (dagger.io/ui.*) are boolean attributes;
					// encode them so the frontend can collapse/passthrough.
					node.Attributes[key] = strconv.FormatBool(bv)
				}
			}
		}
	}

	return node
}
