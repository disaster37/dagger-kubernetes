package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// Sentinel errors for routing.
var (
	ErrNoBackend      = errors.New("no cache backend available")
	ErrRouteNotFound  = errors.New("cache route not found")
	ErrInvalidOCIPath = errors.New("invalid OCI request path")

	// errRoutesUnavailable is returned when an operation needs the routing
	// table but the router was constructed without a repo (nil).
	errRoutesUnavailable = errors.New("routing table unavailable")
)

// probeTimeout bounds each per-backend HEAD probe during self-healing.
const probeTimeout = 10 * time.Second

// RegistryRouter picks a backend for each OCI request and persists the
// object→backend mapping. It is safe for concurrent use.
type RegistryRouter struct {
	backends []domain.RegistryBackend
	clients  map[string]*repository.RegistryStatsClient // backendID -> client
	routes   *repository.CacheRoutesRepo
	logger   *logrus.Logger

	mu      sync.RWMutex
	charges map[string]int64 // backendID -> stored bytes (last refresh)
	down    map[string]bool  // backendID -> unhealthy
}

// NewRegistryRouter returns a router over the given backends. Credentials are
// held per-backend and never leave the router except as Basic auth on proxied
// requests to that backend.
func NewRegistryRouter(
	backends []domain.RegistryBackend,
	routes *repository.CacheRoutesRepo,
	logger *logrus.Logger,
) *RegistryRouter {
	r := &RegistryRouter{
		backends: append([]domain.RegistryBackend(nil), backends...),
		clients:  make(map[string]*repository.RegistryStatsClient, len(backends)),
		routes:   routes,
		logger:   logger,
		charges:  make(map[string]int64),
		down:     make(map[string]bool),
	}
	for _, b := range backends {
		r.clients[b.ID] = repository.NewRegistryStatsClientWithAuth(b.InternalAddr, b.Username, b.Password)
	}
	return r
}

// Backends returns a copy of the configured backends.
func (r *RegistryRouter) Backends() []domain.RegistryBackend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]domain.RegistryBackend(nil), r.backends...)
}

// BackendByID returns the backend and ok.
func (r *RegistryRouter) BackendByID(id string) (domain.RegistryBackend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range r.backends {
		if b.ID == id {
			return b, true
		}
	}
	return domain.RegistryBackend{}, false
}

// ClientByID returns the stats client for a backend (for stats/purge/GC).
func (r *RegistryRouter) ClientByID(id string) (*repository.RegistryStatsClient, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.clients[id]
	return c, ok
}

// HealthyBackends returns backends not marked down, ordered least-charged first.
func (r *RegistryRouter) HealthyBackends() []domain.RegistryBackend {
	r.mu.RLock()
	backends := append([]domain.RegistryBackend(nil), r.backends...)
	charges := make(map[string]int64, len(r.charges))
	for k, v := range r.charges {
		charges[k] = v
	}
	down := make(map[string]bool, len(r.down))
	for k, v := range r.down {
		down[k] = v
	}
	r.mu.RUnlock()

	out := make([]domain.RegistryBackend, 0, len(backends))
	for _, b := range backends {
		if down[b.ID] {
			continue
		}
		out = append(out, b)
	}
	// Stable sort: ties keep the configured backends order (deterministic).
	sort.SliceStable(out, func(i, j int) bool {
		return charges[out[i].ID] < charges[out[j].ID]
	})
	return out
}

// MarkDown marks a backend unhealthy.
func (r *RegistryRouter) MarkDown(backendID string) {
	r.mu.Lock()
	r.down[backendID] = true
	r.mu.Unlock()
}

// MarkUp marks a backend healthy.
func (r *RegistryRouter) MarkUp(backendID string) {
	r.mu.Lock()
	r.down[backendID] = false
	r.mu.Unlock()
}

// SetCharges replaces the charge map (called by CacheStatsService.probe after
// walking all backends).
func (r *RegistryRouter) SetCharges(charges map[string]int64) {
	r.mu.Lock()
	r.charges = make(map[string]int64, len(charges))
	for k, v := range charges {
		r.charges[k] = v
	}
	r.mu.Unlock()
}

// RouteForPull resolves a manifest pull (GET/HEAD /v2/<repo>/manifests/<ref>).
func (r *RegistryRouter) RouteForPull(ctx context.Context, repo, ref string) (domain.RegistryBackend, error) {
	if b, ok, err := r.lookupManifest(ctx, repo, ref); ok || err != nil {
		return b, err
	}
	b, err := r.probeBackends(ctx, func(probeCtx context.Context, c *repository.RegistryStatsClient) (bool, error) {
		return c.ProbeManifest(probeCtx, repo, ref)
	})
	if err == nil {
		r.selfHealManifest(ctx, repo, ref, b.ID)
	}
	return b, err
}

// RouteForBlobPull resolves a blob pull (GET/HEAD /v2/<repo>/blobs/<digest>).
// The blob route table is keyed by digest only (content-addressed); repo is
// needed only to build the probe path.
func (r *RegistryRouter) RouteForBlobPull(ctx context.Context, repo, digest string) (domain.RegistryBackend, error) {
	if r.routes != nil {
		if backendID, ok, err := r.routes.LookupBlob(ctx, digest); err != nil {
			return domain.RegistryBackend{}, err
		} else if ok {
			if b, ok := r.BackendByID(backendID); ok {
				return b, nil
			}
		}
	}
	b, err := r.probeBackends(ctx, func(probeCtx context.Context, c *repository.RegistryStatsClient) (bool, error) {
		return c.ProbeBlob(probeCtx, repo, digest)
	})
	if err == nil {
		r.selfHealBlob(ctx, digest, b.ID)
	}
	return b, err
}

// lookupManifest returns the backend recorded for repo+ref. ok=false means no
// route (caller should probe); err is propagated from the routing table.
func (r *RegistryRouter) lookupManifest(ctx context.Context, repo, ref string) (domain.RegistryBackend, bool, error) {
	if r.routes == nil {
		return domain.RegistryBackend{}, false, nil
	}
	route, ok, err := r.routes.LookupManifest(ctx, repo, ref)
	if err != nil || !ok {
		return domain.RegistryBackend{}, ok, err
	}
	b, ok := r.BackendByID(route.BackendID)
	return b, ok, nil
}

// probeBackends iterates healthy backends least-charged first, running probe
// with a bounded timeout on each. Transport errors mark the backend down; the
// first hit is returned. It returns ErrRouteNotFound when no backend has the
// object (or all are down).
func (r *RegistryRouter) probeBackends(
	ctx context.Context,
	probe func(context.Context, *repository.RegistryStatsClient) (bool, error),
) (domain.RegistryBackend, error) {
	for _, b := range r.HealthyBackends() {
		client, ok := r.ClientByID(b.ID)
		if !ok {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		exists, err := probe(probeCtx, client)
		cancel()
		if err != nil {
			r.MarkDown(b.ID)
			continue
		}
		if exists {
			return b, nil
		}
	}
	return domain.RegistryBackend{}, ErrRouteNotFound
}

// RouteForPush picks the least-charged healthy backend for a new manifest PUT.
func (r *RegistryRouter) RouteForPush(repo string) (domain.RegistryBackend, error) {
	backends := r.HealthyBackends()
	if len(backends) == 0 {
		return domain.RegistryBackend{}, ErrNoBackend
	}
	return backends[0], nil
}

// RouteForUploadStart picks the least-charged healthy backend for a new blob
// upload (POST /v2/<repo>/blobs/uploads/).
func (r *RegistryRouter) RouteForUploadStart(repo string) (domain.RegistryBackend, error) {
	return r.RouteForPush(repo)
}

// RouteForUploadResume resolves an in-flight upload by uuid (PATCH/PUT).
func (r *RegistryRouter) RouteForUploadResume(ctx context.Context, uuid string) (domain.RegistryBackend, error) {
	if r.routes == nil {
		return domain.RegistryBackend{}, ErrRouteNotFound
	}
	s, ok, err := r.routes.LookupUpload(ctx, uuid)
	if err != nil {
		return domain.RegistryBackend{}, err
	}
	if !ok {
		return domain.RegistryBackend{}, ErrRouteNotFound
	}
	if b, ok := r.BackendByID(s.BackendID); ok {
		return b, nil
	}
	return domain.RegistryBackend{}, ErrRouteNotFound
}

// RecordUploadSession persists upload_uuid → backend (called from the proxy
// on a successful POST uploads).
func (r *RegistryRouter) RecordUploadSession(ctx context.Context, uuid, repo, backendID string) error {
	if r.routes == nil {
		return errRoutesUnavailable
	}
	return r.routes.RecordUpload(ctx, uuid, repo, backendID)
}

// CompleteUpload deletes the session and records the blob route (called from
// the proxy post-process on a successful PUT ?digest=).
func (r *RegistryRouter) CompleteUpload(ctx context.Context, uuid, digest, backendID string) error {
	if r.routes == nil {
		return errRoutesUnavailable
	}
	if err := r.routes.DeleteUpload(ctx, uuid); err != nil {
		return err
	}
	return r.routes.UpsertBlob(ctx, digest, backendID)
}

// RecordManifest upserts the repo+tag → backend route (called from the proxy
// post-process on a successful manifest PUT).
func (r *RegistryRouter) RecordManifest(ctx context.Context, repo, tag, digest, backendID string, storedBytes int64) error {
	if r.routes == nil {
		return errRoutesUnavailable
	}
	return r.routes.UpsertManifest(ctx, repo, tag, digest, backendID, storedBytes)
}

// RefreshCharges recomputes the charge map from the routing table (fallback
// when no catalog walk has run yet).
func (r *RegistryRouter) RefreshCharges(ctx context.Context) error {
	if r.routes == nil {
		return errRoutesUnavailable
	}
	charges, err := r.routes.AllCharges(ctx)
	if err != nil {
		return err
	}
	r.SetCharges(charges)
	return nil
}

func (r *RegistryRouter) selfHealManifest(ctx context.Context, repo, ref, backendID string) {
	if r.routes == nil {
		return
	}
	if err := r.routes.UpsertManifest(ctx, repo, ref, "", backendID, 0); err != nil {
		r.logger.WithError(err).Warn("self-heal manifest route upsert failed")
	}
}

func (r *RegistryRouter) selfHealBlob(ctx context.Context, digest, backendID string) {
	if r.routes == nil {
		return
	}
	if err := r.routes.UpsertBlob(ctx, digest, backendID); err != nil {
		r.logger.WithError(err).Warn("self-heal blob route upsert failed")
	}
}
