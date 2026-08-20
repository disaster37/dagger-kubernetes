package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// lifecycleTraceMetaRepo reuses fakeTraceMetaRepo (history_purge_test.go) and
// adds PipelineLifecycle-specific test helpers.
type lifecycleTraceMetaRepo struct {
	*fakeTraceMetaRepo
}

func newLifecycleTraceMetaRepo() *lifecycleTraceMetaRepo {
	return &lifecycleTraceMetaRepo{fakeTraceMetaRepo: newFakeTraceMetaRepo()}
}

func (r *lifecycleTraceMetaRepo) put(traceID, status string, startedAt time.Time) {
	r.UpsertIngest(context.Background(), &domain.TraceMeta{TraceID: traceID, Status: status, StartedAt: startedAt, UpdatedAt: startedAt})
}

func (r *lifecycleTraceMetaRepo) snapshot(traceID string) (status, reason string) {
	m, err := r.Get(context.Background(), traceID)
	if err != nil {
		return "", ""
	}
	return m.Status, m.FailureReason
}

// captureBroadcaster records every Broadcast call.
type captureBroadcaster struct {
	mu     sync.Mutex
	events map[string][]any
}

func (b *captureBroadcaster) Broadcast(traceID string, event any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.events == nil {
		b.events = make(map[string][]any)
	}
	b.events[traceID] = append(b.events[traceID], event)
}

func (b *captureBroadcaster) sawEvent(traceID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events[traceID]) > 0
}

func newLifecycleForTest(t *testing.T, grace time.Duration, stale domain.PipelineStaleSweep) (*PipelineLifecycle, *lifecycleTraceMetaRepo, *captureBroadcaster, *prometheus.Registry) {
	t.Helper()
	repo := newLifecycleTraceMetaRepo()
	sessions := &stubSessionStore{}
	bc := &captureBroadcaster{}
	reg := prometheus.NewRegistry()
	metrics := observ.NewMetrics(reg)
	attribution := NewAttributionService(nil, nil, repo, testLogger())
	pl := NewPipelineLifecycle(attribution, repo, sessions, bc, domain.PipelineConfig{
		DisconnectGrace: grace,
		StaleSweep:      stale,
	}, testLogger(), metrics)
	return pl, repo, bc, reg
}

// counterVecValue returns the value of the labelled counter family with the
// given name, for the first metric carrying the given label value.
func counterVecValue(t *testing.T, reg *prometheus.Registry, name, labelValue string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetValue() == labelValue {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func TestOnTunnelClosed(t *testing.T) {
	t.Run("grace zero marks immediately", func(t *testing.T) {
		pl, repo, bc, reg := newLifecycleForTest(t, 0, domain.PipelineStaleSweep{})
		repo.put("t1", "running", time.Now().UTC())

		pl.OnTunnelClosed("t1", 0)

		status, reason := repo.snapshot("t1")
		if status != "failed" || reason != reasonClientConnectionLost {
			t.Fatalf("status=%q reason=%q, want failed/%q", status, reason, reasonClientConnectionLost)
		}
		if !bc.sawEvent("t1") {
			t.Fatal("expected broadcast on transition")
		}
		if got := counterVecValue(t, reg, "dagger_kubernetes_pipeline_disconnect_failed_total", "tunnel_close"); got != 1 {
			t.Fatalf("tunnel_close metric = %v, want 1", got)
		}
	})

	t.Run("remaining in flight no-op", func(t *testing.T) {
		pl, repo, bc, _ := newLifecycleForTest(t, 0, domain.PipelineStaleSweep{})
		repo.put("t1", "running", time.Now().UTC())

		pl.OnTunnelClosed("t1", 1)

		if status, _ := repo.snapshot("t1"); status != "running" {
			t.Fatalf("status = %q, want running (another tunnel active)", status)
		}
		if bc.sawEvent("t1") {
			t.Fatal("no broadcast expected")
		}
	})

	t.Run("empty trace id no-op", func(t *testing.T) {
		pl, repo, bc, _ := newLifecycleForTest(t, 0, domain.PipelineStaleSweep{})
		pl.OnTunnelClosed("", 0)
		if bc.sawEvent("") {
			t.Fatal("no broadcast expected for empty trace id")
		}
		if len(repo.traces) != 0 {
			t.Fatal("no trace should be created")
		}
	})

	t.Run("grace fires after delay", func(t *testing.T) {
		pl, repo, _, _ := newLifecycleForTest(t, 20*time.Millisecond, domain.PipelineStaleSweep{})
		repo.put("t1", "running", time.Now().UTC())

		pl.OnTunnelClosed("t1", 0)
		if status, _ := repo.snapshot("t1"); status != "running" {
			t.Fatalf("status = %q immediately after close, want running (grace pending)", status)
		}

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if status, _ := repo.snapshot("t1"); status == "failed" {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("timer never fired markFailed")
	})

	t.Run("grace cancelled by reopen", func(t *testing.T) {
		pl, repo, _, _ := newLifecycleForTest(t, 50*time.Millisecond, domain.PipelineStaleSweep{})
		repo.put("t1", "running", time.Now().UTC())

		pl.OnTunnelClosed("t1", 0)
		pl.OnTunnelOpen("t1")

		time.Sleep(120 * time.Millisecond)
		if status, _ := repo.snapshot("t1"); status != "running" {
			t.Fatalf("status = %q, want running (grace timer cancelled)", status)
		}
	})
}

func TestGraceTimerLateFireAfterReopenDoesNotFail(t *testing.T) {
	// Use a very long grace so the real timer never fires during the test;
	// we drive fireGrace directly to simulate the "Stop returned false" race,
	// where the callback goroutine was already dispatched when OnTunnelOpen
	// cancelled the pending entry.
	pl, repo, _, _ := newLifecycleForTest(t, time.Hour, domain.PipelineStaleSweep{})
	repo.put("t1", "running", time.Now().UTC())

	pl.OnTunnelClosed("t1", 0)

	pl.mu.Lock()
	pc := pl.pending["t1"]
	pl.mu.Unlock()
	if pc == nil {
		t.Fatal("expected a pending grace timer after OnTunnelClosed")
	}

	// A reconnect cancels the pending close (deletes pending[t1]).
	pl.OnTunnelOpen("t1")

	// The timer callback fires late (the race where Stop already returned
	// false). It must NOT mark the reopened trace failed.
	pl.fireGrace("t1", pc.gen)

	if status, _ := repo.snapshot("t1"); status != "running" {
		t.Fatalf("status = %q, want running (reopen cancelled the pending close)", status)
	}
}

func TestGraceTimerReplacedDoesNotFail(t *testing.T) {
	// A timer that was replaced by a newer timer must not mark the trace
	// failed (the identity re-check gates the transition).
	pl, repo, _, _ := newLifecycleForTest(t, time.Hour, domain.PipelineStaleSweep{})
	repo.put("t1", "running", time.Now().UTC())

	pl.OnTunnelClosed("t1", 0)
	pl.mu.Lock()
	first := pl.pending["t1"].gen
	pl.mu.Unlock()

	// Simulate the same trace closing again without an intervening open
	// (defensive replacement path in OnTunnelClosed).
	pl.OnTunnelClosed("t1", 0)

	// The first timer fires late: it must not fail the trace because it is no
	// longer the pending generation.
	pl.fireGrace("t1", first)

	if status, _ := repo.snapshot("t1"); status != "running" {
		t.Fatalf("status = %q, want running (stale timer must not fail)", status)
	}
}

func TestRunStaleSweep(t *testing.T) {
	now := time.Now().UTC()
	staleCfg := domain.PipelineStaleSweep{Enabled: true, Schedule: time.Minute, StaleAfter: time.Hour}

	pl, repo, bc, reg := newLifecycleForTest(t, 0, staleCfg)
	pl.sessions = newStaleSweepSessions()

	repo.put("stale-running", "running", now.Add(-2*time.Hour))
	repo.put("active-running", "running", now.Add(-2*time.Hour))
	repo.put("success-old", "success", now.Add(-2*time.Hour))
	repo.put("young-running", "running", now.Add(-10*time.Minute))

	summary := pl.RunStaleSweep(context.Background())

	if summary.Scanned != 3 {
		t.Fatalf("scanned = %d, want 3 (only stale_after-aged traces)", summary.Scanned)
	}
	if summary.Marked != 1 {
		t.Fatalf("marked = %d, want 1", summary.Marked)
	}
	if summary.SkippedActive != 1 {
		t.Fatalf("skipped_active = %d, want 1", summary.SkippedActive)
	}

	if status, reason := repo.snapshot("stale-running"); status != "failed" || reason != reasonClientSessionExpired {
		t.Fatalf("stale-running = %q/%q, want failed/%q", status, reason, reasonClientSessionExpired)
	}
	if status, _ := repo.snapshot("active-running"); status != "running" {
		t.Fatalf("active-running = %q, want running (active lease)", status)
	}
	if status, _ := repo.snapshot("success-old"); status != "success" {
		t.Fatalf("success-old = %q, want success (terminal)", status)
	}
	if status, _ := repo.snapshot("young-running"); status != "running" {
		t.Fatalf("young-running = %q, want running (younger than stale_after)", status)
	}

	if !bc.sawEvent("stale-running") {
		t.Fatal("expected broadcast for the transitioned trace")
	}
	if bc.sawEvent("active-running") || bc.sawEvent("success-old") {
		t.Fatal("broadcast must only fire on transition")
	}
	if got := counterVecValue(t, reg, "dagger_kubernetes_pipeline_disconnect_failed_total", "stale_sweep"); got != 1 {
		t.Fatalf("stale_sweep metric = %v, want 1", got)
	}
}

func TestStartStaleSweepDisabled(t *testing.T) {
	pl, _, _, _ := newLifecycleForTest(t, 0, domain.PipelineStaleSweep{Enabled: false, Schedule: time.Minute, StaleAfter: time.Hour})
	stop := pl.StartStaleSweep(context.Background())
	stop() // must not panic
}

func TestStartStaleSweepNonPositiveSchedule(t *testing.T) {
	pl, _, _, _ := newLifecycleForTest(t, 0, domain.PipelineStaleSweep{Enabled: true, Schedule: 0, StaleAfter: time.Hour})
	stop := pl.StartStaleSweep(context.Background())
	stop() // must not panic
}

func TestStartStaleSweepStopIdempotent(t *testing.T) {
	// The returned stop func must be safe to call multiple times
	// (CWE-248: double close of the done channel would panic).
	pl, _, _, _ := newLifecycleForTest(t, 0, domain.PipelineStaleSweep{Enabled: true, Schedule: time.Minute, StaleAfter: time.Hour})
	stop := pl.StartStaleSweep(context.Background())
	stop()
	stop()
	stop()
}

func TestStopCancelsPendingGraceTimers(t *testing.T) {
	// Stop must cancel all pending grace timers so late callbacks do not
	// issue Raft applies after the store has closed (CWE-362).
	pl, repo, _, _ := newLifecycleForTest(t, time.Hour, domain.PipelineStaleSweep{})
	repo.put("t1", "running", time.Now().UTC())
	repo.put("t2", "running", time.Now().UTC())

	pl.OnTunnelClosed("t1", 0)
	pl.OnTunnelClosed("t2", 0)

	pl.mu.Lock()
	if len(pl.pending) != 2 {
		pl.mu.Unlock()
		t.Fatalf("pending = %d, want 2 before Stop", len(pl.pending))
	}
	pl.mu.Unlock()

	pl.Stop()

	pl.mu.Lock()
	remaining := len(pl.pending)
	pl.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("pending = %d after Stop, want 0", remaining)
	}

	// Stop is idempotent.
	pl.Stop()

	// Give any racing callbacks a moment; the trace must stay running
	// because the timers were cancelled before they fired.
	time.Sleep(50 * time.Millisecond)
	for _, id := range []string{"t1", "t2"} {
		if status, _ := repo.snapshot(id); status != "running" {
			t.Fatalf("trace %s = %q, want running (timers cancelled by Stop)", id, status)
		}
	}
}

// newStaleSweepSessions returns a SessionStore with one active lease
// (InFlight == 1) for the "active-running" trace.
func newStaleSweepSessions() *stubSessionStore {
	s := &stubSessionStore{}
	l := s.Register("fp-active", "v0.21.4", "pod-0", "inst-1", "active-running", "")
	l.InFlight = 1
	return s
}
