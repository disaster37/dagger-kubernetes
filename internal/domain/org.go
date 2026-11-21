package domain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultOrgName is the fixed, immutable slug of the bootstrap
	// organization. Its storage ID is a generated "org-<16hex>" UID persisted
	// at BootstrapOrgMetaKey.
	DefaultOrgName               = "default"
	DefaultOrgDisplayName        = "Default"
	BootstrapOrgMetaKey          = "bootstrap_org_id"
	DefaultOrgDisplayNameMetaKey = "default_org_display_name"
)

// DefaultOrgFeatures are enabled for every new organization.
var DefaultOrgFeatures = []string{"CLOUD_CHECKS", "CLOUD_MODULES"}

var (
	ErrOrgQuotaExceeded    = errors.New("organization quota exceeded")
	ErrAmbiguousOrg        = errors.New("ambiguous organization")
	ErrNoOrgMembership     = errors.New("user is not a member of any organization")
	ErrOrgNotEmpty         = errors.New("organization still contains resources")
	ErrSharedFleetMultiOrg = errors.New("shared fleet isolation forbids multiple organizations")
	ErrBootstrapNotReady   = errors.New("organization bootstrap in progress")
)

// OrgRole is a direct organization membership role.
type OrgRole string

const (
	OrgAdmin  OrgRole = "org_admin"
	OrgMember OrgRole = "member"
)

// QuotaResource names one org-level quota dimension.
type QuotaResource string

const (
	QuotaGroups             QuotaResource = "groups"
	QuotaProjects           QuotaResource = "projects"
	QuotaMembers            QuotaResource = "members"
	QuotaConcurrentSessions QuotaResource = "concurrent_sessions"
	QuotaEngines            QuotaResource = "engines"
	QuotaCacheBytes         QuotaResource = "cache_bytes"
)

// QuotaThreshold defines optional soft (warning) and hard (rejection)
// ceilings. 0 means unlimited / inherit cluster default. When both Soft > 0
// and Hard > 0, Soft must be <= Hard.
type QuotaThreshold struct {
	Soft int64 `json:"soft" mapstructure:"soft"`
	Hard int64 `json:"hard" mapstructure:"hard"`
}

// Validate checks the threshold invariants.
func (t QuotaThreshold) Validate(name string) error {
	if t.Soft < 0 || t.Hard < 0 {
		return fmt.Errorf("%s quota thresholds must be >= 0: %w", name, ErrValidation)
	}
	if t.Soft > 0 && t.Hard > 0 && t.Soft > t.Hard {
		return fmt.Errorf("%s soft limit (%d) must be <= hard limit (%d): %w", name, t.Soft, t.Hard, ErrValidation)
	}
	return nil
}

// OrgQuotas holds per-organization soft/hard resource limits. Zero values
// inherit from Config.Org.DefaultQuotas (which themselves default to 0 =
// unlimited).
type OrgQuotas struct {
	MaxGroups             QuotaThreshold `json:"max_groups" mapstructure:"max_groups"`
	MaxProjects           QuotaThreshold `json:"max_projects" mapstructure:"max_projects"`
	MaxMembers            QuotaThreshold `json:"max_members" mapstructure:"max_members"`
	MaxConcurrentSessions QuotaThreshold `json:"max_concurrent_sessions" mapstructure:"max_concurrent_sessions"`
	MaxEngines            QuotaThreshold `json:"max_engines" mapstructure:"max_engines"`
	MaxCacheBytes         QuotaThreshold `json:"max_cache_bytes" mapstructure:"max_cache_bytes"`
	// MaxPipelineRetention limits how long traces are retained (0 inherits
	// history.gc.max_age).
	MaxPipelineRetention time.Duration `json:"max_pipeline_retention" mapstructure:"max_pipeline_retention"`
}

// Validate checks every quota threshold.
func (q *OrgQuotas) Validate() error {
	if q == nil {
		return nil
	}
	for _, item := range []struct {
		name string
		val  QuotaThreshold
	}{
		{"max_groups", q.MaxGroups},
		{"max_projects", q.MaxProjects},
		{"max_members", q.MaxMembers},
		{"max_concurrent_sessions", q.MaxConcurrentSessions},
		{"max_engines", q.MaxEngines},
		{"max_cache_bytes", q.MaxCacheBytes},
	} {
		if err := item.val.Validate(item.name); err != nil {
			return err
		}
	}
	if q.MaxPipelineRetention < 0 {
		return fmt.Errorf("max_pipeline_retention must be >= 0: %w", ErrValidation)
	}
	return nil
}

// Effective merges org-specific overrides onto defaults per field.
//
//nolint:gocritic // value semantics keep quota snapshots immutable
func (q *OrgQuotas) Effective(defaults OrgQuotas) OrgQuotas {
	pick := func(own, def QuotaThreshold) QuotaThreshold {
		if own.Soft > 0 || own.Hard > 0 {
			return own
		}
		return def
	}
	ret := q.MaxPipelineRetention
	if ret <= 0 {
		ret = defaults.MaxPipelineRetention
	}
	return OrgQuotas{
		MaxGroups:             pick(q.MaxGroups, defaults.MaxGroups),
		MaxProjects:           pick(q.MaxProjects, defaults.MaxProjects),
		MaxMembers:            pick(q.MaxMembers, defaults.MaxMembers),
		MaxConcurrentSessions: pick(q.MaxConcurrentSessions, defaults.MaxConcurrentSessions),
		MaxEngines:            pick(q.MaxEngines, defaults.MaxEngines),
		MaxCacheBytes:         pick(q.MaxCacheBytes, defaults.MaxCacheBytes),
		MaxPipelineRetention:  ret,
	}
}

// Org is the top-level tenant and mandatory root namespace.
type Org struct {
	ID          string    `json:"id"`           // immutable: "org-<16hex>" for all orgs
	Name        string    `json:"name"`         // immutable lowercase DNS-label slug ("default" for bootstrap org)
	DisplayName string    `json:"display_name"` // mutable presentation title
	Features    []string  `json:"features"`
	Quotas      OrgQuotas `json:"quotas"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// OrgQuotaUsage reports live usage against effective quotas for UI/API display.
type OrgQuotaUsage struct {
	Groups             int64     `json:"groups"`
	Projects           int64     `json:"projects"`
	Members            int64     `json:"members"`
	ConcurrentSessions int64     `json:"concurrent_sessions"`
	Engines            int64     `json:"engines"`
	CacheBytes         int64     `json:"cache_bytes"`
	Effective          OrgQuotas `json:"effective"`
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

	// Bootstrap idempotently commits the bootstrap organization
	// (kindBootstrapOrg) and normalizes legacy rows into it.
	Bootstrap(ctx context.Context, o *Org, defaultGroupID, displayNameMeta string) (*Org, error)
	// BootstrapOrgID returns meta["bootstrap_org_id"] or ErrBootstrapNotReady.
	BootstrapOrgID(ctx context.Context) (string, error)
	// GetBootstrap returns the committed bootstrap organization.
	GetBootstrap(ctx context.Context) (*Org, error)
	// DeleteCascade atomically removes an org and all of its child entities.
	DeleteCascade(ctx context.Context, id string) error
	// GetMemberRole returns the direct membership role for (orgID, userID).
	GetMemberRole(ctx context.Context, orgID, userID string) (OrgRole, bool, error)
	// DistinctMemberCount counts distinct users in orgID.
	DistinctMemberCount(ctx context.Context, orgID string) (int, error)

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
