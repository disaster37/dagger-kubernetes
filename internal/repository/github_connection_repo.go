package repository

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GitHubConnectionRepo is the Raft implementation of
// domain.GitHubConnectionRepository.
type GitHubConnectionRepo struct{ store *RaftStore }

var _ domain.GitHubConnectionRepository = (*GitHubConnectionRepo)(nil)

// NewGitHubConnectionRepo returns a GitHubConnectionRepo.
func NewGitHubConnectionRepo(store *RaftStore) *GitHubConnectionRepo {
	return &GitHubConnectionRepo{store: store}
}

func (r *GitHubConnectionRepo) Upsert(ctx context.Context, conn *domain.UserGitHubConnection) error {
	now := time.Now().UTC()
	if conn.ConnectedAt.IsZero() {
		conn.ConnectedAt = now
	}
	conn.UpdatedAt = now
	return r.store.applyCtx(ctx, kindUpsertGitHubConnection, cmdGitHubConnectionFrom(conn))
}

func (r *GitHubConnectionRepo) GetByUser(ctx context.Context, userID string) (*domain.UserGitHubConnection, error) {
	return r.store.fsmRead().readGitHubConnection(userID)
}

func (r *GitHubConnectionRepo) DeleteByUser(ctx context.Context, userID string) error {
	return r.store.applyCtx(ctx, kindDeleteGitHubConnection, cmdDeleteGitHubConnection{UserID: userID})
}

// GitHubAccountSourceRepo is the Raft implementation of
// domain.GitHubAccountSourceRepository.
type GitHubAccountSourceRepo struct{ store *RaftStore }

var _ domain.GitHubAccountSourceRepository = (*GitHubAccountSourceRepo)(nil)

// NewGitHubAccountSourceRepo returns a GitHubAccountSourceRepo.
func NewGitHubAccountSourceRepo(store *RaftStore) *GitHubAccountSourceRepo {
	return &GitHubAccountSourceRepo{store: store}
}

func (r *GitHubAccountSourceRepo) Upsert(ctx context.Context, src *domain.GitHubAccountSource) error {
	now := time.Now().UTC()
	if src.CreatedAt.IsZero() {
		src.CreatedAt = now
	}
	src.UpdatedAt = now
	return r.store.applyCtx(ctx, kindUpsertGitHubAccountSource, cmdGitHubAccountSourceFrom(src))
}

func (r *GitHubAccountSourceRepo) Get(ctx context.Context, id string) (*domain.GitHubAccountSource, error) {
	return r.store.fsmRead().readGitHubAccountSource(id)
}

func (r *GitHubAccountSourceRepo) GetByOrgAndOwner(ctx context.Context, orgID, owner string) (*domain.GitHubAccountSource, error) {
	return r.store.fsmRead().readGitHubAccountSourceByOwner(orgID, owner)
}

func (r *GitHubAccountSourceRepo) ListByOrg(ctx context.Context, orgID string) ([]*domain.GitHubAccountSource, error) {
	return r.store.fsmRead().listGitHubAccountSources(orgID), nil
}

func (r *GitHubAccountSourceRepo) Delete(ctx context.Context, id string) error {
	return r.store.applyCtx(ctx, kindDeleteGitHubAccountSource, cmdDeleteGitHubAccountSource{ID: id})
}

// GitHubOAuthStateRepo is the Raft implementation of
// domain.GitHubOAuthStateRepository.
type GitHubOAuthStateRepo struct{ store *RaftStore }

var _ domain.GitHubOAuthStateRepository = (*GitHubOAuthStateRepo)(nil)

// NewGitHubOAuthStateRepo returns a GitHubOAuthStateRepo.
func NewGitHubOAuthStateRepo(store *RaftStore) *GitHubOAuthStateRepo {
	return &GitHubOAuthStateRepo{store: store}
}

func (r *GitHubOAuthStateRepo) Create(ctx context.Context, st *domain.GitHubOAuthState) error {
	return r.store.applyCtx(ctx, kindCreateGitHubOAuthState, cmdGitHubOAuthStateFrom(st))
}

func (r *GitHubOAuthStateRepo) Get(ctx context.Context, stateID string) (*domain.GitHubOAuthState, error) {
	return r.store.fsmRead().readGitHubOAuthState(stateID)
}

func (r *GitHubOAuthStateRepo) Consume(ctx context.Context, stateID string, now time.Time) (*domain.GitHubOAuthState, error) {
	resp, err := r.store.applyCtxResponse(ctx, kindConsumeGitHubOAuthState, cmdConsumeGitHubOAuthState{StateID: stateID, At: now})
	if err := fsmError(resp, err); err != nil {
		return nil, err
	}
	st, ok := resp.(*domain.GitHubOAuthState)
	if !ok || st == nil {
		return nil, domain.ErrOAuthStateInvalid
	}
	return st, nil
}

func (f *FSM) readGitHubConnection(userID string) (*domain.UserGitHubConnection, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	conn, ok := s.githubConnections[userID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *conn
	cp.Scopes = append([]string(nil), conn.Scopes...)
	return &cp, nil
}

func (f *FSM) readGitHubAccountSource(id string) (*domain.GitHubAccountSource, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.githubAccountSources[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyGitHubAccountSource(src), nil
}

func (f *FSM) readGitHubAccountSourceByOwner(orgID, owner string) (*domain.GitHubAccountSource, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.githubAccountsByOrgOwner[githubAccountOwnerKey(orgID, owner)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	src, ok := s.githubAccountSources[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyGitHubAccountSource(src), nil
}

func (f *FSM) listGitHubAccountSources(orgID string) []*domain.GitHubAccountSource {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.GitHubAccountSource, 0)
	for _, src := range s.githubAccountSources {
		if src.OrgID == orgID {
			out = append(out, copyGitHubAccountSource(src))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *FSM) readGitHubOAuthState(stateID string) (*domain.GitHubOAuthState, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.githubOAuthStates[stateID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *st
	return &cp, nil
}

func copyGitHubAccountSource(src *domain.GitHubAccountSource) *domain.GitHubAccountSource {
	cp := *src
	cp.Scopes = append([]string(nil), src.Scopes...)
	return &cp
}

func copyRepoSetting(rs *domain.RepoSetting) *domain.RepoSetting {
	if rs == nil {
		return nil
	}
	cp := *rs
	return &cp
}

func (f *FSM) readRepoSetting(orgID, canonicalRepo string) (*domain.RepoSetting, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	rs, ok := s.repoSettings[repoSettingKey(orgID, canonicalRepo)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyRepoSetting(rs), nil
}

func (f *FSM) readRepoSettingByCanonical(canonicalRepo string) (*domain.RepoSetting, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	orgID, ok := s.repoClaimsByRepo[repoClaimKey(canonicalRepo)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	rs, ok := s.repoSettings[repoSettingKey(orgID, canonicalRepo)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyRepoSetting(rs), nil
}

func (f *FSM) listRepoSettingsByOrg(orgID string) []*domain.RepoSetting {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.RepoSetting, 0)
	for _, rs := range s.repoSettings {
		if rs.OrgID == orgID {
			out = append(out, copyRepoSetting(rs))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Repo) < strings.ToLower(out[j].Repo)
	})
	return out
}

func (f *FSM) listRepoSettingsBySource(sourceID string) []*domain.RepoSetting {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.RepoSetting, 0)
	for _, rs := range s.repoSettings {
		if rs.SourceID == sourceID || rs.ForgeSourceID == sourceID {
			out = append(out, copyRepoSetting(rs))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Repo) < strings.ToLower(out[j].Repo)
	})
	return out
}
