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
	Provider      ForgeKind
	Event         string // push | tag | pull_request | release
	Action        string
	Repo          string
	SHA           string
	Ref           string
	DefaultBranch string
	TargetBranch  string
	Tag           string
	PRNumber      int
	SourceRepoID  string
	TargetRepoID  string
	IsFork        bool
	Release       string
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
// RegisterWebhook/DeleteWebhook/SetCommitStatus/CreateCheckRun receive the
// PLAINTEXT credential (token) — never the sealed ciphertext stored on
// ForgeSource.
type ForgeProvider interface {
	Kind() ForgeKind
	RegisterWebhook(ctx context.Context, source *ForgeSource, token, callbackURL, secret string) (string, error)
	DeleteWebhook(ctx context.Context, source *ForgeSource, token, webhookID string) error
	VerifyWebhook(source *ForgeSource, secret string, headers map[string]string, body []byte) bool
	SetCommitStatus(ctx context.Context, source *ForgeSource, token string, run *CheckRun) error
	CreateCheckRun(ctx context.Context, source *ForgeSource, token string, run *CheckRun) error
}

// CheckItem is one discovered check and its live/terminal state.
type CheckItem struct {
	Name      string     `json:"name"`
	State     CheckState `json:"state"`
	TraceID   string     `json:"trace_id,omitempty"`
	Summary   string     `json:"summary,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// CheckRunRecord is the replicated Cloud Checks run state.
type CheckRunRecord struct {
	ID             string            `json:"id"`
	OrgID          string            `json:"org_id"`
	ProjectID      string            `json:"project_id"`
	SourceID       string            `json:"source_id"`
	Provider       ForgeKind         `json:"provider"`
	Repo           string            `json:"repo"`
	Event          string            `json:"event"`
	SHA            string            `json:"sha"`
	Ref            string            `json:"ref"`
	DefaultBranch  string            `json:"default_branch,omitempty"`
	TargetBranch   string            `json:"target_branch,omitempty"`
	Tag            string            `json:"tag,omitempty"`
	PRNumber       int               `json:"pr_number,omitempty"`
	IsFork         bool              `json:"is_fork"`
	IsRelease      bool              `json:"is_release"`
	Status         string            `json:"status"` // waiting_approval | running | success | failed | canceled
	IncludeSecrets bool              `json:"include_secrets"`
	ApprovedBy     string            `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time        `json:"approved_at,omitempty"`
	Checks         []CheckItem       `json:"checks,omitempty"`
	WorkflowFiles  map[string]string `json:"workflow_files,omitempty"` // raw .dagger/workflows/*.yaml captured in load phase
	ContextFiles   map[string][]byte `json:"context_files,omitempty"`  // <=1MiB structured files referenced by file()/next_semver()
	Error          string            `json:"error,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// CheckRunFilter filters CheckRunRecord listings.
type CheckRunFilter struct {
	OrgID     string
	ProjectID string
	Status    string
	Repo      string
	Limit     int
}

// CheckRunRepository persists Cloud Checks run records.
type CheckRunRepository interface {
	Upsert(ctx context.Context, r *CheckRunRecord) error
	Get(ctx context.Context, id string) (*CheckRunRecord, error)
	List(ctx context.Context, f *CheckRunFilter) ([]*CheckRunRecord, error)
	Delete(ctx context.Context, id string) error
}

// CheckRunSpec is the leader-side description of one check-run Job.
type CheckRunSpec struct {
	ID            string
	OrgID         string
	ProjectID     string
	SourceID      string
	Provider      ForgeKind
	Repo          string
	SHA           string
	Ref           string
	DefaultBranch string
	TargetBranch  string
	Tag           string
	PRNumber      int
	IsFork        bool
	IsRelease     bool
	CheckName     string
	TraceID       string
	Environment   string
	Token         string
	CloneToken    string
	Event         string
	Secrets       map[string]string
}

// CheckJobRunner executes check-run Jobs (implemented in repository).
type CheckJobRunner interface {
	Run(ctx context.Context, spec *CheckRunSpec) error
	CancelSuperseded(ctx context.Context, projectID, ref string) error
	JobStatus(ctx context.Context, runID string) (JobState, error)
}

// JobState is the observed state of a runner Job.
type JobState string

const (
	JobStateUnknown   JobState = "unknown"
	JobStateActive    JobState = "active"
	JobStateSucceeded JobState = "succeeded"
	JobStateFailed    JobState = "failed"
)
