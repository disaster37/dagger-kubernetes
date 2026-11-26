package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
)

// TestEngineMetricsIngest proves engine-emitted OTLP resource metrics
// (changelog 0.21.10, entry 23) are accepted at POST /v1/metrics and forwarded
// verbatim to the collector, so VictoriaMetrics can store them.
func TestEngineMetricsIngest(t *testing.T) {
	type forwarded struct {
		path string
		body []byte
	}
	received := make(chan forwarded, 4)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		received <- forwarded{path: r.URL.Path, body: body.Bytes()}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	srv := newParityServerWith(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, nil, nil, func(cfg *handler.ServerConfig) {
		cfg.CollectorURL = collector.URL
	})
	_ = srv

	metrics := map[string]any{
		"resourceMetrics": []any{
			map[string]any{
				"resource": map[string]any{
					"attributes": []any{
						map[string]any{"key": "service.name", "value": map[string]any{"stringValue": "dagger-engine"}},
						map[string]any{"key": "dagger.io/engine.version", "value": map[string]any{"stringValue": "v0.21.10"}},
					},
				},
				"scopeMetrics": []any{
					map[string]any{
						"metrics": []any{
							map[string]any{
								"name": "container.cpu.usage",
								"sum": map[string]any{
									"dataPoints": []any{
										map[string]any{"asDouble": 0.42, "timeUnixNano": "1700000000000000000"},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	payload, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.base+"/v1/metrics", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.admin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dagger-Org", "acme")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest status = %d body=%s", resp.StatusCode, body)
	}
	select {
	case got := <-received:
		if got.path != "/v1/metrics" {
			t.Fatalf("forwarded path = %q, want /v1/metrics", got.path)
		}
		var decoded map[string]any
		if err := json.Unmarshal(got.body, &decoded); err != nil {
			t.Fatalf("forwarded body is not JSON: %v", err)
		}
		if _, ok := decoded["resourceMetrics"]; !ok {
			t.Fatalf("forwarded body lost resourceMetrics: %s", got.body)
		}
	default:
		t.Fatal("collector did not receive the metrics payload")
	}
}
