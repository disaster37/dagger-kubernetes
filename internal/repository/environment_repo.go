package repository

import (
	"context"
	"sort"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// EnvironmentRepo is the Raft implementation of domain.EnvironmentRepository.
type EnvironmentRepo struct{ store *RaftStore }

var _ domain.EnvironmentRepository = (*EnvironmentRepo)(nil)

// NewEnvironmentRepo returns an EnvironmentRepo.
func NewEnvironmentRepo(store *RaftStore) *EnvironmentRepo { return &EnvironmentRepo{store: store} }

func (r *EnvironmentRepo) Upsert(ctx context.Context, env *domain.Environment) error {
	if env.CreatedAt.IsZero() {
		env.CreatedAt = time.Now().UTC()
	}
	env.UpdatedAt = time.Now().UTC()
	return r.store.applyCtx(ctx, kindUpsertEnvironment, cmdEnvironment{Environment: *env})
}

func (r *EnvironmentRepo) Get(ctx context.Context, orgID, name string) (*domain.Environment, error) {
	return r.store.fsmRead().readEnvironment(orgID, name)
}

func (r *EnvironmentRepo) ListByOrg(ctx context.Context, orgID string) ([]*domain.Environment, error) {
	return r.store.fsmRead().listEnvironments(orgID), nil
}

func (r *EnvironmentRepo) Delete(ctx context.Context, orgID, name string) error {
	return r.store.applyCtx(ctx, kindDeleteEnvironment, cmdDeleteEnvironment{OrgID: orgID, Name: name})
}

func (f *FSM) readEnvironment(orgID, name string) (*domain.Environment, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	env, ok := s.environments[environmentKey(orgID, name)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *env
	cp.ApproverGroupIDs = append([]string(nil), env.ApproverGroupIDs...)
	return &cp, nil
}

func (f *FSM) listEnvironments(orgID string) []*domain.Environment {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.Environment
	for _, env := range s.environments {
		if env.OrgID != orgID {
			continue
		}
		cp := *env
		cp.ApproverGroupIDs = append([]string(nil), env.ApproverGroupIDs...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
