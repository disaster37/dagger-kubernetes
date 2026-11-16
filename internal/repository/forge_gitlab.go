package repository

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GitLabForge implements domain.ForgeProvider for gitlab.com and self-hosted
// GitLab.
type GitLabForge struct {
	apiURL string
}

var _ domain.ForgeProvider = (*GitLabForge)(nil)

// NewGitLabForge returns a GitLab provider.
func NewGitLabForge(apiURL string) *GitLabForge {
	if apiURL == "" {
		apiURL = "https://gitlab.com/api/v4"
	}
	return &GitLabForge{apiURL: strings.TrimSuffix(apiURL, "/")}
}

func (g *GitLabForge) Kind() domain.ForgeKind { return domain.ForgeGitLab }

func (g *GitLabForge) projectPath(repo string) string {
	return url.PathEscape(repo)
}

func (g *GitLabForge) RegisterWebhook(ctx context.Context, source *domain.ForgeSource, token, callbackURL, secret string) (string, error) {
	form := url.Values{}
	form.Set("url", callbackURL)
	form.Set("token", secret)
	form.Set("push_events", "true")
	form.Set("tag_push_events", "true")
	form.Set("merge_requests_events", "true")
	form.Set("releases_events", "true")
	data, err := forgeRequest(ctx, "POST", fmt.Sprintf("%s/projects/%s/hooks", g.apiURL, g.projectPath(source.Repo)), "PRIVATE-TOKEN", token, []byte(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return "", err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(data, &out)
	return fmt.Sprintf("%d", out.ID), nil
}

func (g *GitLabForge) DeleteWebhook(ctx context.Context, source *domain.ForgeSource, token, webhookID string) error {
	_, err := forgeRequest(ctx, "DELETE", fmt.Sprintf("%s/projects/%s/hooks/%s", g.apiURL, g.projectPath(source.Repo), webhookID), "PRIVATE-TOKEN", token, nil, "")
	return err
}

func (g *GitLabForge) VerifyWebhook(_ *domain.ForgeSource, secret string, headers map[string]string, _ []byte) bool {
	// Constant-time compare: the GitLab webhook token is a shared secret and
	// must not leak via a timing side channel (CWE-208).
	return secret != "" && hmac.Equal([]byte(secret), []byte(headers["X-Gitlab-Token"]))
}

func (g *GitLabForge) SetCommitStatus(ctx context.Context, source *domain.ForgeSource, token string, run *domain.CheckRun) error {
	form := url.Values{}
	form.Set("state", gitlabState(run.State))
	form.Set("name", run.Name)
	form.Set("description", run.Title)
	form.Set("target_url", run.DetailsURL)
	_, err := forgeRequest(ctx, "POST", fmt.Sprintf("%s/projects/%s/statuses/%s", g.apiURL, g.projectPath(source.Repo), run.HeadSHA), "PRIVATE-TOKEN", token, []byte(form.Encode()), "application/x-www-form-urlencoded")
	return err
}

func (g *GitLabForge) CreateCheckRun(ctx context.Context, source *domain.ForgeSource, token string, run *domain.CheckRun) error {
	return g.SetCommitStatus(ctx, source, token, run)
}

func gitlabState(state domain.CheckState) string {
	switch state {
	case domain.CheckSuccess:
		return "success"
	case domain.CheckFailure:
		return "failed"
	case domain.CheckError:
		return "canceled"
	default:
		return "pending"
	}
}
