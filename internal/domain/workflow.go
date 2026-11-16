package domain

import (
	"context"
	"time"
)

// Workflow is a Dagger-native CD workflow parsed from
// .dagger/workflows/*.y{a,}ml.
type Workflow struct {
	Name        string                   `json:"name"`
	On          WorkflowTriggers         `json:"on"`
	AfterChecks bool                     `json:"after_checks"`
	Jobs        map[string]WorkflowJob   `json:"jobs"`
	Inputs      map[string]WorkflowInput `json:"inputs,omitempty"`
}

// WorkflowTriggers declares which events start a workflow.
type WorkflowTriggers struct {
	Push             bool              `json:"push,omitempty"`
	PullRequest      bool              `json:"pull_request,omitempty"`
	Tag              bool              `json:"tag,omitempty"`
	Release          bool              `json:"release,omitempty"`
	Schedule         []string          `json:"schedule,omitempty"`
	WorkflowDispatch *WorkflowDispatch `json:"workflow_dispatch,omitempty"`
	WorkflowCall     bool              `json:"workflow_call,omitempty"`
	Artifact         []string          `json:"artifact,omitempty"`
}

// WorkflowDispatch declares typed manual inputs.
type WorkflowDispatch struct {
	Inputs map[string]WorkflowInput `json:"inputs,omitempty"`
}

// WorkflowInput is a typed workflow_dispatch input.
type WorkflowInput struct {
	Type        string   `json:"type"` // string | boolean | number | choice | artifact
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// WorkflowJob is one job in a workflow.
type WorkflowJob struct {
	Call            string             `json:"call,omitempty"`
	Trigger         *WorkflowTrigger   `json:"trigger,omitempty"`
	Needs           []string           `json:"needs,omitempty"`
	If              string             `json:"if,omitempty"`
	Outputs         map[string]string  `json:"outputs,omitempty"`
	Artifacts       []WorkflowArtifact `json:"artifacts,omitempty"`
	UsesArtifacts   []string           `json:"uses_artifacts,omitempty"`
	Environment     string             `json:"environment,omitempty"`
	Timeout         time.Duration      `json:"timeout,omitempty"`
	ApprovalTimeout time.Duration      `json:"approval_timeout,omitempty"`
	Secrets         []string           `json:"secrets,omitempty"`
}

// WorkflowTrigger starts another repository's workflow natively.
type WorkflowTrigger struct {
	Repo     string         `json:"repo"`
	Workflow string         `json:"workflow"`
	Wait     bool           `json:"wait,omitempty"`
	Timeout  time.Duration  `json:"timeout,omitempty"`
	Inputs   map[string]any `json:"inputs,omitempty"`
}

// WorkflowArtifact declares an artifact produced by a call job.
type WorkflowArtifact struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// WorkflowGitContext is the git context exposed to workflow expressions.
type WorkflowGitContext struct {
	SHA             string `json:"sha"`
	ShortSHA        string `json:"short_sha"`
	Ref             string `json:"ref"`
	Branch          string `json:"branch"`
	TargetBranch    string `json:"target_branch,omitempty"`
	DefaultBranch   string `json:"default_branch,omitempty"`
	Tag             string `json:"tag,omitempty"`
	TagVersion      string `json:"tag_version,omitempty"`
	Repo            string `json:"repo"`
	PRNumber        int    `json:"pr_number,omitempty"`
	IsBranch        bool   `json:"is_branch"`
	IsDefaultBranch bool   `json:"is_default_branch"`
	IsPR            bool   `json:"is_pr"`
	IsTag           bool   `json:"is_tag"`
	IsRelease       bool   `json:"is_release"`
}

// WorkflowArtifactRef references an artifact that triggered a run.
type WorkflowArtifactRef struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	SHA256    string `json:"sha256,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

// WorkflowDef is an indexed workflow catalog entry.
type WorkflowDef struct {
	ID           string            `json:"id"` // "<org_id>/<project_id>/<workflow_name>"
	OrgID        string            `json:"org_id"`
	ProjectID    string            `json:"project_id"`
	Repo         string            `json:"repo"`
	DefaultRef   string            `json:"default_ref"`
	DefaultSHA   string            `json:"default_sha"`
	Workflow     Workflow          `json:"workflow"`
	ContextFiles map[string][]byte `json:"context_files,omitempty"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// WorkflowRun is one execution of a workflow.
type WorkflowRun struct {
	ID              string                    `json:"id"`
	Workflow        string                    `json:"workflow"`
	OrgID           string                    `json:"org_id"`
	ProjectID       string                    `json:"project_id"`
	SHA             string                    `json:"sha"`
	Ref             string                    `json:"ref"`
	Status          string                    `json:"status"`
	Number          int64                     `json:"number"`
	Jobs            map[string]WorkflowJobRun `json:"jobs"`
	Spec            *Workflow                 `json:"spec,omitempty"`
	Git             WorkflowGitContext        `json:"git"`
	Inputs          map[string]any            `json:"inputs,omitempty"`
	TriggerArtifact *WorkflowArtifactRef      `json:"trigger_artifact,omitempty"`
	ContextFiles    map[string][]byte         `json:"-"`
	TriggeredBy     string                    `json:"triggered_by,omitempty"`
	CreatedAt       time.Time                 `json:"created_at"`
	UpdatedAt       time.Time                 `json:"updated_at"`
}

// WorkflowJobRun is the state of one job in a run.
type WorkflowJobRun struct {
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	TraceID          string            `json:"trace_id,omitempty"`
	Outputs          map[string]string `json:"outputs,omitempty"`
	Artifacts        []string          `json:"artifacts,omitempty"`
	Environment      string            `json:"environment,omitempty"`
	WaitingSince     *time.Time        `json:"waiting_since,omitempty"`
	ApprovalDeadline *time.Time        `json:"approval_deadline,omitempty"`
	ApprovedBy       string            `json:"approved_by,omitempty"`
	ApprovedAt       *time.Time        `json:"approved_at,omitempty"`
	RejectedBy       string            `json:"rejected_by,omitempty"`
	TriggeredRunID   string            `json:"triggered_run_id,omitempty"`
	Error            string            `json:"error,omitempty"`
}

// WorkflowExprEvaluator evaluates ${{ ... }} expressions in a sandbox.
type WorkflowExprEvaluator interface {
	Eval(expression string, env map[string]any, repoRoot string) (any, error)
}

// WorkflowFileParser parses a workflow YAML document.
type WorkflowFileParser interface {
	Parse(name string, data []byte) (*Workflow, error)
}

// ArtifactStore stores workflow artifacts.
type ArtifactStore interface {
	Put(ctx context.Context, orgID, projectID, runID, name string, data []byte) (string, error)
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, orgID, projectID, runID string) ([]string, error)
	DeleteRun(ctx context.Context, orgID, projectID, runID string) error
}

// WorkflowCallSpec is one call: job to execute.
type WorkflowCallSpec struct {
	RunID         string
	OrgID         string
	ProjectID     string
	JobName       string
	Call          string
	Environment   string
	TraceID       string
	Secrets       map[string]string
	Timeout       time.Duration
	Repo          string
	SHA           string
	Ref           string
	Token         string
	CloneToken    string
	Flags         []string
	Artifacts     []WorkflowArtifact
	UsesArtifacts []string
}

// WorkflowJobRunner executes call: jobs (implemented in repository).
type WorkflowJobRunner interface {
	RunCall(ctx context.Context, spec *WorkflowCallSpec) (map[string]string, error)
}

// WorkflowRunFilter filters workflow run listings.
type WorkflowRunFilter struct {
	OrgID     string
	ProjectID string
	Workflow  string
	Status    string
	Limit     int
}

// WorkflowRepository persists the workflow catalog and replicated runs.
type WorkflowRepository interface {
	SyncProjectCatalog(ctx context.Context, orgID, projectID string, defs []*WorkflowDef) error
	ListDefs(ctx context.Context, orgID, projectID string) ([]*WorkflowDef, error)
	GetDef(ctx context.Context, orgID, projectID, workflowName string) (*WorkflowDef, error)
	CreateRun(ctx context.Context, run *WorkflowRun) (*WorkflowRun, error) // atomically assigns Number & ID via Raft
	UpsertRun(ctx context.Context, run *WorkflowRun) error
	GetRun(ctx context.Context, runID string) (*WorkflowRun, error)
	ListRuns(ctx context.Context, f *WorkflowRunFilter) ([]*WorkflowRun, error)
	DeleteRun(ctx context.Context, runID string) error
}
