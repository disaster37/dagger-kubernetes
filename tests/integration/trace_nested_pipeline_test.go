package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// TestNestedPipelineTreeNests proves a nested pipeline whose spans arrive
// child-before-parent still renders nested under the initiating step, with the
// outer span as root (changelog 1.0, entry 30; ADR-061 G5).
//
// There is no live Tempo in the test environment, so the reconstructor is
// pointed at a fake Tempo API serving the same JSON shape Tempo returns.
func TestNestedPipelineTreeNests(t *testing.T) {
	payload := map[string]any{
		"batches": []any{
			map[string]any{
				"scopeSpans": []any{
					map[string]any{
						"spans": []any{
							map[string]any{
								"spanId": "nested-child", "parentSpanId": "nested-root",
								"traceId": "abcdef0123456789abcdef0123456789", "name": "nested-op",
								"startTimeUnixNano": "3000000",
							},
							map[string]any{
								"spanId": "nested-root", "parentSpanId": "root",
								"traceId": "abcdef0123456789abcdef0123456789", "name": "nested-run",
								"startTimeUnixNano": "2000000",
							},
							map[string]any{
								"spanId": "root", "traceId": "abcdef0123456789abcdef0123456789", "name": "outer",
								"startTimeUnixNano": "1000000", "endTimeUnixNano": "4000000",
								"status": map[string]any{"code": "STATUS_CODE_OK"},
							},
						},
					},
				},
			},
		},
	}
	tempo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/traces/abcdef0123456789abcdef0123456789" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer tempo.Close()

	trace, err := repository.NewSpanTreeReconstructor(tempo.URL).GetTrace("abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if trace.RootSpan == nil || trace.RootSpan.Name != "outer" {
		t.Fatalf("root = %+v, want outer", trace.RootSpan)
	}
	if len(trace.RootSpan.Children) != 1 || trace.RootSpan.Children[0].Name != "nested-run" {
		t.Fatalf("nested run not adopted under root: %+v", trace.RootSpan.Children)
	}
	if len(trace.RootSpan.Children[0].Children) != 1 || trace.RootSpan.Children[0].Children[0].Name != "nested-op" {
		t.Fatalf("nested op not under nested run: %+v", trace.RootSpan.Children[0].Children)
	}
}
