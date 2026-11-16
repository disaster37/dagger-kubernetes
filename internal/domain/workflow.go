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
	Call          string             `json:"call,omitempty"`
	Trigger       *WorkflowTrigger   `json:"trigger,omitempty"`
	Needs         []string           `json:"needs,omitempty"`
	If            string             `json:"if,omitempty"`
	Outputs       map[string]string  `json:"outputs,omitempty"`
	Artifacts     []WorkflowArtifact `json:"artifacts,omitempty"`
	UsesArtifacts []string           `json:"uses_artifacts,omitempty"`
	Environment   string             `json:"environment,omitempty"`
	Timeout       time.Duration      `json:"timeout,omitempty"`
	Secrets       []string           `json:"secrets,omitempty"`
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

// WorkflowRun is one execution of a workflow.
type WorkflowRun struct {
	ID        string                    `json:"id"`
	Workflow  string                    `json:"workflow"`
	OrgID     string                    `json:"org_id"`
	ProjectID string                    `json:"project_id"`
	SHA       string                    `json:"sha"`
	Ref       string                    `json:"ref"`
	Status    string                    `json:"status"`
	Number    int64                     `json:"number"`
	Jobs      map[string]WorkflowJobRun `json:"jobs"`
	CreatedAt time.Time                 `json:"created_at"`
	UpdatedAt time.Time                 `json:"updated_at"`
}

// WorkflowJobRun is the state of one job in a run.
type WorkflowJobRun struct {
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	TraceID    string            `json:"trace_id,omitempty"`
	Outputs    map[string]string `json:"outputs,omitempty"`
	Artifacts  []string          `json:"artifacts,omitempty"`
	ApprovedBy string            `json:"approved_by,omitempty"`
	ApprovedAt *time.Time        `json:"approved_at,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// WorkflowExprEvaluator evaluates ${{ ... }} expressions in a sandbox.
type WorkflowExprEvaluator interface {
	Eval(expr string, env map[string]any) (any, error)
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
	RunID       string
	OrgID       string
	ProjectID   string
	JobName     string
	Call        string
	Environment string
	TraceID     string
	Secrets     map[string]string
	Timeout     time.Duration
}

// WorkflowJobRunner executes call: jobs (implemented in repository).
type WorkflowJobRunner interface {
	RunCall(ctx context.Context, spec *WorkflowCallSpec) (map[string]string, error)
}
