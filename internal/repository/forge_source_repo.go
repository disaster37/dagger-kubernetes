package repository

import (
	"context"
	"sort"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// ForgeSourceRepo is the Raft implementation of domain.ForgeSourceRepository.
type ForgeSourceRepo struct{ store *RaftStore }

var _ domain.ForgeSourceRepository = (*ForgeSourceRepo)(nil)

// NewForgeSourceRepo returns a ForgeSourceRepo.
func NewForgeSourceRepo(store *RaftStore) *ForgeSourceRepo { return &ForgeSourceRepo{store: store} }

func (r *ForgeSourceRepo) Upsert(ctx context.Context, s *domain.ForgeSource) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	s.UpdatedAt = time.Now().UTC()
	return r.store.applyCtx(ctx, kindUpsertForgeSource, cmdForgeSource{Source: *s})
}

func (r *ForgeSourceRepo) Get(ctx context.Context, id string) (*domain.ForgeSource, error) {
	return r.store.fsmRead().readForgeSource(id)
}

func (r *ForgeSourceRepo) ListByOrg(ctx context.Context, orgID string) ([]*domain.ForgeSource, error) {
	return r.store.fsmRead().listForgeSources(orgID), nil
}

func (r *ForgeSourceRepo) Delete(ctx context.Context, id string) error {
	return r.store.applyCtx(ctx, kindDeleteForgeSource, cmdForgeSource{Source: domain.ForgeSource{ID: id}})
}

func (f *FSM) readForgeSource(id string) (*domain.ForgeSource, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.forgeSources[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *src
	return &cp, nil
}

func (f *FSM) listForgeSources(orgID string) []*domain.ForgeSource {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.ForgeSource
	for _, src := range s.forgeSources {
		if src.OrgID == orgID {
			cp := *src
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
