package service

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// TraceEventBroadcaster fans out a live SSE event (implemented by
// repository.LiveHub in the handler wiring). Defined in service to avoid
// importing repository.
type TraceEventBroadcaster interface {
	Broadcast(traceID string, event any)
}

// Disconnect reason strings (bounded to 256 bytes by the repository/service).
const (
	reasonClientConnectionLost = "client connection lost"
	reasonClientSessionExpired = "client session expired"
)

// pendingClose is a per-trace grace timer entry. gen is a monotonically
// increasing token captured by the timer callback so a stale (cancelled or
// replaced) timer can be identified without capturing a mutable *time.Timer in
// the closure (which would race under -race).
type pendingClose struct {
	timer *time.Timer
	gen   uint64
}

// PipelineLifecycle coordinates pipeline disconnect detection (ADR-019): it
// marks a trace failed when the owning L4 data-plane tunnel closes, and runs a
// background staleness sweeper to recover orphaned running traces after a
// supervisor restart/crash (in-memory leases are lost on restart).
type PipelineLifecycle struct {
	attribution *AttributionService
	traceMeta   domain.TraceMetaRepository
	sessions    domain.SessionStore
	broadcaster TraceEventBroadcaster // may be nil
	grace       time.Duration
	staleCfg    domain.PipelineStaleSweep
	logger      *logrus.Logger
	metrics     *observ.Metrics // may be nil

	mu      sync.Mutex
	pending map[string]*pendingClose // traceID -> grace timer (grace > 0 only)
	gen     uint64
}

// NewPipelineLifecycle returns a PipelineLifecycle.
func NewPipelineLifecycle(attribution *AttributionService, traceMeta domain.TraceMetaRepository,
	sessions domain.SessionStore, broadcaster TraceEventBroadcaster,
	cfg domain.PipelineConfig, logger *logrus.Logger, metrics *observ.Metrics) *PipelineLifecycle {
	return &PipelineLifecycle{
		attribution: attribution,
		traceMeta:   traceMeta,
		sessions:    sessions,
		broadcaster: broadcaster,
		grace:       cfg.DisconnectGrace,
		staleCfg:    cfg.StaleSweep,
		logger:      logger,
		metrics:     metrics,
		pending:     make(map[string]*pendingClose),
	}
}

// OnTunnelOpen cancels any pending grace timer for traceID (no-op when
// grace == 0). Called by handleDataConn after IncInFlight.
func (p *PipelineLifecycle) OnTunnelOpen(traceID string) {
	if p.grace <= 0 {
		return
	}
	p.mu.Lock()
	if pc, ok := p.pending[traceID]; ok {
		pc.timer.Stop()
		delete(p.pending, traceID)
	}
	p.mu.Unlock()
}

// OnTunnelClosed is called when a data-plane tunnel exits. It no-ops when the
// trace ID is empty or another tunnel is still in flight for the lease. With
// grace == 0 the trace is failed synchronously; with grace > 0 a per-trace
// timer is started (a new OnTunnelOpen for the same trace cancels it).
func (p *PipelineLifecycle) OnTunnelClosed(traceID string, remainingInFlight int) {
	if traceID == "" || remainingInFlight > 0 {
		return
	}
	if p.grace <= 0 {
		p.markFailed(context.Background(), "tunnel_close", traceID, reasonClientConnectionLost)
		return
	}

	p.mu.Lock()
	if old, ok := p.pending[traceID]; ok {
		old.timer.Stop()
	}
	p.gen++
	gen := p.gen
	pc := &pendingClose{gen: gen}
	pc.timer = time.AfterFunc(p.grace, func() {
		p.fireGrace(traceID, gen)
	})
	p.pending[traceID] = pc
	p.mu.Unlock()
}

// fireGrace runs when a grace timer fires (or is invoked directly by tests to
// simulate a late firing). It fails the trace only if gen is still the pending
// generation for traceID. The re-check happens under p.mu, so an OnTunnelOpen
// that cancelled (deleted) the pending entry — even after Stop returned false
// and the callback was already dispatched — prevents the markFailed: an
// actively-reopened trace must never be failed by a stale close timer.
func (p *PipelineLifecycle) fireGrace(traceID string, gen uint64) {
	p.mu.Lock()
	pc, ok := p.pending[traceID]
	if !ok || pc.gen != gen {
		p.mu.Unlock()
		return
	}
	delete(p.pending, traceID)
	p.mu.Unlock()
	p.markFailed(context.Background(), "tunnel_close", traceID, reasonClientConnectionLost)
}

// markFailed best-effort transitions a non-terminal trace to "failed" and, on
// a real transition, broadcasts a live SSE re-fetch event and bumps the
// disconnect-failure metric. It returns whether the transition happened.
func (p *PipelineLifecycle) markFailed(ctx context.Context, source, traceID, reason string) bool {
	if !p.attribution.MarkFailed(ctx, traceID, reason) {
		return false
	}
	if p.broadcaster != nil {
		p.broadcaster.Broadcast(traceID, map[string]string{"type": "trace_update"})
	}
	if p.metrics != nil {
		p.metrics.PipelineDisconnectFailedTotal.WithLabelValues(source).Inc()
	}
	return true
}

// StaleSweepSummary is the result of a single RunStaleSweep pass.
type StaleSweepSummary struct {
	Scanned       int
	Marked        int
	SkippedActive int
}

// RunStaleSweep marks running traces older than stale_after with no active
// lease (InFlight == 0) as failed. It is the supervisor-restart / crash
// fallback: leases are in-memory and lost on restart, so the disconnect
// handler cannot run for them.
func (p *PipelineLifecycle) RunStaleSweep(ctx context.Context) StaleSweepSummary {
	var summary StaleSweepSummary
	if p.staleCfg.StaleAfter <= 0 {
		return summary
	}

	cutoff := time.Now().Add(-p.staleCfg.StaleAfter)
	candidates, err := p.traceMeta.ListBefore(ctx, cutoff, false)
	if err != nil {
		p.logger.WithError(err).Warn("stale sweep: list candidates failed")
		return summary
	}

	active := make(map[string]bool)
	for _, l := range p.sessions.List() {
		if l.InFlight > 0 {
			active[l.TraceID] = true
		}
	}

	for _, m := range candidates {
		if ctx.Err() != nil {
			break
		}
		summary.Scanned++
		if m.Status != "" && m.Status != "running" {
			continue
		}
		if active[m.TraceID] {
			summary.SkippedActive++
			continue
		}
		if p.markFailed(ctx, "stale_sweep", m.TraceID, reasonClientSessionExpired) {
			summary.Marked++
		}
	}
	return summary
}

// StartStaleSweep launches the background ticker goroutine and returns a stop
// func. No-op when staleCfg.Enabled is false or the schedule is non-positive.
// The returned stop func is safe to call multiple times (CWE-248).
func (p *PipelineLifecycle) StartStaleSweep(ctx context.Context) (stop func()) {
	if !p.staleCfg.Enabled {
		return func() {}
	}
	if p.staleCfg.Schedule <= 0 {
		p.logger.Warn("pipeline stale sweep schedule is non-positive; sweeper disabled")
		return func() {}
	}
	ticker := time.NewTicker(p.staleCfg.Schedule)
	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				summary := p.RunStaleSweep(ctx)
				p.logger.WithFields(logrus.Fields{
					"scanned":        summary.Scanned,
					"marked":         summary.Marked,
					"skipped_active": summary.SkippedActive,
				}).Debug("pipeline stale sweep completed")
			}
		}
	}()
	return func() {
		stopOnce.Do(func() { close(done) })
	}
}

// Stop cancels all pending grace timers. It is safe to call multiple times.
// Call on shutdown to prevent late grace-timer callbacks from issuing Raft
// applies after the store has closed (CWE-362 / goroutine leak on shutdown).
// With the default disconnect_grace=0 there are no pending timers, so this is
// a no-op.
func (p *PipelineLifecycle) Stop() {
	p.mu.Lock()
	for traceID, pc := range p.pending {
		pc.timer.Stop()
		delete(p.pending, traceID)
	}
	p.mu.Unlock()
}
