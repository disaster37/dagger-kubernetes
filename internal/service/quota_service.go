package service

import (
	"context"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// QuotaService enforces per-group engine-session quotas on engine admission.
type QuotaService struct {
	sessions domain.SessionStore
	groups   domain.GroupRepository
	logger   *logrus.Logger
}

// NewQuotaService returns a QuotaService.
func NewQuotaService(sessions domain.SessionStore, groups domain.GroupRepository, logger *logrus.Logger) *QuotaService {
	return &QuotaService{sessions: sessions, groups: groups, logger: logger}
}

// CheckEngineAccess decides whether an identity may provision a new engine.
// Admins bypass quota. Users with no groups get ErrNoGroups; users whose
// groups all have agent_available=false get ErrAgentUnavailable; otherwise
// admission succeeds if ANY available group has remaining capacity
// (max_runner_sessions; 0 = unlimited).
func (q *QuotaService) CheckEngineAccess(ctx context.Context, id *domain.Identity) error {
	if id.IsAdmin() {
		return nil
	}

	gs, err := q.groups.GroupsForUser(ctx, id.UserID)
	if err != nil {
		return err
	}
	if len(gs) == 0 {
		return domain.ErrNoGroups
	}

	var available []*domain.Group
	for _, g := range gs {
		if g.AgentAvailable {
			available = append(available, g)
		}
	}
	if len(available) == 0 {
		return domain.ErrAgentUnavailable
	}

	usage := q.usageSnapshot(ctx)
	for _, g := range available {
		if g.MaxRunnerSessions == 0 || usage[g.ID] < g.MaxRunnerSessions {
			return nil
		}
	}
	q.logger.WithFields(logrus.Fields{
		"user_id": id.UserID,
		"groups":  groupIDs(available),
		"usage":   usage,
	}).Warn("quota exhausted")
	return domain.ErrQuotaExhausted
}

// UsageByGroup returns the active session count per group (for the admin UI).
func (q *QuotaService) UsageByGroup(ctx context.Context) (map[string]int, error) {
	return q.usageSnapshot(ctx), nil
}

// usageSnapshot computes per-group active session counts. A multi-group
// user's lease counts against EACH of their groups (decision D3).
func (q *QuotaService) usageSnapshot(ctx context.Context) map[string]int {
	out := make(map[string]int)

	memberships, err := q.groups.AllMemberships(ctx)
	if err != nil {
		q.logger.WithError(err).Warn("usage snapshot: memberships unavailable")
		return out
	}

	leases := q.sessions.List()
	perUser := make(map[string]int)
	for _, l := range leases {
		if l.UserID != "" {
			perUser[l.UserID]++
		}
	}

	for gid, userIDs := range memberships {
		for _, uid := range userIDs {
			out[gid] += perUser[uid]
		}
	}
	return out
}
