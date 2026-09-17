package repository

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestDeleteSeries(t *testing.T) {
	var gotPath string
	var gotQuery []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()["match[]"]
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	matchers := []string{`{trace_id="abc123"}`, `{job="dagger"}`}
	if err := client.DeleteSeries(context.Background(), matchers); err != nil {
		t.Fatalf("DeleteSeries: %v", err)
	}
	if gotPath != "/api/v1/admin/tsdb/delete_series" {
		t.Fatalf("path = %s, want /api/v1/admin/tsdb/delete_series", gotPath)
	}
	if len(gotQuery) != 2 || gotQuery[0] != `{trace_id="abc123"}` || gotQuery[1] != `{job="dagger"}` {
		t.Fatalf("match[] = %v", gotQuery)
	}
}

func TestDeleteSeriesEmptyMatchers(t *testing.T) {
	client := NewMetricsClient("http://victoria:8428")
	if err := client.DeleteSeries(context.Background(), nil); err != nil {
		t.Fatalf("DeleteSeries(nil): %v", err)
	}
}

func TestDeleteSeriesUnconfigured(t *testing.T) {
	client := NewMetricsClient("")
	err := client.DeleteSeries(context.Background(), []string{`{trace_id="abc123"}`})
	if err == nil || !strings.Contains(err.Error(), "victoria URL not configured") {
		t.Fatalf("err = %v, want victoria URL not configured", err)
	}
}

func TestDeleteSeriesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	err := client.DeleteSeries(context.Background(), []string{`{trace_id="abc123"}`})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want containing 500", err)
	}
}

func TestDeleteTraceSeries(t *testing.T) {
	const traceID = "401ccb197124a8ff2028720fcb5eaa06"
	var gotQuery []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()["match[]"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	if err := client.DeleteTraceSeries(context.Background(), traceID); err != nil {
		t.Fatalf("DeleteTraceSeries: %v", err)
	}
	if len(gotQuery) != 1 || gotQuery[0] != fmt.Sprintf(`{trace_id=%q}`, traceID) {
		t.Fatalf("match[] = %v", gotQuery)
	}
}

// TestDeleteTraceSeriesInvalidTraceID verifies the VictoriaMetrics delete path
// rejects a non-hex trace ID before it reaches the PromQL selector, mirroring
// the Loki path (CWE-94/CWE-74 defense-in-depth).
func TestDeleteTraceSeriesInvalidTraceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("delete request reached backend for invalid trace ID: %s", r.URL)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	err := client.DeleteTraceSeries(context.Background(), `abc"} or {__name__=~"secret`)
	if err == nil {
		t.Fatal("expected error for invalid trace ID")
	}
	if !strings.Contains(err.Error(), "invalid trace ID format") {
		t.Fatalf("err = %v, want invalid trace ID format", err)
	}
}

// TestDeleteTraceSeriesUnconfigured verifies the VictoriaMetrics delete path
// fails closed when no backend URL is configured.
func TestDeleteTraceSeriesUnconfigured(t *testing.T) {
	client := NewMetricsClient("")
	err := client.DeleteTraceSeries(context.Background(), "401ccb197124a8ff2028720fcb5eaa06")
	if err == nil || !strings.Contains(err.Error(), "victoria URL not configured") {
		t.Fatalf("err = %v, want victoria URL not configured", err)
	}
}

func TestQueryRangeSumsAndSorts(t *testing.T) {
	var gotPath, gotQuery, gotStart, gotEnd, gotStep string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		gotStart = r.URL.Query().Get("start")
		gotEnd = r.URL.Query().Get("end")
		gotStep = r.URL.Query().Get("step")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[` +
			`{"metric":{"pod":"a"},"values":[[100,"1.5"],[110,"2"]]},` +
			`{"metric":{"pod":"b"},"values":[[100,"0.5"],[120,"3"]]}]}}`))
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	start := time.Unix(100, 0)
	end := time.Unix(200, 0)
	points, err := client.QueryRange(context.Background(), `up`, start, end, 15*time.Second)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if gotPath != "/api/v1/query_range" {
		t.Fatalf("path = %s, want /api/v1/query_range", gotPath)
	}
	if gotQuery != "up" || gotStart != "100" || gotEnd != "200" || gotStep != "15" {
		t.Fatalf("query params = query=%q start=%q end=%q step=%q", gotQuery, gotStart, gotEnd, gotStep)
	}
	want := []domain.MetricPoint{{T: 100, V: 2}, {T: 110, V: 2}, {T: 120, V: 3}}
	if len(points) != len(want) {
		t.Fatalf("points = %v, want %v", points, want)
	}
	for i := range want {
		if points[i] != want[i] {
			t.Fatalf("points[%d] = %v, want %v", i, points[i], want[i])
		}
	}
}

func TestQueryRangeEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	points, err := client.QueryRange(context.Background(), `up`, time.Unix(0, 0), time.Unix(1, 0), time.Second)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("points = %v, want empty", points)
	}
}

func TestQueryRangeUnconfigured(t *testing.T) {
	client := NewMetricsClient("")
	_, err := client.QueryRange(context.Background(), `up`, time.Unix(0, 0), time.Unix(1, 0), time.Second)
	if err == nil || !strings.Contains(err.Error(), "victoria URL not configured") {
		t.Fatalf("err = %v, want victoria URL not configured", err)
	}
}

func TestQueryRangeEmptyQuery(t *testing.T) {
	client := NewMetricsClient("http://victoria:8428")
	_, err := client.QueryRange(context.Background(), "", time.Unix(0, 0), time.Unix(1, 0), time.Second)
	if err == nil || !strings.Contains(err.Error(), "query must not be empty") {
		t.Fatalf("err = %v, want query must not be empty", err)
	}
}

func TestQueryRangeNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	_, err := client.QueryRange(context.Background(), `up`, time.Unix(0, 0), time.Unix(1, 0), time.Second)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want containing 502", err)
	}
}

func TestQueryRangeInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	_, err := client.QueryRange(context.Background(), `up`, time.Unix(0, 0), time.Unix(1, 0), time.Second)
	if err == nil || !strings.Contains(err.Error(), "decode victoria query_range response") {
		t.Fatalf("err = %v, want decode error", err)
	}
}
