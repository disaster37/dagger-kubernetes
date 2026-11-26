package repository

import (
	"context"
	"sort"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// SecretRepo is the Raft implementation of domain.SecretRepository.
type SecretRepo struct{ store *RaftStore }

var _ domain.SecretRepository = (*SecretRepo)(nil)

// NewSecretRepo returns a SecretRepo.
func NewSecretRepo(store *RaftStore) *SecretRepo { return &SecretRepo{store: store} }

func (r *SecretRepo) Upsert(ctx context.Context, s *domain.Secret) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	s.UpdatedAt = time.Now().UTC()
	return r.store.applyCtx(ctx, kindUpsertSecret, cmdSecretFrom(s))
}

func (r *SecretRepo) Get(ctx context.Context, scope domain.SecretScope, scopeID, name string) (*domain.Secret, error) {
	return r.store.fsmRead().readSecret(scope, scopeID, name)
}

func (r *SecretRepo) List(ctx context.Context, scope domain.SecretScope, scopeID string) ([]*domain.Secret, error) {
	return r.store.fsmRead().listSecrets(scope, scopeID), nil
}

func (r *SecretRepo) Delete(ctx context.Context, scope domain.SecretScope, scopeID, name string) error {
	return r.store.applyCtx(ctx, kindDeleteSecret, cmdDeleteSecret{Scope: scope, ScopeID: scopeID, Name: name})
}

func (f *FSM) readSecret(scope domain.SecretScope, scopeID, name string) (*domain.Secret, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	sec, ok := s.secrets[secretKey(scope, scopeID, name)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *sec
	return &cp, nil
}

func (f *FSM) listSecrets(scope domain.SecretScope, scopeID string) []*domain.Secret {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.Secret
	for _, sec := range s.secrets {
		if sec.Scope == scope && sec.ScopeID == scopeID {
			cp := *sec
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
