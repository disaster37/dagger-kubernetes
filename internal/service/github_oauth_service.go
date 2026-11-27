package service

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	goauthoauth "github.com/disaster37/go-auth/oauth"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

const (
	defaultGitHubStateTTL     = 10 * time.Minute
	maxGitHubRepoMutations    = 100
	maxGitHubDeviceSessions   = 1024
	githubDeviceSessionMaxTTL = 15 * time.Minute

	// defaultGitHubRefreshSkew is the proactive-refresh margin: an access token
	// within this window of expiry is rotated before it can 401 mid-request.
	defaultGitHubRefreshSkew = 5 * time.Minute
	// githubRefreshPersistAttempts/githubRefreshPersistBackoff bound the retry
	// of a rotated token that failed to apply in Raft (leadership change).
	githubRefreshPersistAttempts = 3
	githubRefreshPersistBackoff  = 100 * time.Millisecond

	githubScopeUser       = "user"
	githubScopeOrgAccount = "org_account"
)

// GitHubIntegrationConfig is the forge credential-linking configuration. It is
// independent from auth.oauth.
type GitHubIntegrationConfig struct {
	PublicURL           string
	DefaultAPIURL       string
	AllowedAPIURLs      []string
	CORSAllowedOrigins  []string
	OAuthEnabled        bool
	ClientID            string
	ClientSecret        string
	Scopes              []string
	StateTTL            time.Duration
	DeviceFlowEnabled   bool
	AllowedRedirectURIs []string
	// RefreshEnabled is the kill switch for refresh-token rotation. When false
	// an expiring token behaves exactly like a non-expiring one (401 → revoke).
	RefreshEnabled bool
	// RefreshSkew is how long before expiry a token is proactively rotated.
	RefreshSkew time.Duration
}

// GitHubIntegrationDeps bundles the service's collaborators.
type GitHubIntegrationDeps struct {
	Connections    domain.GitHubConnectionRepository
	AccountSources domain.GitHubAccountSourceRepository
	RepoSettings   domain.RepoSettingRepository
	ForgeSources   domain.ForgeSourceRepository
	OAuthStates    domain.GitHubOAuthStateRepository
	DeviceSessions domain.GitHubDeviceSessionStore
	Users          domain.UserRepository
	Orgs           *OrgService
	Projects       *ProjectService
	Sealer         domain.TokenSealer
	GitHub         domain.GitHubAPIClient
	ForgeProvider  domain.ForgeProvider
	Logger         *logrus.Logger
	// IsLeader reports whether this pod is the Raft leader. Only the leader may
	// rotate a token, because only the leader can persist the rotation. nil is
	// treated as leader (single-node / tests).
	IsLeader func() bool
	// Metrics is optional; every counter is nil-guarded.
	Metrics *observ.Metrics
}

// GitHubAdminOrg identifies a Dagger organization the caller administers.
type GitHubAdminOrg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OAuthCompleteResult is returned after a successful OAuth/device-flow token
// capture.
type OAuthCompleteResult struct {
	Connection       *domain.UserGitHubConnection `json:"connection"`
	AutoConnectedOrg *domain.GitHubAccountSource  `json:"auto_connected_org,omitempty"`
	TargetOrgSlug    string                       `json:"target_org_slug,omitempty"`
	AvailableOwners  []domain.GitHubOwner         `json:"available_owners"`
	AdminOrgs        []GitHubAdminOrg             `json:"admin_orgs"`
}

// DeviceFlowStartResult is returned when a GitHub device flow begins.
type DeviceFlowStartResult struct {
	SessionID       string    `json:"session_id"`
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	ExpiresAt       time.Time `json:"expires_at"`
	Interval        int       `json:"interval"`
}

// GitHubIntegrationService implements GitHub OAuth credential linking, account
// sources, and per-repo claims.
type GitHubIntegrationService struct {
	cfg            GitHubIntegrationConfig
	connections    domain.GitHubConnectionRepository
	accountSources domain.GitHubAccountSourceRepository
	repoSettings   domain.RepoSettingRepository
	forgeSources   domain.ForgeSourceRepository
	oauthStates    domain.GitHubOAuthStateRepository
	deviceSessions domain.GitHubDeviceSessionStore
	users          domain.UserRepository
	orgs           *OrgService
	sealer         domain.TokenSealer
	github         domain.GitHubAPIClient
	forgeProvider  domain.ForgeProvider
	logger         *logrus.Logger
	leaderFn       func() bool
	metrics        *observ.Metrics
	refreshes      githubRefreshGate
}

var _ domain.GitHubCloudIntegrator = (*GitHubIntegrationService)(nil)

// NewGitHubIntegrationService returns a GitHubIntegrationService.
//
//nolint:gocritic // config/deps are startup-only immutable values
func NewGitHubIntegrationService(cfg GitHubIntegrationConfig, deps GitHubIntegrationDeps) *GitHubIntegrationService {
	if cfg.StateTTL <= 0 {
		cfg.StateTTL = defaultGitHubStateTTL
	}
	if cfg.DefaultAPIURL == "" {
		cfg.DefaultAPIURL = "https://api.github.com"
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"repo", "read:org", "admin:repo_hook"}
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = defaultGitHubRefreshSkew
	}
	return &GitHubIntegrationService{
		cfg:            cfg,
		connections:    deps.Connections,
		accountSources: deps.AccountSources,
		repoSettings:   deps.RepoSettings,
		forgeSources:   deps.ForgeSources,
		oauthStates:    deps.OAuthStates,
		deviceSessions: deps.DeviceSessions,
		users:          deps.Users,
		orgs:           deps.Orgs,
		sealer:         deps.Sealer,
		github:         deps.GitHub,
		forgeProvider:  deps.ForgeProvider,
		logger:         deps.Logger,
		leaderFn:       deps.IsLeader,
		metrics:        deps.Metrics,
		refreshes:      githubRefreshGate{calls: map[string]*githubRefreshCall{}},
	}
}

// OAuthEnabled reports whether the forge GitHub OAuth app is configured.
func (s *GitHubIntegrationService) OAuthEnabled() bool { return s.cfg.OAuthEnabled }

// DeviceFlowEnabled reports whether the UI device-flow fallback is available.
// Device flow only needs an OAuth App client ID (no secret and no callback
// URL), so it is independent of the redirect flow's OAuthEnabled gate.
func (s *GitHubIntegrationService) DeviceFlowEnabled() bool {
	return s.cfg.ClientID != "" && s.cfg.DeviceFlowEnabled
}

// BuildOAuthURL creates a replicated single-use state and returns the GitHub
// authorize URL.
func (s *GitHubIntegrationService) BuildOAuthURL(ctx context.Context, userID, orgID, redirectURI string) (string, error) {
	if err := s.requireHuman(ctx, userID); err != nil {
		return "", err
	}
	if orgID != "" && !s.isOrgAdmin(ctx, orgID, userID, false) {
		return "", fmt.Errorf("%w: organization administrator role required", domain.ErrForbidden)
	}
	if !s.cfg.OAuthEnabled {
		return "", fmt.Errorf("%w: GitHub OAuth app integration is not configured on this self-hosted deployment; set forge.github.oauth.enabled=true or configure integrations via %s/orgs/<org>/integrations/github", domain.ErrGitHubOAuthDisabled, strings.TrimRight(s.cfg.PublicURL, "/"))
	}
	if err := s.validateRedirectURI(redirectURI); err != nil {
		return "", err
	}
	stateID := randomHex(32)
	verifier, err := goauthoauth.GeneratePKCEVerifier()
	if err != nil {
		return "", fmt.Errorf("generate pkce verifier: %w", err)
	}
	now := time.Now().UTC()
	state := &domain.GitHubOAuthState{
		StateID: stateID, UserID: userID, OrgID: orgID, RedirectURI: redirectURI,
		PKCEVerifier: verifier, CreatedAt: now, ExpiresAt: now.Add(s.cfg.StateTTL),
	}
	if err := s.oauthStates.Create(ctx, state); err != nil {
		return "", err
	}
	return s.github.AuthorizeURL(s.cfg.ClientID, redirectURI, stateID, goauthoauth.ComputePKCEChallenge(verifier), s.cfg.Scopes), nil
}

// CompleteOAuth consumes a state and exchanges its code, sealing the resulting
// token to the caller. A caller/state user mismatch is rejected WITHOUT
// consuming the state or exchanging the code.
func (s *GitHubIntegrationService) CompleteOAuth(ctx context.Context, callerUserID string, callerIsGlobalAdmin bool, code, stateID string) (*OAuthCompleteResult, error) {
	if !validCode(code) || !validStateID(stateID) {
		return nil, domain.ErrOAuthStateInvalid
	}
	state, err := s.oauthStates.Get(ctx, stateID)
	if err != nil || state.Consumed || !time.Now().UTC().Before(state.ExpiresAt) {
		return nil, domain.ErrOAuthStateInvalid
	}
	if subtle.ConstantTimeCompare([]byte(callerUserID), []byte(state.UserID)) != 1 {
		s.logger.WithFields(logrus.Fields{
			"event":            "github_oauth_state_user_mismatch",
			"expected_user_id": state.UserID,
			"caller_user_id":   callerUserID,
		}).Warn("github oauth state submitted by a different user")
		expected, current := "", ""
		if u, err := s.users.Get(ctx, state.UserID); err == nil {
			expected = u.Username
		}
		if u, err := s.users.Get(ctx, callerUserID); err == nil {
			current = u.Username
		}
		return nil, &domain.OAuthStateUserMismatchError{ExpectedUsername: expected, CurrentUsername: current}
	}
	if _, err := s.oauthStates.Consume(ctx, stateID, time.Now().UTC()); err != nil {
		return nil, domain.ErrOAuthStateInvalid
	}
	token, err := s.github.ExchangeCode(ctx, code, state.RedirectURI, state.PKCEVerifier)
	if err != nil {
		return nil, err
	}
	return s.completeWithToken(ctx, callerUserID, callerIsGlobalAdmin, state.OrgID, token)
}

// StartDeviceFlow initiates the GitHub RFC 8628 device flow.
func (s *GitHubIntegrationService) StartDeviceFlow(ctx context.Context, userID, orgID string) (*DeviceFlowStartResult, error) {
	if err := s.requireHuman(ctx, userID); err != nil {
		return nil, err
	}
	if !s.DeviceFlowEnabled() {
		return nil, domain.ErrGitHubDeviceFlowDisabled
	}
	if orgID != "" && !s.isOrgAdmin(ctx, orgID, userID, false) {
		return nil, fmt.Errorf("%w: organization administrator role required", domain.ErrForbidden)
	}
	prompt, err := s.github.StartDeviceFlow(ctx, s.cfg.Scopes)
	if err != nil {
		return nil, err
	}
	sessionID := randomHex(16)
	ttl := time.Duration(prompt.ExpiresIn) * time.Second
	if ttl <= 0 || ttl > githubDeviceSessionMaxTTL {
		ttl = githubDeviceSessionMaxTTL
	}
	session := &domain.GitHubDeviceSession{
		SessionID: sessionID, UserID: userID, OrgID: orgID, DeviceCode: prompt.DeviceCode,
		UserCode: prompt.UserCode, VerificationURI: prompt.VerificationURI,
		Interval: prompt.Interval, ExpiresAt: time.Now().UTC().Add(ttl),
	}
	if err := s.deviceSessions.Create(session); err != nil {
		return nil, err
	}
	return &DeviceFlowStartResult{
		SessionID: sessionID, UserCode: prompt.UserCode, VerificationURI: prompt.VerificationURI,
		ExpiresAt: session.ExpiresAt, Interval: session.Interval,
	}, nil
}

// PollDeviceFlow performs one device-flow poll and completes the connection on
// success. Pending/slow_down are returned as sentinel errors.
func (s *GitHubIntegrationService) PollDeviceFlow(ctx context.Context, userID string, callerIsGlobalAdmin bool, sessionID string) (*OAuthCompleteResult, error) {
	if err := s.requireHuman(ctx, userID); err != nil {
		return nil, err
	}
	session, err := s.deviceSessions.Get(sessionID)
	if err != nil {
		return nil, domain.ErrOAuthStateInvalid
	}
	if subtle.ConstantTimeCompare([]byte(userID), []byte(session.UserID)) != 1 {
		return nil, fmt.Errorf("%w: device session belongs to another user", domain.ErrForbidden)
	}
	if session.Consumed || !time.Now().UTC().Before(session.ExpiresAt) {
		s.deviceSessions.Delete(sessionID)
		return nil, domain.ErrOAuthStateInvalid
	}
	token, err := s.github.PollDeviceFlow(ctx, session.DeviceCode)
	if err != nil {
		if errors.Is(err, domain.ErrGitHubDeviceSlowDown) {
			_ = s.deviceSessions.UpdateInterval(sessionID, session.Interval+5)
		}
		return nil, err
	}
	if err := s.deviceSessions.Consume(sessionID); err != nil {
		return nil, domain.ErrOAuthStateInvalid
	}
	result, err := s.completeWithToken(ctx, userID, callerIsGlobalAdmin, session.OrgID, token)
	if err != nil {
		return nil, err
	}
	s.deviceSessions.Delete(sessionID)
	return result, nil
}

// completeWithToken seals the token pair, upserts the user connection, and
// auto-connects a sole-admin org when applicable.
func (s *GitHubIntegrationService) completeWithToken(ctx context.Context, userID string, callerIsGlobalAdmin bool, boundOrgID string, token *domain.GitHubTokenResult) (*OAuthCompleteResult, error) {
	if token == nil || token.AccessToken == "" {
		return nil, domain.ErrGitHubTokenRevoked
	}
	profile, apiScopes, err := s.github.GetAuthenticatedUser(ctx, s.cfg.DefaultAPIURL, token.AccessToken)
	if err != nil {
		return nil, err
	}
	if profile == nil || profile.Login == "" {
		return nil, fmt.Errorf("github user profile is empty: %w", domain.ErrGitHubTokenRevoked)
	}
	now := time.Now().UTC()
	cred := credentialFromTokenResult(token, now)
	if len(cred.Scopes) == 0 {
		cred.Scopes = apiScopes
	}
	pair, err := s.sealCredential(domain.SecretScopeGitHubUser, userID, &cred)
	if err != nil {
		return nil, fmt.Errorf("seal github user token: %w", err)
	}
	conn := &domain.UserGitHubConnection{
		UserID: userID, GitHubUserID: profile.ID, GitHubLogin: profile.Login, AvatarURL: profile.AvatarURL,
		BaseURL: s.cfg.DefaultAPIURL, Scopes: cred.Scopes, TokenCiphertext: pair.access,
		RefreshTokenCiphertext: pair.refresh, TokenExpiresAt: pair.accessExp, RefreshExpiresAt: pair.refreshExp,
		TokenVersion: 1, Status: domain.GitHubConnectionActive, ConnectedAt: now, UpdatedAt: now,
	}
	if existing, err := s.connections.GetByUser(ctx, userID); err == nil {
		conn.ConnectedAt = existing.ConnectedAt
		// Re-authing replaces the whole token pair, so advance the version
		// rather than resetting it: a rotation already in flight holds the old
		// version and must fail its CAS instead of clobbering this re-auth.
		conn.TokenVersion = existing.TokenVersion + 1
	}
	if err := s.connections.Upsert(ctx, conn); err != nil {
		return nil, err
	}

	owners, err := s.github.ListUserOrgs(ctx, s.cfg.DefaultAPIURL, token.AccessToken)
	if err != nil {
		s.logger.WithError(err).WithField("user_id", userID).Warn("list github orgs failed")
		owners = nil
	}
	available := dedupeOwners(profile, owners)
	adminOrgs := s.adminOrgs(ctx, userID, callerIsGlobalAdmin)
	acct, targetSlug, err := s.autoConnect(ctx, userID, boundOrgID, profile, &cred, adminOrgs)
	if err != nil {
		return nil, err
	}
	s.logger.WithFields(logrus.Fields{
		"event":          "github_user_connected",
		"user_id":        userID,
		"github_login":   profile.Login,
		"scopes":         cred.Scopes,
		"target_org":     targetSlug,
		"auto_connected": acct != nil,
		"refreshable":    pair.refresh != "",
	}).Info("github user connected")

	return &OAuthCompleteResult{
		Connection: conn, AutoConnectedOrg: acct, TargetOrgSlug: targetSlug,
		AvailableOwners: available, AdminOrgs: adminOrgs,
	}, nil
}

// autoConnect provisions a personal GitHubAccountSource for an explicitly
// bound org or the caller's only administered org.
func (s *GitHubIntegrationService) autoConnect(ctx context.Context, userID, boundOrgID string, profile *domain.GitHubUserProfile, cred *githubCredential, adminOrgs []GitHubAdminOrg) (*domain.GitHubAccountSource, string, error) {
	var target *domain.Org
	if boundOrgID != "" {
		for _, candidate := range adminOrgs {
			if candidate.ID == boundOrgID {
				o, err := s.orgs.Get(ctx, boundOrgID)
				if err != nil {
					return nil, "", err
				}
				target = o
				break
			}
		}
	} else if len(adminOrgs) == 1 {
		o, err := s.orgs.Get(ctx, adminOrgs[0].ID)
		if err != nil {
			return nil, "", err
		}
		target = o
	}
	if target == nil {
		return nil, "", nil
	}
	acct, err := s.upsertAccountSource(ctx, target.ID, profile.Login, profile.Type, s.cfg.DefaultAPIURL, cred, userID)
	if err != nil {
		return nil, "", err
	}
	return acct, target.Name, nil
}

// ConnectUserPAT validates and stores a user-scoped personal access token.
func (s *GitHubIntegrationService) ConnectUserPAT(ctx context.Context, userID, baseURL, pat string) (*domain.UserGitHubConnection, error) {
	if err := s.requireHuman(ctx, userID); err != nil {
		return nil, err
	}
	base, err := s.validateAPIBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if err := validatePAT(pat); err != nil {
		return nil, err
	}
	profile, scopes, err := s.github.GetAuthenticatedUser(ctx, base, pat)
	if err != nil {
		return nil, err
	}
	ct, err := s.sealer.SealRaw(domain.SecretScopeGitHubUser, userID, pat)
	if err != nil {
		return nil, fmt.Errorf("seal github user token: %w", err)
	}
	now := time.Now().UTC()
	conn := &domain.UserGitHubConnection{
		UserID: userID, GitHubUserID: profile.ID, GitHubLogin: profile.Login, AvatarURL: profile.AvatarURL,
		BaseURL: base, Scopes: scopes, TokenCiphertext: ct, TokenVersion: 1,
		Status: domain.GitHubConnectionActive, ConnectedAt: now, UpdatedAt: now,
	}
	if existing, err := s.connections.GetByUser(ctx, userID); err == nil {
		conn.ConnectedAt = existing.ConnectedAt
		conn.TokenVersion = existing.TokenVersion + 1
	}
	if err := s.connections.Upsert(ctx, conn); err != nil {
		return nil, err
	}
	s.logger.WithFields(logrus.Fields{"event": "github_user_connected_pat", "user_id": userID, "github_login": profile.Login, "scopes": scopes}).Info("github user connected with pat")
	return conn, nil
}

// GetUserConnection returns the caller's connection metadata.
func (s *GitHubIntegrationService) GetUserConnection(ctx context.Context, userID string) (*domain.UserGitHubConnection, error) {
	return s.connections.GetByUser(ctx, userID)
}

// DisconnectUser erases the caller's personal connection and pending states.
func (s *GitHubIntegrationService) DisconnectUser(ctx context.Context, userID string) error {
	if err := s.connections.DeleteByUser(ctx, userID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	s.logger.WithFields(logrus.Fields{"event": "github_user_disconnected", "user_id": userID}).Info("github user disconnected")
	return nil
}

// ListUserOwners lists the GitHub user and organizations reachable by the
// caller's token.
func (s *GitHubIntegrationService) ListUserOwners(ctx context.Context, userID string) ([]domain.GitHubOwner, error) {
	var (
		profile *domain.GitHubUserProfile
		orgs    []domain.GitHubOwner
	)
	if err := s.callWithUserToken(ctx, userID, func(conn *domain.UserGitHubConnection, token string) error {
		p, _, err := s.github.GetAuthenticatedUser(ctx, conn.BaseURL, token)
		if err != nil {
			return err
		}
		found, err := s.github.ListUserOrgs(ctx, conn.BaseURL, token)
		if err != nil {
			return err
		}
		profile, orgs = p, found
		return nil
	}); err != nil {
		return nil, err
	}
	return dedupeOwners(profile, orgs), nil
}

// ListOrgAccounts lists an org's GitHub account sources (ciphertext omitted by
// the domain JSON tags).
func (s *GitHubIntegrationService) ListOrgAccounts(ctx context.Context, orgID string) ([]*domain.GitHubAccountSource, error) {
	return s.accountSources.ListByOrg(ctx, orgID)
}

// ConnectOrgAccount links (or rotates) an org-scoped GitHub owner token.
func (s *GitHubIntegrationService) ConnectOrgAccount(ctx context.Context, actorUserID string, actorIsGlobalAdmin bool, orgID, owner, baseURL, pat string) (*domain.GitHubAccountSource, error) {
	if !s.isOrgAdmin(ctx, orgID, actorUserID, actorIsGlobalAdmin) {
		return nil, fmt.Errorf("%w: organization administrator role required", domain.ErrForbidden)
	}
	base, err := s.validateAPIBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	var (
		cred    githubCredential
		profile *domain.GitHubUserProfile
		orgs    []domain.GitHubOwner
	)
	if pat != "" {
		if err := validatePAT(pat); err != nil {
			return nil, err
		}
		cred.AccessToken = pat
		if profile, cred.Scopes, err = s.github.GetAuthenticatedUser(ctx, base, pat); err != nil {
			return nil, err
		}
		if orgs, err = s.github.ListUserOrgs(ctx, base, pat); err != nil {
			return nil, err
		}
	} else if err := s.callWithUserToken(ctx, actorUserID, func(conn *domain.UserGitHubConnection, token string) error {
		base = conn.BaseURL
		p, scopes, err := s.github.GetAuthenticatedUser(ctx, base, token)
		if err != nil {
			return err
		}
		found, err := s.github.ListUserOrgs(ctx, base, token)
		if err != nil {
			return err
		}
		profile, orgs = p, found
		cred = s.credentialFromUserConnection(conn, token, scopes)
		return nil
	}); err != nil {
		return nil, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = profile.Login
	}
	if !githubOwnerAllowed(owner, profile, orgs) {
		return nil, fmt.Errorf("%w: github owner %q is not accessible through this token", domain.ErrForbidden, owner)
	}
	acct, err := s.upsertAccountSource(ctx, orgID, owner, ownerType(owner, profile, orgs), base, &cred, actorUserID)
	if err != nil {
		return nil, err
	}
	return acct, nil
}

// upsertAccountSource seals the credential under the org-account AAD and
// re-seals child forge sources when the account token rotates.
func (s *GitHubIntegrationService) upsertAccountSource(ctx context.Context, orgID, owner, ownerType, baseURL string, cred *githubCredential, connectedBy string) (*domain.GitHubAccountSource, error) {
	acctID := domain.GitHubAccountSourceID(orgID, baseURL, owner)
	scopeID, err := domain.GitHubOrgAccountScopeID(orgID, acctID)
	if err != nil {
		return nil, err
	}
	pair, err := s.sealCredential(domain.SecretScopeGitHubOrgAccount, scopeID, cred)
	if err != nil {
		return nil, fmt.Errorf("seal github account token: %w", err)
	}
	now := time.Now().UTC()
	acct := &domain.GitHubAccountSource{
		ID: acctID, OrgID: orgID, Owner: owner, OwnerType: ownerType, BaseURL: baseURL,
		ConnectedByUserID: connectedBy, Scopes: cred.Scopes, TokenCiphertext: pair.access,
		RefreshTokenCiphertext: pair.refresh, TokenExpiresAt: pair.accessExp, RefreshExpiresAt: pair.refreshExp,
		TokenVersion: 1, Status: domain.GitHubConnectionActive, CreatedAt: now, UpdatedAt: now,
	}
	if existing, err := s.accountSources.Get(ctx, acctID); err == nil {
		acct.CreatedAt = existing.CreatedAt
		// A re-link replaces the whole token pair: advance the version so an
		// in-flight rotation holding the old version fails its CAS instead of
		// clobbering this re-link.
		acct.TokenVersion = existing.TokenVersion + 1
	}
	if err := s.accountSources.Upsert(ctx, acct); err != nil {
		return nil, err
	}
	if err := s.reSealAccountForgeSources(ctx, orgID, acctID, cred.AccessToken); err != nil {
		return nil, err
	}
	return acct, nil
}

// reSealAccountForgeSources re-seals every per-repo runtime ForgeSource of an
// account with the account's current access token, so a rotation reaches the
// webhook/check/clone paths.
func (s *GitHubIntegrationService) reSealAccountForgeSources(ctx context.Context, orgID, acctID, accessToken string) error {
	sources, err := s.forgeSources.ListByOrg(ctx, orgID)
	if err != nil {
		return err
	}
	for _, src := range sources {
		if src.AccountSourceID != acctID {
			continue
		}
		srcCT, err := s.sealer.SealRaw(domain.SecretScopeOrg, orgID, accessToken)
		if err != nil {
			return err
		}
		src.TokenCiphertext = srcCT
		if err := s.forgeSources.Upsert(ctx, src); err != nil {
			return err
		}
	}
	return nil
}

// DisconnectOrgAccount erases an org account token, optionally cascading its
// repo claims and remote webhooks.
func (s *GitHubIntegrationService) DisconnectOrgAccount(ctx context.Context, actorUserID string, actorIsGlobalAdmin bool, orgID, accountID string, cascade bool) error {
	if !s.isOrgAdmin(ctx, orgID, actorUserID, actorIsGlobalAdmin) {
		return fmt.Errorf("%w: organization administrator role required", domain.ErrForbidden)
	}
	acct, err := s.accountSources.Get(ctx, accountID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	if acct.OrgID != orgID {
		return fmt.Errorf("%w: github account belongs to another organization", domain.ErrForbidden)
	}
	settings, err := s.repoSettings.ListBySource(ctx, accountID)
	if err != nil {
		return err
	}
	owned := make([]*domain.RepoSetting, 0, len(settings))
	for _, setting := range settings {
		if setting.OrgID == orgID {
			owned = append(owned, setting)
		}
	}
	if !cascade && len(owned) > 0 {
		return fmt.Errorf("%w: github account still has claimed repositories", domain.ErrConflict)
	}
	for _, setting := range owned {
		s.deleteRemoteWebhook(ctx, orgID, setting.ForgeSourceID)
		if err := s.repoSettings.Delete(ctx, orgID, setting.Repo, setting.ForgeSourceID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	if err := s.accountSources.Delete(ctx, accountID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	s.logger.WithFields(logrus.Fields{"event": "github_org_account_disconnected", "org_id": orgID, "account_id": accountID, "cascade": cascade}).Info("github org account disconnected")
	return nil
}

// ListSourceRepositories returns the account's repositories with claim
// eligibility for the org.
func (s *GitHubIntegrationService) ListSourceRepositories(ctx context.Context, org *domain.Org, installationID string) ([]domain.SourceRepositoryItem, error) {
	acct, err := s.accountSources.Get(ctx, installationID)
	if err != nil {
		return nil, err
	}
	if acct.OrgID != org.ID {
		return nil, domain.ErrNotFound
	}
	var repos []domain.GitHubRemoteRepo
	if err := s.callWithAccountToken(ctx, acct, func(token string) error {
		found, err := s.github.ListAccessibleRepos(ctx, acct.BaseURL, token)
		repos = found
		return err
	}); err != nil {
		return nil, err
	}
	return s.mapSourceRepositories(ctx, org, acct, repos, "", ""), nil
}

// ListOrgGitHubRepositories returns the picker list for an org account.
func (s *GitHubIntegrationService) ListOrgGitHubRepositories(ctx context.Context, actorUserID string, actorIsGlobalAdmin bool, org *domain.Org, accountID, ownerFilter, search string) ([]domain.SourceRepositoryItem, error) {
	if !s.isOrgAdmin(ctx, org.ID, actorUserID, actorIsGlobalAdmin) {
		return nil, fmt.Errorf("%w: organization administrator role required", domain.ErrForbidden)
	}
	var acct *domain.GitHubAccountSource
	var err error
	if accountID != "" {
		acct, err = s.accountSources.Get(ctx, accountID)
		if err != nil {
			return nil, err
		}
		if acct.OrgID != org.ID {
			return nil, domain.ErrNotFound
		}
	} else {
		accounts, err := s.accountSources.ListByOrg(ctx, org.ID)
		if err != nil {
			return nil, err
		}
		if len(accounts) == 0 {
			return []domain.SourceRepositoryItem{}, nil
		}
		acct = accounts[0]
	}
	var repos []domain.GitHubRemoteRepo
	if err := s.callWithAccountToken(ctx, acct, func(token string) error {
		found, err := s.github.ListAccessibleRepos(ctx, acct.BaseURL, token)
		repos = found
		return err
	}); err != nil {
		return nil, err
	}
	return s.mapSourceRepositories(ctx, org, acct, repos, ownerFilter, search), nil
}

// mapSourceRepositories annotates remote repos with claim state.
func (s *GitHubIntegrationService) mapSourceRepositories(ctx context.Context, org *domain.Org, acct *domain.GitHubAccountSource, repos []domain.GitHubRemoteRepo, ownerFilter, search string) []domain.SourceRepositoryItem {
	out := make([]domain.SourceRepositoryItem, 0, len(repos))
	search = strings.ToLower(strings.TrimSpace(search))
	ownerFilter = strings.ToLower(strings.TrimSpace(ownerFilter))
	if ownerFilter == "" && acct.Owner != "" {
		ownerFilter = strings.ToLower(acct.Owner)
	}
	for _, repo := range repos {
		owner, name := splitOwnerRepo(repo.FullName)
		if owner == "" {
			owner, name = repo.Owner, repo.Name
		}
		if ownerFilter != "" && !strings.EqualFold(owner, ownerFilter) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(repo.FullName), search) && !strings.Contains(strings.ToLower(name), search) {
			continue
		}
		canonical := domain.CanonicalForgeRepo(domain.ForgeGitHub, acct.BaseURL, fmt.Sprintf("%s/%s", owner, name))
		item := domain.SourceRepositoryItem{
			Repository: canonical, FullName: fmt.Sprintf("%s/%s", owner, name), Private: repo.Private,
			Archived: repo.Archived, AdminPerm: repo.AdminPerm, Eligible: true,
		}
		if setting, err := s.repoSettings.GetByCanonicalRepo(ctx, canonical); err == nil {
			item.Selected = setting.OrgID == org.ID
			if setting.OrgID != org.ID {
				item.Eligible = false
				if other, err := s.orgs.Get(ctx, setting.OrgID); err == nil {
					name := other.Name
					item.ClaimedByOrgName = &name
				}
			}
		}
		out = append(out, item)
	}
	return out
}

// UpdateOrgRepoSettings materializes per-repo ForgeSources + webhooks and
// persists trigger flags.
func (s *GitHubIntegrationService) UpdateOrgRepoSettings(ctx context.Context, actorUserID string, actorIsGlobalAdmin bool, org *domain.Org, settings []domain.RepoSettingMutation) error {
	if !s.isOrgAdmin(ctx, org.ID, actorUserID, actorIsGlobalAdmin) {
		return fmt.Errorf("%w: organization administrator role required", domain.ErrForbidden)
	}
	if len(settings) == 0 || len(settings) > maxGitHubRepoMutations {
		return fmt.Errorf("%w: between 1 and %d repository settings are required", domain.ErrValidation, maxGitHubRepoMutations)
	}
	for _, in := range settings {
		if err := s.applyRepoSetting(ctx, actorUserID, org, in); err != nil {
			return err
		}
	}
	return nil
}

//nolint:gocyclo // materialization + rollback branches
func (s *GitHubIntegrationService) applyRepoSetting(ctx context.Context, actorUserID string, org *domain.Org, in domain.RepoSettingMutation) error {
	if strings.TrimSpace(in.Repo) == "" {
		return fmt.Errorf("%w: repo is required", domain.ErrValidation)
	}
	acct, legacy, owner, name, canonical, err := s.resolveRepoAccount(ctx, org, in)
	if err != nil {
		return err
	}
	existing, existingErr := s.repoSettings.Get(ctx, org.ID, canonical)
	if existingErr != nil && !errors.Is(existingErr, domain.ErrNotFound) {
		return existingErr
	}
	if existing != nil {
		setting := *existing
		applyTriggers(&setting, in, false)
		return s.repoSettings.BatchUpsert(ctx, org.ID, []domain.RepoSettingWrite{{Setting: &setting}})
	}
	if legacy != nil {
		setting := &domain.RepoSetting{
			OrgID: org.ID, Repo: canonical, Provider: domain.ForgeGitHub,
			SourceID: firstNonEmptyString(accountIDOrEmpty(acct), legacy.ID), ForgeSourceID: legacy.ID,
			IsPublic: in.IsPublic != nil && *in.IsPublic, ClaimedByUserID: actorUserID,
		}
		applyTriggers(setting, in, true)
		return s.repoSettings.BatchUpsert(ctx, org.ID, []domain.RepoSettingWrite{{Setting: setting}})
	}
	if acct == nil {
		return fmt.Errorf("repository %s %w", in.Repo, domain.ErrRepoNotInInstallation)
	}
	if !strings.EqualFold(acct.Owner, owner) {
		return fmt.Errorf("repository %s %w", in.Repo, domain.ErrRepoNotInInstallation)
	}
	var (
		remote *domain.GitHubRemoteRepo
		token  string
	)
	if err := s.callWithAccountToken(ctx, acct, func(callToken string) error {
		token = callToken
		found, err := s.github.GetRepository(ctx, acct.BaseURL, callToken, owner, name)
		remote = found
		return err
	}); err != nil {
		if passthroughAccountError(err) {
			return err
		}
		return fmt.Errorf("repository %s is not available through this GitHub connection: %w", canonical, domain.ErrRepoInaccessible)
	}
	forgeID := domain.MaterializedForgeSourceID(org.ID, canonical)
	webhookSecret := randomHex(32)
	tokenCT, err := s.sealer.SealRaw(domain.SecretScopeOrg, org.ID, token)
	if err != nil {
		return err
	}
	secretCT, err := s.sealer.SealRaw(domain.SecretScopeOrg, org.ID, webhookSecret)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	src := &domain.ForgeSource{
		ID: forgeID, OrgID: org.ID, Provider: domain.ForgeGitHub, BaseURL: acct.BaseURL,
		Repo: fmt.Sprintf("%s/%s", owner, name), AccountSourceID: acct.ID,
		TokenCiphertext: tokenCT, WebhookSecretCiphertext: secretCT, CreatedAt: now, UpdatedAt: now,
	}
	if s.forgeProvider != nil {
		callback := fmt.Sprintf("%s/v1/webhooks/github/%s", strings.TrimRight(s.cfg.PublicURL, "/"), forgeID)
		if err := s.forgeProvider.DeleteWebhook(ctx, src, token, ""); err != nil {
			s.logger.WithError(err).WithField("source_id", forgeID).Warn("github webhook pre-clean failed")
		}
		if _, err := s.forgeProvider.RegisterWebhook(ctx, src, token, callback, webhookSecret); err != nil {
			if strings.Contains(err.Error(), "status 401") {
				return fmt.Errorf("%w: github rejected the webhook credential; reconnect the account", domain.ErrGitHubTokenRevoked)
			}
			if strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "status 404") || strings.Contains(strings.ToLower(err.Error()), "archived") {
				return fmt.Errorf("cannot register webhook on %s: repository is not available through this GitHub connection (admin:repo_hook and repository admin permission required): %w", canonical, domain.ErrRepoInaccessible)
			}
			return err
		}
	}
	isPublic := !remote.Private
	if in.IsPublic != nil {
		isPublic = *in.IsPublic
	}
	setting := &domain.RepoSetting{
		OrgID: org.ID, Repo: canonical, Provider: domain.ForgeGitHub, SourceID: acct.ID,
		ForgeSourceID: forgeID, IsPublic: isPublic, ClaimedByUserID: actorUserID,
	}
	applyTriggers(setting, in, true)
	if err := s.repoSettings.BatchUpsert(ctx, org.ID, []domain.RepoSettingWrite{{Setting: setting, ForgeSource: src}}); err != nil {
		if s.forgeProvider != nil {
			_ = s.forgeProvider.DeleteWebhook(ctx, src, token, "")
		}
		return err
	}
	s.logger.WithFields(logrus.Fields{"event": "github_repo_claimed", "org_id": org.ID, "repo": canonical, "actor_user_id": actorUserID}).Info("github repository claimed")
	return nil
}

// resolveRepoAccount finds the target account/legacy source and canonical repo.
//
//nolint:gocyclo,gocritic // source resolution branches
func (s *GitHubIntegrationService) resolveRepoAccount(ctx context.Context, org *domain.Org, in domain.RepoSettingMutation) (*domain.GitHubAccountSource, *domain.ForgeSource, string, string, string, error) {
	owner, name := splitOwnerRepo(in.Repo)
	if owner == "" || name == "" {
		return nil, nil, "", "", "", fmt.Errorf("%w: repo must be owner/name", domain.ErrValidation)
	}
	var acct *domain.GitHubAccountSource
	if in.SourceID != nil && *in.SourceID != "" {
		found, err := s.accountSources.Get(ctx, *in.SourceID)
		if err == nil {
			if found.OrgID != org.ID {
				return nil, nil, "", "", "", domain.ErrNotFound
			}
			acct = found
		} else if !errors.Is(err, domain.ErrNotFound) {
			return nil, nil, "", "", "", err
		} else {
			legacy, err := s.forgeSources.Get(ctx, *in.SourceID)
			if err != nil || !sameRepo(legacy.Repo, in.Repo) {
				return nil, nil, "", "", "", fmt.Errorf("repository %s %w", in.Repo, domain.ErrRepoNotInInstallation)
			}
			if legacy.OrgID != org.ID {
				return nil, nil, "", "", "", domain.ErrNotFound
			}
			canonical := domain.CanonicalForgeRepo(legacy.Provider, legacy.BaseURL, legacy.Repo)
			return nil, legacy, owner, name, canonical, nil
		}
	}
	if acct == nil {
		found, err := s.accountSources.GetByOrgAndOwner(ctx, org.ID, owner)
		if err == nil {
			acct = found
		} else if !errors.Is(err, domain.ErrNotFound) {
			return nil, nil, "", "", "", err
		}
	}
	// Lazy-migrate an existing standalone forge source for the same repo.
	sources, err := s.forgeSources.ListByOrg(ctx, org.ID)
	if err != nil {
		return nil, nil, "", "", "", err
	}
	for _, src := range sources {
		if src.Provider != domain.ForgeGitHub || !sameRepo(src.Repo, in.Repo) {
			continue
		}
		if acct == nil {
			canonical := domain.CanonicalForgeRepo(src.Provider, src.BaseURL, src.Repo)
			return nil, src, owner, name, canonical, nil
		}
		break
	}
	base := s.cfg.DefaultAPIURL
	if acct != nil {
		base = acct.BaseURL
	}
	canonical := domain.CanonicalForgeRepo(domain.ForgeGitHub, base, fmt.Sprintf("%s/%s", owner, name))
	if acct == nil && (in.SourceID == nil || *in.SourceID == "") {
		return nil, nil, "", "", "", fmt.Errorf("repository %s %w", in.Repo, domain.ErrRepoNotInInstallation)
	}
	return acct, nil, owner, name, canonical, nil
}

// DeleteOrgRepoSetting releases a claim and best-effort deletes its webhook.
func (s *GitHubIntegrationService) DeleteOrgRepoSetting(ctx context.Context, actorUserID string, actorIsGlobalAdmin bool, org *domain.Org, repo string) error {
	if !s.isOrgAdmin(ctx, org.ID, actorUserID, actorIsGlobalAdmin) {
		return fmt.Errorf("%w: organization administrator role required", domain.ErrForbidden)
	}
	if setting, err := s.findRepoSetting(ctx, org.ID, repo); err == nil {
		s.deleteRemoteWebhook(ctx, org.ID, setting.ForgeSourceID)
		return s.repoSettings.Delete(ctx, org.ID, setting.Repo, setting.ForgeSourceID)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	sources, err := s.forgeSources.ListByOrg(ctx, org.ID)
	if err != nil {
		return err
	}
	for _, src := range sources {
		if src.Provider != domain.ForgeGitHub || !sameRepo(src.Repo, repo) {
			continue
		}
		s.deleteRemoteWebhook(ctx, org.ID, src.ID)
		return s.forgeSources.Delete(ctx, src.ID)
	}
	return nil
}

// GetRepoSetting returns one org repo setting.
func (s *GitHubIntegrationService) GetRepoSetting(ctx context.Context, orgID, repo string) (*domain.RepoSetting, error) {
	return s.findRepoSetting(ctx, orgID, repo)
}

// findRepoSetting resolves a setting by canonical form or by matching the
// host-stripped owner/name tail (covers GHES/custom API hosts and legacy rows).
func (s *GitHubIntegrationService) findRepoSetting(ctx context.Context, orgID, repo string) (*domain.RepoSetting, error) {
	setting, err := s.repoSettings.Get(ctx, orgID, s.canonicalRepo(repo, ""))
	if err == nil {
		return setting, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	rows, err := s.repoSettings.ListByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, candidate := range rows {
		if sameRepo(candidate.Repo, repo) {
			return candidate, nil
		}
	}
	return nil, domain.ErrNotFound
}

// deleteRemoteWebhook best-effort deletes the remote hook for a forge source.
func (s *GitHubIntegrationService) deleteRemoteWebhook(ctx context.Context, orgID, forgeSourceID string) {
	if s.forgeProvider == nil || forgeSourceID == "" {
		return
	}
	src, err := s.forgeSources.Get(ctx, forgeSourceID)
	if err != nil {
		return
	}
	token, err := s.sealer.OpenRaw(domain.SecretScopeOrg, orgID, src.TokenCiphertext)
	if err != nil {
		s.logger.WithError(err).WithField("source_id", forgeSourceID).Warn("github webhook cleanup skipped")
		return
	}
	if err := s.forgeProvider.DeleteWebhook(ctx, src, token, ""); err != nil {
		s.logger.WithError(err).WithField("source_id", forgeSourceID).Warn("github webhook cleanup failed")
	}
}

// DeviceSessionInterval returns the current poll interval for the caller's
// device session (used to surface slow_down back to the UI).
func (s *GitHubIntegrationService) DeviceSessionInterval(userID, sessionID string) int {
	session, err := s.deviceSessions.Get(sessionID)
	if err != nil || subtle.ConstantTimeCompare([]byte(userID), []byte(session.UserID)) != 1 {
		return 0
	}
	return session.Interval
}

// canonicalRepo canonicalizes a repo slug using baseURL (or the default API
// host when empty).
func (s *GitHubIntegrationService) canonicalRepo(repo, baseURL string) string {
	if baseURL == "" {
		baseURL = s.cfg.DefaultAPIURL
	}
	return domain.CanonicalForgeRepo(domain.ForgeGitHub, baseURL, repoTail(repo))
}

// markUserRevoked persists a revoked status so githubConnection reports null
// and the UI prompts re-authorization.
func (s *GitHubIntegrationService) markUserRevoked(ctx context.Context, userID string) {
	conn, err := s.connections.GetByUser(ctx, userID)
	if err != nil {
		return
	}
	conn.Status = domain.GitHubConnectionRevoked
	conn.UpdatedAt = time.Now().UTC()
	if err := s.connections.Upsert(ctx, conn); err != nil {
		s.logger.WithError(err).WithField("user_id", userID).Warn("mark github connection revoked failed")
	}
}

// markAccountRevoked persists a revoked org-account status.
func (s *GitHubIntegrationService) markAccountRevoked(ctx context.Context, acct *domain.GitHubAccountSource) {
	acct.Status = domain.GitHubConnectionRevoked
	acct.UpdatedAt = time.Now().UTC()
	if err := s.accountSources.Upsert(ctx, acct); err != nil {
		s.logger.WithError(err).WithField("account_id", acct.ID).Warn("mark github account revoked failed")
	}
}

func (s *GitHubIntegrationService) accountToken(acct *domain.GitHubAccountSource) (string, error) {
	scopeID, err := domain.GitHubOrgAccountScopeID(acct.OrgID, acct.ID)
	if err != nil {
		return "", err
	}
	token, err := s.sealer.OpenRaw(domain.SecretScopeGitHubOrgAccount, scopeID, acct.TokenCiphertext)
	if err != nil {
		return "", fmt.Errorf("%w: github account token cannot be opened", domain.ErrGitHubTokenRevoked)
	}
	return token, nil
}

func (s *GitHubIntegrationService) userToken(ctx context.Context, userID string) (*domain.UserGitHubConnection, string, error) {
	conn, err := s.connections.GetByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, "", fmt.Errorf("%w: link a GitHub account first", domain.ErrGitHubNotConnected)
		}
		return nil, "", fmt.Errorf("get github connection: %w", err)
	}
	if conn.Status != domain.GitHubConnectionActive {
		return nil, "", fmt.Errorf("%w: github connection token is expired or revoked", domain.ErrGitHubTokenRevoked)
	}
	token, err := s.sealer.OpenRaw(domain.SecretScopeGitHubUser, userID, conn.TokenCiphertext)
	if err != nil {
		return nil, "", fmt.Errorf("%w: github connection token cannot be opened", domain.ErrGitHubTokenRevoked)
	}
	return conn, token, nil
}

// githubCredential is the plaintext token material linked to a user connection
// or an org account source. Tokens are never logged.
type githubCredential struct {
	AccessToken      string
	RefreshToken     string
	TokenExpiresAt   time.Time
	RefreshExpiresAt time.Time
	Scopes           []string
}

// sealedTokenPair is a githubCredential after scope-bound sealing.
type sealedTokenPair struct {
	access     string
	refresh    string
	accessExp  time.Time
	refreshExp time.Time
}

// credentialFromTokenResult turns a GitHub token response into absolute expiry
// metadata. GitHub omits expires_in / refresh_token_expires_in for a
// non-expiring app, which leaves zero expiries (= never refresh).
func credentialFromTokenResult(res *domain.GitHubTokenResult, now time.Time) githubCredential {
	cred := githubCredential{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		Scopes:       append([]string(nil), res.Scopes...),
	}
	if res.ExpiresIn > 0 {
		cred.TokenExpiresAt = now.Add(time.Duration(res.ExpiresIn) * time.Second)
	}
	if res.RefreshTokenExpiresIn > 0 {
		cred.RefreshExpiresAt = now.Add(time.Duration(res.RefreshTokenExpiresIn) * time.Second)
	}
	return cred
}

// credentialFromUserConnection copies a user connection's token material for
// re-sealing under an org-account scope. When the sealed refresh token cannot
// be opened the account degrades to an access-token-only credential (like a
// PAT) instead of failing the link.
func (s *GitHubIntegrationService) credentialFromUserConnection(conn *domain.UserGitHubConnection, accessToken string, scopes []string) githubCredential {
	cred := githubCredential{
		AccessToken:      accessToken,
		TokenExpiresAt:   conn.TokenExpiresAt,
		RefreshExpiresAt: conn.RefreshExpiresAt,
		Scopes:           scopes,
	}
	if conn.RefreshTokenCiphertext == "" {
		return cred
	}
	refresh, err := s.sealer.OpenRaw(domain.SecretScopeGitHubUser, conn.UserID, conn.RefreshTokenCiphertext)
	if err != nil {
		s.logger.WithError(err).WithField("user_id", conn.UserID).Warn("github refresh token cannot be opened for org linking")
		cred.RefreshExpiresAt = time.Time{}
		return cred
	}
	cred.RefreshToken = refresh
	return cred
}

// sealCredential seals the access token and, when present, the refresh token
// under scope/scopeID.
func (s *GitHubIntegrationService) sealCredential(scope domain.SecretScope, scopeID string, cred *githubCredential) (sealedTokenPair, error) {
	access, err := s.sealer.SealRaw(scope, scopeID, cred.AccessToken)
	if err != nil {
		return sealedTokenPair{}, err
	}
	pair := sealedTokenPair{access: access, accessExp: cred.TokenExpiresAt}
	if cred.RefreshToken == "" {
		return pair, nil
	}
	refresh, err := s.sealer.SealRaw(scope, scopeID, cred.RefreshToken)
	if err != nil {
		return sealedTokenPair{}, err
	}
	pair.refresh, pair.refreshExp = refresh, cred.RefreshExpiresAt
	return pair, nil
}

// refreshDue reports whether an access token expiring at expiresAt is inside
// the proactive-refresh window. A zero expiry never refreshes.
func refreshDue(expiresAt, now time.Time, skew time.Duration) bool {
	return !expiresAt.IsZero() && !now.Before(expiresAt.Add(-skew))
}

// refreshTokenExpired reports whether the refresh token itself is past its
// lifetime (GitHub returns it as refresh_token_expires_in).
func refreshTokenExpired(refreshExpiresAt, now time.Time) bool {
	return !refreshExpiresAt.IsZero() && !now.Before(refreshExpiresAt)
}

// refreshable reports whether a sealed credential can be rotated at all.
func (s *GitHubIntegrationService) refreshable(refreshCiphertext string) bool {
	return s.cfg.RefreshEnabled && refreshCiphertext != ""
}

func (s *GitHubIntegrationService) isLeader() bool {
	return s.leaderFn == nil || s.leaderFn()
}

// recordRefresh increments the refresh counter (nil-safe).
func (s *GitHubIntegrationService) recordRefresh(scope, result string) {
	if s.metrics == nil {
		return
	}
	s.metrics.GitHubTokenRefreshTotal.WithLabelValues(scope, result).Inc()
}

// githubRefreshGate is a per-scope single-flight gate: the first caller rotates
// a scope while concurrent callers wait for it and then reload the persisted
// record, so a burst of requests performs exactly one RefreshToken call.
type githubRefreshGate struct {
	mu    sync.Mutex
	calls map[string]*githubRefreshCall
}

type githubRefreshCall struct {
	done chan struct{}
	err  error
}

func (g *githubRefreshGate) do(ctx context.Context, scope string, fn func(context.Context) error) error {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*githubRefreshCall{}
	}
	if inFlight, ok := g.calls[scope]; ok {
		g.mu.Unlock()
		select {
		case <-inFlight.done:
			return inFlight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &githubRefreshCall{done: make(chan struct{})}
	g.calls[scope] = call
	g.mu.Unlock()

	err := fn(ctx)
	call.err = err
	g.mu.Lock()
	delete(g.calls, scope)
	g.mu.Unlock()
	close(call.done)
	return err
}

// resolveUserToken loads the caller's connection and returns an active access
// token, rotating it first when it is within RefreshSkew of expiry. A follower
// never rotates: it serves the still-valid token and only reports not_leader on
// the reactive (401) path.
func (s *GitHubIntegrationService) resolveUserToken(ctx context.Context, userID string) (*domain.UserGitHubConnection, string, error) {
	conn, token, err := s.userToken(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	if !s.refreshable(conn.RefreshTokenCiphertext) || !refreshDue(conn.TokenExpiresAt, time.Now().UTC(), s.cfg.RefreshSkew) || !s.isLeader() {
		return conn, token, nil
	}
	return s.refreshUserToken(ctx, conn)
}

// resolveAccountToken returns an active access token for an org account source,
// rotating it first when it is within RefreshSkew of expiry.
func (s *GitHubIntegrationService) resolveAccountToken(ctx context.Context, acct *domain.GitHubAccountSource) (string, error) {
	latest, err := s.accountSources.Get(ctx, acct.ID)
	if err != nil {
		return "", err
	}
	if latest.Status != domain.GitHubConnectionActive {
		return "", fmt.Errorf("%w: github account token is expired or revoked", domain.ErrGitHubTokenRevoked)
	}
	token, err := s.accountToken(latest)
	if err != nil {
		return "", err
	}
	if !s.refreshable(latest.RefreshTokenCiphertext) || !refreshDue(latest.TokenExpiresAt, time.Now().UTC(), s.cfg.RefreshSkew) || !s.isLeader() {
		return token, nil
	}
	return s.refreshAccountToken(ctx, latest)
}

// callWithUserToken runs fn with the caller's active token and, on a GitHub 401
// for a refreshable credential, performs ONE leader-gated rotation and retries.
func (s *GitHubIntegrationService) callWithUserToken(ctx context.Context, userID string, fn func(conn *domain.UserGitHubConnection, token string) error) error {
	conn, token, err := s.resolveUserToken(ctx, userID)
	if err != nil {
		return err
	}
	err = fn(conn, token)
	if err == nil || !errors.Is(err, domain.ErrGitHubTokenRevoked) {
		return err
	}
	if !s.refreshable(conn.RefreshTokenCiphertext) {
		s.markUserRevoked(ctx, userID)
		return err
	}
	if !s.isLeader() {
		return s.notLeaderRefresh(githubScopeUser, userID)
	}
	conn, token, err = s.refreshUserToken(ctx, conn)
	if err != nil {
		return err
	}
	return fn(conn, token)
}

// callWithAccountToken is callWithUserToken for an org account source.
func (s *GitHubIntegrationService) callWithAccountToken(ctx context.Context, acct *domain.GitHubAccountSource, fn func(token string) error) error {
	token, err := s.resolveAccountToken(ctx, acct)
	if err != nil {
		return err
	}
	err = fn(token)
	if err == nil || !errors.Is(err, domain.ErrGitHubTokenRevoked) {
		return err
	}
	latest, loadErr := s.accountSources.Get(ctx, acct.ID)
	if loadErr != nil {
		return loadErr
	}
	if !s.refreshable(latest.RefreshTokenCiphertext) {
		s.markAccountRevoked(ctx, latest)
		return err
	}
	if !s.isLeader() {
		return s.notLeaderRefresh(githubScopeOrgAccount, acct.ID)
	}
	token, err = s.refreshAccountToken(ctx, latest)
	if err != nil {
		return err
	}
	return fn(token)
}

// refreshUserToken rotates the credential observed on conn (leader-only,
// single-flight) and returns the freshly persisted token. Concurrent callers
// share one rotation; a rotation already applied by someone else surfaces as
// ErrConflict from the gate and is served by reloading.
func (s *GitHubIntegrationService) refreshUserToken(ctx context.Context, conn *domain.UserGitHubConnection) (*domain.UserGitHubConnection, string, error) {
	err := s.refreshes.do(ctx, fmt.Sprintf("%s:%s", githubScopeUser, conn.UserID), func(ctx context.Context) error {
		return s.rotateUserToken(ctx, conn)
	})
	if err != nil && !errors.Is(err, domain.ErrConflict) {
		return nil, "", err
	}
	return s.userToken(ctx, conn.UserID)
}

// refreshAccountToken is refreshUserToken for an org account source.
func (s *GitHubIntegrationService) refreshAccountToken(ctx context.Context, acct *domain.GitHubAccountSource) (string, error) {
	err := s.refreshes.do(ctx, fmt.Sprintf("%s:%s", githubScopeOrgAccount, acct.ID), func(ctx context.Context) error {
		return s.rotateAccountToken(ctx, acct)
	})
	if err != nil && !errors.Is(err, domain.ErrConflict) {
		return "", err
	}
	latest, err := s.accountSources.Get(ctx, acct.ID)
	if err != nil {
		return "", err
	}
	return s.accountToken(latest)
}

// rotateUserToken performs one rotation of the credential observed on observed.
// A credential already replaced (concurrent rotation or re-auth) is reported as
// ErrConflict so the caller reloads instead of rotating twice.
func (s *GitHubIntegrationService) rotateUserToken(ctx context.Context, observed *domain.UserGitHubConnection) error {
	userID := observed.UserID
	conn, err := s.connections.GetByUser(ctx, userID)
	if err != nil {
		return err
	}
	if conn.Status != domain.GitHubConnectionActive {
		return fmt.Errorf("%w: github connection token is expired or revoked", domain.ErrGitHubTokenRevoked)
	}
	if !s.refreshable(conn.RefreshTokenCiphertext) {
		return nil
	}
	if conn.RefreshTokenCiphertext != observed.RefreshTokenCiphertext {
		return fmt.Errorf("github connection %s: %w", userID, domain.ErrConflict)
	}
	now := time.Now().UTC()
	if refreshTokenExpired(conn.RefreshExpiresAt, now) {
		return s.revokeByRefresh(githubScopeUser, userID, "refresh_token_expired", func() { s.markUserRevoked(ctx, userID) })
	}
	plaintext, err := s.sealer.OpenRaw(domain.SecretScopeGitHubUser, userID, conn.RefreshTokenCiphertext)
	if err != nil {
		return s.revokeByRefresh(githubScopeUser, userID, "unseal_failed", func() { s.markUserRevoked(ctx, userID) })
	}
	cred, err := s.rotateToken(ctx, plaintext)
	if err != nil {
		return s.refreshError(githubScopeUser, userID, err, func() { s.markUserRevoked(ctx, userID) })
	}
	pair, err := s.sealCredential(domain.SecretScopeGitHubUser, userID, &cred)
	if err != nil {
		return fmt.Errorf("seal rotated github user token: %w", err)
	}
	expected := conn.TokenVersion
	conn.TokenCiphertext = pair.access
	conn.RefreshTokenCiphertext = pair.refresh
	conn.TokenExpiresAt = pair.accessExp
	conn.RefreshExpiresAt = pair.refreshExp
	conn.TokenVersion = expected + 1
	if len(cred.Scopes) > 0 {
		conn.Scopes = cred.Scopes
	}
	return s.persistRotation(ctx, githubScopeUser, userID, conn.TokenVersion, func() error {
		return s.connections.UpsertIfVersion(ctx, conn, expected)
	})
}

// rotateAccountToken performs one rotation of the credential observed on acct
// and re-seals its per-repo forge sources. A credential already replaced is
// reported as ErrConflict (see rotateUserToken).
func (s *GitHubIntegrationService) rotateAccountToken(ctx context.Context, acct *domain.GitHubAccountSource) error {
	current, err := s.accountSources.Get(ctx, acct.ID)
	if err != nil {
		return err
	}
	if current.Status != domain.GitHubConnectionActive {
		return fmt.Errorf("%w: github account token is expired or revoked", domain.ErrGitHubTokenRevoked)
	}
	if !s.refreshable(current.RefreshTokenCiphertext) {
		return nil
	}
	if current.RefreshTokenCiphertext != acct.RefreshTokenCiphertext {
		return fmt.Errorf("github account source %s: %w", acct.ID, domain.ErrConflict)
	}
	scopeID, err := domain.GitHubOrgAccountScopeID(current.OrgID, current.ID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if refreshTokenExpired(current.RefreshExpiresAt, now) {
		return s.revokeByRefresh(githubScopeOrgAccount, current.ID, "refresh_token_expired", func() { s.markAccountRevoked(ctx, current) })
	}
	plaintext, err := s.sealer.OpenRaw(domain.SecretScopeGitHubOrgAccount, scopeID, current.RefreshTokenCiphertext)
	if err != nil {
		return s.revokeByRefresh(githubScopeOrgAccount, current.ID, "unseal_failed", func() { s.markAccountRevoked(ctx, current) })
	}
	cred, err := s.rotateToken(ctx, plaintext)
	if err != nil {
		return s.refreshError(githubScopeOrgAccount, current.ID, err, func() { s.markAccountRevoked(ctx, current) })
	}
	pair, err := s.sealCredential(domain.SecretScopeGitHubOrgAccount, scopeID, &cred)
	if err != nil {
		return fmt.Errorf("seal rotated github account token: %w", err)
	}
	expected := current.TokenVersion
	current.TokenCiphertext = pair.access
	current.RefreshTokenCiphertext = pair.refresh
	current.TokenExpiresAt = pair.accessExp
	current.RefreshExpiresAt = pair.refreshExp
	current.TokenVersion = expected + 1
	if len(cred.Scopes) > 0 {
		current.Scopes = cred.Scopes
	}
	if err := s.persistRotation(ctx, githubScopeOrgAccount, current.ID, current.TokenVersion, func() error {
		return s.accountSources.UpsertIfVersion(ctx, current, expected)
	}); err != nil {
		return err
	}
	if err := s.reSealAccountForgeSources(ctx, current.OrgID, current.ID, cred.AccessToken); err != nil {
		s.logger.WithError(err).WithFields(logrus.Fields{
			"event": "github_refresh_reseal_failed", "scope": githubScopeOrgAccount, "scope_id": current.ID,
		}).Warn("re-seal github forge sources after token rotation failed")
	}
	return nil
}

// rotateToken exchanges a plaintext refresh token for a fresh token pair.
func (s *GitHubIntegrationService) rotateToken(ctx context.Context, refreshToken string) (githubCredential, error) {
	res, err := s.github.RefreshToken(ctx, refreshToken)
	if err != nil {
		return githubCredential{}, err
	}
	if res == nil || res.AccessToken == "" {
		return githubCredential{}, fmt.Errorf("%w: github token endpoint returned no access token", domain.ErrGitHubRefreshUnavailable)
	}
	res.Normalize()
	return credentialFromTokenResult(res, time.Now().UTC()), nil
}

// persistRotation persists a rotated credential with a bounded retry. A stale
// version (ErrConflict) is returned so the caller reloads the winner; any other
// persistent failure is logged and surfaced as transient (never revokes).
func (s *GitHubIntegrationService) persistRotation(ctx context.Context, scope, scopeID string, tokenVersion uint64, persist func() error) error {
	backoff := githubRefreshPersistBackoff
	var err error
	for attempt := 0; attempt < githubRefreshPersistAttempts; attempt++ {
		if err = persist(); err == nil {
			s.recordRefresh(scope, "success")
			s.logger.WithFields(logrus.Fields{
				"event": "github_token_refreshed", "scope": scope, "scope_id": scopeID, "token_version": tokenVersion,
			}).Info("github token refreshed")
			return nil
		}
		if !retryableRefreshPersist(err) {
			return err
		}
		if attempt < githubRefreshPersistAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	s.logger.WithError(err).WithFields(logrus.Fields{
		"event": "github_refresh_persist_failed", "scope": scope, "scope_id": scopeID,
	}).Error("rotated github token could not be persisted")
	return fmt.Errorf("%w: persist rotated github token: %w", domain.ErrGitHubRefreshUnavailable, err)
}

// retryableRefreshPersist reports whether a failed Raft apply may succeed on a
// retry (leadership transfer / apply timeout). A stale version is final.
func retryableRefreshPersist(err error) bool {
	if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
		return false
	}
	return errors.Is(err, domain.ErrNotLeader) || errors.Is(err, domain.ErrRaftTimeout)
}

// refreshError converts a failed rotation into the caller-visible error: a
// rejected refresh token revokes the credential, every other failure is
// transient and must never revoke.
func (s *GitHubIntegrationService) refreshError(scope, scopeID string, err error, revoke func()) error {
	if errors.Is(err, domain.ErrGitHubRefreshInvalid) {
		return s.revokeByRefresh(scope, scopeID, "bad_refresh_token", revoke)
	}
	reason := "transient"
	switch {
	case errors.Is(err, domain.ErrNotLeader):
		reason = "not_leader"
	case errors.Is(err, domain.ErrRaftTimeout):
		reason = "raft_timeout"
	}
	s.recordRefresh(scope, reason)
	s.logger.WithError(err).WithFields(logrus.Fields{
		"event": "github_token_refresh_failed", "scope": scope, "scope_id": scopeID, "reason": reason,
	}).Warn("github token refresh failed")
	return fmt.Errorf("%w: %w", domain.ErrGitHubRefreshUnavailable, err)
}

// notLeaderRefresh reports a follower that cannot rotate a credential it could
// not persist (mirrors the OIDC errOAuthNotLeaderRefresh rule, issue #61).
func (s *GitHubIntegrationService) notLeaderRefresh(scope, scopeID string) error {
	s.recordRefresh(scope, "not_leader")
	s.logger.WithFields(logrus.Fields{
		"event": "github_token_refresh_failed", "scope": scope, "scope_id": scopeID, "reason": "not_leader",
	}).Warn("github token refresh requires the raft leader")
	return fmt.Errorf("%w: github token refresh requires the raft leader", domain.ErrNotLeader)
}

// revokeByRefresh marks a credential revoked because its refresh token can no
// longer be used (rejected by GitHub, already expired, or unopenable).
func (s *GitHubIntegrationService) revokeByRefresh(scope, scopeID, reason string, revoke func()) error {
	revoke()
	s.recordRefresh(scope, "bad_refresh_token")
	s.logger.WithFields(logrus.Fields{
		"event": "github_connection_revoked_by_refresh", "scope": scope, "scope_id": scopeID, "reason": reason,
	}).Warn("github connection revoked: refresh token unusable")
	return fmt.Errorf("%w: the github refresh token is no longer usable; reconnect the account", domain.ErrGitHubTokenRevoked)
}

// passthroughAccountError reports whether a GitHub call failure must be
// surfaced as-is instead of being masked as repository inaccessibility.
func passthroughAccountError(err error) bool {
	for _, sentinel := range []error{
		domain.ErrGitHubTokenRevoked, domain.ErrGitHubRateLimited, domain.ErrValidation,
		domain.ErrNotLeader, domain.ErrRaftTimeout, domain.ErrGitHubRefreshUnavailable,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

func (s *GitHubIntegrationService) requireHuman(ctx context.Context, userID string) error {
	if userID == "" {
		return domain.ErrUnauthenticated
	}
	u, err := s.users.Get(ctx, userID)
	if err != nil {
		return domain.ErrUnauthenticated
	}
	if u.AccessBlocked() {
		return fmt.Errorf("%w: account access is disabled", domain.ErrForbidden)
	}
	return nil
}

func (s *GitHubIntegrationService) isOrgAdmin(ctx context.Context, orgID, userID string, globalAdmin bool) bool {
	if globalAdmin {
		return true
	}
	if s.orgs == nil || userID == "" || orgID == "" {
		return false
	}
	return s.orgs.IsOrgAdmin(ctx, orgID, userID)
}

func (s *GitHubIntegrationService) adminOrgs(ctx context.Context, userID string, globalAdmin bool) []GitHubAdminOrg {
	if s.orgs == nil {
		return []GitHubAdminOrg{}
	}
	var (
		orgs []*domain.Org
		err  error
	)
	if globalAdmin {
		orgs, err = s.orgs.List(ctx)
	} else {
		orgs, err = s.orgs.OrgsForUser(ctx, userID)
	}
	if err != nil {
		return []GitHubAdminOrg{}
	}
	out := make([]GitHubAdminOrg, 0)
	for _, o := range orgs {
		if globalAdmin || s.orgs.IsOrgAdmin(ctx, o.ID, userID) {
			out = append(out, GitHubAdminOrg{ID: o.ID, Name: o.Name})
		}
	}
	return out
}

func (s *GitHubIntegrationService) validateRedirectURI(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return domain.ErrInvalidRedirectURI
	}
	if u.Path != "/github/callback" || strings.Contains(raw, "\\") || strings.HasPrefix(raw, "//") {
		return domain.ErrInvalidRedirectURI
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return domain.ErrInvalidRedirectURI
	}
	if u.Scheme == "http" && !isLoopbackURLHost(u.Hostname()) && strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.cfg.PublicURL)), "https://") {
		return domain.ErrInvalidRedirectURI
	}
	allowed := make([]string, 0, len(s.cfg.AllowedRedirectURIs)+len(s.cfg.CORSAllowedOrigins)+1)
	if origin := urlOrigin(s.cfg.PublicURL); origin != "" {
		allowed = append(allowed, fmt.Sprintf("%s/github/callback", origin))
	}
	for _, origin := range s.cfg.CORSAllowedOrigins {
		if o := urlOrigin(origin); o != "" {
			allowed = append(allowed, fmt.Sprintf("%s/github/callback", o))
		}
	}
	allowed = append(allowed, s.cfg.AllowedRedirectURIs...)
	for _, candidate := range allowed {
		if sameURL(candidate, raw) {
			return nil
		}
	}
	return domain.ErrInvalidRedirectURI
}

func (s *GitHubIntegrationService) validateAPIBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		raw = s.cfg.DefaultAPIURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: invalid github api base url", domain.ErrValidation)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackURLHost(u.Hostname())) {
		return "", fmt.Errorf("%w: github api base url must use https", domain.ErrValidation)
	}
	candidates := append([]string{s.cfg.DefaultAPIURL}, s.cfg.AllowedAPIURLs...)
	for _, candidate := range candidates {
		if sameURL(strings.TrimRight(candidate, "/"), raw) {
			return raw, nil
		}
	}
	return "", fmt.Errorf("%w: github api base url is not allowed", domain.ErrValidation)
}

func validatePAT(pat string) error {
	if pat == "" || len(pat) > 512 {
		return fmt.Errorf("%w: github token must be 1..512 characters", domain.ErrValidation)
	}
	for _, r := range pat {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("%w: github token contains invalid characters", domain.ErrValidation)
		}
	}
	return nil
}

func validCode(code string) bool {
	if code == "" || len(code) > 512 {
		return false
	}
	for _, r := range code {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func validStateID(stateID string) bool {
	if len(stateID) != 64 {
		return false
	}
	_, err := hex.DecodeString(stateID)
	return err == nil
}

func splitOwnerRepo(repo string) (owner, name string) {
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	if len(parts) < 2 {
		return "", parts[0]
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

func repoTail(repo string) string {
	owner, name := splitOwnerRepo(repo)
	if owner == "" {
		return name
	}
	return fmt.Sprintf("%s/%s", owner, name)
}

func sameRepo(a, b string) bool {
	ao, an := splitOwnerRepo(a)
	bo, bn := splitOwnerRepo(b)
	return strings.EqualFold(ao, bo) && strings.EqualFold(an, bn)
}

func applyTriggers(setting *domain.RepoSetting, in domain.RepoSettingMutation, materialize bool) {
	if in.IsPublic != nil {
		setting.IsPublic = *in.IsPublic
	}
	if in.RunChecksOnDefaultBranch != nil {
		setting.RunChecksOnDefaultBranch = *in.RunChecksOnDefaultBranch
	} else if materialize {
		setting.RunChecksOnDefaultBranch = true
	}
	if in.RunChecksOnPullRequest != nil {
		setting.RunChecksOnPullRequest = *in.RunChecksOnPullRequest
	} else if materialize {
		setting.RunChecksOnPullRequest = true
	}
	if in.RunChecksOnBranchPush != nil {
		setting.RunChecksOnBranchPush = *in.RunChecksOnBranchPush
	}
}

func dedupeOwners(profile *domain.GitHubUserProfile, orgs []domain.GitHubOwner) []domain.GitHubOwner {
	out := []domain.GitHubOwner{}
	seen := map[string]bool{}
	if profile != nil && profile.Login != "" {
		out = append(out, domain.GitHubOwner{Login: profile.Login, Type: firstNonEmptyString(profile.Type, "User"), AvatarURL: profile.AvatarURL})
		seen[strings.ToLower(profile.Login)] = true
	}
	for _, org := range orgs {
		key := strings.ToLower(org.Login)
		if org.Login == "" || seen[key] {
			continue
		}
		seen[key] = true
		org.Type = firstNonEmptyString(org.Type, "Organization")
		out = append(out, org)
	}
	return out
}

func githubOwnerAllowed(owner string, profile *domain.GitHubUserProfile, orgs []domain.GitHubOwner) bool {
	if profile != nil && strings.EqualFold(owner, profile.Login) {
		return true
	}
	for _, org := range orgs {
		if strings.EqualFold(owner, org.Login) {
			return true
		}
	}
	return false
}

func ownerType(owner string, profile *domain.GitHubUserProfile, orgs []domain.GitHubOwner) string {
	if profile != nil && strings.EqualFold(owner, profile.Login) {
		return firstNonEmptyString(profile.Type, "User")
	}
	for _, org := range orgs {
		if strings.EqualFold(owner, org.Login) {
			return "Organization"
		}
	}
	return "User"
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func accountIDOrEmpty(acct *domain.GitHubAccountSource) string {
	if acct == nil {
		return ""
	}
	return acct.ID
}

func urlOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}

func sameURL(a, b string) bool {
	ua, err := url.Parse(strings.TrimRight(strings.TrimSpace(a), "/"))
	if err != nil {
		return false
	}
	ub, err := url.Parse(strings.TrimRight(strings.TrimSpace(b), "/"))
	if err != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) && ua.EscapedPath() == ub.EscapedPath()
}

func isLoopbackURLHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// MemoryGitHubDeviceSessionStore is the leader-local bounded device-session
// store.
type MemoryGitHubDeviceSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*domain.GitHubDeviceSession
}

var _ domain.GitHubDeviceSessionStore = (*MemoryGitHubDeviceSessionStore)(nil)

// NewMemoryGitHubDeviceSessionStore returns an empty store.
func NewMemoryGitHubDeviceSessionStore() *MemoryGitHubDeviceSessionStore {
	return &MemoryGitHubDeviceSessionStore{sessions: make(map[string]*domain.GitHubDeviceSession)}
}

func (s *MemoryGitHubDeviceSessionStore) Create(sess *domain.GitHubDeviceSession) error {
	if sess == nil || sess.SessionID == "" || sess.UserID == "" {
		return domain.ErrValidation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for id, existing := range s.sessions {
		if existing.Consumed || !now.Before(existing.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
	if len(s.sessions) >= maxGitHubDeviceSessions {
		var victim string
		var expiry time.Time
		for id, existing := range s.sessions {
			if victim == "" || existing.ExpiresAt.Before(expiry) || (existing.ExpiresAt.Equal(expiry) && id < victim) {
				victim, expiry = id, existing.ExpiresAt
			}
		}
		delete(s.sessions, victim)
	}
	cp := *sess
	s.sessions[sess.SessionID] = &cp
	return nil
}

func (s *MemoryGitHubDeviceSessionStore) Get(sessionID string) (*domain.GitHubDeviceSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *sess
	return &cp, nil
}

func (s *MemoryGitHubDeviceSessionStore) UpdateInterval(sessionID string, intervalSec int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return domain.ErrNotFound
	}
	sess.Interval = intervalSec
	return nil
}

func (s *MemoryGitHubDeviceSessionStore) Consume(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok || sess.Consumed {
		return domain.ErrOAuthStateInvalid
	}
	sess.Consumed = true
	return nil
}

func (s *MemoryGitHubDeviceSessionStore) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}
