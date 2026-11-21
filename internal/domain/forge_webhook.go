package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParseForgeWebhook normalizes a provider webhook payload into WebhookEvent.
func ParseForgeWebhook(provider ForgeKind, event string, body []byte) (*WebhookEvent, error) {
	switch provider {
	case ForgeGitHub:
		return parseGitHubWebhook(event, body)
	case ForgeGitLab:
		return parseGitLabWebhook(event, body)
	case ForgeGitea:
		return parseGiteaWebhook(event, body)
	default:
		return nil, fmt.Errorf("unsupported forge %q", provider)
	}
}

func parseGitHubWebhook(event string, body []byte) (*WebhookEvent, error) {
	return parseGitStyleWebhook(ForgeGitHub, event, body)
}

// parseGitStyleWebhook handles the GitHub/Gitea shared payload shape.
func parseGitStyleWebhook(provider ForgeKind, event string, body []byte) (*WebhookEvent, error) {
	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		PullRequest struct {
			Number int `json:"number"`
			Head   struct {
				SHA  string `json:"sha"`
				Ref  string `json:"ref"`
				Repo struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref  string `json:"ref"`
				Repo struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"base"`
		} `json:"pull_request"`
		Release struct {
			TagName string `json:"tag_name"`
		} `json:"release"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode %s webhook: %w", provider, err)
	}
	ev := &WebhookEvent{Provider: provider, Event: event, Repo: payload.Repository.FullName, SHA: payload.After, Ref: payload.Ref, DefaultBranch: payload.Repository.DefaultBranch}
	switch event {
	case "push":
		if strings.HasPrefix(payload.Ref, "refs/tags/") {
			ev.Event = "tag"
			ev.Tag = strings.TrimPrefix(payload.Ref, "refs/tags/")
		}
	case "pull_request":
		ev.PRNumber = payload.PullRequest.Number
		ev.SHA = payload.PullRequest.Head.SHA
		ev.TargetBranch = payload.PullRequest.Base.Ref
		ev.SourceRepoID = fmt.Sprintf("%d", payload.PullRequest.Head.Repo.ID)
		ev.TargetRepoID = fmt.Sprintf("%d", payload.PullRequest.Base.Repo.ID)
		ev.IsFork = ev.SourceRepoID != ev.TargetRepoID
	case "release":
		ev.Release = payload.Release.TagName
		ev.Tag = payload.Release.TagName
	}
	return ev, nil
}

func parseGitLabWebhook(event string, body []byte) (*WebhookEvent, error) {
	var payload struct {
		ObjectKind  string `json:"object_kind"`
		Ref         string `json:"ref"`
		After       string `json:"after"`
		CheckoutSHA string `json:"checkout_sha"`
		Project     struct {
			PathWithNamespace string `json:"path_with_namespace"`
			DefaultBranch     string `json:"default_branch"`
		} `json:"project"`
		ObjectAttributes struct {
			IID          int    `json:"iid"`
			TargetBranch string `json:"target_branch"`
			SourceBranch string `json:"source_branch"`
			Source       struct {
				ID int64 `json:"id"`
			} `json:"source"`
			Target struct {
				ID int64 `json:"id"`
			} `json:"target"`
			LastCommit struct {
				ID string `json:"id"`
			} `json:"last_commit"`
		} `json:"object_attributes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode gitlab webhook: %w", err)
	}
	ev := &WebhookEvent{Provider: ForgeGitLab, Event: event, Repo: payload.Project.PathWithNamespace, SHA: payload.After, Ref: payload.Ref, DefaultBranch: payload.Project.DefaultBranch}
	if ev.SHA == "" {
		ev.SHA = payload.CheckoutSHA
	}
	switch payload.ObjectKind {
	case "merge_request":
		ev.Event = "pull_request"
		ev.PRNumber = payload.ObjectAttributes.IID
		ev.SHA = payload.ObjectAttributes.LastCommit.ID
		ev.TargetBranch = payload.ObjectAttributes.TargetBranch
		ev.SourceRepoID = fmt.Sprintf("%d", payload.ObjectAttributes.Source.ID)
		ev.TargetRepoID = fmt.Sprintf("%d", payload.ObjectAttributes.Target.ID)
		ev.IsFork = ev.SourceRepoID != ev.TargetRepoID
	case "tag_push":
		ev.Event = "tag"
		ev.Tag = strings.TrimPrefix(payload.Ref, "refs/tags/")
	case "release":
		ev.Event = "release"
	}
	return ev, nil
}

func parseGiteaWebhook(event string, body []byte) (*WebhookEvent, error) {
	return parseGitStyleWebhook(ForgeGitea, event, body)
}
