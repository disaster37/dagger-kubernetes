package service

import (
	"encoding/json"
	"strconv"
	"time"
)

// TraceIngestSummary is the parsed root-span metadata extracted from an OTLP
// HTTP/JSON body, used to enrich trace_meta during ingest.
type TraceIngestSummary struct {
	TraceID    string
	CIRepo     string // from root span attr "dagger.io/ci.repo"
	CIProvider string // "dagger.io/ci"
	Version    string // "dagger.io/engine.version"
	Status     string // root span status code mapping
	DurationMS int64  // root span end-start when both present
	StartedAt  time.Time
}

// otlpSpan is a minimal subset of the OTLP/HTTP JSON span shape.
type otlpSpan struct {
	TraceID        string     `json:"traceId"`
	SpanID         string     `json:"spanId"`
	ParentSpanID   string     `json:"parentSpanId"`
	Name           string     `json:"name"`
	StartTimeUnixN string     `json:"startTimeUnixNano"`
	EndTimeUnixN   string     `json:"endTimeUnixNano"`
	Status         otlpStatus `json:"status"`
	Attributes     []otlpAttr `json:"attributes"`
}

type otlpStatus struct {
	Code float64 `json:"code"`
}

type otlpAttr struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// ExtractTraceSummaries parses an OTLP/HTTP JSON body and returns one summary
// per distinct root trace (root = span whose parentSpanID is absent among the
// payload's span IDs). Malformed JSON yields nil (caller proceeds without
// metadata). Tolerates both the resourceSpans and the batches (Tempo) shapes.
func ExtractTraceSummaries(body []byte) []TraceIngestSummary {
	if len(body) == 0 {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}

	spans := extractAllSpans(raw)
	if len(spans) == 0 {
		return nil
	}

	// Index span IDs present in this payload.
	present := make(map[string]bool, len(spans))
	for _, s := range spans {
		present[s.SpanID] = true
	}

	// Root = span whose parent is absent in this payload.
	byTrace := make(map[string]*otlpSpan)
	for i := range spans {
		s := &spans[i]
		if s.ParentSpanID == "" || !present[s.ParentSpanID] {
			// First root per trace wins.
			if _, ok := byTrace[s.TraceID]; !ok {
				byTrace[s.TraceID] = s
			}
		}
	}

	out := make([]TraceIngestSummary, 0, len(byTrace))
	for traceID, root := range byTrace {
		out = append(out, buildSummary(traceID, root))
	}
	return out
}

func extractAllSpans(raw map[string]json.RawMessage) []otlpSpan {
	var spans []otlpSpan

	// OTLP/HTTP shape: {resourceSpans:[{scopeSpans:[{spans:[...]}]}]}
	for _, b := range unmarshalBatches(raw["resourceSpans"]) {
		spans = append(spans, spansFromScopeSpans(b["scopeSpans"])...)
	}

	// Tempo shape: {batches:[{scopeSpans:[{spans:[...]}]}]} (or spans directly).
	for _, b := range unmarshalBatches(raw["batches"]) {
		spans = append(spans, spansFromScopeSpans(b["scopeSpans"])...)
		// Some Tempo payloads put spans directly under the batch.
		if direct, ok := b["spans"]; ok {
			var s []otlpSpan
			if err := json.Unmarshal(direct, &s); err == nil {
				spans = append(spans, s...)
			}
		}
	}

	return spans
}

// unmarshalBatches decodes a top-level batch array (resourceSpans/batches).
// A missing key or malformed array yields nil.
func unmarshalBatches(raw json.RawMessage) []map[string]json.RawMessage {
	if raw == nil {
		return nil
	}
	var batches []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &batches); err != nil {
		return nil
	}
	return batches
}

func spansFromScopeSpans(raw json.RawMessage) []otlpSpan {
	var scopes []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &scopes); err != nil {
		return nil
	}
	var out []otlpSpan
	for _, sc := range scopes {
		var s []otlpSpan
		if err := json.Unmarshal(sc["spans"], &s); err == nil {
			out = append(out, s...)
		}
	}
	return out
}

func buildSummary(traceID string, root *otlpSpan) TraceIngestSummary {
	sum := TraceIngestSummary{TraceID: traceID}

	// Status code mapping (reuse trace_store semantics).
	switch int(root.Status.Code) {
	case 1:
		sum.Status = "success"
	case 2:
		sum.Status = "failed"
	default:
		sum.Status = "unset"
	}

	// Start time + duration.
	if startNS, err := strconv.ParseInt(root.StartTimeUnixN, 10, 64); err == nil && startNS > 0 {
		sum.StartedAt = time.Unix(0, startNS).UTC()
		if endNS, err := strconv.ParseInt(root.EndTimeUnixN, 10, 64); err == nil && endNS > 0 {
			sum.DurationMS = (endNS - startNS) / int64(time.Millisecond)
		}
	}

	// Attributes.
	for _, a := range root.Attributes {
		sv := attrStringValue(a.Value)
		switch a.Key {
		case "dagger.io/ci.repo":
			sum.CIRepo = sv
		case "dagger.io/ci":
			sum.CIProvider = sv
		case "dagger.io/engine.version":
			sum.Version = sv
		}
	}

	return sum
}

// attrStringValue extracts the stringValue from an OTLP attribute value, which
// is encoded as {"stringValue":"..."} (or other typed variants we ignore).
func attrStringValue(v json.RawMessage) string {
	var typed struct {
		StringValue string `json:"stringValue"`
	}
	if err := json.Unmarshal(v, &typed); err != nil {
		return ""
	}
	return typed.StringValue
}
