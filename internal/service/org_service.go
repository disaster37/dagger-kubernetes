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

// generatedOrgIDRe matches the storage UID format of every organization.
var generatedOrgIDRe = regexp.MustCompile(`^org-[0-9a-f]{16}$`)

// ValidateOrgName reports whether name is an acceptable organization slug:
// DNS-label compatible and not a reserved selector ("default", "_shared",
// "all") or a generated org-<16hex> ID.
func ValidateOrgName(name string) bool {
	if !orgNameRe.MatchString(name) {
		return false
	}
	switch name {
	case domain.DefaultOrgName, "_shared", "all":
		return false
	}
	return !generatedOrgIDRe.MatchString(name)
}

// OrgMetaStore reads and writes replicated cluster metadata.
type OrgMetaStore interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
}

// OrgService implements organization tenancy, bootstrap and memberships.
type OrgService struct {
	orgs           domain.OrgRepository
	groups         domain.GroupRepository
	tokens         *TokenService
	meta           OrgMetaStore
	logger         *logrus.Logger
	fleetIsolation string
	defaultQuotas  domain.OrgQuotas
	onTokenChange  func(ctx context.Context, orgID, token string)
}

// NewOrgService returns an OrgService.
func NewOrgService(orgs domain.OrgRepository, groups domain.GroupRepository, tokens *TokenService, logger *logrus.Logger) *OrgService {
	return &OrgService{orgs: orgs, groups: groups, tokens: tokens, logger: logger}
}

// WithMetaStore wires the replicated metadata store used for bootstrap
// display-name reconciliation.
func (s *OrgService) WithMetaStore(meta OrgMetaStore) *OrgService {
	s.meta = meta
	return s
}

// WithFleetIsolation configures the fleet isolation mode guard.
func (s *OrgService) WithFleetIsolation(isolation string) *OrgService {
	s.fleetIsolation = isolation
	return s
}

// WithDefaultQuotas configures the cluster-wide org quota defaults.
//
//nolint:gocritic // quota structs are value-semantic
func (s *OrgService) WithDefaultQuotas(q domain.OrgQuotas) *OrgService {
	s.defaultQuotas = q
	return s
}

// SetOnTokenChange registers a callback invoked whenever an org token is
// generated, rotated, or revoked (used to sync the per-org engine Secret).
func (s *OrgService) SetOnTokenChange(fn func(ctx context.Context, orgID, token string)) {
	s.onTokenChange = fn
}

// OrgInput is the create payload.
type OrgInput struct {
	Name        string
	DisplayName string
	Features    []string
	Quotas      domain.OrgQuotas
}

// OrgUpdateInput is the update payload. The slug (Name) is immutable.
type OrgUpdateInput struct {
	Name        string
	DisplayName *string
	Features    []string
}

// Create creates an organization with a generated org-<16hex> storage UID and
// its bootstrap "default" group.
//
//nolint:gocritic // input structs are value-semantic
func (s *OrgService) Create(ctx context.Context, in OrgInput) (*domain.Org, error) {
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !ValidateOrgName(name) {
		return nil, fmt.Errorf("%w: invalid org name %q", domain.ErrValidation, in.Name)
	}
	if s.fleetIsolation == "shared" {
		existing, err := s.orgs.List(ctx)
		if err != nil {
			return nil, err
		}
		if len(existing) >= 1 {
			return nil, domain.ErrSharedFleetMultiOrg
		}
	}
	if err := in.Quotas.Validate(); err != nil {
		return nil, err
	}
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = name
	}
	if len(display) > 128 {
		return nil, fmt.Errorf("%w: display name must be at most 128 characters", domain.ErrValidation)
	}
	features := in.Features
	if len(features) == 0 {
		features = append([]string(nil), domain.DefaultOrgFeatures...)
	}
	now := time.Now().UTC()
	id, err := s.newOrgID(ctx)
	if err != nil {
		return nil, err
	}
	o := &domain.Org{
		ID:          id,
		Name:        name,
		DisplayName: display,
		Features:    features,
		Quotas:      in.Quotas,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.orgs.Create(ctx, o); err != nil {
		return nil, err
	}
	grp := &domain.Group{
		ID:             fmt.Sprintf("grp-%s", id[4:]),
		OrgID:          id,
		Name:           domain.DefaultOrgName,
		AgentAvailable: true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.groups.Create(ctx, grp); err != nil {
		return nil, fmt.Errorf("create default group: %w", err)
	}
	return o, nil
}

// newOrgID generates a fresh 20-char "org-<16hex>" UID that no live org owns.
func (s *OrgService) newOrgID(ctx context.Context) (string, error) {
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("org-%s", randomHex(8))
		if _, err := s.orgs.Get(ctx, id); errors.Is(err, domain.ErrNotFound) {
			return id, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("generate org id: %w", domain.ErrConflict)
}

// EnsureBootstrap commits the bootstrap organization on the leader and
// reconciles its display name against the configured value.
func (s *OrgService) EnsureBootstrap(ctx context.Context, configuredDisplayName string) (*domain.Org, error) {
	configured := strings.TrimSpace(configuredDisplayName)
	if configured == "" {
		configured = domain.DefaultOrgDisplayName
	}
	boot, err := s.orgs.GetBootstrap(ctx)
	if errors.Is(err, domain.ErrBootstrapNotReady) || errors.Is(err, domain.ErrNotFound) {
		if byName, nErr := s.orgs.GetByName(ctx, domain.DefaultOrgName); nErr == nil {
			return nil, fmt.Errorf("bootstrap org slug %q already owned by non-bootstrap org %s: %w", domain.DefaultOrgName, byName.ID, domain.ErrConflict)
		}
		id := fmt.Sprintf("org-%s", randomHex(8))
		created, bErr := s.orgs.Bootstrap(ctx, &domain.Org{
			ID:          id,
			Name:        domain.DefaultOrgName,
			DisplayName: configured,
			Features:    append([]string(nil), domain.DefaultOrgFeatures...),
			Quotas:      s.defaultQuotas,
		}, fmt.Sprintf("grp-%s", id[4:]), configured)
		if bErr != nil {
			return nil, bErr
		}
		return created, nil
	}
	if err != nil {
		return nil, err
	}
	if boot.Name != domain.DefaultOrgName {
		return nil, fmt.Errorf("bootstrap org %q has corrupted slug %q: %w", boot.ID, boot.Name, domain.ErrValidation)
	}
	return s.reconcileBootstrapDisplayName(ctx, boot, configured)
}

func (s *OrgService) reconcileBootstrapDisplayName(ctx context.Context, boot *domain.Org, configured string) (*domain.Org, error) {
	if s.meta == nil {
		return boot, nil
	}
	lastApplied, err := s.meta.Get(ctx, domain.DefaultOrgDisplayNameMetaKey)
	if errors.Is(err, domain.ErrNotFound) || lastApplied == "" {
		if boot.DisplayName == "" || boot.DisplayName == domain.DefaultOrgDisplayName || configured != domain.DefaultOrgDisplayName {
			boot.DisplayName = configured
			if uErr := s.orgs.Update(ctx, boot); uErr != nil {
				return nil, uErr
			}
		}
		if sErr := s.meta.Set(ctx, domain.DefaultOrgDisplayNameMetaKey, configured); sErr != nil {
			return nil, sErr
		}
		return boot, nil
	}
	if err != nil {
		return nil, err
	}
	if lastApplied == configured {
		return boot, nil
	}
	boot.DisplayName = configured
	if uErr := s.orgs.Update(ctx, boot); uErr != nil {
		return nil, uErr
	}
	if sErr := s.meta.Set(ctx, domain.DefaultOrgDisplayNameMetaKey, configured); sErr != nil {
		return nil, sErr
	}
	return boot, nil
}

// BootstrapOrgID returns the committed bootstrap organization UID.
func (s *OrgService) BootstrapOrgID(ctx context.Context) (string, error) {
	return s.orgs.BootstrapOrgID(ctx)
}

// GetBootstrap returns the committed bootstrap organization.
func (s *OrgService) GetBootstrap(ctx context.Context) (*domain.Org, error) {
	return s.orgs.GetBootstrap(ctx)
}

// Resolve looks an org up by storage ID first, then by slug.
func (s *OrgService) Resolve(ctx context.Context, idOrSlug string) (*domain.Org, error) {
	if idOrSlug == "" {
		return nil, fmt.Errorf("org: %w", domain.ErrValidation)
	}
	if o, err := s.orgs.Get(ctx, idOrSlug); err == nil {
		return o, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	return s.orgs.GetByName(ctx, strings.ToLower(idOrSlug))
}

// ResolveForIdentity resolves the request organization with zero silent
// fallbacks. explicit may come from :org, X-Dagger-Org, or ?org=.
//
//nolint:gocyclo // resolution matrix: token, explicit, admin, membership
func (s *OrgService) ResolveForIdentity(ctx context.Context, id *domain.Identity, explicit string) (*domain.Org, error) {
	if id == nil {
		return nil, domain.ErrUnauthenticated
	}
	if _, err := s.orgs.BootstrapOrgID(ctx); err != nil {
		return nil, err
	}
	if id.OrgID != "" {
		org, err := s.orgs.Get(ctx, id.OrgID)
		if err != nil {
			return nil, domain.ErrUnauthenticated
		}
		if explicit != "" && explicit != org.ID && !strings.EqualFold(explicit, org.Name) {
			return nil, fmt.Errorf("%w: token is scoped to organization %q", domain.ErrForbidden, org.Name)
		}
		return org, nil
	}
	if explicit != "" {
		org, err := s.Resolve(ctx, explicit)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				if id.IsAdmin() {
					return nil, domain.ErrNotFound
				}
				return nil, domain.ErrForbidden
			}
			return nil, err
		}
		ok, err := s.IsOrgMember(ctx, org.ID, id.UserID)
		if err != nil {
			return nil, err
		}
		if !id.IsAdmin() && !ok {
			return nil, domain.ErrForbidden
		}
		return org, nil
	}
	if id.IsAdmin() {
		all, err := s.orgs.List(ctx)
		if err != nil {
			return nil, err
		}
		switch len(all) {
		case 0:
			return nil, domain.ErrBootstrapNotReady
		case 1:
			return all[0], nil
		default:
			return nil, fmt.Errorf("%w: set X-Dagger-Org or use an org-scoped token", domain.ErrAmbiguousOrg)
		}
	}
	orgs, err := s.OrgsForUser(ctx, id.UserID)
	if err != nil {
		return nil, err
	}
	switch len(orgs) {
	case 0:
		return nil, domain.ErrNoOrgMembership
	case 1:
		return orgs[0], nil
	default:
		return nil, fmt.Errorf("%w: set X-Dagger-Org or use an org-scoped token", domain.ErrAmbiguousOrg)
	}
}

// Get returns an org by storage ID.
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

// Update updates the display name/features. The slug is immutable.
func (s *OrgService) Update(ctx context.Context, idOrSlug string, in OrgUpdateInput) (*domain.Org, error) {
	o, err := s.Resolve(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	if in.Name != "" && in.Name != o.Name {
		return nil, fmt.Errorf("%w: organization name cannot be changed", domain.ErrValidation)
	}
	if in.DisplayName != nil {
		display := strings.TrimSpace(*in.DisplayName)
		if display == "" || len(display) > 128 {
			return nil, fmt.Errorf("%w: display name must be 1..128 characters", domain.ErrValidation)
		}
		o.DisplayName = display
	}
	if in.Features != nil {
		o.Features = in.Features
	}
	if err := s.orgs.Update(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// UpdateQuotas replaces the org's quota limits and emits an audit log entry.
//
//nolint:gocritic // quota structs are value-semantic
func (s *OrgService) UpdateQuotas(ctx context.Context, actor *domain.Identity, idOrSlug string, q domain.OrgQuotas) (*domain.Org, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	o, err := s.Resolve(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	prev := o.Quotas
	o.Quotas = q
	if err := s.orgs.Update(ctx, o); err != nil {
		return nil, err
	}
	fields := logrus.Fields{"org_id": o.ID, "org_name": o.Name, "old_quotas": prev, "new_quotas": q}
	if actor != nil {
		fields["actor_id"] = actor.UserID
		fields["actor_username"] = actor.Username
	}
	s.logger.WithFields(fields).Info("organization quotas updated")
	return o, nil
}

// Delete removes an org (non-cascading) or cascades every child entity when
// force is true. onCascade runs the external cleanup (fleets, S3, metrics).
func (s *OrgService) Delete(ctx context.Context, idOrSlug string, force bool, onCascade func(ctx context.Context, orgID string) error) error {
	o, err := s.Resolve(ctx, idOrSlug)
	if err != nil {
		return err
	}
	bootID, err := s.orgs.BootstrapOrgID(ctx)
	if err == nil && o.ID == bootID {
		return fmt.Errorf("org %s: cannot delete bootstrap organization: %w", o.ID, domain.ErrForbidden)
	}
	if force {
		if err := s.orgs.DeleteCascade(ctx, o.ID); err != nil {
			return err
		}
		if onCascade != nil {
			if err := onCascade(ctx, o.ID); err != nil {
				return err
			}
		}
		if s.onTokenChange != nil {
			s.onTokenChange(ctx, o.ID, "")
		}
		return nil
	}
	if err := s.orgs.Delete(ctx, o.ID); err != nil {
		return err
	}
	if s.onTokenChange != nil {
		s.onTokenChange(ctx, o.ID, "")
	}
	return nil
}

// SetMember adds/updates a direct org membership.
func (s *OrgService) SetMember(ctx context.Context, orgID, userID string, role domain.OrgRole) error {
	if role != domain.OrgAdmin && role != domain.OrgMember {
		return fmt.Errorf("%w: invalid org role %q", domain.ErrValidation, role)
	}
	return s.orgs.SetMember(ctx, orgID, userID, role)
}

// RemoveMember removes a direct org membership and cascades that org's group
// memberships and live leases.
func (s *OrgService) RemoveMember(ctx context.Context, orgID, userID string) error {
	return s.orgs.RemoveMember(ctx, orgID, userID)
}

// Members lists direct org memberships.
func (s *OrgService) Members(ctx context.Context, orgID string) ([]*domain.OrgMembership, error) {
	return s.orgs.Members(ctx, orgID)
}

// OrgsForUser returns the union of direct org memberships and orgs derived
// from the user's active group memberships, sorted by name.
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

// IsOrgMember reports whether userID has a direct or group-derived membership
// in orgID.
func (s *OrgService) IsOrgMember(ctx context.Context, orgID, userID string) (bool, error) {
	if _, ok, err := s.orgs.GetMemberRole(ctx, orgID, userID); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	groups, err := s.groups.GroupsForUserInOrg(ctx, orgID, userID)
	if err != nil {
		return false, err
	}
	return len(groups) > 0, nil
}

// MemberRole returns userID's direct org role in orgID.
func (s *OrgService) MemberRole(ctx context.Context, orgID, userID string) (domain.OrgRole, bool, error) {
	return s.orgs.GetMemberRole(ctx, orgID, userID)
}

// IsOrgAdmin reports whether userID is a direct org_admin of orgID.
func (s *OrgService) IsOrgAdmin(ctx context.Context, orgID, userID string) bool {
	role, ok, err := s.orgs.GetMemberRole(ctx, orgID, userID)
	return err == nil && ok && role == domain.OrgAdmin
}

// GenerateToken issues (or rotates) the org-scoped dag_<slug>_ token.
func (s *OrgService) GenerateToken(ctx context.Context, orgID string) (string, *domain.APIToken, error) {
	o, err := s.orgs.Get(ctx, orgID)
	if err != nil {
		return "", nil, err
	}
	if s.tokens == nil {
		return "", nil, fmt.Errorf("%w: token service unavailable", domain.ErrValidation)
	}
	pt, meta, err := s.tokens.GenerateFor(ctx, orgTokenSubject(o.ID), fmt.Sprintf("dag_%s_", o.Name))
	if err != nil {
		return "", nil, err
	}
	if s.onTokenChange != nil {
		s.onTokenChange(ctx, o.ID, pt)
	}
	return pt, meta, nil
}

// EnsureToken returns the existing org token plaintext when recoverable, or
// generates a new one.
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
	if err := s.tokens.Revoke(ctx, orgTokenSubject(orgID)); err != nil {
		return err
	}
	if s.onTokenChange != nil {
		s.onTokenChange(ctx, orgID, "")
	}
	return nil
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
