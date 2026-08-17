package repository

import (
	"context"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// ProjectRepo is the Raft implementation of domain.ProjectRepository.
type ProjectRepo struct {
	store *RaftStore
}

var _ domain.ProjectRepository = (*ProjectRepo)(nil)

// NewProjectRepo returns a ProjectRepo backed by store.
func NewProjectRepo(store *RaftStore) *ProjectRepo {
	return &ProjectRepo{store: store}
}

func (r *ProjectRepo) Create(ctx context.Context, p *domain.Project) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	return r.upsert(ctx, p, true)
}

func (r *ProjectRepo) upsert(ctx context.Context, p *domain.Project, create bool) error {
	return r.store.applyCtx(ctx, kindUpsertProject, cmdProject{Project: *p, Create: create})
}

func (r *ProjectRepo) Get(ctx context.Context, id string) (*domain.Project, error) {
	return r.store.fsmRead().readProjectByID(id)
}

func (r *ProjectRepo) GetByName(ctx context.Context, name string) (*domain.Project, error) {
	return r.store.fsmRead().readProjectByName(name)
}

func (r *ProjectRepo) List(ctx context.Context) ([]*domain.Project, error) {
	return r.store.fsmRead().listProjects(), nil
}

func (r *ProjectRepo) Update(ctx context.Context, p *domain.Project) error {
	p.UpdatedAt = time.Now().UTC()
	return r.upsert(ctx, p, false)
}

func (r *ProjectRepo) Delete(ctx context.Context, id string) error {
	return r.store.applyCtx(ctx, kindDeleteProject, id)
}
