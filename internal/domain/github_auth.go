package domain

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GitHub integration sentinel errors.
var (
	ErrGitHubOAuthDisabled            = errors.New("github oauth integration is disabled")
	ErrGitHubDeviceFlowDisabled       = errors.New("github device flow integration is disabled")
	ErrGitHubTokenRevoked             = errors.New("github token is expired or revoked")
	ErrRepoClaimedByOtherOrg          = errors.New("repository is already claimed by another organization")
	ErrRepoNotInInstallation          = errors.New("repository does not belong to installation")
	ErrRepoInaccessible               = errors.New("repository is not available through this connection")
	ErrInvalidRedirectURI             = errors.New("redirect_uri is not allowed")
	ErrOAuthStateInvalid              = errors.New("oauth state is invalid, expired, or already used")
	ErrGitHubRateLimited              = errors.New("github api rate limit exceeded")
	ErrGitHubDeviceAuthorizationDelay = errors.New("github device authorization pending")
	ErrGitHubDeviceSlowDown           = errors.New("github device flow slow down")
	ErrGitHubDeviceFlowDenied         = errors.New("github device authorization was denied or expired")
	// ErrGitHubRefreshInvalid means GitHub rejected the refresh token itself
	// (bad_refresh_token / incorrect_client_credentials): the connection must
	// be re-authorized. Distinct from ErrGitHubTokenRevoked so a transient
	// refresh failure is never conflated with revocation.
	ErrGitHubRefreshInvalid = errors.New("github refresh token is invalid or expired")
	// ErrGitHubRefreshUnavailable means the refresh could not be completed for
	// a transient reason (token endpoint 5xx/timeout, Raft persist failure):
	// the stored credential is intact and the request should be retried, never
	// reconnected.
	ErrGitHubRefreshUnavailable = errors.New("github token refresh is temporarily unavailable")
)

// GitHubConnectionStatus represents the health of a stored GitHub token.
type GitHubConnectionStatus string

const (
	GitHubConnectionActive  GitHubConnectionStatus = "active"
	GitHubConnectionRevoked GitHubConnectionStatus = "revoked"
)

// UserGitHubConnection is a user-scoped GitHub identity + sealed OAuth/PAT token.
type UserGitHubConnection struct {
	UserID          string   `json:"user_id"`
	GitHubUserID    int64    `json:"github_user_id"`
	GitHubLogin     string   `json:"github_login"`
	AvatarURL       string   `json:"avatar_url,omitempty"`
	BaseURL         string   `json:"base_url"`
	Scopes          []string `json:"scopes,omitempty"`
	TokenCiphertext string   `json:"-"`
	// RefreshTokenCiphertext seals the OAuth refresh token. Empty for PATs and
	// for non-expiring OAuth Apps (GitHub returns no refresh token there).
	RefreshTokenCiphertext string                 `json:"-"`
	TokenExpiresAt         time.Time              `json:"token_expires_at,omitempty"`   // UTC; zero = non-expiring / unknown
	RefreshExpiresAt       time.Time              `json:"refresh_expires_at,omitempty"` // UTC; zero = non-expiring / unknown
	TokenVersion           uint64                 `json:"token_version,omitempty"`      // monotonic rotation counter (CAS)
	Status                 GitHubConnectionStatus `json:"status"`
	ConnectedAt            time.Time              `json:"connected_at"`
	UpdatedAt              time.Time              `json:"updated_at"`
}

// GitHubAccountSource is an org-scoped, owner-level GitHub integration
// ("installation"). One record per (OrgID, normalized BaseURL, lower(Owner)).
type GitHubAccountSource struct {
	ID                string   `json:"id"`
	OrgID             string   `json:"org_id"`
	Owner             string   `json:"owner"`
	OwnerType         string   `json:"owner_type"` // "User" | "Organization" | "PAT"
	BaseURL           string   `json:"base_url"`
	ConnectedByUserID string   `json:"connected_by_user_id,omitempty"`
	Scopes            []string `json:"scopes,omitempty"`
	TokenCiphertext   string   `json:"-"`
	// RefreshTokenCiphertext seals the OAuth refresh token. Empty for PATs and
	// for non-expiring OAuth Apps (GitHub returns no refresh token there).
	RefreshTokenCiphertext string                 `json:"-"`
	TokenExpiresAt         time.Time              `json:"token_expires_at,omitempty"`   // UTC; zero = non-expiring / unknown
	RefreshExpiresAt       time.Time              `json:"refresh_expires_at,omitempty"` // UTC; zero = non-expiring / unknown
	TokenVersion           uint64                 `json:"token_version,omitempty"`      // monotonic rotation counter (CAS)
	AppID                  string                 `json:"app_id,omitempty"`
	InstallationID         string                 `json:"installation_id,omitempty"`
	Status                 GitHubConnectionStatus `json:"status"`
	CreatedAt              time.Time              `json:"created_at"`
	UpdatedAt              time.Time              `json:"updated_at"`
}

// GitHubAccountSourceID computes the deterministic 23-char ID "ghacct-<16hex>"
// for (orgID, baseURL, owner).
func GitHubAccountSourceID(orgID, baseURL, owner string) string {
	normBase := strings.TrimRight(strings.ToLower(strings.TrimSpace(baseURL)), "/")
	if normBase == "" {
		normBase = "https://api.github.com"
	}
	raw := fmt.Sprintf("%s\x00%s\x00%s", orgID, normBase, strings.ToLower(strings.TrimSpace(owner)))
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("ghacct-%x", sum[:8])
}

// MaterializedForgeSourceID computes the deterministic 23-char ID "ghrepo-<16hex>"
// for an auto-materialized per-repo ForgeSource.
func MaterializedForgeSourceID(orgID, ownerRepo string) string {
	raw := fmt.Sprintf("%s\x00%s", orgID, strings.ToLower(strings.TrimSpace(ownerRepo)))
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("ghrepo-%x", sum[:8])
}

// RepoSetting persists per-repository claim ownership and Cloud Checks trigger flags.
type RepoSetting struct {
	OrgID                    string    `json:"org_id"`
	Repo                     string    `json:"repo"` // canonical "<host>/<owner>/<name>", e.g. "github.com/acme/api"
	Provider                 ForgeKind `json:"provider"`
	SourceID                 string    `json:"source_id"`       // GitHubAccountSource.ID (or standalone ForgeSource.ID)
	ForgeSourceID            string    `json:"forge_source_id"` // concrete per-repo ForgeSource.ID
	IsPublic                 bool      `json:"is_public"`
	RunChecksOnDefaultBranch bool      `json:"run_checks_on_default_branch"`
	RunChecksOnPullRequest   bool      `json:"run_checks_on_pull_request"`
	RunChecksOnBranchPush    bool      `json:"run_checks_on_branch_push"`
	ClaimedByUserID          string    `json:"claimed_by_user_id,omitempty"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

// AllowsWebhookEvent reports whether the trigger flags on s permit dispatching ev.
// A nil receiver (legacy ForgeSource without an explicit RepoSetting) allows all events.
func (s *RepoSetting) AllowsWebhookEvent(ev *WebhookEvent) bool {
	if s == nil || ev == nil {
		return true
	}
	switch ev.Event {
	case "pull_request":
		return s.RunChecksOnPullRequest
	case "push":
		branch := strings.TrimPrefix(ev.Ref, "refs/heads/")
		if ev.DefaultBranch != "" && branch == ev.DefaultBranch {
			return s.RunChecksOnDefaultBranch
		}
		// When webhook payload omits default_branch, treat "main"/"master" as default
		// only if DefaultBranch is empty; otherwise non-default branch uses BranchPush.
		if ev.DefaultBranch == "" && (branch == "main" || branch == "master") {
			return s.RunChecksOnDefaultBranch
		}
		return s.RunChecksOnBranchPush
	case "tag", "release":
		return s.RunChecksOnDefaultBranch || s.RunChecksOnBranchPush
	default:
		return false
	}
}

// GitHubOAuthState is a replicated, single-use OAuth CSRF + PKCE state record.
type GitHubOAuthState struct {
	StateID      string    `json:"state_id"`
	UserID       string    `json:"user_id"`
	OrgID        string    `json:"org_id,omitempty"`
	RedirectURI  string    `json:"redirect_uri"`
	PKCEVerifier string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Consumed     bool      `json:"consumed"`
}

// GitHubDeviceSession tracks an in-flight outbound GitHub RFC 8628 device flow
// initiated by an authenticated supervisor user. Leader-pinned in memory.
type GitHubDeviceSession struct {
	SessionID       string    `json:"session_id"`
	UserID          string    `json:"user_id"`
	OrgID           string    `json:"org_id,omitempty"`
	DeviceCode      string    `json:"-"`
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	Interval        int       `json:"interval"`
	ExpiresAt       time.Time `json:"expires_at"`
	Consumed        bool      `json:"consumed"`
}

// GitHubUserProfile is the normalized profile returned by GET /user.
type GitHubUserProfile struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Type      string `json:"type,omitempty"`
}

// GitHubOwner describes a selectable GitHub user or organization login.
type GitHubOwner struct {
	Login     string `json:"login"`
	Type      string `json:"type"` // "User" | "Organization"
	AvatarURL string `json:"avatar_url,omitempty"`
}

// GitHubRemoteRepo is a repository returned by the GitHub REST API.
type GitHubRemoteRepo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"` // "owner/name"
	Owner         string `json:"owner"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	AdminPerm     bool   `json:"admin_perm"`
}

// GitHubTokenResult is the normalized result of exchanging an OAuth code,
// polling a device code, or refreshing a token with GitHub. JSON tags match
// GitHub's /login/oauth/access_token response fields so the repository layer
// can decode the upstream body directly.
type GitHubTokenResult struct {
	AccessToken           string `json:"access_token"`
	TokenType             string `json:"token_type"`
	Scope                 string `json:"scope"` // comma-separated, as GitHub returns it
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int    `json:"expires_in"`               // seconds until access-token expiry
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"` // seconds until refresh-token expiry

	Scopes []string `json:"-"` // derived from Scope by Normalize
}

// Normalize derives Scopes from Scope and defaults TokenType to "bearer".
func (r *GitHubTokenResult) Normalize() {
	r.Scopes = nil
	for _, s := range strings.Split(r.Scope, ",") {
		if s = strings.TrimSpace(s); s != "" {
			r.Scopes = append(r.Scopes, s)
		}
	}
	if r.TokenType == "" {
		r.TokenType = "bearer"
	}
}

// GitHubDeviceCodePrompt is returned by GitHub's POST /login/device/code.
type GitHubDeviceCodePrompt struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       int
	Interval        int
}

// GitHubRateLimitError carries upstream rate-limit reset metadata.
type GitHubRateLimitError struct {
	ResetAt    time.Time
	RetryAfter time.Duration
}

func (e *GitHubRateLimitError) Error() string {
	if !e.ResetAt.IsZero() {
		return fmt.Sprintf("%s (resets at %s)", ErrGitHubRateLimited.Error(), e.ResetAt.UTC().Format(time.RFC3339))
	}
	return ErrGitHubRateLimited.Error()
}

func (e *GitHubRateLimitError) Unwrap() error { return ErrGitHubRateLimited }

// OAuthStateUserMismatchError is returned when a valid, unexpired OAuth state
// is submitted by a browser authenticated as a different supervisor user than
// the user who initiated the flow. Unwraps to ErrForbidden so generic callers
// treat it as 403 while the OAuth callback handler can render the switch-account prompt.
type OAuthStateUserMismatchError struct {
	ExpectedUsername string
	CurrentUsername  string
}

func (e *OAuthStateUserMismatchError) Error() string {
	return fmt.Sprintf("oauth state belongs to user %q, current user is %q", e.ExpectedUsername, e.CurrentUsername)
}

func (e *OAuthStateUserMismatchError) Unwrap() error { return ErrForbidden }

// RepoSettingMutation is one item of UpdateOrgRepoSettings / UpdateOrgRepoSetting.
type RepoSettingMutation struct {
	Repo                     string
	IsPublic                 *bool
	SourceID                 *string
	RunChecksOnDefaultBranch *bool
	RunChecksOnPullRequest   *bool
	RunChecksOnBranchPush    *bool
}

// SourceRepositoryItem is one entry returned for GraphQL sourceRepositories / UI picker.
type SourceRepositoryItem struct {
	Repository       string  `json:"repository"` // canonical "github.com/owner/name"
	FullName         string  `json:"full_name"`  // "owner/name"
	Private          bool    `json:"private"`
	Archived         bool    `json:"archived"`
	AdminPerm        bool    `json:"admin_perm"`
	Selected         bool    `json:"selected"`
	Eligible         bool    `json:"eligible"`
	ClaimedByOrgName *string `json:"claimed_by_org_name,omitempty"`
}

// GitHubConnectionRepository persists user-scoped GitHub connections.
type GitHubConnectionRepository interface {
	Upsert(ctx context.Context, conn *UserGitHubConnection) error
	// UpsertIfVersion applies conn only when the stored record's TokenVersion
	// still equals expectedVersion, returning ErrConflict (stale) or
	// ErrNotFound otherwise. It is the compare-and-swap used by token rotation.
	UpsertIfVersion(ctx context.Context, conn *UserGitHubConnection, expectedVersion uint64) error
	GetByUser(ctx context.Context, userID string) (*UserGitHubConnection, error)
	DeleteByUser(ctx context.Context, userID string) error
}

// GitHubAccountSourceRepository persists org-scoped GitHub account sources.
type GitHubAccountSourceRepository interface {
	Upsert(ctx context.Context, src *GitHubAccountSource) error
	// UpsertIfVersion is the TokenVersion compare-and-swap for account sources.
	UpsertIfVersion(ctx context.Context, src *GitHubAccountSource, expectedVersion uint64) error
	Get(ctx context.Context, id string) (*GitHubAccountSource, error)
	GetByOrgAndOwner(ctx context.Context, orgID, owner string) (*GitHubAccountSource, error)
	ListByOrg(ctx context.Context, orgID string) ([]*GitHubAccountSource, error)
	Delete(ctx context.Context, id string) error
}

// RepoSettingWrite pairs a RepoSetting with an optional materialized ForgeSource
// to commit atomically inside one Raft log entry.
type RepoSettingWrite struct {
	Setting     *RepoSetting
	ForgeSource *ForgeSource // nil when updating triggers on an already-materialized source
}

// RepoSettingRepository persists per-repo claims and trigger settings.
type RepoSettingRepository interface {
	BatchUpsert(ctx context.Context, orgID string, writes []RepoSettingWrite) error
	Get(ctx context.Context, orgID, canonicalRepo string) (*RepoSetting, error)
	GetByCanonicalRepo(ctx context.Context, canonicalRepo string) (*RepoSetting, error)
	ListByOrg(ctx context.Context, orgID string) ([]*RepoSetting, error)
	ListBySource(ctx context.Context, sourceID string) ([]*RepoSetting, error)
	Delete(ctx context.Context, orgID, canonicalRepo string, deleteForgeSourceID string) error
}

// GitHubOAuthStateRepository persists replicated single-use OAuth states.
type GitHubOAuthStateRepository interface {
	Create(ctx context.Context, st *GitHubOAuthState) error
	Get(ctx context.Context, stateID string) (*GitHubOAuthState, error)
	Consume(ctx context.Context, stateID string, now time.Time) (*GitHubOAuthState, error)
}

// GitHubDeviceSessionStore tracks leader-local device-flow sessions.
type GitHubDeviceSessionStore interface {
	Create(sess *GitHubDeviceSession) error
	Get(sessionID string) (*GitHubDeviceSession, error)
	UpdateInterval(sessionID string, intervalSec int) error
	Consume(sessionID string) error
	Delete(sessionID string)
}

// TokenSealer abstracts scope-bound AES-256-GCM sealing for services.
type TokenSealer interface {
	SealRaw(scope SecretScope, scopeID, plaintext string) (string, error)
	OpenRaw(scope SecretScope, scopeID, ciphertext string) (string, error)
}

// GitHubAPIClient is the outbound GitHub OAuth + REST adapter (implemented in repository).
type GitHubAPIClient interface {
	AuthorizeURL(clientID, redirectURI, state, codeChallenge string, scopes []string) string
	ExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (*GitHubTokenResult, error)
	StartDeviceFlow(ctx context.Context, scopes []string) (*GitHubDeviceCodePrompt, error)
	PollDeviceFlow(ctx context.Context, deviceCode string) (*GitHubTokenResult, error)
	RefreshToken(ctx context.Context, refreshToken string) (*GitHubTokenResult, error)
	GetAuthenticatedUser(ctx context.Context, apiBaseURL, token string) (*GitHubUserProfile, []string, error)
	ListUserOrgs(ctx context.Context, apiBaseURL, token string) ([]GitHubOwner, error)
	ListAccessibleRepos(ctx context.Context, apiBaseURL, token string) ([]GitHubRemoteRepo, error)
	GetRepository(ctx context.Context, apiBaseURL, token, owner, repo string) (*GitHubRemoteRepo, error)
}

// GitHubCloudIntegrator bridges CloudGraphQL (in repository) to GitHubIntegrationService
// (in service) without introducing a repository -> service import cycle.
type GitHubCloudIntegrator interface {
	BuildOAuthURL(ctx context.Context, userID, orgID, redirectURI string) (string, error)
	ListSourceRepositories(ctx context.Context, org *Org, installationID string) ([]SourceRepositoryItem, error)
	UpdateOrgRepoSettings(ctx context.Context, actorUserID string, actorIsGlobalAdmin bool, org *Org, settings []RepoSettingMutation) error
	DeleteOrgRepoSetting(ctx context.Context, actorUserID string, actorIsGlobalAdmin bool, org *Org, repo string) error
}
