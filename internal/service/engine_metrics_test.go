package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// stubMetricsQueryer records the queries it receives and returns canned points
// or an error. Recorded (dagger_engine_*) queries return recordedPoints; live
// (container_*) queries return points.
type stubMetricsQueryer struct {
	queries        []string
	points         []domain.MetricPoint
	recordedPoints []domain.MetricPoint
	err            error
}

func (s *stubMetricsQueryer) QueryRange(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]domain.MetricPoint, error) {
	s.queries = append(s.queries, query)
	if s.err != nil {
		return nil, s.err
	}
	if strings.HasPrefix(query, recordedMetricPrefix) {
		return s.recordedPoints, nil
	}
	return s.points, nil
}

// liveQueries returns the recorded queries that are not recorded-metric reads.
func (s *stubMetricsQueryer) liveQueries() []string {
	var out []string
	for _, q := range s.queries {
		if !strings.HasPrefix(q, recordedMetricPrefix) {
			out = append(out, q)
		}
	}
	return out
}

func testMetricsLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

func TestEngineMetricsTraceMetricsNilMeta(t *testing.T) {
	q := &stubMetricsQueryer{}
	svc := NewEngineMetricsService(q, "dagger-kubernetes", 15*time.Second, time.Minute, testMetricsLogger())

	tm, err := svc.TraceMetrics(context.Background(), nil)
	if err != nil {
		t.Fatalf("TraceMetrics(nil): %v", err)
	}
	if tm == nil || len(tm.Series) != 0 {
		t.Fatalf("series = %v, want empty", tm)
	}
	if tm.StepSeconds != 15 {
		t.Fatalf("step_seconds = %d, want 15", tm.StepSeconds)
	}
	if len(q.queries) != 0 {
		t.Fatalf("queries = %v, want none", q.queries)
	}
}

func TestEngineMetricsUnknownVersion(t *testing.T) {
	q := &stubMetricsQueryer{}
	svc := NewEngineMetricsService(q, "dagger-kubernetes", 15*time.Second, time.Minute, testMetricsLogger())

	tm, err := svc.TraceMetrics(context.Background(), &domain.TraceMeta{TraceID: "abc"})
	if err != nil {
		t.Fatalf("TraceMetrics: %v", err)
	}
	if len(tm.Series) != 0 {
		t.Fatalf("series = %v, want empty", tm.Series)
	}
	if len(q.queries) != 0 {
		t.Fatalf("queries = %v, want none", q.queries)
	}
}

func TestEngineMetricsNilQueryer(t *testing.T) {
	svc := NewEngineMetricsService(nil, "dagger-kubernetes", 15*time.Second, time.Minute, testMetricsLogger())
	tm, err := svc.TraceMetrics(context.Background(), &domain.TraceMeta{TraceID: "abc", Version: "v0.21.4"})
	if err != nil {
		t.Fatalf("TraceMetrics: %v", err)
	}
	if len(tm.Series) != 0 {
		t.Fatalf("series = %v, want empty", tm.Series)
	}
}

// TestEngineMetricsRecordedFirst verifies recorded dagger_engine_* samples win
// over the live cAdvisor query when present.
func TestEngineMetricsRecordedFirst(t *testing.T) {
	const traceID = "abcdef0123456789abcdef0123456789"
	q := &stubMetricsQueryer{
		points:         []domain.MetricPoint{{T: 1, V: 99}},
		recordedPoints: []domain.MetricPoint{{T: 100, V: 1.5}},
	}
	svc := NewEngineMetricsService(q, "my-ns", 15*time.Second, time.Minute, testMetricsLogger())

	meta := &domain.TraceMeta{TraceID: traceID, Version: "v0.21.4", StartedAt: time.Unix(1000, 0), DurationMS: 60000}
	tm, err := svc.TraceMetrics(context.Background(), meta)
	if err != nil {
		t.Fatalf("TraceMetrics: %v", err)
	}
	if len(tm.Series) != len(defaultMetricQueries) {
		t.Fatalf("series = %d, want %d", len(tm.Series), len(defaultMetricQueries))
	}
	if tm.Series[0].Name != "cpu" || tm.Series[0].Unit != "cores" {
		t.Fatalf("series[0] = %+v, want cpu/cores", tm.Series[0])
	}
	if len(tm.Series[0].Points) != 1 || tm.Series[0].Points[0].V != 1.5 {
		t.Fatalf("series[0].points = %+v, want recorded 1.5", tm.Series[0].Points)
	}
	if live := q.liveQueries(); len(live) != 0 {
		t.Fatalf("live queries = %v, want none when recorded data exists", live)
	}
	for _, query := range q.queries {
		if !strings.HasPrefix(query, recordedMetricPrefix) {
			t.Fatalf("query %q is not a recorded-metric read", query)
		}
		if !strings.Contains(query, `trace_id="`+traceID+`"`) {
			t.Fatalf("query %q missing trace_id label", query)
		}
	}
}

// TestEngineMetricsFallbackToLive verifies the live cAdvisor query runs (with
// the configured rate window substituted) when nothing was recorded.
func TestEngineMetricsFallbackToLive(t *testing.T) {
	q := &stubMetricsQueryer{points: []domain.MetricPoint{{T: 1, V: 2}}}
	svc := NewEngineMetricsService(q, "my-ns", 15*time.Second, time.Minute, testMetricsLogger())

	meta := &domain.TraceMeta{
		TraceID:    "abcdef0123456789abcdef0123456789",
		Version:    "v0.21.4",
		StartedAt:  time.Unix(1000, 0),
		DurationMS: 60000,
	}
	tm, err := svc.TraceMetrics(context.Background(), meta)
	if err != nil {
		t.Fatalf("TraceMetrics: %v", err)
	}
	if len(tm.Series) != len(defaultMetricQueries) {
		t.Fatalf("series = %d, want %d", len(tm.Series), len(defaultMetricQueries))
	}
	live := q.liveQueries()
	if len(live) != len(defaultMetricQueries) {
		t.Fatalf("live queries = %d, want %d", len(live), len(defaultMetricQueries))
	}
	for _, query := range live {
		if strings.Contains(query, "{ns}") || strings.Contains(query, "{pod}") || strings.Contains(query, "{rate}") {
			t.Fatalf("query %q still contains template vars", query)
		}
		if !strings.Contains(query, `namespace="my-ns"`) {
			t.Fatalf("query %q missing namespace substitution", query)
		}
		if !strings.Contains(query, `pod=~"dagger-engine-v0-21-4-.*"`) {
			t.Fatalf("query %q missing pod substitution", query)
		}
		if strings.Contains(query, "rate(") && !strings.Contains(query, "[60s]") {
			t.Fatalf("query %q missing rate window substitution", query)
		}
	}
	if tm.TraceID != meta.TraceID {
		t.Fatalf("trace_id = %q, want %q", tm.TraceID, meta.TraceID)
	}
	if tm.StartTime.Unix() != 1000 {
		t.Fatalf("start_time = %v, want unix 1000", tm.StartTime)
	}
	if tm.EndTime.Unix() != 1060 {
		t.Fatalf("end_time = %v, want unix 1060", tm.EndTime)
	}
}

// TestEngineMetricsRejectsUnsafeVersion verifies a client-supplied engine
// version cannot break out of the PromQL pod selector (CWE-89/CWE-943).
func TestEngineMetricsRejectsUnsafeVersion(t *testing.T) {
	q := &stubMetricsQueryer{points: []domain.MetricPoint{{T: 1, V: 2}}}
	svc := NewEngineMetricsService(q, "ns", 15*time.Second, time.Minute, testMetricsLogger())

	meta := &domain.TraceMeta{
		TraceID: "abcdef0123456789abcdef0123456789",
		Version: `v0.21.4"}[5m]) or vector(1) or rate(up{pod=~"x`,
	}
	tm, err := svc.TraceMetrics(context.Background(), meta)
	if err != nil {
		t.Fatalf("TraceMetrics: %v", err)
	}
	if len(tm.Series) != 0 {
		t.Fatalf("series = %v, want empty", tm.Series)
	}
	if len(q.queries) != 0 {
		t.Fatalf("queries = %v, want none", q.queries)
	}
}

// TestEngineMetricsEscapesNamespace verifies the namespace label value is
// escaped before interpolation (defense-in-depth).
func TestEngineMetricsEscapesNamespace(t *testing.T) {
	q := &stubMetricsQueryer{points: []domain.MetricPoint{{T: 1, V: 2}}}
	svc := NewEngineMetricsService(q, `ns"evil`, 15*time.Second, time.Minute, testMetricsLogger())

	meta := &domain.TraceMeta{TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4", StartedAt: time.Unix(1000, 0), DurationMS: 1000}
	if _, err := svc.TraceMetrics(context.Background(), meta); err != nil {
		t.Fatalf("TraceMetrics: %v", err)
	}
	live := q.liveQueries()
	if len(live) == 0 {
		t.Fatal("expected live queries")
	}
	for _, query := range live {
		if strings.Contains(query, `namespace="ns"evil"`) {
			t.Fatalf("query %q contains unescaped namespace", query)
		}
		if !strings.Contains(query, `namespace="ns\"evil"`) {
			t.Fatalf("query %q missing escaped namespace", query)
		}
	}
}

func TestEngineMetricsStepAndRateClamp(t *testing.T) {
	svc := NewEngineMetricsService(&stubMetricsQueryer{}, "ns", 0, 0, testMetricsLogger())
	if svc.step != time.Second {
		t.Fatalf("step = %v, want 1s", svc.step)
	}
	if svc.rateWindow != time.Second {
		t.Fatalf("rateWindow = %v, want 1s", svc.rateWindow)
	}
}

func TestEngineMetricsRateString(t *testing.T) {
	svc := NewEngineMetricsService(&stubMetricsQueryer{}, "ns", 15*time.Second, 90*time.Second, testMetricsLogger())
	if got := svc.rateString(); got != "90s" {
		t.Fatalf("rateString = %q, want 90s", got)
	}
}

func TestEngineMetricsAllQueriesFail(t *testing.T) {
	q := &stubMetricsQueryer{err: errors.New("boom")}
	svc := NewEngineMetricsService(q, "ns", 15*time.Second, time.Minute, testMetricsLogger())

	_, err := svc.TraceMetrics(context.Background(), &domain.TraceMeta{TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom", err)
	}
}

func TestEngineMetricsPartialFailure(t *testing.T) {
	// A queryer that fails only the first live query, then succeeds.
	q := &flakyMetricsQueryer{}
	svc := NewEngineMetricsService(q, "ns", 15*time.Second, time.Minute, testMetricsLogger())

	tm, err := svc.TraceMetrics(context.Background(), &domain.TraceMeta{TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4"})
	if err != nil {
		t.Fatalf("TraceMetrics: %v", err)
	}
	if len(tm.Series) != len(defaultMetricQueries)-1 {
		t.Fatalf("series = %d, want %d", len(tm.Series), len(defaultMetricQueries)-1)
	}
}

type flakyMetricsQueryer struct {
	calls int
}

func (f *flakyMetricsQueryer) QueryRange(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]domain.MetricPoint, error) {
	if strings.HasPrefix(query, recordedMetricPrefix) {
		return nil, nil
	}
	f.calls++
	if f.calls == 1 {
		return nil, errors.New("first query failed")
	}
	return []domain.MetricPoint{{T: 1, V: 1}}, nil
}

func TestMetricsWindow(t *testing.T) {
	now := time.Unix(100000, 0)
	cases := []struct {
		name      string
		meta      *domain.TraceMeta
		wantStart time.Time
		wantEnd   time.Time
	}{
		{
			name:      "finished trace",
			meta:      &domain.TraceMeta{StartedAt: time.Unix(1000, 0), DurationMS: 60000},
			wantStart: time.Unix(1000, 0),
			wantEnd:   time.Unix(1060, 0),
		},
		{
			name:      "running trace uses now",
			meta:      &domain.TraceMeta{StartedAt: time.Unix(99000, 0), DurationMS: 0},
			wantStart: time.Unix(99000, 0),
			wantEnd:   now,
		},
		{
			name:      "unknown start uses last 24h",
			meta:      &domain.TraceMeta{},
			wantStart: now.Add(-maxMetricsWindow),
			wantEnd:   now,
		},
		{
			name:      "window clamped to max",
			meta:      &domain.TraceMeta{StartedAt: time.Unix(1000, 0), DurationMS: int64(48 * time.Hour / time.Millisecond)},
			wantStart: time.Unix(1000, 0),
			wantEnd:   time.Unix(1000, 0).Add(maxMetricsWindow),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := metricsWindow(tc.meta, now)
			if !start.Equal(tc.wantStart) {
				t.Fatalf("start = %v, want %v", start, tc.wantStart)
			}
			if !end.Equal(tc.wantEnd) {
				t.Fatalf("end = %v, want %v", end, tc.wantEnd)
			}
		})
	}
}
