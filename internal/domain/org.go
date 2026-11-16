package domain

import (
	"context"
	"time"
)

// DefaultOrgID is the deterministic bootstrap organization every legacy row is
// migrated into.
const DefaultOrgID = "default"

// DefaultOrgFeatures are enabled for every new organization.
var DefaultOrgFeatures = []string{"CLOUD_CHECKS", "CLOUD_MODULES"}

// OrgRole is a direct organization membership role.
type OrgRole string

const (
	OrgAdmin  OrgRole = "org_admin"
	OrgMember OrgRole = "member"
)

// Org is the top-level tenant. Groups, Projects, and TraceMeta belong to one
// Org.
type Org struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"` // lowercase slug
	DisplayName string    `json:"display_name"`
	Features    []string  `json:"features"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// OrgMembership is a direct user membership in an organization.
type OrgMembership struct {
	OrgID     string    `json:"org_id"`
	UserID    string    `json:"user_id"`
	Role      OrgRole   `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// OrgRepository is the persistence interface for organizations and direct
// memberships.
type OrgRepository interface {
	Create(ctx context.Context, o *Org) error
	Get(ctx context.Context, id string) (*Org, error)
	GetByName(ctx context.Context, name string) (*Org, error)
	List(ctx context.Context) ([]*Org, error)
	Update(ctx context.Context, o *Org) error
	Delete(ctx context.Context, id string) error

	SetMember(ctx context.Context, orgID, userID string, role OrgRole) error
	RemoveMember(ctx context.Context, orgID, userID string) error
	Members(ctx context.Context, orgID string) ([]*OrgMembership, error)
	OrgsForUser(ctx context.Context, userID string) ([]*Org, error)
	AllMemberships(ctx context.Context) (map[string][]*OrgMembership, error)
}

// HasFeature reports whether the org enables feature.
func (o *Org) HasFeature(feature string) bool {
	if o == nil {
		return false
	}
	for _, f := range o.Features {
		if f == feature {
			return true
		}
	}
	return false
}
