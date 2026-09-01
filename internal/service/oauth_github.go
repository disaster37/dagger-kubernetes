package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GitHubOAuthService implements the GitHub OAuth login flow (server-side code
// exchange). The tokenURL and apiBaseURL fields are overridable for tests.
type GitHubOAuthService struct {
	clientID     string
	clientSecret string
	redirectURL  string
	allowedOrgs  []string
	allowedTeams []string
	defaultGroup string
	tokenURL     string
	apiBaseURL   string
	http         *http.Client
	users        *UserService
	groups       domain.GroupRepository
	jwt          *JWTService
	logger       *logrus.Logger
	mapper       *GroupMapper
	encKey       []byte // AES-256 key for encrypting upstream credentials; nil = disabled
}

// NewGitHubOAuthService returns a GitHubOAuthService. encKey is the AES-256 key
// used to encrypt upstream OAuth credentials at rest; nil disables encryption.
func NewGitHubOAuthService(cfg *domain.OAuthConfig, mapper *GroupMapper, users *UserService, groups domain.GroupRepository, jwtSvc *JWTService, logger *logrus.Logger, encKey []byte) *GitHubOAuthService {
	return &GitHubOAuthService{ //nolint:gosec // G101: OAuth client secret is config-derived, not hardcoded.
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		allowedOrgs:  cfg.AllowedOrgs,
		allowedTeams: cfg.AllowedTeams,
		defaultGroup: cfg.DefaultGroup,
		tokenURL:     "https://github.com/login/oauth/access_token",
		apiBaseURL:   "https://api.github.com",
		http:         &http.Client{Timeout: 10 * time.Second},
		users:        users,
		groups:       groups,
		jwt:          jwtSvc,
		logger:       logger,
		mapper:       mapper,
		encKey:       encKey,
	}
}

// LoginURL returns the GitHub authorize URL with the given state token. All
// parameters are percent-encoded so config values containing reserved
// characters cannot corrupt the query string.
func (s *GitHubOAuthService) LoginURL(state string) string {
	v := url.Values{}
	v.Set("client_id", s.clientID)
	v.Set("redirect_uri", s.redirectURL)
	v.Set("scope", "read:org")
	v.Set("state", state)
	return fmt.Sprintf("https://github.com/login/oauth/authorize?%s", v.Encode())
}

// Complete exchanges the code for a GitHub access token, fetches the user
// profile, orgs and (when needed) teams, enforces allowed_orgs/allowed_teams,
// ensures a local user, optionally auto-joins the default group, adds mapped
// supervisor groups, and issues a JWT pair.
func (s *GitHubOAuthService) Complete(ctx context.Context, code string) (access, refresh string, u *domain.User, err error) {
	accessToken, err := s.exchangeCode(ctx, code)
	if err != nil {
		return "", "", nil, fmt.Errorf("exchange code: %w", err)
	}

	ghUser, err := s.fetchUser(ctx, accessToken)
	if err != nil {
		return "", "", nil, fmt.Errorf("fetch github user: %w", err)
	}

	orgs, err := s.fetchOrgs(ctx, accessToken)
	if err != nil {
		return "", "", nil, fmt.Errorf("fetch github orgs: %w", err)
	}

	var teams []string
	if len(s.allowedTeams) > 0 || s.mapper.Active() {
		teams, err = s.fetchTeams(ctx, accessToken)
		if err != nil {
			if len(s.allowedTeams) > 0 {
				return "", "", nil, fmt.Errorf("fetch github teams: %w", err)
			}
			s.logger.WithError(err).Warn("oauth: github teams fetch failed; mapping will use orgs only")
			teams = nil
		}
	}

	if len(s.allowedOrgs) > 0 && !orgsIntersect(s.allowedOrgs, orgs) {
		return "", "", nil, domain.ErrForbidden
	}
	if len(s.allowedTeams) > 0 && !orgsIntersect(s.allowedTeams, teams) {
		return "", "", nil, domain.ErrForbidden
	}

	providerGroups := make([]string, 0, len(orgs)+len(teams))
	providerGroups = append(providerGroups, orgs...)
	providerGroups = append(providerGroups, teams...)

	mappedGroups := s.mapper.mapIfActive(providerGroups)

	cred := &oauthCredential{Provider: "github", AccessToken: accessToken}
	access, refresh, u, err = completeOAuthLogin(ctx, s.users, s.groups, s.jwt, s.logger, s.encKey, "github", strconv.Itoa(ghUser.ID), ghUser.Login, s.defaultGroup, mappedGroups, cred)
	if err != nil {
		return "", "", nil, fmt.Errorf("github oauth: %w", err)
	}
	return access, refresh, u, nil
}

func (s *GitHubOAuthService) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {s.clientID},
		"client_secret": {s.clientSecret},
		"code":          {code},
		"redirect_uri":  {s.redirectURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("github token error: %s", out.Error)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("empty access token")
	}
	return out.AccessToken, nil
}

type githubUser struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
}

func (s *GitHubOAuthService) fetchUser(ctx context.Context, accessToken string) (*githubUser, error) {
	var u githubUser
	if err := s.getJSON(ctx, "/user", accessToken, &u); err != nil {
		return nil, err
	}
	if u.ID == 0 || u.Login == "" {
		return nil, fmt.Errorf("incomplete github user")
	}
	return &u, nil
}

func (s *GitHubOAuthService) fetchOrgs(ctx context.Context, accessToken string) ([]string, error) {
	var out []struct {
		Login string `json:"login"`
	}
	if err := s.getJSON(ctx, "/user/orgs", accessToken, &out); err != nil {
		return nil, err
	}
	orgs := make([]string, 0, len(out))
	for _, o := range out {
		orgs = append(orgs, o.Login)
	}
	return orgs, nil
}

// fetchTeams fetches the user's teams across orgs and returns "org/team" slugs.
func (s *GitHubOAuthService) fetchTeams(ctx context.Context, accessToken string) ([]string, error) {
	var out []struct {
		Slug string `json:"slug"`
		Org  struct {
			Login string `json:"login"`
		} `json:"organization"`
	}
	if err := s.getJSON(ctx, "/user/teams", accessToken, &out); err != nil {
		return nil, err
	}
	teams := make([]string, 0, len(out))
	for _, t := range out {
		if t.Slug == "" || t.Org.Login == "" {
			continue
		}
		teams = append(teams, fmt.Sprintf("%s/%s", t.Org.Login, t.Slug))
	}
	return teams, nil
}

// getJSON issues an authenticated GET against the GitHub API and decodes the
// JSON response into out.
func (s *GitHubOAuthService) getJSON(ctx context.Context, path, accessToken string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBaseURL+path, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dagger-kubernetes")

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github api %s status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Revalidate re-checks the user's current IdP group membership using the stored
// credential and returns the current provider group names. Returns
// domain.ErrSessionRevoked when the credential is invalid/expired beyond refresh
// (user must re-login) and domain.ErrForbidden when membership no longer
// satisfies the allowlist.
func (s *GitHubOAuthService) Revalidate(ctx context.Context, u *domain.User) ([]string, error) {
	if u.OAuthTokenCiphertext == "" {
		return nil, errOAuthNoCredential
	}
	cred, err := decryptOAuthCredential(s.encKey, u.OAuthTokenCiphertext)
	if err != nil || cred == nil {
		return nil, domain.ErrSessionRevoked
	}
	if _, err := s.fetchUser(ctx, cred.AccessToken); err != nil {
		return nil, domain.ErrSessionRevoked // 401/404 => user/token gone
	}
	orgs, err := s.fetchOrgs(ctx, cred.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("fetch github orgs: %w", err)
	}
	var teams []string
	if len(s.allowedTeams) > 0 || s.mapper.Active() {
		teams, err = s.fetchTeams(ctx, cred.AccessToken)
		if err != nil {
			if len(s.allowedTeams) > 0 {
				return nil, fmt.Errorf("fetch github teams: %w", err)
			}
			s.logger.WithError(err).Warn("oauth: github teams fetch failed during revalidation")
		}
	}
	if len(s.allowedOrgs) > 0 && !orgsIntersect(s.allowedOrgs, orgs) {
		return nil, domain.ErrForbidden
	}
	if len(s.allowedTeams) > 0 && !orgsIntersect(s.allowedTeams, teams) {
		return nil, domain.ErrForbidden
	}
	out := make([]string, 0, len(orgs)+len(teams))
	out = append(out, orgs...)
	out = append(out, teams...)
	return out, nil
}

var _ OAuthProvider = (*GitHubOAuthService)(nil)
