package service

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// orgAdmissionStripes bounds the per-org admission mutex stripe count.
const orgAdmissionStripes = 64

// QuotaService enforces org-level soft/hard limits for every principal
// (including RoleAdmin) plus the legacy per-group max_runner_sessions.
type QuotaService struct {
	orgs        domain.OrgRepository
	sessions    domain.SessionStore
	groups      domain.GroupRepository
	projects    domain.ProjectRepository
	logger      *logrus.Logger
	metrics     *observ.Metrics
	defaults    domain.OrgQuotas
	retention   time.Duration
	engineUsage func(ctx context.Context, orgID string) (int64, error)
	cacheUsage  func(orgID string) (int64, error)

	stripes [orgAdmissionStripes]sync.Mutex
}

// NewQuotaService returns a QuotaService.
func NewQuotaService(sessions domain.SessionStore, groups domain.GroupRepository, logger *logrus.Logger) *QuotaService {
	return &QuotaService{sessions: sessions, groups: groups, logger: logger}
}

// WithMetrics wires org quota Prometheus metrics.
func (q *QuotaService) WithMetrics(m *observ.Metrics) *QuotaService {
	q.metrics = m
	return q
}

// WithOrgRepository wires org/usage lookups.
func (q *QuotaService) WithOrgRepository(orgs domain.OrgRepository, projects domain.ProjectRepository) *QuotaService {
	q.orgs = orgs
	q.projects = projects
	return q
}

// WithDefaults wires the cluster-wide quota defaults and retention fallback.
//
//nolint:gocritic // quota structs are value-semantic
func (q *QuotaService) WithDefaults(defaults domain.OrgQuotas, retention time.Duration) *QuotaService {
	q.defaults = defaults
	q.retention = retention
	return q
}

// WithEngineUsage wires the fleet engine-count provider.
func (q *QuotaService) WithEngineUsage(fn func(ctx context.Context, orgID string) (int64, error)) *QuotaService {
	q.engineUsage = fn
	return q
}

// WithCacheUsage wires the per-org remote-cache byte counter.
func (q *QuotaService) WithCacheUsage(fn func(orgID string) (int64, error)) *QuotaService {
	q.cacheUsage = fn
	return q
}

// LockOrg serializes admission mutations for orgID (striped mutex).
func (q *QuotaService) LockOrg(orgID string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(orgID))
	idx := h.Sum32() % orgAdmissionStripes
	q.stripes[idx].Lock()
	return q.stripes[idx].Unlock
}

// EffectiveQuotas returns the org's quotas merged onto cluster defaults.
func (q *QuotaService) EffectiveQuotas(ctx context.Context, orgID string) (domain.OrgQuotas, error) {
	if q.orgs == nil || orgID == "" {
		return q.defaults, nil
	}
	o, err := q.orgs.Get(ctx, orgID)
	if err != nil {
		return domain.OrgQuotas{}, err
	}
	return o.Quotas.Effective(q.defaults), nil
}

// EffectiveCacheMaxBytes returns the org's hard cache bound, falling back to
// the configured remote-cache maximum when the org has no quota.
func (q *QuotaService) EffectiveCacheMaxBytes(ctx context.Context, orgID string, fallback int64) int64 {
	eff, err := q.EffectiveQuotas(ctx, orgID)
	if err != nil {
		return fallback
	}
	if eff.MaxCacheBytes.Hard > 0 {
		return eff.MaxCacheBytes.Hard
	}
	return fallback
}

// EffectiveRetention returns the org's trace retention, falling back to
// history.gc.max_age.
func (q *QuotaService) EffectiveRetention(ctx context.Context, orgID string) time.Duration {
	eff, err := q.EffectiveQuotas(ctx, orgID)
	if err != nil {
		return q.retention
	}
	if eff.MaxPipelineRetention > 0 {
		return eff.MaxPipelineRetention
	}
	return q.retention
}

// CheckResource enforces one quota dimension. It returns a soft-limit warning
// (admission still allowed) or a wrapped ErrOrgQuotaExceeded on a hard cap.
func (q *QuotaService) CheckResource(ctx context.Context, id *domain.Identity, orgID string, res domain.QuotaResource, delta int64) (string, error) {
	eff, err := q.EffectiveQuotas(ctx, orgID)
	if err != nil {
		return "", err
	}
	var th domain.QuotaThreshold
	switch res {
	case domain.QuotaGroups:
		th = eff.MaxGroups
	case domain.QuotaProjects:
		th = eff.MaxProjects
	case domain.QuotaMembers:
		th = eff.MaxMembers
	case domain.QuotaConcurrentSessions:
		th = eff.MaxConcurrentSessions
	case domain.QuotaEngines:
		th = eff.MaxEngines
	case domain.QuotaCacheBytes:
		th = eff.MaxCacheBytes
	}
	usage, err := q.usage(ctx, orgID, res)
	if err != nil {
		return "", err
	}
	return q.evaluate(id, orgID, res, usage, delta, th)
}

// evaluate applies soft/hard semantics for usage+delta against th.
func (q *QuotaService) evaluate(id *domain.Identity, orgID string, res domain.QuotaResource, usage, delta int64, th domain.QuotaThreshold) (string, error) {
	projected := usage + delta
	if th.Hard > 0 && projected > th.Hard {
		q.logger.WithFields(logrus.Fields{
			"org_id":     orgID,
			"resource":   res,
			"usage":      usage,
			"hard_limit": th.Hard,
			"user_id":    identityUserID(id),
		}).Warn("organization hard quota rejected")
		if q.metrics != nil {
			q.metrics.OrgQuotaHardRejectedTotal.WithLabelValues(orgID, string(res)).Inc()
		}
		return "", fmt.Errorf("%w: %s limit (%d) reached", domain.ErrOrgQuotaExceeded, res, th.Hard)
	}
	if th.Soft > 0 && projected > th.Soft {
		warning := fmt.Sprintf("%s soft limit exceeded (%d/%d)", res, projected, th.Soft)
		q.logger.WithFields(logrus.Fields{
			"org_id":     orgID,
			"resource":   res,
			"usage":      usage,
			"soft_limit": th.Soft,
			"hard_limit": th.Hard,
			"user_id":    identityUserID(id),
		}).Warn("organization soft quota exceeded")
		if q.metrics != nil {
			q.metrics.OrgQuotaSoftExceededTotal.WithLabelValues(orgID, string(res)).Inc()
		}
		return warning, nil
	}
	return "", nil
}

func identityUserID(id *domain.Identity) string {
	if id == nil {
		return ""
	}
	return id.UserID
}

// usage counts live usage of one resource.
func (q *QuotaService) usage(ctx context.Context, orgID string, res domain.QuotaResource) (int64, error) {
	switch res {
	case domain.QuotaGroups:
		if q.groups == nil {
			return 0, nil
		}
		gs, err := q.groups.ListByOrg(ctx, orgID)
		return int64(len(gs)), err
	case domain.QuotaProjects:
		if q.projects == nil {
			return 0, nil
		}
		ps, err := q.projects.ListByOrg(ctx, orgID)
		return int64(len(ps)), err
	case domain.QuotaMembers:
		if q.orgs == nil {
			return 0, nil
		}
		n, err := q.orgs.DistinctMemberCount(ctx, orgID)
		return int64(n), err
	case domain.QuotaConcurrentSessions:
		if q.sessions == nil {
			return 0, nil
		}
		return int64(q.sessions.CountByOrg(orgID)), nil
	case domain.QuotaEngines:
		if q.engineUsage == nil {
			return 0, nil
		}
		return q.engineUsage(ctx, orgID)
	case domain.QuotaCacheBytes:
		if q.cacheUsage == nil {
			return 0, nil
		}
		return q.cacheUsage(orgID)
	}
	return 0, nil
}

// CheckGroupCreate enforces the groups hard cap.
func (q *QuotaService) CheckGroupCreate(ctx context.Context, id *domain.Identity, orgID string) (string, error) {
	return q.CheckResource(ctx, id, orgID, domain.QuotaGroups, 1)
}

// CheckProjectCreate enforces the projects hard cap.
func (q *QuotaService) CheckProjectCreate(ctx context.Context, id *domain.Identity, orgID string) (string, error) {
	return q.CheckResource(ctx, id, orgID, domain.QuotaProjects, 1)
}

// CheckMemberAdd enforces the members hard cap.
func (q *QuotaService) CheckMemberAdd(ctx context.Context, id *domain.Identity, orgID string, delta int64) (string, error) {
	return q.CheckResource(ctx, id, orgID, domain.QuotaMembers, delta)
}

// CheckEngineAccess decides whether an identity may provision a new engine in
// orgID. The org-level concurrent-session cap applies to every role; RoleAdmin
// retains only the per-group max_runner_sessions bypass.
func (q *QuotaService) CheckEngineAccess(ctx context.Context, orgID string, id *domain.Identity) (string, error) {
	if orgID == "" && id != nil {
		orgID = id.OrgID
	}
	warning, err := q.CheckResource(ctx, id, orgID, domain.QuotaConcurrentSessions, 1)
	if err != nil {
		return "", err
	}
	if id.IsAdmin() {
		return warning, nil
	}
	gs, err := q.groups.GroupsForUserInOrg(ctx, orgID, id.UserID)
	if err != nil {
		return warning, err
	}
	if len(gs) == 0 && id.OrgID != "" && strings.HasPrefix(id.UserID, "org:") {
		gs, err = q.groups.ListByOrg(ctx, orgID)
		if err != nil {
			return warning, err
		}
	}
	if len(gs) == 0 {
		if id.OrgID != "" && strings.HasPrefix(id.UserID, "org:") {
			return warning, nil
		}
		return warning, domain.ErrNoGroups
	}
	var available []*domain.Group
	for _, g := range gs {
		if g.AgentAvailable {
			available = append(available, g)
		}
	}
	if len(available) == 0 {
		return warning, domain.ErrAgentUnavailable
	}
	usage := q.usageSnapshot(ctx, orgID)
	for _, g := range available {
		if g.MaxRunnerSessions == 0 || usage[g.ID] < g.MaxRunnerSessions {
			return warning, nil
		}
	}
	return warning, domain.ErrQuotaExhausted
}

// CheckEngineScaleUp enforces the org engines hard cap before scaling up.
func (q *QuotaService) CheckEngineScaleUp(ctx context.Context, orgID string, id *domain.Identity, delta int64) (string, error) {
	return q.CheckResource(ctx, id, orgID, domain.QuotaEngines, delta)
}

// CheckCacheWrite enforces the org cache-bytes hard cap.
func (q *QuotaService) CheckCacheWrite(ctx context.Context, orgID string, delta int64) (string, error) {
	return q.CheckResource(ctx, &domain.Identity{}, orgID, domain.QuotaCacheBytes, delta)
}

// UsageSnapshot reports live usage against effective quotas for an org.
func (q *QuotaService) UsageSnapshot(ctx context.Context, orgID string) (*domain.OrgQuotaUsage, error) {
	eff, err := q.EffectiveQuotas(ctx, orgID)
	if err != nil {
		return nil, err
	}
	usage := &domain.OrgQuotaUsage{Effective: eff}
	for _, res := range []domain.QuotaResource{
		domain.QuotaGroups, domain.QuotaProjects, domain.QuotaMembers,
		domain.QuotaConcurrentSessions, domain.QuotaEngines, domain.QuotaCacheBytes,
	} {
		n, err := q.usage(ctx, orgID, res)
		if err != nil {
			return nil, err
		}
		switch res {
		case domain.QuotaGroups:
			usage.Groups = n
		case domain.QuotaProjects:
			usage.Projects = n
		case domain.QuotaMembers:
			usage.Members = n
		case domain.QuotaConcurrentSessions:
			usage.ConcurrentSessions = n
		case domain.QuotaEngines:
			usage.Engines = n
		case domain.QuotaCacheBytes:
			usage.CacheBytes = n
		}
	}
	if q.metrics != nil {
		for _, item := range []struct {
			name string
			n    int64
		}{
			{string(domain.QuotaGroups), usage.Groups},
			{string(domain.QuotaProjects), usage.Projects},
			{string(domain.QuotaMembers), usage.Members},
			{string(domain.QuotaConcurrentSessions), usage.ConcurrentSessions},
			{string(domain.QuotaEngines), usage.Engines},
			{string(domain.QuotaCacheBytes), usage.CacheBytes},
		} {
			q.metrics.OrgQuotaUsage.WithLabelValues(orgID, item.name).Set(float64(item.n))
		}
		for _, item := range []struct {
			name string
			th   domain.QuotaThreshold
		}{
			{string(domain.QuotaGroups), eff.MaxGroups},
			{string(domain.QuotaProjects), eff.MaxProjects},
			{string(domain.QuotaMembers), eff.MaxMembers},
			{string(domain.QuotaConcurrentSessions), eff.MaxConcurrentSessions},
			{string(domain.QuotaEngines), eff.MaxEngines},
			{string(domain.QuotaCacheBytes), eff.MaxCacheBytes},
		} {
			q.metrics.OrgQuotaLimit.WithLabelValues(orgID, item.name, "soft").Set(float64(item.th.Soft))
			q.metrics.OrgQuotaLimit.WithLabelValues(orgID, item.name, "hard").Set(float64(item.th.Hard))
		}
	}
	return usage, nil
}

// UsageByGroup returns the active session count per group (org-wide).
func (q *QuotaService) UsageByGroup(ctx context.Context) (map[string]int, error) {
	return q.usageSnapshot(ctx, ""), nil
}

// usageSnapshot computes per-group active session counts. A multi-group
// user's lease counts against EACH of their groups (decision D3). When orgID
// is non-empty, only that org's groups and leases are counted.
func (q *QuotaService) usageSnapshot(ctx context.Context, orgID string) map[string]int {
	out := make(map[string]int)
	memberships, err := q.groups.AllMemberships(ctx)
	if err != nil {
		q.logger.WithError(err).Warn("usage snapshot: memberships unavailable")
		return out
	}
	perUser := make(map[string]int)
	for _, l := range q.sessions.List() {
		if l.UserID == "" {
			continue
		}
		if orgID != "" && l.OrgID != orgID {
			continue
		}
		perUser[l.UserID]++
	}
	for gid, userIDs := range memberships {
		if orgID != "" {
			g, gErr := q.groups.Get(ctx, gid)
			if gErr != nil || g.OrgID != orgID {
				continue
			}
		}
		for _, uid := range userIDs {
			out[gid] += perUser[uid]
		}
	}
	return out
}
