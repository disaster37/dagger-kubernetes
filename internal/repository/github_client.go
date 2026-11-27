package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	goauthoauth "github.com/disaster37/go-auth/oauth"
	"golang.org/x/oauth2"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	githubPerPage         = 100
	maxGitHubPages        = 10
	maxGitHubResponse     = 2 << 20
	githubUserAgent       = "dagger-kubernetes"
	githubAPIVersion      = "2022-11-28"
	maxGitHubErrorSnippet = 256
)

// GitHubClientConfig configures the outbound GitHub OAuth + REST adapter.
type GitHubClientConfig struct {
	ClientID       string
	ClientSecret   string
	OAuthBaseURL   string
	DefaultAPIURL  string
	CACertPath     string
	HTTPTimeout    time.Duration
	AllowedAPIURLs []string
}

// GitHubClient implements domain.GitHubAPIClient using golang.org/x/oauth2 and
// the shared go-auth HTTP client (custom CA support, TLS >= 1.2, cross-origin
// redirect refusal).
type GitHubClient struct {
	cfg       GitHubClientConfig
	oauthBase string
	apiURL    string
	http      *http.Client
}

var _ domain.GitHubAPIClient = (*GitHubClient)(nil)

// NewGitHubClient returns a GitHubClient. A PEM CA bundle is loaded at startup
// and an unreadable/invalid bundle fails fast.
//
//nolint:gocritic // config is a startup-only immutable value
func NewGitHubClient(cfg GitHubClientConfig) (*GitHubClient, error) {
	if cfg.OAuthBaseURL == "" {
		cfg.OAuthBaseURL = "https://github.com"
	}
	if cfg.DefaultAPIURL == "" {
		cfg.DefaultAPIURL = "https://api.github.com"
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 15 * time.Second
	}
	client, err := goauthoauth.NewOAuthHTTPClient(cfg.CACertPath, cfg.HTTPTimeout)
	if err != nil {
		return nil, fmt.Errorf("create github http client: %w", err)
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		if len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &GitHubClient{
		cfg:       cfg,
		oauthBase: strings.TrimRight(cfg.OAuthBaseURL, "/"),
		apiURL:    strings.TrimRight(cfg.DefaultAPIURL, "/"),
		http:      client,
	}, nil
}

// oauthConfig builds the OAuth2 config for a redirect URI and scope set.
func (c *GitHubClient) oauthConfig(redirectURI string, scopes []string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		RedirectURL:  redirectURI,
		Scopes:       scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:       fmt.Sprintf("%s/login/oauth/authorize", c.oauthBase),
			TokenURL:      fmt.Sprintf("%s/login/oauth/access_token", c.oauthBase),
			DeviceAuthURL: fmt.Sprintf("%s/login/device/code", c.oauthBase),
		},
	}
}

// AuthorizeURL builds the GitHub authorization URL with PKCE S256.
func (c *GitHubClient) AuthorizeURL(clientID, redirectURI, state, codeChallenge string, scopes []string) string {
	cfg := c.oauthConfig(redirectURI, scopes)
	if clientID != "" {
		cfg.ClientID = clientID
	}
	return cfg.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

// ExchangeCode exchanges an authorization code (with PKCE verifier). GitHub
// accepts the client credentials as HTTP Basic auth on this endpoint (the
// golang.org/x/oauth2 behavior this replaces).
func (c *GitHubClient) ExchangeCode(ctx context.Context, code, redirectURI, codeVerifier string) (*domain.GitHubTokenResult, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	}
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	res, err := c.exchangeToken(ctx, form, true)
	if err != nil {
		return nil, fmt.Errorf("exchange github code: %w", err)
	}
	return res, nil
}

// RefreshToken rotates an expiring OAuth App token. client_secret is only sent
// when configured: tokens minted by the device flow refresh without one.
func (c *GitHubClient) RefreshToken(ctx context.Context, refreshToken string) (*domain.GitHubTokenResult, error) {
	form := url.Values{
		"client_id":     {c.cfg.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if c.cfg.ClientSecret != "" {
		form.Set("client_secret", c.cfg.ClientSecret)
	}
	res, err := c.exchangeToken(ctx, form, false)
	if err != nil {
		return nil, fmt.Errorf("refresh github token: %w", err)
	}
	return res, nil
}

// githubTokenEndpointError is a non-credential error code returned by
// /login/oauth/access_token (for example authorization_pending).
type githubTokenEndpointError struct{ code string }

func (e *githubTokenEndpointError) Error() string {
	return fmt.Sprintf("github token endpoint: %s", e.code)
}

// exchangeToken POSTs form to <oauthBase>/login/oauth/access_token, decodes the
// full token response and normalizes it. A rejected refresh token maps to
// domain.ErrGitHubRefreshInvalid; any other failure (network, 5xx, unknown
// error code) is returned as a transient error and never as a revocation.
// The form may carry a refresh token, so it is never logged.
func (c *GitHubClient) exchangeToken(ctx context.Context, form url.Values, basicAuth bool) (*domain.GitHubTokenResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/login/oauth/access_token", c.oauthBase), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build github token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", githubUserAgent)
	if basicAuth {
		req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readGitHubBody(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		domain.GitHubTokenResult
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode github token response: %w", err)
	}
	if out.Error != "" {
		switch out.Error {
		case "bad_refresh_token", "incorrect_client_credentials":
			return nil, fmt.Errorf("%w: %s", domain.ErrGitHubRefreshInvalid, out.Error)
		default:
			return nil, &githubTokenEndpointError{code: out.Error}
		}
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github token endpoint: status %d: %s", resp.StatusCode, scrubGitHubBody(body))
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("github token endpoint returned no access token")
	}
	res := out.GitHubTokenResult
	res.Normalize()
	return &res, nil
}

// StartDeviceFlow begins an RFC 8628 device authorization.
func (c *GitHubClient) StartDeviceFlow(ctx context.Context, scopes []string) (*domain.GitHubDeviceCodePrompt, error) {
	cfg := c.oauthConfig("", scopes)
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)
	resp, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("start github device flow: %w", err)
	}
	expiresIn := int(time.Until(resp.Expiry).Seconds())
	if resp.Expiry.IsZero() {
		expiresIn = 900
	}
	return &domain.GitHubDeviceCodePrompt{
		DeviceCode:      resp.DeviceCode,
		UserCode:        resp.UserCode,
		VerificationURI: resp.VerificationURI,
		ExpiresIn:       expiresIn,
		Interval:        int(resp.Interval),
	}, nil
}

// PollDeviceFlow performs a single RFC 8628 token poll (never blocks).
func (c *GitHubClient) PollDeviceFlow(ctx context.Context, deviceCode string) (*domain.GitHubTokenResult, error) {
	form := url.Values{
		"client_id":   {c.cfg.ClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	if c.cfg.ClientSecret != "" {
		form.Set("client_secret", c.cfg.ClientSecret)
	}
	res, err := c.exchangeToken(ctx, form, false)
	if err != nil {
		var endpointErr *githubTokenEndpointError
		if errors.As(err, &endpointErr) {
			switch endpointErr.code {
			case "authorization_pending":
				return nil, domain.ErrGitHubDeviceAuthorizationDelay
			case "slow_down":
				return nil, domain.ErrGitHubDeviceSlowDown
			case "expired_token", "access_denied":
				return nil, domain.ErrGitHubDeviceFlowDenied
			default:
				return nil, fmt.Errorf("github device flow: %s: %w", endpointErr.code, domain.ErrGitHubDeviceFlowDenied)
			}
		}
		return nil, fmt.Errorf("poll github device flow: %w", err)
	}
	return res, nil
}

// GetAuthenticatedUser fetches GET /user and the granted OAuth scopes.
func (c *GitHubClient) GetAuthenticatedUser(ctx context.Context, apiBaseURL, token string) (*domain.GitHubUserProfile, []string, error) {
	var out struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
		Type      string `json:"type"`
	}
	header, err := c.getJSON(ctx, apiBaseURL, token, "/user", &out)
	if err != nil {
		return nil, nil, err
	}
	return &domain.GitHubUserProfile{ID: out.ID, Login: out.Login, AvatarURL: out.AvatarURL, Type: out.Type}, splitScopes(header.Get("X-OAuth-Scopes")), nil
}

// ListUserOrgs lists GET /user/orgs across bounded pages.
func (c *GitHubClient) ListUserOrgs(ctx context.Context, apiBaseURL, token string) ([]domain.GitHubOwner, error) {
	out := make([]domain.GitHubOwner, 0)
	for page := 1; page <= maxGitHubPages; page++ {
		var rows []struct {
			Login     string `json:"login"`
			AvatarURL string `json:"avatar_url"`
		}
		if err := c.getJSONPage(ctx, apiBaseURL, token, fmt.Sprintf("/user/orgs?per_page=%d&page=%d", githubPerPage, page), &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			out = append(out, domain.GitHubOwner{Login: row.Login, Type: "Organization", AvatarURL: row.AvatarURL})
		}
		if len(rows) < githubPerPage {
			break
		}
	}
	return out, nil
}

// ListAccessibleRepos lists the token's accessible repositories across bounded
// pages (1000 repositories maximum).
func (c *GitHubClient) ListAccessibleRepos(ctx context.Context, apiBaseURL, token string) ([]domain.GitHubRemoteRepo, error) {
	out := make([]domain.GitHubRemoteRepo, 0)
	for page := 1; page <= maxGitHubPages; page++ {
		var rows []githubRepoPayload
		path := fmt.Sprintf("/user/repos?per_page=%d&page=%d&affiliation=owner,collaborator,organization_member&sort=updated", githubPerPage, page)
		if err := c.getJSONPage(ctx, apiBaseURL, token, path, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			out = append(out, row.toDomain())
		}
		if len(rows) < githubPerPage {
			break
		}
	}
	return out, nil
}

// GetRepository fetches a single repository by owner/name.
func (c *GitHubClient) GetRepository(ctx context.Context, apiBaseURL, token, owner, repo string) (*domain.GitHubRemoteRepo, error) {
	owner, err := pathSegment(owner)
	if err != nil {
		return nil, err
	}
	repo, err = pathSegment(repo)
	if err != nil {
		return nil, err
	}
	var out githubRepoPayload
	if _, err := c.getJSON(ctx, apiBaseURL, token, fmt.Sprintf("/repos/%s/%s", owner, repo), &out); err != nil {
		return nil, err
	}
	remote := out.toDomain()
	return &remote, nil
}

type githubRepoPayload struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	Permissions struct {
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

func (p *githubRepoPayload) toDomain() domain.GitHubRemoteRepo {
	return domain.GitHubRemoteRepo{
		ID: p.ID, Name: p.Name, FullName: p.FullName, Owner: p.Owner.Login,
		Private: p.Private, Archived: p.Archived, DefaultBranch: p.DefaultBranch, AdminPerm: p.Permissions.Admin,
	}
}

func (c *GitHubClient) getJSONPage(ctx context.Context, apiBaseURL, token, path string, out any) error {
	_, err := c.getJSON(ctx, apiBaseURL, token, path, out)
	return err
}

// getJSON performs a hardened GET (bounded retries, body cap, rate-limit
// mapping) and decodes the JSON response.
func (c *GitHubClient) getJSON(ctx context.Context, apiBaseURL, token, path string, out any) (http.Header, error) {
	if apiBaseURL == "" {
		apiBaseURL = c.apiURL
	}
	apiBaseURL = strings.TrimRight(apiBaseURL, "/")
	body, header, err := c.doWithRetries(ctx, fmt.Sprintf("%s%s", apiBaseURL, path), token)
	if err != nil {
		return header, err
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return header, fmt.Errorf("decode github response: %w", err)
		}
	}
	return header, nil
}

func (c *GitHubClient) doWithRetries(ctx context.Context, endpoint, token string) ([]byte, http.Header, error) {
	const attempts = 3
	backoff := 200 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
		if err != nil {
			return nil, nil, fmt.Errorf("build github request: %w", err)
		}
		c.standardHeaders(req)
		if token != "" {
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("github request: %w", err)
			if !retryableNetworkError(err) || attempt == attempts-1 {
				return nil, nil, lastErr
			}
			if err := sleepGitHubBackoff(ctx, backoff); err != nil {
				return nil, nil, err
			}
			backoff *= 2
			continue
		}
		body, readErr := readGitHubBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, resp.Header, readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, resp.Header, nil
		}
		if rateLimitError(resp, body) {
			return nil, resp.Header, githubRateLimit(resp)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, resp.Header, fmt.Errorf("%w: github api rejected the token", domain.ErrGitHubTokenRevoked)
		}
		lastErr = fmt.Errorf("github api GET %s: status %d: %s", sanitizeGitHubURL(endpoint), resp.StatusCode, scrubGitHubBody(body))
		if (resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout) && attempt < attempts-1 {
			if err := sleepGitHubBackoff(ctx, backoff); err != nil {
				return nil, resp.Header, err
			}
			backoff *= 2
			continue
		}
		return nil, resp.Header, lastErr
	}
	return nil, nil, lastErr
}

func (c *GitHubClient) standardHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", githubUserAgent)
}

func readGitHubBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxGitHubResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read github response: %w", err)
	}
	if len(data) > maxGitHubResponse {
		return nil, fmt.Errorf("github response exceeds %d bytes", maxGitHubResponse)
	}
	return data, nil
}

func retryableNetworkError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"connection reset", "broken pipe", "unexpected eof", "timeout", "temporarily unavailable"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func sleepGitHubBackoff(ctx context.Context, backoff time.Duration) error {
	// Full jitter in [0, backoff), respecting cancellation.
	delay := time.Duration(rand.Int63n(int64(backoff))) //nolint:gosec // non-cryptographic jitter
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func rateLimitError(resp *http.Response, body []byte) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	return resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(string(body)), "rate limit")
}

func githubRateLimit(resp *http.Response) error {
	out := &domain.GitHubRateLimitError{}
	if raw := resp.Header.Get("Retry-After"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil {
			out.RetryAfter = time.Duration(seconds) * time.Second
		} else if at, err := http.ParseTime(raw); err == nil {
			out.RetryAfter = time.Until(at)
		}
	}
	if raw := resp.Header.Get("X-RateLimit-Reset"); raw != "" {
		if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil {
			out.ResetAt = time.Unix(epoch, 0).UTC()
		}
	}
	return out
}

// scrubGitHubBody truncates an upstream body snippet to printable ASCII and
// removes token-like prefixes so credentials never appear in errors (CWE-209).
func scrubGitHubBody(body []byte) string {
	s := string(body)
	for _, prefix := range []string{"gho_", "ghp_", "github_pat_", "ghr_"} {
		for {
			idx := strings.Index(s, prefix)
			if idx < 0 {
				break
			}
			end := idx + len(prefix)
			for end < len(s) {
				c := s[end]
				if c == ' ' || c == '"' || c == '\n' || c == '\r' || c == '\t' || c == ',' || c == '}' {
					break
				}
				end++
			}
			s = fmt.Sprintf("%s[redacted]%s", s[:idx], s[end:])
		}
	}
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxGitHubErrorSnippet {
			break
		}
		if r >= 0x20 && r <= 0x7e {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sanitizeGitHubURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "github api"
	}
	return fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Path)
}

func splitScopes(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		scope := strings.TrimSpace(part)
		if scope != "" {
			out = append(out, scope)
		}
	}
	return out
}

// pathSegment escapes one GitHub path segment and rejects traversal.
func pathSegment(segment string) (string, error) {
	if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "/\\") {
		return "", fmt.Errorf("%w: invalid github path segment", domain.ErrValidation)
	}
	return url.PathEscape(segment), nil
}

// githubRepoPath builds the escaped "owner/repo" path element.
func githubRepoPath(repo string) (string, error) {
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	if len(parts) == 3 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	if len(parts) != 2 {
		return "", fmt.Errorf("%w: github repo must be owner/name", domain.ErrValidation)
	}
	owner, err := pathSegment(parts[0])
	if err != nil {
		return "", err
	}
	name, err := pathSegment(parts[1])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s", owner, name), nil
}
