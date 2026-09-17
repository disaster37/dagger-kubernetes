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
// or an error.
type stubMetricsQueryer struct {
	queries []string
	points  []domain.MetricPoint
	err     error
}

func (s *stubMetricsQueryer) QueryRange(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]domain.MetricPoint, error) {
	s.queries = append(s.queries, query)
	if s.err != nil {
		return nil, s.err
	}
	return s.points, nil
}

func testMetricsLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

func TestEngineMetricsTraceMetricsNilMeta(t *testing.T) {
	q := &stubMetricsQueryer{}
	svc := NewEngineMetricsService(q, "dagger-kubernetes", 15*time.Second, testMetricsLogger())

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
	svc := NewEngineMetricsService(q, "dagger-kubernetes", 15*time.Second, testMetricsLogger())

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
	svc := NewEngineMetricsService(nil, "dagger-kubernetes", 15*time.Second, testMetricsLogger())
	tm, err := svc.TraceMetrics(context.Background(), &domain.TraceMeta{TraceID: "abc", Version: "v0.21.4"})
	if err != nil {
		t.Fatalf("TraceMetrics: %v", err)
	}
	if len(tm.Series) != 0 {
		t.Fatalf("series = %v, want empty", tm.Series)
	}
}

func TestEngineMetricsPromQLSubstitution(t *testing.T) {
	q := &stubMetricsQueryer{points: []domain.MetricPoint{{T: 1, V: 2}}}
	svc := NewEngineMetricsService(q, "my-ns", 15*time.Second, testMetricsLogger())

	meta := &domain.TraceMeta{
		TraceID:    "abc",
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
	if len(q.queries) != len(defaultMetricQueries) {
		t.Fatalf("queries = %d, want %d", len(q.queries), len(defaultMetricQueries))
	}
	for _, query := range q.queries {
		if strings.Contains(query, "{ns}") || strings.Contains(query, "{pod}") {
			t.Fatalf("query %q still contains template vars", query)
		}
		if !strings.Contains(query, `namespace="my-ns"`) {
			t.Fatalf("query %q missing namespace substitution", query)
		}
		if !strings.Contains(query, `pod=~"dagger-engine-v0-21-4-.*"`) {
			t.Fatalf("query %q missing pod substitution", query)
		}
	}
	if tm.TraceID != "abc" {
		t.Fatalf("trace_id = %q, want abc", tm.TraceID)
	}
	if tm.StartTime.Unix() != 1000 {
		t.Fatalf("start_time = %v, want unix 1000", tm.StartTime)
	}
	if tm.EndTime.Unix() != 1060 {
		t.Fatalf("end_time = %v, want unix 1060", tm.EndTime)
	}
}

func TestEngineMetricsStepClamp(t *testing.T) {
	svc := NewEngineMetricsService(&stubMetricsQueryer{}, "ns", 0, testMetricsLogger())
	if svc.step != time.Second {
		t.Fatalf("step = %v, want 1s", svc.step)
	}
}

func TestEngineMetricsAllQueriesFail(t *testing.T) {
	q := &stubMetricsQueryer{err: errors.New("boom")}
	svc := NewEngineMetricsService(q, "ns", 15*time.Second, testMetricsLogger())

	_, err := svc.TraceMetrics(context.Background(), &domain.TraceMeta{TraceID: "abc", Version: "v0.21.4"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom", err)
	}
}

func TestEngineMetricsPartialFailure(t *testing.T) {
	// A queryer that fails only the first query, then succeeds.
	q := &flakyMetricsQueryer{}
	svc := NewEngineMetricsService(q, "ns", 15*time.Second, testMetricsLogger())

	tm, err := svc.TraceMetrics(context.Background(), &domain.TraceMeta{TraceID: "abc", Version: "v0.21.4"})
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

func (f *flakyMetricsQueryer) QueryRange(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]domain.MetricPoint, error) {
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
