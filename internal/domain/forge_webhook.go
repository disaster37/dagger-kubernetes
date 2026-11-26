package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WebhookDisposition is the classification of one forge delivery: whether it is
// actionable, the reason when it is not, the raw provider event name, and the
// parsed event for accepted deliveries.
type WebhookDisposition struct {
	Accepted bool
	Reason   string
	RawEvent string
	Event    *WebhookEvent
}

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

// ClassifyAndParseForgeWebhook decides whether a delivery is actionable before
// parsing it. Non-actionable deliveries carry a reason and never produce an
// event, so callers can ACK them without any project/record/dispatch write.
func ClassifyAndParseForgeWebhook(provider ForgeKind, rawEvent string, body []byte) (*WebhookDisposition, error) {
	switch provider {
	case ForgeGitHub, ForgeGitea:
		return classifyGitStyleWebhook(provider, rawEvent, body)
	case ForgeGitLab:
		return classifyGitLabWebhook(rawEvent, body)
	default:
		return nil, fmt.Errorf("unsupported forge %q", provider)
	}
}

// classifyGitStyleWebhook classifies GitHub/Gitea deliveries (shared payload
// shape) and parses accepted events.
func classifyGitStyleWebhook(provider ForgeKind, rawEvent string, body []byte) (*WebhookDisposition, error) {
	d := &WebhookDisposition{RawEvent: rawEvent}
	switch rawEvent {
	case "ping":
		d.Reason = "ping"
		return d, nil
	case "push":
		var payload struct {
			Ref   string `json:"ref"`
			After string `json:"after"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode %s webhook: %w", provider, err)
		}
		if isDeletedRef(payload.Ref, payload.After) {
			d.Reason = "ref_deleted"
			return d, nil
		}
	case "pull_request":
		var payload struct {
			Action      string `json:"action"`
			PullRequest struct {
				Head struct {
					SHA string `json:"sha"`
				} `json:"head"`
			} `json:"pull_request"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode %s webhook: %w", provider, err)
		}
		if !pullRequestActionAllowed(payload.Action) || payload.PullRequest.Head.SHA == "" {
			d.Reason = "unsupported_action"
			return d, nil
		}
	case "release":
		var payload struct {
			Action  string `json:"action"`
			Release struct {
				TagName string `json:"tag_name"`
			} `json:"release"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode %s webhook: %w", provider, err)
		}
		if !releaseActionAllowed(payload.Action) || payload.Release.TagName == "" {
			d.Reason = "unsupported_action"
			return d, nil
		}
	default:
		d.Reason = "unsupported_event"
		return d, nil
	}
	ev, err := ParseForgeWebhook(provider, rawEvent, body)
	if err != nil {
		return nil, err
	}
	if rawEvent == "release" && ev.Tag != "" {
		ev.Ref = fmt.Sprintf("refs/tags/%s", ev.Tag)
	}
	d.Accepted = true
	d.Event = ev
	return d, nil
}

// classifyGitLabWebhook classifies GitLab deliveries and parses accepted ones.
func classifyGitLabWebhook(rawEvent string, body []byte) (*WebhookDisposition, error) {
	d := &WebhookDisposition{RawEvent: rawEvent}
	switch rawEvent {
	case "Push Hook", "Tag Push Hook":
		var payload struct {
			Ref   string `json:"ref"`
			After string `json:"after"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode gitlab webhook: %w", err)
		}
		if isDeletedRef(payload.Ref, payload.After) {
			d.Reason = "ref_deleted"
			return d, nil
		}
	case "Merge Request Hook":
		var payload struct {
			ObjectAttributes struct {
				Action string `json:"action"`
			} `json:"object_attributes"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode gitlab webhook: %w", err)
		}
		if !mergeRequestActionAllowed(payload.ObjectAttributes.Action) {
			d.Reason = "unsupported_action"
			return d, nil
		}
	case "Release Hook":
		var payload struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode gitlab webhook: %w", err)
		}
		if payload.Action != "" && payload.Action != "create" {
			d.Reason = "unsupported_action"
			return d, nil
		}
	default:
		d.Reason = "unsupported_event"
		return d, nil
	}
	ev, err := ParseForgeWebhook(ForgeGitLab, rawEvent, body)
	if err != nil {
		return nil, err
	}
	d.Accepted = true
	d.Event = ev
	return d, nil
}

// isDeletedRef reports whether a push deletes its ref (branch/tag removal).
func isDeletedRef(ref, after string) bool {
	return after == "" || isAllZeroSHA(after) || ref == ""
}

func isAllZeroSHA(s string) bool {
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return s != ""
}

// pullRequestActionAllowed reports whether a GitHub/Gitea pull_request action
// should trigger checks. "" is accepted to preserve synthetic fixtures.
func pullRequestActionAllowed(action string) bool {
	switch action {
	case "", "opened", "synchronize", "synchronized", "reopened":
		return true
	default:
		return false
	}
}

// releaseActionAllowed reports whether a GitHub/Gitea release action should
// trigger checks. "" is accepted to preserve synthetic fixtures.
func releaseActionAllowed(action string) bool {
	switch action {
	case "", "published", "created":
		return true
	default:
		return false
	}
}

// mergeRequestActionAllowed reports whether a GitLab merge-request action should
// trigger checks.
func mergeRequestActionAllowed(action string) bool {
	switch action {
	case "open", "update", "reopen":
		return true
	default:
		return false
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
	case "push":
		ev.Event = "push"
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
