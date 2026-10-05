package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// stubRunningRepo overrides ListRunning so the recorder's own filtering can be
// exercised (fakeTraceMetaRepo.ListRunning already drops empty versions).
type stubRunningRepo struct {
	*fakeTraceMetaRepo
	running []*domain.TraceMeta
	err     error
}

func (s *stubRunningRepo) ListRunning(context.Context) ([]*domain.TraceMeta, error) {
	return s.running, s.err
}

type recordedWrite struct {
	metric string
	labels map[string]string
	points []domain.MetricPoint
}

// fakeRecorderStore is safe for concurrent use: the recorder's Start goroutine
// writes while the test reads.
type fakeRecorderStore struct {
	mu             sync.Mutex
	instantQueries []string
	instantVal     float64
	instantOK      bool
	instantErr     error
	writes         []recordedWrite
	writeErr       error
}

func (f *fakeRecorderStore) QueryInstant(_ context.Context, query string, _ time.Time) (value float64, ok bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instantQueries = append(f.instantQueries, query)
	return f.instantVal, f.instantOK, f.instantErr
}

func (f *fakeRecorderStore) WriteSamples(_ context.Context, metricName string, labels map[string]string, points []domain.MetricPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, recordedWrite{metric: metricName, labels: labels, points: points})
	return f.writeErr
}

func (f *fakeRecorderStore) snapshotWrites() []recordedWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedWrite(nil), f.writes...)
}

func (f *fakeRecorderStore) snapshotQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.instantQueries...)
}

func TestTraceMetricsRecorderNonLeaderNoop(t *testing.T) {
	store := &fakeRecorderStore{instantOK: true, instantVal: 1}
	repo := &stubRunningRepo{running: []*domain.TraceMeta{{
		TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4", Status: "running", StartedAt: time.Now(),
	}}}
	rec := NewTraceMetricsRecorder(store, repo, "ns", time.Minute, time.Minute, 24*time.Hour, func() bool { return false }, testMetricsLogger())

	rec.Run(context.Background())
	if len(store.snapshotQueries()) != 0 || len(store.snapshotWrites()) != 0 {
		t.Fatalf("non-leader ran: queries=%v writes=%v", store.snapshotQueries(), store.snapshotWrites())
	}
}

func TestTraceMetricsRecorderSamplesValidTraces(t *testing.T) {
	now := time.Now()
	valid := &domain.TraceMeta{TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4", Status: "running", StartedAt: now.Add(-time.Minute)}
	store := &fakeRecorderStore{instantOK: true, instantVal: 2.5}
	repo := &stubRunningRepo{running: []*domain.TraceMeta{
		valid,
		{TraceID: "not-hex", Version: "v0.21.4", Status: "running", StartedAt: now},
		{TraceID: "abcdef0123456789abcdef0123456789", Version: "", Status: "running", StartedAt: now},
		{TraceID: "abcdef0123456789abcdef0123456789", Version: `bad"version`, Status: "running", StartedAt: now},
		{TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4", Status: "running"},
		{TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4", Status: "running", StartedAt: now.Add(-48 * time.Hour)},
	}}
	rec := NewTraceMetricsRecorder(store, repo, "my-ns", time.Minute, time.Minute, 24*time.Hour, func() bool { return true }, testMetricsLogger())

	rec.Run(context.Background())

	writes := store.snapshotWrites()
	if len(writes) != len(defaultMetricQueries) {
		t.Fatalf("writes = %d, want %d", len(writes), len(defaultMetricQueries))
	}
	for i, w := range writes {
		if w.metric != recordedMetricName(defaultMetricQueries[i].name) {
			t.Fatalf("write[%d].metric = %q, want %q", i, w.metric, recordedMetricName(defaultMetricQueries[i].name))
		}
		if w.labels["trace_id"] != valid.TraceID || w.labels["version"] != valid.Version {
			t.Fatalf("write[%d].labels = %v", i, w.labels)
		}
		if len(w.points) != 1 || w.points[0].V != 2.5 {
			t.Fatalf("write[%d].points = %v, want one 2.5", i, w.points)
		}
	}
	for _, q := range store.snapshotQueries() {
		if strings.Contains(q, "{ns}") || strings.Contains(q, "{pod}") || strings.Contains(q, "{rate}") {
			t.Fatalf("query %q still contains template vars", q)
		}
		if !strings.Contains(q, `namespace="my-ns"`) || !strings.Contains(q, `pod=~"dagger-engine-v0-21-4-.*"`) {
			t.Fatalf("query %q missing substitutions", q)
		}
	}
}

func TestTraceMetricsRecorderSkipsNonFinite(t *testing.T) {
	store := &fakeRecorderStore{instantOK: false}
	repo := &stubRunningRepo{running: []*domain.TraceMeta{{
		TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4", Status: "running", StartedAt: time.Now(),
	}}}
	rec := NewTraceMetricsRecorder(store, repo, "ns", time.Minute, time.Minute, 24*time.Hour, func() bool { return true }, testMetricsLogger())

	rec.Run(context.Background())
	if writes := store.snapshotWrites(); len(writes) != 0 {
		t.Fatalf("writes = %v, want none when no finite value", writes)
	}
}

func TestTraceMetricsRecorderErrorsAreNonFatal(t *testing.T) {
	store := &fakeRecorderStore{instantOK: true, instantVal: 1, instantErr: errors.New("query boom"), writeErr: errors.New("write boom")}
	repo := &stubRunningRepo{running: []*domain.TraceMeta{{
		TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4", Status: "running", StartedAt: time.Now(),
	}}}
	rec := NewTraceMetricsRecorder(store, repo, "ns", time.Minute, time.Minute, 24*time.Hour, func() bool { return true }, testMetricsLogger())

	// Must not panic; a failing query/write is logged and skipped.
	rec.Run(context.Background())
	if writes := store.snapshotWrites(); len(writes) != 0 {
		t.Fatalf("writes = %v, want none when queries fail", writes)
	}
}

func TestTraceMetricsRecorderListErrorNonFatal(t *testing.T) {
	store := &fakeRecorderStore{}
	repo := &stubRunningRepo{err: errors.New("list boom")}
	rec := NewTraceMetricsRecorder(store, repo, "ns", time.Minute, time.Minute, 24*time.Hour, func() bool { return true }, testMetricsLogger())

	rec.Run(context.Background())
	if queries := store.snapshotQueries(); len(queries) != 0 {
		t.Fatalf("queries = %v, want none on list error", queries)
	}
}

func TestTraceMetricsRecorderStartNonPositiveInterval(t *testing.T) {
	rec := NewTraceMetricsRecorder(&fakeRecorderStore{}, &stubRunningRepo{}, "ns", 0, time.Minute, 24*time.Hour, func() bool { return true }, testMetricsLogger())
	stop := rec.Start(context.Background())
	stop()
	stop() // idempotent
}

func TestTraceMetricsRecorderStartRunsAndStops(t *testing.T) {
	store := &fakeRecorderStore{instantOK: true, instantVal: 1}
	repo := &stubRunningRepo{running: []*domain.TraceMeta{{
		TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4", Status: "running", StartedAt: time.Now(),
	}}}
	rec := NewTraceMetricsRecorder(store, repo, "ns", 5*time.Millisecond, time.Minute, 24*time.Hour, func() bool { return true }, testMetricsLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := rec.Start(ctx)
	deadline := time.After(2 * time.Second)
	for len(store.snapshotWrites()) == 0 {
		select {
		case <-deadline:
			t.Fatal("recorder did not run")
		case <-time.After(5 * time.Millisecond):
		}
	}
	stop()
}
