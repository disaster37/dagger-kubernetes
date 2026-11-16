package domain

import (
	"context"
	"time"
)

// Environment is a protected deployment environment with approval rules.
type Environment struct {
	ID                  string        `json:"id"` // "<org_id>/<name>"
	OrgID               string        `json:"org_id"`
	Name                string        `json:"name"` // lowercase slug, e.g. "rec", "prod"
	ApproverGroupIDs    []string      `json:"approver_group_ids"`
	PreventSelfApproval bool          `json:"prevent_self_approval"`
	ApprovalTimeout     time.Duration `json:"approval_timeout"` // 0 = no timeout
	CreatedAt           time.Time     `json:"created_at"`
	UpdatedAt           time.Time     `json:"updated_at"`
}

// EnvironmentRepository persists protected environments.
type EnvironmentRepository interface {
	Upsert(ctx context.Context, env *Environment) error
	Get(ctx context.Context, orgID, name string) (*Environment, error)
	ListByOrg(ctx context.Context, orgID string) ([]*Environment, error)
	Delete(ctx context.Context, orgID, name string) error
}
