package telemetry

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

type SpanNode struct {
	SpanID      string              `json:"span_id"`
	ParentSpanID string             `json:"parent_span_id"`
	TraceID     string              `json:"trace_id"`
	Name        string              `json:"name"`
	Status      string              `json:"status"`
	StartTime   time.Time           `json:"start_time"`
	Duration    time.Duration       `json:"duration_ms"`
	Attributes  map[string]string   `json:"attributes"`
	Children    []*SpanNode         `json:"children"`
	Logs        []SpanLog           `json:"logs,omitempty"`
}

type SpanLog struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

type TraceInfo struct {
	TraceID    string    `json:"trace_id"`
	RootSpan   *SpanNode `json:"root_span"`
	Status     string    `json:"status"`
	StartTime  time.Time `json:"start_time"`
	Duration   time.Duration `json:"duration_ms"`
	Version    string    `json:"version"`
	CIProvider string    `json:"ci_provider,omitempty"`
	CIRepo     string    `json:"ci_repo,omitempty"`
}

type SpanTreeReconstructor struct {
	tempoURL   string
	httpClient *http.Client
}

func NewSpanTreeReconstructor(tempoURL string) *SpanTreeReconstructor {
	return &SpanTreeReconstructor{
		tempoURL: tempoURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (r *SpanTreeReconstructor) GetTrace(traceID string) (*TraceInfo, error) {
	resp, err := r.httpClient.Get(r.tempoURL + "/api/traces/" + traceID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var raw map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	return r.reconstruct(traceID, raw), nil
}

func (r *SpanTreeReconstructor) reconstruct(traceID string, raw map[string]interface{}) *TraceInfo {
	spans := extractSpans(raw)

	nodes := make(map[string]*SpanNode)
	for _, s := range spans {
		nodes[s.SpanID] = s
	}

	var root *SpanNode
	for _, s := range spans {
		parent, ok := nodes[s.ParentSpanID]
		if !ok {
			root = s
		} else {
			parent.Children = append(parent.Children, s)
		}
	}

	info := &TraceInfo{
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

func extractSpans(raw map[string]interface{}) []*SpanNode {
	batch, ok := raw["batches"]
	if !ok {
		return nil
	}

	batches, ok := batch.([]interface{})
	if !ok {
		return nil
	}

	var spans []*SpanNode
	for _, b := range batches {
		batchMap, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		scopeSpans, ok := batchMap["scopeSpans"]
		if !ok {
			continue
		}
		ssList, ok := scopeSpans.([]interface{})
		if !ok {
			continue
		}
		for _, ss := range ssList {
			ssMap, ok := ss.(map[string]interface{})
			if !ok {
				continue
			}
			spanList, ok := ssMap["spans"]
			if !ok {
				continue
			}
			sl, ok := spanList.([]interface{})
			if !ok {
				continue
			}
			for _, sp := range sl {
				spMap, ok := sp.(map[string]interface{})
				if !ok {
					continue
				}
				node := mapToSpanNode(spMap)
				if node != nil {
					spans = append(spans, node)
				}
			}
		}
	}

	return spans
}

func mapToSpanNode(m map[string]interface{}) *SpanNode {
	name, _ := m["name"].(string)
	spanID, _ := m["spanId"].(string)
	parentSpanID, _ := m["parentSpanId"].(string)
	traceID, _ := m["traceId"].(string)

	if spanID == "" {
		return nil
	}

	node := &SpanNode{
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
