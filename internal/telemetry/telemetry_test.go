package telemetry

import (
	"testing"
)

func TestNewLogsClient(t *testing.T) {
	client := NewLogsClient("http://loki:3100")
	if client == nil {
		t.Fatal("nil logs client")
	}
	if client.lokiURL != "http://loki:3100" {
		t.Fatalf("lokiURL = %q", client.lokiURL)
	}
}

func TestNewLogsClientEmptyURL(t *testing.T) {
	client := NewLogsClient("")
	if client == nil {
		t.Fatal("nil logs client")
	}
}

func TestNewMetricsClient(t *testing.T) {
	client := NewMetricsClient("http://victoria:8428")
	if client == nil {
		t.Fatal("nil metrics client")
	}
	if client.victoriaURL != "http://victoria:8428" {
		t.Fatalf("victoriaURL = %q", client.victoriaURL)
	}
}

func TestNewMetricsClientEmptyURL(t *testing.T) {
	client := NewMetricsClient("")
	if client == nil {
		t.Fatal("nil metrics client")
	}
}

func TestNewSpanTreeReconstructor(t *testing.T) {
	r := NewSpanTreeReconstructor("http://tempo:3200")
	if r == nil {
		t.Fatal("nil reconstructor")
	}
	if r.tempoURL != "http://tempo:3200" {
		t.Fatalf("tempoURL = %q", r.tempoURL)
	}
}

func TestSanitizeLogQLValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{`contains"quote`, `contains\"quote`},
		{`has{brace`, `has\{brace`},
		{"multi\nline", `multi\nline`},
		{`back\slash`, `back\\slash`},
	}

	for _, tt := range tests {
		got := sanitizeLogQLValue(tt.input)
		if got != tt.want {
			t.Fatalf("sanitizeLogQLValue(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseNanos(t *testing.T) {
	ts, err := parseNanos("1700000000000000000")
	if err != nil {
		t.Fatalf("parseNanos: %v", err)
	}
	if ts.UnixNano() != 1700000000000000000 {
		t.Fatalf("unexpected timestamp: %v", ts)
	}
}

func TestParseNanosInvalid(t *testing.T) {
	_, err := parseNanos("not-a-number")
	if err == nil {
		t.Fatal("expected error for invalid nanos")
	}
}

func TestNewLiveHub(t *testing.T) {
	hub := NewLiveHub()
	if hub == nil {
		t.Fatal("nil hub")
	}
	if hub.ClientCount("test-trace") != 0 {
		t.Fatal("expected 0 clients initially")
	}
}

func TestLiveHubClientCounts(t *testing.T) {
	hub := NewLiveHub()

	c1 := &LiveClient{TraceID: "t1", Send: make(chan []byte, 256), done: make(chan struct{})}
	c2 := &LiveClient{TraceID: "t1", Send: make(chan []byte, 256), done: make(chan struct{})}

	// Directly add to internal map (bypass writePump due to nil writer).
	hub.mu.Lock()
	if hub.clients["t1"] == nil {
		hub.clients["t1"] = make(map[*LiveClient]bool)
	}
	hub.clients["t1"][c1] = true
	hub.clients["t1"][c2] = true
	hub.mu.Unlock()

	if hub.ClientCount("t1") != 2 {
		t.Fatalf("expected 2 clients, got %d", hub.ClientCount("t1"))
	}

	hub.mu.Lock()
	delete(hub.clients["t1"], c1)
	if len(hub.clients["t1"]) == 0 {
		delete(hub.clients, "t1")
	}
	hub.mu.Unlock()

	if hub.ClientCount("t1") != 1 {
		t.Fatalf("expected 1 client after removal, got %d", hub.ClientCount("t1"))
	}
}

func TestLiveHubBroadcastNoClients(t *testing.T) {
	hub := NewLiveHub()
	// Should not panic.
	hub.BroadcastSpanUpdate("test-trace", map[string]string{"status": "running"})
}

func TestLiveHubBroadcastWithClient(t *testing.T) {
	hub := NewLiveHub()

	client := &LiveClient{
		TraceID: "test-trace",
		Send:    make(chan []byte, 256),
		done:    make(chan struct{}),
	}

	// Directly add to internal map to bypass writePump.
	hub.mu.Lock()
	if hub.clients["test-trace"] == nil {
		hub.clients["test-trace"] = make(map[*LiveClient]bool)
	}
	hub.clients["test-trace"][client] = true
	hub.mu.Unlock()

	hub.BroadcastSpanUpdate("test-trace", map[string]string{"status": "done"})

	select {
	case msg := <-client.Send:
		if len(msg) == 0 {
			t.Fatal("empty message received")
		}
	default:
		t.Fatal("expected message in send channel")
	}
}

func TestLiveClientDone(t *testing.T) {
	c := &LiveClient{
		TraceID: "test-trace",
		Send:    make(chan []byte, 256),
		done:    make(chan struct{}),
	}
	done := c.Done()
	if done == nil {
		t.Fatal("nil done channel")
	}
}

func TestExtractSpansNilInput(t *testing.T) {
	spans := extractSpans(nil)
	if spans != nil {
		t.Fatalf("expected nil, got %v", spans)
	}
}

func TestExtractSpansNoBatches(t *testing.T) {
	spans := extractSpans(map[string]interface{}{})
	if spans != nil {
		t.Fatalf("expected nil, got %v", spans)
	}
}

func TestExtractSpansBatchesWrongType(t *testing.T) {
	spans := extractSpans(map[string]interface{}{"batches": "not-a-slice"})
	if spans != nil {
		t.Fatalf("expected nil, got %v", spans)
	}
}

func TestMapToSpanNodeNil(t *testing.T) {
	node := mapToSpanNode(nil)
	if node != nil {
		t.Fatalf("expected nil, got %v", node)
	}
}

func TestMapToSpanNodeEmptyID(t *testing.T) {
	node := mapToSpanNode(map[string]interface{}{"spanId": ""})
	if node != nil {
		t.Fatalf("expected nil, got %v", node)
	}
}

func TestMapToSpanNodeBasic(t *testing.T) {
	node := mapToSpanNode(map[string]interface{}{
		"spanId":  "abc123",
		"traceId": "trace-1",
		"name":    "test-operation",
	})
	if node == nil {
		t.Fatal("nil span node")
	}
	if node.SpanID != "abc123" {
		t.Fatalf("spanID = %q", node.SpanID)
	}
	if node.Name != "test-operation" {
		t.Fatalf("name = %q", node.Name)
	}
}

func TestMapToSpanNodeStatus(t *testing.T) {
	node := mapToSpanNode(map[string]interface{}{
		"spanId": "abc123",
		"status": map[string]interface{}{"code": float64(2)},
	})
	if node == nil {
		t.Fatal("nil node")
	}
	if node.Status != "failed" {
		t.Fatalf("expected failed status, got %q", node.Status)
	}
}

func TestMapToSpanNodeTimestamps(t *testing.T) {
	node := mapToSpanNode(map[string]interface{}{
		"spanId":            "abc123",
		"startTimeUnixNano": "1000000000",
		"endTimeUnixNano":   "5000000000",
	})
	if node == nil {
		t.Fatal("nil node")
	}
	if node.StartTime.UnixNano() != 1000000000 {
		t.Fatalf("startTime = %v", node.StartTime.UnixNano())
	}
	if node.Duration != 4000000000 {
		t.Fatalf("duration = %v", node.Duration)
	}
}

func TestMapToSpanNodeAttributes(t *testing.T) {
	node := mapToSpanNode(map[string]interface{}{
		"spanId": "abc123",
		"attributes": []interface{}{
			map[string]interface{}{
				"key": "dagger.io/engine.version",
				"value": map[string]interface{}{
					"stringValue": "v0.21.4",
				},
			},
		},
	})
	if node == nil {
		t.Fatal("nil node")
	}
	if node.Attributes["dagger.io/engine.version"] != "v0.21.4" {
		t.Fatalf("attribute = %q", node.Attributes["dagger.io/engine.version"])
	}
}

func TestReconstructEmptyTrace(t *testing.T) {
	r := &SpanTreeReconstructor{tempoURL: "http://tempo:3200"}
	info := r.reconstruct("test-trace", map[string]interface{}{})
	if info == nil {
		t.Fatal("nil trace info")
	}
	if info.TraceID != "test-trace" {
		t.Fatalf("traceID = %q", info.TraceID)
	}
}

func TestReconstructWithRoot(t *testing.T) {
	r := &SpanTreeReconstructor{tempoURL: "http://tempo:3200"}
	info := r.reconstruct("test-trace", map[string]interface{}{
		"batches": []interface{}{
			map[string]interface{}{
				"scopeSpans": []interface{}{
					map[string]interface{}{
						"spans": []interface{}{
							map[string]interface{}{
								"spanId":            "root",
								"traceId":           "test-trace",
								"name":              "root-op",
								"startTimeUnixNano": "1000000",
								"endTimeUnixNano":   "2000000",
								"status":            map[string]interface{}{"code": float64(1)},
							},
							map[string]interface{}{
								"spanId":       "child",
								"parentSpanId": "root",
								"traceId":      "test-trace",
								"name":         "child-op",
							},
						},
					},
				},
			},
		},
	})
	if info == nil {
		t.Fatal("nil trace info")
	}
	if info.RootSpan == nil {
		t.Fatal("nil root span")
	}
	if info.RootSpan.Name != "root-op" {
		t.Fatalf("root name = %q", info.RootSpan.Name)
	}
	if len(info.RootSpan.Children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(info.RootSpan.Children))
	}
	if info.RootSpan.Children[0].Name != "child-op" {
		t.Fatalf("child name = %q", info.RootSpan.Children[0].Name)
	}
	if info.Status != "success" {
		t.Fatalf("status = %q", info.Status)
	}
}
