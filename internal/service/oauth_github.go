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
	defaultGroup string
	tokenURL     string
	apiBaseURL   string
	http         *http.Client
	users        *UserService
	groups       domain.GroupRepository
	jwt          *JWTService
	logger       *logrus.Logger
}

// NewGitHubOAuthService returns a GitHubOAuthService.
func NewGitHubOAuthService(cfg *domain.OAuthConfig, users *UserService, groups domain.GroupRepository, jwtSvc *JWTService, logger *logrus.Logger) *GitHubOAuthService {
	return &GitHubOAuthService{ //nolint:gosec // G101: OAuth client secret is config-derived, not hardcoded.
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		allowedOrgs:  cfg.AllowedOrgs,
		defaultGroup: cfg.DefaultGroup,
		tokenURL:     "https://github.com/login/oauth/access_token",
		apiBaseURL:   "https://api.github.com",
		http:         &http.Client{Timeout: 10 * time.Second},
		users:        users,
		groups:       groups,
		jwt:          jwtSvc,
		logger:       logger,
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
// profile and orgs, enforces allowed_orgs, ensures a local user, optionally
// auto-joins the default group, and issues a JWT pair.
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

	if len(s.allowedOrgs) > 0 && !orgsIntersect(s.allowedOrgs, orgs) {
		return "", "", nil, domain.ErrForbidden
	}

	access, refresh, u, err = completeOAuthUser(ctx, s.users, s.groups, s.jwt, s.logger, "github", strconv.Itoa(ghUser.ID), ghUser.Login, s.defaultGroup)
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

var _ OAuthProvider = (*GitHubOAuthService)(nil)
