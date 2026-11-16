package domain

import (
	"context"
	"time"
)

// ForgeKind identifies a git forge provider.
type ForgeKind string

const (
	ForgeGitHub ForgeKind = "github"
	ForgeGitLab ForgeKind = "gitlab"
	ForgeGitea  ForgeKind = "gitea"
)

// ForgeSource is a configured repository integration.
type ForgeSource struct {
	ID                      string    `json:"id"`
	OrgID                   string    `json:"org_id"`
	ProjectID               string    `json:"project_id,omitempty"`
	Provider                ForgeKind `json:"provider"`
	BaseURL                 string    `json:"base_url"`
	Repo                    string    `json:"repo"` // owner/repo or group/project
	TokenCiphertext         string    `json:"-"`
	WebhookSecretCiphertext string    `json:"-"`
	AppID                   string    `json:"app_id,omitempty"`
	InstallationID          string    `json:"installation_id,omitempty"`
	PrivateKeyCiphertext    string    `json:"-"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// ForgeSourceRepository persists forge integrations.
type ForgeSourceRepository interface {
	Upsert(ctx context.Context, s *ForgeSource) error
	Get(ctx context.Context, id string) (*ForgeSource, error)
	ListByOrg(ctx context.Context, orgID string) ([]*ForgeSource, error)
	Delete(ctx context.Context, id string) error
}

// WebhookEvent is the normalized forge webhook payload.
type WebhookEvent struct {
	Provider     ForgeKind
	Event        string // push | tag | pull_request | release
	Action       string
	Repo         string
	SHA          string
	Ref          string
	Tag          string
	PRNumber     int
	SourceRepoID string
	TargetRepoID string
	IsFork       bool
	Release      string
}

// CheckState is a commit-status/check-run state.
type CheckState string

const (
	CheckPending CheckState = "pending"
	CheckSuccess CheckState = "success"
	CheckFailure CheckState = "failure"
	CheckError   CheckState = "error"
)

// CheckRun is one check result reported to a forge.
type CheckRun struct {
	Name       string
	HeadSHA    string
	State      CheckState
	Title      string
	Summary    string
	DetailsURL string
}

// ForgeProvider is the outbound forge API adapter (implemented in repository).
type ForgeProvider interface {
	Kind() ForgeKind
	RegisterWebhook(ctx context.Context, source *ForgeSource, callbackURL, secret string) (string, error)
	DeleteWebhook(ctx context.Context, source *ForgeSource, webhookID string) error
	VerifyWebhook(source *ForgeSource, secret string, headers map[string]string, body []byte) bool
	SetCommitStatus(ctx context.Context, source *ForgeSource, token string, run *CheckRun) error
	CreateCheckRun(ctx context.Context, source *ForgeSource, token string, run *CheckRun) error
}

// CheckRunSpec is the leader-side description of one check-run Job.
type CheckRunSpec struct {
	ID          string
	OrgID       string
	ProjectID   string
	SourceID    string
	Provider    ForgeKind
	Repo        string
	SHA         string
	Ref         string
	PRNumber    int
	IsFork      bool
	CheckName   string
	TraceID     string
	Environment string
	Secrets     map[string]string
}

// CheckJobRunner executes check-run Jobs (implemented in repository).
type CheckJobRunner interface {
	Run(ctx context.Context, spec *CheckRunSpec) error
	CancelSuperseded(ctx context.Context, projectID, ref string) error
}
