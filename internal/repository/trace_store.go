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

	nodes := make(map[string]*domain.SpanNode)
	for _, s := range spans {
		nodes[s.SpanID] = s
	}

	var root *domain.SpanNode
	for _, s := range spans {
		parent, ok := nodes[s.ParentSpanID]
		if !ok {
			root = s
		} else {
			parent.Children = append(parent.Children, s)
		}
	}

	info := &domain.TraceInfo{
		TraceID:  traceID,
		RootSpan: root,
		Status:   "running",
	}

	if root != nil {
		info.StartTime = root.StartTime
		info.Duration = root.Duration
		info.Status = root.Status
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

	return info
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
	}

	if status, ok := m["status"].(map[string]interface{}); ok {
		if code, ok := status["code"].(float64); ok {
			switch code {
			case 0:
				node.Status = "unset"
			case 1:
				node.Status = "success"
			case 2:
				node.Status = "failed"
			}
		}
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
				}
			}
		}
	}

	return node
}
