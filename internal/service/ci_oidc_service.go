package service

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// CIOIDCService verifies secretless CI OIDC ID tokens (GitHub Actions and
// GitLab CI) and exchanges them for an org-scoped Dagger token.
type CIOIDCService struct {
	issuers  map[string]string
	audience string
	orgs     *OrgService
	projects *ProjectService
	logger   *logrus.Logger

	mu        sync.Mutex
	providers map[string]*oidc.Provider
}

// NewCIOIDCService returns a CIOIDCService. issuers maps provider name
// ("github"|"gitlab") to its OIDC issuer URL.
func NewCIOIDCService(issuers map[string]string, audience string, orgs *OrgService, projects *ProjectService, logger *logrus.Logger) *CIOIDCService {
	return &CIOIDCService{
		issuers:   issuers,
		audience:  audience,
		orgs:      orgs,
		projects:  projects,
		logger:    logger,
		providers: make(map[string]*oidc.Provider),
	}
}

// CIOIDCResult is the POST /v1/oidc response.
type CIOIDCResult struct {
	Token   string `json:"token"`
	OrgID   string `json:"org_id"`
	OrgName string `json:"org_name"`
}

// Exchange verifies idToken for provider and returns the org token for the
// project's org. projectName is the canonical repo slug (e.g.
// "github.com/acme/api").
func (s *CIOIDCService) Exchange(ctx context.Context, provider, idToken, explicitOrg, projectName string) (*CIOIDCResult, error) {
	issuer, ok := s.issuers[provider]
	if !ok || issuer == "" {
		return nil, fmt.Errorf("%w: unsupported oidc provider %q", domain.ErrValidation, provider)
	}
	p, err := s.provider(ctx, provider, issuer)
	if err != nil {
		return nil, err
	}
	verifier := p.Verifier(&oidc.Config{ClientID: s.audience})
	tok, err := verifier.Verify(ctx, idToken)
	if err != nil {
		return nil, fmt.Errorf("%w: verify oidc token: %v", domain.ErrUnauthenticated, err)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: decode oidc claims: %v", domain.ErrUnauthenticated, err)
	}
	repo := oidcClaimString(claims, "repository", "project_path")
	if repo == "" {
		return nil, fmt.Errorf("%w: oidc token has no repository claim", domain.ErrUnauthenticated)
	}
	if projectName != "" && !repoMatches(repo, projectName) {
		return nil, fmt.Errorf("%w: oidc repository %q does not match project %q", domain.ErrForbidden, repo, projectName)
	}
	org, err := s.resolveOrg(ctx, explicitOrg, projectName)
	if err != nil {
		return nil, err
	}
	proj, err := s.projects.GetOrCreateByName(ctx, org.ID, projectName)
	if err != nil {
		return nil, err
	}
	_ = proj
	token, err := s.orgs.EnsureToken(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	return &CIOIDCResult{Token: token, OrgID: org.ID, OrgName: org.Name}, nil
}

// resolveOrg determines the organization for a secretless CI exchange.
func (s *CIOIDCService) resolveOrg(ctx context.Context, explicitOrg, projectName string) (*domain.Org, error) {
	if explicitOrg != "" {
		org, err := s.orgs.Resolve(ctx, explicitOrg)
		if err != nil {
			return nil, fmt.Errorf("%w: organization not found", domain.ErrForbidden)
		}
		return org, nil
	}
	matches, err := s.projects.ListByNameAcrossOrgs(ctx, projectName)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 1:
		return s.orgs.Get(ctx, matches[0].OrgID)
	case 0:
	default:
		return nil, fmt.Errorf("%w: project exists in multiple organizations", domain.ErrAmbiguousOrg)
	}
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
		return nil, fmt.Errorf("%w: organization is required", domain.ErrAmbiguousOrg)
	}
}

func (s *CIOIDCService) provider(ctx context.Context, name, issuer string) (*oidc.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.providers[name]; ok {
		return p, nil
	}
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discover oidc issuer %s: %w", issuer, err)
	}
	s.providers[name] = p
	return p, nil
}

func oidcClaimString(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := claims[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// repoMatches reports whether the OIDC repository claim matches the canonical
// project slug (which includes the forge host). The claim is compared against
// the exact path component after the host — never an arbitrary suffix — so a
// token for a same-named repo in a different org cannot match (CWE-863).
func repoMatches(claim, project string) bool {
	claim = strings.Trim(claim, "/")
	if claim == "" {
		return false
	}
	if claim == project {
		return true
	}
	idx := strings.Index(project, "/")
	if idx < 0 {
		return false
	}
	return project[idx+1:] == claim
}
