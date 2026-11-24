package repository

import (
	"context"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// RepoSettingRepo is the Raft implementation of domain.RepoSettingRepository.
type RepoSettingRepo struct{ store *RaftStore }

var _ domain.RepoSettingRepository = (*RepoSettingRepo)(nil)

// NewRepoSettingRepo returns a RepoSettingRepo.
func NewRepoSettingRepo(store *RaftStore) *RepoSettingRepo {
	return &RepoSettingRepo{store: store}
}

func (r *RepoSettingRepo) BatchUpsert(ctx context.Context, orgID string, writes []domain.RepoSettingWrite) error {
	now := time.Now().UTC()
	cmds := make([]cmdRepoSettingWrite, 0, len(writes))
	for _, w := range writes {
		if w.Setting == nil {
			return domain.ErrValidation
		}
		setting := *w.Setting
		if setting.CreatedAt.IsZero() {
			setting.CreatedAt = now
		}
		setting.UpdatedAt = now
		cmd := cmdRepoSettingWrite{Setting: setting}
		if w.ForgeSource != nil {
			src := *w.ForgeSource
			if src.CreatedAt.IsZero() {
				src.CreatedAt = now
			}
			src.UpdatedAt = now
			cmd.ForgeSource = cmdForgeSourceFrom(&src)
		}
		cmds = append(cmds, cmd)
	}
	return r.store.applyCtx(ctx, kindBatchUpsertRepoSettings, cmdBatchUpsertRepoSettings{OrgID: orgID, Writes: cmds})
}

func (r *RepoSettingRepo) Get(ctx context.Context, orgID, canonicalRepo string) (*domain.RepoSetting, error) {
	return r.store.fsmRead().readRepoSetting(orgID, canonicalRepo)
}

func (r *RepoSettingRepo) GetByCanonicalRepo(ctx context.Context, canonicalRepo string) (*domain.RepoSetting, error) {
	return r.store.fsmRead().readRepoSettingByCanonical(canonicalRepo)
}

func (r *RepoSettingRepo) ListByOrg(ctx context.Context, orgID string) ([]*domain.RepoSetting, error) {
	return r.store.fsmRead().listRepoSettingsByOrg(orgID), nil
}

func (r *RepoSettingRepo) ListBySource(ctx context.Context, sourceID string) ([]*domain.RepoSetting, error) {
	return r.store.fsmRead().listRepoSettingsBySource(sourceID), nil
}

func (r *RepoSettingRepo) Delete(ctx context.Context, orgID, canonicalRepo string, deleteForgeSourceID string) error {
	return r.store.applyCtx(ctx, kindDeleteRepoSetting, cmdDeleteRepoSetting{OrgID: orgID, Repo: canonicalRepo, DeleteForgeSourceID: deleteForgeSourceID})
}
