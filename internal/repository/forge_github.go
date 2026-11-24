package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
	repoPath, err := githubRepoPath(source.Repo)
	if err != nil {
		return "", err
	}
	body, err := jsonBody(map[string]any{
		"name":   "web",
		"active": true,
		// Narrow to the events Cloud Checks acts on (push covers branch + tag);
		// the handler-side classifier stays authoritative for pre-existing ["*"]
		// hooks.
		"events": []string{"push", "pull_request", "release"},
		"config": map[string]string{"url": callbackURL, "content_type": "json", "secret": secret},
	})
	if err != nil {
		return "", err
	}
	data, err := forgeRequest(ctx, "POST", fmt.Sprintf("%s/repos/%s/hooks", g.apiBase(source), repoPath), "Authorization", fmt.Sprintf("Bearer %s", token), body, "application/json")
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
	auth := fmt.Sprintf("Bearer %s", token)
	repoPath, err := githubRepoPath(source.Repo)
	if err != nil {
		return err
	}
	if webhookID != "" {
		hookID, idErr := pathSegment(webhookID)
		if idErr != nil {
			return idErr
		}
		_, err := forgeRequest(ctx, "DELETE", fmt.Sprintf("%s/repos/%s/hooks/%s", g.apiBase(source), repoPath, hookID), "Authorization", auth, nil, "")
		return err
	}
	listURL := fmt.Sprintf("%s/repos/%s/hooks", g.apiBase(source), repoPath)
	return deleteMatchingHooks(ctx, listURL, "Authorization", auth, source, func(id int64) string {
		return fmt.Sprintf("%s/repos/%s/hooks/%d", g.apiBase(source), repoPath, id)
	})
}

func (g *GitHubForge) VerifyWebhook(_ *domain.ForgeSource, secret string, headers map[string]string, body []byte) bool {
	return verifyHMACSHA256(secret, body, headers["X-Hub-Signature-256"])
}

func (g *GitHubForge) SetCommitStatus(ctx context.Context, source *domain.ForgeSource, token string, run *domain.CheckRun) error {
	repoPath, err := githubRepoPath(source.Repo)
	if err != nil {
		return err
	}
	sha := url.PathEscape(run.HeadSHA)
	body, err := jsonBody(map[string]string{
		"state":       string(run.State),
		"context":     run.Name,
		"description": run.Title,
		"target_url":  run.DetailsURL,
	})
	if err != nil {
		return err
	}
	_, err = forgeRequest(ctx, "POST", fmt.Sprintf("%s/repos/%s/statuses/%s", g.apiBase(source), repoPath, sha), "Authorization", fmt.Sprintf("Bearer %s", token), body, "application/json")
	return err
}

func (g *GitHubForge) CreateCheckRun(ctx context.Context, source *domain.ForgeSource, token string, run *domain.CheckRun) error {
	repoPath, err := githubRepoPath(source.Repo)
	if err != nil {
		return err
	}
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
	_, err = forgeRequest(ctx, "POST", fmt.Sprintf("%s/repos/%s/check-runs", g.apiBase(source), repoPath), "Authorization", fmt.Sprintf("Bearer %s", token), body, "application/json")
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
