package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GitHubForge implements domain.ForgeProvider for github.com and GHES.
type GitHubForge struct {
	apiURL string
}

var _ domain.ForgeProvider = (*GitHubForge)(nil)

// NewGitHubForge returns a GitHub provider. apiURL defaults to
// https://api.github.com.
func NewGitHubForge(apiURL string) *GitHubForge {
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	return &GitHubForge{apiURL: strings.TrimSuffix(apiURL, "/")}
}

func (g *GitHubForge) Kind() domain.ForgeKind { return domain.ForgeGitHub }

func (g *GitHubForge) apiBase(source *domain.ForgeSource) string {
	if source != nil && source.BaseURL != "" {
		return strings.TrimSuffix(source.BaseURL, "/")
	}
	return g.apiURL
}

func (g *GitHubForge) RegisterWebhook(ctx context.Context, source *domain.ForgeSource, token, callbackURL, secret string) (string, error) {
	body, err := jsonBody(map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"*"},
		"config": map[string]string{"url": callbackURL, "content_type": "json", "secret": secret},
	})
	if err != nil {
		return "", err
	}
	data, err := forgeRequest(ctx, "POST", fmt.Sprintf("%s/repos/%s/hooks", g.apiBase(source), source.Repo), "Authorization", fmt.Sprintf("Bearer %s", token), body, "application/json")
	if err != nil {
		return "", err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(data, &out)
	return fmt.Sprintf("%d", out.ID), nil
}

func (g *GitHubForge) DeleteWebhook(ctx context.Context, source *domain.ForgeSource, token, webhookID string) error {
	_, err := forgeRequest(ctx, "DELETE", fmt.Sprintf("%s/repos/%s/hooks/%s", g.apiBase(source), source.Repo, webhookID), "Authorization", fmt.Sprintf("Bearer %s", token), nil, "")
	return err
}

func (g *GitHubForge) VerifyWebhook(_ *domain.ForgeSource, secret string, headers map[string]string, body []byte) bool {
	return verifyHMACSHA256(secret, body, headers["X-Hub-Signature-256"])
}

func (g *GitHubForge) SetCommitStatus(ctx context.Context, source *domain.ForgeSource, token string, run *domain.CheckRun) error {
	body, err := jsonBody(map[string]string{
		"state":       string(run.State),
		"context":     run.Name,
		"description": run.Title,
		"target_url":  run.DetailsURL,
	})
	if err != nil {
		return err
	}
	_, err = forgeRequest(ctx, "POST", fmt.Sprintf("%s/repos/%s/statuses/%s", g.apiBase(source), source.Repo, run.HeadSHA), "Authorization", fmt.Sprintf("Bearer %s", token), body, "application/json")
	return err
}

func (g *GitHubForge) CreateCheckRun(ctx context.Context, source *domain.ForgeSource, token string, run *domain.CheckRun) error {
	payload := map[string]any{
		"name":        run.Name,
		"head_sha":    run.HeadSHA,
		"output":      map[string]string{"title": run.Title, "summary": run.Summary},
		"details_url": run.DetailsURL,
	}
	if run.State == domain.CheckPending {
		// A pending check must be reported as in_progress without a conclusion;
		// sending "completed" would prematurely close the check run (CWE-636).
		payload["status"] = "in_progress"
	} else {
		payload["status"] = "completed"
		payload["conclusion"] = githubConclusion(run.State)
	}
	body, err := jsonBody(payload)
	if err != nil {
		return err
	}
	_, err = forgeRequest(ctx, "POST", fmt.Sprintf("%s/repos/%s/check-runs", g.apiBase(source), source.Repo), "Authorization", fmt.Sprintf("Bearer %s", token), body, "application/json")
	return err
}

func githubConclusion(state domain.CheckState) string {
	switch state {
	case domain.CheckSuccess:
		return "success"
	case domain.CheckFailure:
		return "failure"
	case domain.CheckError:
		return "cancelled"
	default:
		return "neutral"
	}
}
