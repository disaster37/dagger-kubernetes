package repository

import (
	"context"
	"sort"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// WorkflowRepo is the Raft implementation of domain.WorkflowRepository.
type WorkflowRepo struct{ store *RaftStore }

var _ domain.WorkflowRepository = (*WorkflowRepo)(nil)

// NewWorkflowRepo returns a WorkflowRepo.
func NewWorkflowRepo(store *RaftStore) *WorkflowRepo { return &WorkflowRepo{store: store} }

func (r *WorkflowRepo) SyncProjectCatalog(ctx context.Context, orgID, projectID string, defs []*domain.WorkflowDef) error {
	return r.store.applyCtx(ctx, kindSyncProjectWorkflowDefs, cmdSyncProjectWorkflowDefs{OrgID: orgID, ProjectID: projectID, Defs: defs})
}

func (r *WorkflowRepo) ListDefs(ctx context.Context, orgID, projectID string) ([]*domain.WorkflowDef, error) {
	return r.store.fsmRead().listWorkflowDefs(orgID, projectID), nil
}

func (r *WorkflowRepo) GetDef(ctx context.Context, orgID, projectID, workflowName string) (*domain.WorkflowDef, error) {
	return r.store.fsmRead().readWorkflowDef(orgID, projectID, workflowName)
}

func (r *WorkflowRepo) CreateRun(ctx context.Context, run *domain.WorkflowRun) (*domain.WorkflowRun, error) {
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	run.UpdatedAt = time.Now().UTC()
	resp, err := r.store.applyCtxResponse(ctx, kindCreateWorkflowRun, cmdWorkflowRunFrom(run))
	if err != nil {
		return nil, err
	}
	out, _ := resp.(*domain.WorkflowRun)
	if out == nil {
		return nil, domain.ErrNotFound
	}
	return out, nil
}

func (r *WorkflowRepo) UpsertRun(ctx context.Context, run *domain.WorkflowRun) error {
	run.UpdatedAt = time.Now().UTC()
	return r.store.applyCtx(ctx, kindUpsertWorkflowRun, cmdWorkflowRunFrom(run))
}

func (r *WorkflowRepo) GetRun(ctx context.Context, runID string) (*domain.WorkflowRun, error) {
	return r.store.fsmRead().readWorkflowRun(runID)
}

func (r *WorkflowRepo) ListRuns(ctx context.Context, f *domain.WorkflowRunFilter) ([]*domain.WorkflowRun, error) {
	return r.store.fsmRead().listWorkflowRuns(f), nil
}

func (r *WorkflowRepo) DeleteRun(ctx context.Context, runID string) error {
	return r.store.applyCtx(ctx, kindDeleteWorkflowRun, cmdDeleteWorkflowRun{ID: runID})
}

func copyWorkflowDef(def *domain.WorkflowDef) *domain.WorkflowDef {
	cp := *def
	cp.ContextFiles = copyBytesMap(def.ContextFiles)
	return &cp
}

func copyWorkflowRun(run *domain.WorkflowRun) *domain.WorkflowRun {
	cp := *run
	cp.ContextFiles = copyBytesMap(run.ContextFiles)
	if run.Spec != nil {
		spec := *run.Spec
		cp.Spec = &spec
	}
	if run.Jobs != nil {
		cp.Jobs = make(map[string]domain.WorkflowJobRun, len(run.Jobs))
		for k := range run.Jobs {
			cp.Jobs[k] = run.Jobs[k]
		}
	}
	return &cp
}

func (f *FSM) readWorkflowDef(orgID, projectID, name string) (*domain.WorkflowDef, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	def, ok := s.workflowDefs[workflowDefKey(orgID, projectID, name)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyWorkflowDef(def), nil
}

func (f *FSM) listWorkflowDefs(orgID, projectID string) []*domain.WorkflowDef {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.WorkflowDef
	for _, def := range s.workflowDefs {
		if def.OrgID != orgID {
			continue
		}
		if projectID != "" && def.ProjectID != projectID {
			continue
		}
		out = append(out, copyWorkflowDef(def))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID == out[j].ProjectID {
			return out[i].Workflow.Name < out[j].Workflow.Name
		}
		return out[i].ProjectID < out[j].ProjectID
	})
	return out
}

func (f *FSM) readWorkflowRun(runID string) (*domain.WorkflowRun, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	run, ok := s.workflowRuns[runID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyWorkflowRun(run), nil
}

//nolint:dupl // mirrors listCheckRuns with a different record type
func (f *FSM) listWorkflowRuns(filter *domain.WorkflowRunFilter) []*domain.WorkflowRun {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.WorkflowRun
	for _, run := range s.workflowRuns {
		if filter != nil {
			if filter.OrgID != "" && run.OrgID != filter.OrgID {
				continue
			}
			if filter.ProjectID != "" && run.ProjectID != filter.ProjectID {
				continue
			}
			if filter.Workflow != "" && run.Workflow != filter.Workflow {
				continue
			}
			if filter.Status != "" && run.Status != filter.Status {
				continue
			}
		}
		out = append(out, copyWorkflowRun(run))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if filter != nil && filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out
}
