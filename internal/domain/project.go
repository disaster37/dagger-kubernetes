package domain

import (
	"context"
	"time"
)

// Project is a CI pipeline (identified by repo slug) assigned to a group.
type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`               // canonical CI repo slug, e.g. "github.com/acme/api"
	GroupID   string    `json:"group_id,omitempty"` // empty = unassigned
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProjectRepository is the persistence interface for projects.
type ProjectRepository interface {
	Create(ctx context.Context, p *Project) error
	Get(ctx context.Context, id string) (*Project, error)
	GetByName(ctx context.Context, name string) (*Project, error)
	List(ctx context.Context) ([]*Project, error)
	Update(ctx context.Context, p *Project) error
	Delete(ctx context.Context, id string) error
}
