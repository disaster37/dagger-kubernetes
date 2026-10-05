package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// TestQueryRangeSkipsNonFinite verifies NaN/Inf samples (which PromQL can
// legitimately produce) are dropped rather than poisoning the JSON response.
func TestQueryRangeSkipsNonFinite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[` +
			`{"metric":{},"values":[[100,"NaN"],[110,"+Inf"],[120,"2"]]}]}}`))
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	points, err := client.QueryRange(context.Background(), `up`, time.Unix(0, 0), time.Unix(200, 0), time.Second)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	want := []domain.MetricPoint{{T: 120, V: 2}}
	if len(points) != len(want) || points[0] != want[0] {
		t.Fatalf("points = %v, want %v", points, want)
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

func TestQueryInstantSumsFinite(t *testing.T) {
	var gotPath, gotQuery, gotTime string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		gotTime = r.URL.Query().Get("time")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"pod":"a"},"value":[100,"1.5"]},` +
			`{"metric":{"pod":"b"},"value":[100,"2.5"]}]}}`))
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	val, ok, err := client.QueryInstant(context.Background(), `up`, time.Unix(100, 0))
	if err != nil {
		t.Fatalf("QueryInstant: %v", err)
	}
	if !ok || val != 4 {
		t.Fatalf("val = %v ok = %v, want 4 true", val, ok)
	}
	if gotPath != "/api/v1/query" {
		t.Fatalf("path = %s, want /api/v1/query", gotPath)
	}
	if gotQuery != "up" || gotTime != "100" {
		t.Fatalf("params = query=%q time=%q", gotQuery, gotTime)
	}
}

func TestQueryInstantEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	val, ok, err := client.QueryInstant(context.Background(), `up`, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("QueryInstant: %v", err)
	}
	if ok || val != 0 {
		t.Fatalf("val = %v ok = %v, want 0 false", val, ok)
	}
}

func TestQueryInstantSkipsNonFinite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{},"value":[100,"NaN"]},` +
			`{"metric":{},"value":[100,"+Inf"]},` +
			`{"metric":{},"value":[100,"2"]}]}}`))
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	val, ok, err := client.QueryInstant(context.Background(), `up`, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("QueryInstant: %v", err)
	}
	if !ok || val != 2 {
		t.Fatalf("val = %v ok = %v, want 2 true", val, ok)
	}
}

func TestQueryInstantNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	_, _, err := client.QueryInstant(context.Background(), `up`, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want containing 502", err)
	}
}

func TestQueryInstantUnconfigured(t *testing.T) {
	client := NewMetricsClient("")
	_, _, err := client.QueryInstant(context.Background(), `up`, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "victoria URL not configured") {
		t.Fatalf("err = %v, want victoria URL not configured", err)
	}
}

func TestQueryInstantEmptyQuery(t *testing.T) {
	client := NewMetricsClient("http://victoria:8428")
	_, _, err := client.QueryInstant(context.Background(), "", time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "query must not be empty") {
		t.Fatalf("err = %v, want query must not be empty", err)
	}
}

func TestWriteSamples(t *testing.T) {
	var gotPath, gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	points := []domain.MetricPoint{{T: 100, V: 1.5}, {T: 115, V: 2}}
	if err := client.WriteSamples(context.Background(), "dagger_engine_cpu", map[string]string{"trace_id": "abc", "version": "v0.21.4"}, points); err != nil {
		t.Fatalf("WriteSamples: %v", err)
	}
	if gotPath != "/api/v1/import" {
		t.Fatalf("path = %s, want /api/v1/import", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", gotContentType)
	}
	lines := strings.Split(strings.TrimSpace(string(gotBody)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2 (%q)", len(lines), gotBody)
	}
	var first importSample
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode line: %v", err)
	}
	if first.Metric["__name__"] != "dagger_engine_cpu" || first.Metric["trace_id"] != "abc" || first.Metric["version"] != "v0.21.4" {
		t.Fatalf("metric = %v", first.Metric)
	}
	if len(first.Values) != 1 || first.Values[0] != 1.5 || len(first.Timestamps) != 1 || first.Timestamps[0] != 100000 {
		t.Fatalf("sample = %+v, want value 1.5 @ 100000ms", first)
	}
}

func TestWriteSamplesEmptyPoints(t *testing.T) {
	client := NewMetricsClient("http://victoria:8428")
	if err := client.WriteSamples(context.Background(), "dagger_engine_cpu", nil, nil); err != nil {
		t.Fatalf("WriteSamples(nil): %v", err)
	}
}

func TestWriteSamplesUnconfigured(t *testing.T) {
	client := NewMetricsClient("")
	err := client.WriteSamples(context.Background(), "dagger_engine_cpu", nil, []domain.MetricPoint{{T: 1, V: 1}})
	if err == nil || !strings.Contains(err.Error(), "victoria URL not configured") {
		t.Fatalf("err = %v, want victoria URL not configured", err)
	}
}

func TestWriteSamplesEmptyMetricName(t *testing.T) {
	client := NewMetricsClient("http://victoria:8428")
	err := client.WriteSamples(context.Background(), "", nil, []domain.MetricPoint{{T: 1, V: 1}})
	if err == nil || !strings.Contains(err.Error(), "metric name must not be empty") {
		t.Fatalf("err = %v, want metric name must not be empty", err)
	}
}

func TestWriteSamplesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	err := client.WriteSamples(context.Background(), "dagger_engine_cpu", nil, []domain.MetricPoint{{T: 1, V: 1}})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want containing 500", err)
	}
}

func TestWriteSamplesDropsNonFinite(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewMetricsClient(srv.URL)
	points := []domain.MetricPoint{{T: 1, V: math.NaN()}, {T: 2, V: math.Inf(1)}, {T: 3, V: 4}}
	if err := client.WriteSamples(context.Background(), "dagger_engine_cpu", nil, points); err != nil {
		t.Fatalf("WriteSamples: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(gotBody)), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1 (%q)", len(lines), gotBody)
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
