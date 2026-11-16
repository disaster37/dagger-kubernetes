package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// orgNameRe bounds organization slugs: lowercase alphanumerics and hyphens,
// starting/ending alphanumeric, 1..63 chars (DNS-label compatible).
var orgNameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateOrgName reports whether name is a valid organization slug.
func ValidateOrgName(name string) bool {
	return orgNameRe.MatchString(name)
}

// OrgService implements organization tenancy and direct memberships.
type OrgService struct {
	orgs   domain.OrgRepository
	groups domain.GroupRepository
	tokens *TokenService
	logger *logrus.Logger
}

// NewOrgService returns an OrgService.
func NewOrgService(orgs domain.OrgRepository, groups domain.GroupRepository, tokens *TokenService, logger *logrus.Logger) *OrgService {
	return &OrgService{orgs: orgs, groups: groups, tokens: tokens, logger: logger}
}

// OrgInput is the create/update payload.
type OrgInput struct {
	Name        string
	DisplayName string
	Features    []string
}

// Create creates an organization. The name is normalized to a lowercase slug.
func (s *OrgService) Create(ctx context.Context, in OrgInput) (*domain.Org, error) {
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !ValidateOrgName(name) {
		return nil, fmt.Errorf("%w: invalid org name %q", domain.ErrValidation, in.Name)
	}
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = name
	}
	features := in.Features
	if len(features) == 0 {
		features = append([]string(nil), domain.DefaultOrgFeatures...)
	}
	now := time.Now().UTC()
	o := &domain.Org{
		ID:          name,
		Name:        name,
		DisplayName: display,
		Features:    features,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.orgs.Create(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// Get returns an org by id.
func (s *OrgService) Get(ctx context.Context, id string) (*domain.Org, error) {
	return s.orgs.Get(ctx, id)
}

// GetByName returns an org by slug.
func (s *OrgService) GetByName(ctx context.Context, name string) (*domain.Org, error) {
	return s.orgs.GetByName(ctx, strings.ToLower(name))
}

// List returns all organizations.
func (s *OrgService) List(ctx context.Context) ([]*domain.Org, error) {
	return s.orgs.List(ctx)
}

// Update updates display name/features.
func (s *OrgService) Update(ctx context.Context, id string, in OrgInput) (*domain.Org, error) {
	o, err := s.orgs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.DisplayName != "" {
		o.DisplayName = in.DisplayName
	}
	if in.Features != nil {
		o.Features = in.Features
	}
	if err := s.orgs.Update(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// Delete removes an org (the bootstrap default org is protected).
func (s *OrgService) Delete(ctx context.Context, id string) error {
	return s.orgs.Delete(ctx, id)
}

// SetMember adds/updates a direct org membership.
func (s *OrgService) SetMember(ctx context.Context, orgID, userID string, role domain.OrgRole) error {
	if role != domain.OrgAdmin && role != domain.OrgMember {
		return fmt.Errorf("%w: invalid org role %q", domain.ErrValidation, role)
	}
	return s.orgs.SetMember(ctx, orgID, userID, role)
}

// RemoveMember removes a direct org membership.
func (s *OrgService) RemoveMember(ctx context.Context, orgID, userID string) error {
	return s.orgs.RemoveMember(ctx, orgID, userID)
}

// Members lists direct org memberships.
func (s *OrgService) Members(ctx context.Context, orgID string) ([]*domain.OrgMembership, error) {
	return s.orgs.Members(ctx, orgID)
}

// OrgsForUser returns the union of direct org memberships and orgs derived
// from the user's active group memberships.
func (s *OrgService) OrgsForUser(ctx context.Context, userID string) ([]*domain.Org, error) {
	direct, err := s.orgs.OrgsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(direct))
	out := make([]*domain.Org, 0, len(direct))
	for _, o := range direct {
		seen[o.ID] = true
		out = append(out, o)
	}
	groups, err := s.groups.GroupsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.OrgID == "" || seen[g.OrgID] {
			continue
		}
		o, err := s.orgs.Get(ctx, g.OrgID)
		if err != nil {
			continue
		}
		seen[o.ID] = true
		out = append(out, o)
	}
	return out, nil
}

// IsOrgAdmin reports whether userID is a direct org_admin of orgID.
func (s *OrgService) IsOrgAdmin(ctx context.Context, orgID, userID string) bool {
	members, err := s.orgs.Members(ctx, orgID)
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.UserID == userID && m.Role == domain.OrgAdmin {
			return true
		}
	}
	return false
}

// EnsureDefault creates the bootstrap "default" org when missing.
func (s *OrgService) EnsureDefault(ctx context.Context) error {
	if _, err := s.orgs.Get(ctx, domain.DefaultOrgID); err == nil {
		return nil
	}
	now := time.Now().UTC()
	return s.orgs.Create(ctx, &domain.Org{
		ID:          domain.DefaultOrgID,
		Name:        domain.DefaultOrgID,
		DisplayName: "Default",
		Features:    append([]string(nil), domain.DefaultOrgFeatures...),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
}

// GenerateToken issues (or rotates) the org-scoped dag_<org>_ token.
func (s *OrgService) GenerateToken(ctx context.Context, orgID string) (string, *domain.APIToken, error) {
	o, err := s.orgs.Get(ctx, orgID)
	if err != nil {
		return "", nil, err
	}
	if s.tokens == nil {
		return "", nil, fmt.Errorf("%w: token service unavailable", domain.ErrValidation)
	}
	return s.tokens.GenerateFor(ctx, orgTokenSubject(o.ID), fmt.Sprintf("dag_%s_", o.Name))
}

// EnsureToken returns the existing org token plaintext when recoverable, or
// generates a new one. It never invalidates an in-flight dag_<org>_ run.
func (s *OrgService) EnsureToken(ctx context.Context, orgID string) (string, error) {
	if s.tokens == nil {
		return "", fmt.Errorf("%w: token service unavailable", domain.ErrValidation)
	}
	pt, err := s.tokens.Reveal(ctx, orgTokenSubject(orgID))
	if err == nil {
		return pt, nil
	}
	if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrTokenNotRecoverable) {
		return "", err
	}
	pt, _, err = s.GenerateToken(ctx, orgID)
	return pt, err
}

// TokenMeta returns the masked org token metadata.
func (s *OrgService) TokenMeta(ctx context.Context, orgID string) (*domain.APIToken, error) {
	if s.tokens == nil {
		return nil, domain.ErrNotFound
	}
	return s.tokens.Meta(ctx, orgTokenSubject(orgID))
}

// RevokeToken deletes the org token.
func (s *OrgService) RevokeToken(ctx context.Context, orgID string) error {
	if s.tokens == nil {
		return nil
	}
	return s.tokens.Revoke(ctx, orgTokenSubject(orgID))
}

// orgTokenSubject is the synthetic token owner for an org-scoped token.
func orgTokenSubject(orgID string) string {
	return fmt.Sprintf("org:%s", orgID)
}

// ParseOrgToken extracts the org slug from a dag_<org>_<hex> token.
func ParseOrgToken(token string) (string, bool) {
	rest, ok := strings.CutPrefix(token, "dag_")
	if !ok {
		return "", false
	}
	idx := strings.LastIndex(rest, "_")
	if idx <= 0 || idx == len(rest)-1 {
		return "", false
	}
	return rest[:idx], true
}
