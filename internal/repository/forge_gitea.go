package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GiteaForge implements domain.ForgeProvider for Gitea/Forgejo.
type GiteaForge struct {
	apiURL string
}

var _ domain.ForgeProvider = (*GiteaForge)(nil)

// NewGiteaForge returns a Gitea provider.
func NewGiteaForge(apiURL string) *GiteaForge {
	return &GiteaForge{apiURL: strings.TrimSuffix(apiURL, "/")}
}

func (g *GiteaForge) Kind() domain.ForgeKind { return domain.ForgeGitea }

func (g *GiteaForge) RegisterWebhook(ctx context.Context, source *domain.ForgeSource, token, callbackURL, secret string) (string, error) {
	body, err := jsonBody(map[string]any{
		"type":   "gitea",
		"active": true,
		"events": []string{"push", "pull_request", "release"},
		"config": map[string]string{"url": callbackURL, "content_type": "json", "secret": secret},
	})
	if err != nil {
		return "", err
	}
	data, err := forgeRequest(ctx, "POST", fmt.Sprintf("%s/repos/%s/hooks", g.apiURL, source.Repo), "Authorization", "token "+token, body, "application/json")
	if err != nil {
		return "", err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(data, &out)
	return fmt.Sprintf("%d", out.ID), nil
}

func (g *GiteaForge) DeleteWebhook(ctx context.Context, source *domain.ForgeSource, token, webhookID string) error {
	_, err := forgeRequest(ctx, "DELETE", fmt.Sprintf("%s/repos/%s/hooks/%s", g.apiURL, source.Repo, webhookID), "Authorization", "token "+token, nil, "")
	return err
}

func (g *GiteaForge) VerifyWebhook(_ *domain.ForgeSource, secret string, headers map[string]string, body []byte) bool {
	return verifyHMACSHA256(secret, body, headers["X-Gitea-Signature"])
}

func (g *GiteaForge) SetCommitStatus(ctx context.Context, source *domain.ForgeSource, token string, run *domain.CheckRun) error {
	body, err := jsonBody(map[string]string{
		"state":       giteaState(run.State),
		"context":     run.Name,
		"description": run.Title,
		"target_url":  run.DetailsURL,
	})
	if err != nil {
		return err
	}
	_, err = forgeRequest(ctx, "POST", fmt.Sprintf("%s/repos/%s/statuses/%s", g.apiURL, source.Repo, run.HeadSHA), "Authorization", "token "+token, body, "application/json")
	return err
}

func (g *GiteaForge) CreateCheckRun(ctx context.Context, source *domain.ForgeSource, token string, run *domain.CheckRun) error {
	return g.SetCommitStatus(ctx, source, token, run)
}

func giteaState(state domain.CheckState) string {
	switch state {
	case domain.CheckSuccess:
		return "success"
	case domain.CheckFailure:
		return "failure"
	case domain.CheckError:
		return "error"
	default:
		return "pending"
	}
}
