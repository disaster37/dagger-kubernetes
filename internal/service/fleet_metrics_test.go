package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

var errQuery = errors.New("query failed")

// recordingQueryer records every PromQL string it receives and returns
// scripted results per query kind (metric / storage usage / storage limit).
// failFirstMetrics makes the first N metric queries fail, exercising partial
// failure without failing the storage queries.
type recordingQueryer struct {
	queries          []string
	metric           []domain.MetricPoint
	metricErr        error
	failFirstMetrics int
	metricCalls      int
	usage            []domain.MetricPoint
	usageErr         error
	limit            []domain.MetricPoint
	limitErr         error
}

func (q *recordingQueryer) QueryRange(_ context.Context, query string, _, _ time.Time, _ time.Duration) ([]domain.MetricPoint, error) {
	q.queries = append(q.queries, query)
	switch {
	case strings.Contains(query, "container_fs_usage_bytes"):
		return q.usage, q.usageErr
	case strings.Contains(query, "container_fs_limit_bytes"):
		return q.limit, q.limitErr
	default:
		q.metricCalls++
		if q.metricCalls <= q.failFirstMetrics {
			return nil, q.metricErr
		}
		if q.metricErr != nil && q.failFirstMetrics == 0 {
			return nil, q.metricErr
		}
		return q.metric, nil
	}
}

func TestFleetMetricsUnsafeVersion(t *testing.T) {
	q := &recordingQueryer{}
	svc := NewFleetMetricsService(q, "dagger-kubernetes", 15*time.Second, 0, testLogger())

	fm, err := svc.FleetMetrics(context.Background(), `bad"} OR up{`, 1)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if fm.Version != `bad"} OR up{` {
		t.Fatalf("version = %q", fm.Version)
	}
	if len(fm.Series) != 0 {
		t.Fatalf("series = %d, want 0", len(fm.Series))
	}
	if len(q.queries) != 0 {
		t.Fatalf("queries = %v, want none", q.queries)
	}
}

func TestFleetMetricsNilQueryer(t *testing.T) {
	svc := NewFleetMetricsService(nil, "dagger-kubernetes", 15*time.Second, 0, testLogger())
	fm, err := svc.FleetMetrics(context.Background(), "v0.21.4", 1)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(fm.Series) != 0 {
		t.Fatalf("series = %d, want 0", len(fm.Series))
	}
}

func TestFleetMetricsStepClamp(t *testing.T) {
	svc := NewFleetMetricsService(&recordingQueryer{}, "ns", 0, 0, testLogger())
	if svc.step != time.Second {
		t.Fatalf("step = %v, want 1s", svc.step)
	}
}

func TestFleetMetricsQueryTemplate(t *testing.T) {
	q := &recordingQueryer{metric: []domain.MetricPoint{{T: 1, V: 2}}}
	svc := NewFleetMetricsService(q, "my-ns", 15*time.Second, 0, testLogger())

	fm, err := svc.FleetMetrics(context.Background(), "v0.21.4", 1)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(fm.Series) != 6 {
		t.Fatalf("series = %d, want 6", len(fm.Series))
	}
	sts := "dagger-engine-v0-21-4"
	for _, query := range q.queries {
		if strings.Contains(query, "{ns}") || strings.Contains(query, "{pod}") {
			t.Fatalf("unsubstituted template var in %q", query)
		}
		if !strings.Contains(query, `namespace="my-ns"`) {
			t.Fatalf("namespace missing in %q", query)
		}
		if !strings.Contains(query, fmt.Sprintf(`pod=~"%s-.*"`, sts)) {
			t.Fatalf("pod selector missing in %q (want %s)", query, sts)
		}
	}
}

func TestFleetMetricsStorage(t *testing.T) {
	cases := []struct {
		name         string
		usage        []domain.MetricPoint
		usageErr     error
		limit        []domain.MetricPoint
		limitErr     error
		storageBytes int64
		replicas     int
		wantNil      bool
		wantUsed     int64
		wantCapacity int64
		wantPct      float64
	}{
		{
			name:         "used and capacity present",
			usage:        []domain.MetricPoint{{T: 1, V: 25}},
			limit:        []domain.MetricPoint{{T: 1, V: 100}},
			replicas:     1,
			wantUsed:     25,
			wantCapacity: 100,
			wantPct:      25,
		},
		{
			name:         "percent clamped to 100",
			usage:        []domain.MetricPoint{{T: 1, V: 200}},
			limit:        []domain.MetricPoint{{T: 1, V: 100}},
			replicas:     1,
			wantUsed:     200,
			wantCapacity: 100,
			wantPct:      100,
		},
		{
			name:         "capacity missing uses fallback",
			usage:        []domain.MetricPoint{{T: 1, V: 50}},
			storageBytes: 100,
			replicas:     3,
			wantUsed:     50,
			wantCapacity: 300,
			wantPct:      50.0 / 300 * 100,
		},
		{
			name:     "capacity 0 and no fallback yields percent -1",
			usage:    []domain.MetricPoint{{T: 1, V: 50}},
			replicas: 1,
			wantUsed: 50, wantCapacity: 0, wantPct: -1,
		},
		{
			name:     "no data yields nil storage",
			wantNil:  true,
			replicas: 1,
		},
		{
			name:  "both storage queries failed yields nil storage",
			usage: nil, usageErr: errQuery,
			limit: nil, limitErr: errQuery,
			replicas: 1,
			wantNil:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &recordingQueryer{
				metric: []domain.MetricPoint{{T: 1, V: 2}},
				usage:  tc.usage, usageErr: tc.usageErr,
				limit: tc.limit, limitErr: tc.limitErr,
			}
			svc := NewFleetMetricsService(q, "ns", 15*time.Second, tc.storageBytes, testLogger())
			fm, err := svc.FleetMetrics(context.Background(), "v0.21.4", tc.replicas)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.wantNil {
				if fm.Storage != nil {
					t.Fatalf("storage = %+v, want nil", fm.Storage)
				}
				return
			}
			if fm.Storage == nil {
				t.Fatalf("storage = nil, want %+v", tc)
			}
			if fm.Storage.UsedBytes != tc.wantUsed {
				t.Fatalf("used = %d, want %d", fm.Storage.UsedBytes, tc.wantUsed)
			}
			if fm.Storage.CapacityBytes != tc.wantCapacity {
				t.Fatalf("capacity = %d, want %d", fm.Storage.CapacityBytes, tc.wantCapacity)
			}
			if diff := fm.Storage.Percent - tc.wantPct; diff > 0.01 || diff < -0.01 {
				t.Fatalf("percent = %f, want %f", fm.Storage.Percent, tc.wantPct)
			}
		})
	}
}

func TestFleetMetricsPartialFailure(t *testing.T) {
	q := &recordingQueryer{
		metric:           []domain.MetricPoint{{T: 1, V: 2}},
		metricErr:        errQuery,
		failFirstMetrics: 2,
	}
	q.usage = []domain.MetricPoint{{T: 1, V: 10}}
	q.limit = []domain.MetricPoint{{T: 1, V: 100}}
	svc := NewFleetMetricsService(q, "ns", 15*time.Second, 0, testLogger())

	// 2 of 6 metric queries fail: the remaining 4 series are returned and no
	// error is surfaced (per-query failures are logged + skipped).
	fm, err := svc.FleetMetrics(context.Background(), "v0.21.4", 1)
	if err != nil {
		t.Fatalf("err = %v, want nil (partial failure is non-fatal)", err)
	}
	if len(fm.Series) != 4 {
		t.Fatalf("series = %d, want 4", len(fm.Series))
	}
	if fm.Storage == nil || fm.Storage.UsedBytes != 10 || fm.Storage.CapacityBytes != 100 {
		t.Fatalf("storage = %+v, want used=10 capacity=100", fm.Storage)
	}
}

func TestFleetMetricsAllQueriesFail(t *testing.T) {
	q := &recordingQueryer{metric: nil, metricErr: errQuery}
	q.usageErr, q.limitErr = errQuery, errQuery
	svc := NewFleetMetricsService(q, "ns", 15*time.Second, 0, testLogger())

	// Every query failed: the last error is surfaced so the handler can log
	// it and answer with an empty-but-valid payload.
	if _, err := svc.FleetMetrics(context.Background(), "v0.21.4", 1); err == nil {
		t.Fatalf("err = nil, want the query error")
	}
}

func TestFleetMetricsStorageOnlyFailureKeepsSeries(t *testing.T) {
	q := &recordingQueryer{metric: []domain.MetricPoint{{T: 1, V: 2}}}
	q.usageErr, q.limitErr = errQuery, errQuery
	svc := NewFleetMetricsService(q, "ns", 15*time.Second, 0, testLogger())

	fm, err := svc.FleetMetrics(context.Background(), "v0.21.4", 1)
	if err != nil {
		t.Fatalf("err = %v, want nil (storage failure is non-fatal)", err)
	}
	if len(fm.Series) != 6 {
		t.Fatalf("series = %d, want 6", len(fm.Series))
	}
	if fm.Storage != nil {
		t.Fatalf("storage = %+v, want nil", fm.Storage)
	}
}
