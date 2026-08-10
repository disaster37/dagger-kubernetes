package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// projectCols is the canonical column list shared by all project queries.
const projectCols = `id, name, group_id, created_at, updated_at`

// ProjectRepo is the SQLite implementation of domain.ProjectRepository.
type ProjectRepo struct {
	db *sql.DB
}

var _ domain.ProjectRepository = (*ProjectRepo)(nil)

// NewProjectRepo returns a ProjectRepo backed by db.
func NewProjectRepo(db *sql.DB) *ProjectRepo {
	return &ProjectRepo{db: db}
}

func (r *ProjectRepo) Create(ctx context.Context, p *domain.Project) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO projects(%s) VALUES(?, ?, ?, ?, ?)`, projectCols),
		p.ID, p.Name, nullString(p.GroupID), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create project %s: %w", p.Name, err)
	}
	return nil
}

func (r *ProjectRepo) Get(ctx context.Context, id string) (*domain.Project, error) {
	return r.queryProject(ctx, fmt.Sprintf("get project %s", id), fmt.Sprintf(`SELECT %s FROM projects WHERE id = ?`, projectCols), id)
}

func (r *ProjectRepo) GetByName(ctx context.Context, name string) (*domain.Project, error) {
	return r.queryProject(ctx, fmt.Sprintf("get project by name %s", name), fmt.Sprintf(`SELECT %s FROM projects WHERE name = ?`, projectCols), name)
}

// queryProject runs a single-row project query and wraps scan errors with label.
func (r *ProjectRepo) queryProject(ctx context.Context, label, query string, args ...any) (*domain.Project, error) {
	p, err := scanProject(r.db.QueryRowContext(ctx, query, args...))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return p, nil
}

func (r *ProjectRepo) List(ctx context.Context) ([]*domain.Project, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM projects ORDER BY created_at ASC`, projectCols))
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects rows: %w", err)
	}
	return out, nil
}

func (r *ProjectRepo) Update(ctx context.Context, p *domain.Project) error {
	p.UpdatedAt = time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `UPDATE projects SET name = ?, group_id = ?, updated_at = ? WHERE id = ?`,
		p.Name, nullString(p.GroupID), p.UpdatedAt, p.ID)
	if err != nil {
		return fmt.Errorf("update project %s: %w", p.ID, err)
	}
	return checkUpdated(res, "update project", p.ID)
}

func (r *ProjectRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project %s: %w", id, err)
	}
	return checkUpdated(res, "delete project", id)
}

func scanProject(row scanner) (*domain.Project, error) {
	p := &domain.Project{}
	var groupID sql.NullString
	err := row.Scan(&p.ID, &p.Name, &groupID, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("project: %w", domain.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	p.GroupID = groupID.String
	return p, nil
}
