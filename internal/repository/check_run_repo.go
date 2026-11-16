package repository

import (
	"context"
	"sort"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// CheckRunRepo is the Raft implementation of domain.CheckRunRepository.
type CheckRunRepo struct{ store *RaftStore }

var _ domain.CheckRunRepository = (*CheckRunRepo)(nil)

// NewCheckRunRepo returns a CheckRunRepo.
func NewCheckRunRepo(store *RaftStore) *CheckRunRepo { return &CheckRunRepo{store: store} }

func (r *CheckRunRepo) Upsert(ctx context.Context, rec *domain.CheckRunRecord) error {
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	rec.UpdatedAt = time.Now().UTC()
	return r.store.applyCtx(ctx, kindUpsertCheckRun, cmdCheckRunFrom(rec))
}

func (r *CheckRunRepo) Get(ctx context.Context, id string) (*domain.CheckRunRecord, error) {
	return r.store.fsmRead().readCheckRun(id)
}

func (r *CheckRunRepo) List(ctx context.Context, f *domain.CheckRunFilter) ([]*domain.CheckRunRecord, error) {
	return r.store.fsmRead().listCheckRuns(f), nil
}

func (r *CheckRunRepo) Delete(ctx context.Context, id string) error {
	return r.store.applyCtx(ctx, kindDeleteCheckRun, cmdDeleteCheckRun{ID: id})
}

func copyCheckRun(rec *domain.CheckRunRecord) *domain.CheckRunRecord {
	cp := *rec
	cp.Checks = append([]domain.CheckItem(nil), rec.Checks...)
	cp.WorkflowFiles = copyStringMap(rec.WorkflowFiles)
	cp.ContextFiles = copyBytesMap(rec.ContextFiles)
	cp.ApprovedAt = copyTime(rec.ApprovedAt)
	return &cp
}

func (f *FSM) readCheckRun(id string) (*domain.CheckRunRecord, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.checkRuns[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyCheckRun(rec), nil
}

//nolint:dupl // mirrors listWorkflowRuns with a different record type
func (f *FSM) listCheckRuns(filter *domain.CheckRunFilter) []*domain.CheckRunRecord {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.CheckRunRecord
	for _, rec := range s.checkRuns {
		if filter != nil {
			if filter.OrgID != "" && rec.OrgID != filter.OrgID {
				continue
			}
			if filter.ProjectID != "" && rec.ProjectID != filter.ProjectID {
				continue
			}
			if filter.Status != "" && rec.Status != filter.Status {
				continue
			}
			if filter.Repo != "" && rec.Repo != filter.Repo {
				continue
			}
		}
		out = append(out, copyCheckRun(rec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if filter != nil && filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out
}
