package repository

import (
	"fmt"
	"strings"

	raftconfig "github.com/disaster37/go-raft/config"
	"gopkg.in/yaml.v3"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// WorkflowFileParser parses .dagger/workflows/*.y{a,}ml documents.
type WorkflowFileParser struct{}

var _ domain.WorkflowFileParser = (*WorkflowFileParser)(nil)

// NewWorkflowFileParser returns a parser.
func NewWorkflowFileParser() *WorkflowFileParser { return &WorkflowFileParser{} }

type rawWorkflow struct {
	Name        string              `yaml:"name"`
	On          rawTriggers         `yaml:"on"`
	AfterChecks *bool               `yaml:"after_checks"`
	Jobs        map[string]rawJob   `yaml:"jobs"`
	Inputs      map[string]rawInput `yaml:"inputs"`
}

type rawTriggers struct {
	Push             any          `yaml:"push"`
	PullRequest      any          `yaml:"pull_request"`
	Tag              any          `yaml:"tag"`
	Release          any          `yaml:"release"`
	Schedule         any          `yaml:"schedule"`
	WorkflowDispatch *rawDispatch `yaml:"workflow_dispatch"`
	WorkflowCall     any          `yaml:"workflow_call"`
	Artifact         any          `yaml:"artifact"`
}

type rawDispatch struct {
	Inputs map[string]rawInput `yaml:"inputs"`
}

type rawInput struct {
	Type        string   `yaml:"type"`
	Description string   `yaml:"description"`
	Required    bool     `yaml:"required"`
	Default     any      `yaml:"default"`
	Options     []string `yaml:"options"`
}

type rawJob struct {
	Call            string            `yaml:"call"`
	Trigger         *rawTrigger       `yaml:"trigger"`
	Needs           any               `yaml:"needs"`
	If              string            `yaml:"if"`
	Outputs         map[string]string `yaml:"outputs"`
	Artifacts       []rawArtifact     `yaml:"artifacts"`
	UsesArtifacts   any               `yaml:"uses_artifacts"`
	Environment     string            `yaml:"environment"`
	Timeout         string            `yaml:"timeout"`
	ApprovalTimeout string            `yaml:"approval_timeout"`
	Secrets         any               `yaml:"secrets"`
}

type rawTrigger struct {
	Repo     string         `yaml:"repo"`
	Workflow string         `yaml:"workflow"`
	Wait     bool           `yaml:"wait"`
	Timeout  string         `yaml:"timeout"`
	Inputs   map[string]any `yaml:"inputs"`
}

type rawArtifact struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Parse parses one workflow document.
func (p *WorkflowFileParser) Parse(name string, data []byte) (*domain.Workflow, error) {
	var raw rawWorkflow
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse workflow %s: %w", name, err)
	}
	wf := &domain.Workflow{
		Name:        raw.Name,
		AfterChecks: true,
		Jobs:        make(map[string]domain.WorkflowJob, len(raw.Jobs)),
		Inputs:      make(map[string]domain.WorkflowInput, len(raw.Inputs)),
	}
	if wf.Name == "" {
		wf.Name = strings.TrimSuffix(name, ".yaml")
	}
	if raw.AfterChecks != nil {
		wf.AfterChecks = *raw.AfterChecks
	}
	wf.On = domain.WorkflowTriggers{
		Push:         truthy(raw.On.Push),
		PullRequest:  truthy(raw.On.PullRequest),
		Tag:          truthy(raw.On.Tag),
		Release:      truthy(raw.On.Release),
		Schedule:     stringList(raw.On.Schedule),
		WorkflowCall: truthy(raw.On.WorkflowCall),
		Artifact:     stringList(raw.On.Artifact),
	}
	if raw.On.WorkflowDispatch != nil {
		wf.On.WorkflowDispatch = &domain.WorkflowDispatch{Inputs: convertInputs(raw.On.WorkflowDispatch.Inputs)}
	}
	for k := range raw.Inputs {
		in := raw.Inputs[k]
		wf.Inputs[k] = convertInput(&in)
	}
	for jobName := range raw.Jobs {
		rj := raw.Jobs[jobName]
		job, err := convertJob(&rj)
		if err != nil {
			return nil, fmt.Errorf("workflow %s job %s: %w", name, jobName, err)
		}
		wf.Jobs[jobName] = job
	}
	return wf, nil
}

func convertJob(rj *rawJob) (domain.WorkflowJob, error) {
	job := domain.WorkflowJob{
		Call:          rj.Call,
		If:            rj.If,
		Outputs:       rj.Outputs,
		Environment:   rj.Environment,
		UsesArtifacts: stringList(rj.UsesArtifacts),
		Secrets:       stringList(rj.Secrets),
		Needs:         stringList(rj.Needs),
	}
	for _, a := range rj.Artifacts {
		job.Artifacts = append(job.Artifacts, domain.WorkflowArtifact{Name: a.Name, Path: a.Path})
	}
	if rj.Timeout != "" {
		d, err := raftconfig.ParseExtendedDuration(rj.Timeout)
		if err != nil {
			return job, fmt.Errorf("timeout: %w", err)
		}
		job.Timeout = d
	}
	if rj.ApprovalTimeout != "" {
		d, err := raftconfig.ParseExtendedDuration(rj.ApprovalTimeout)
		if err != nil {
			return job, fmt.Errorf("approval timeout: %w", err)
		}
		job.ApprovalTimeout = d
	}
	if rj.Trigger != nil {
		tr := &domain.WorkflowTrigger{Repo: rj.Trigger.Repo, Workflow: rj.Trigger.Workflow, Wait: rj.Trigger.Wait, Inputs: rj.Trigger.Inputs}
		if rj.Trigger.Timeout != "" {
			d, err := raftconfig.ParseExtendedDuration(rj.Trigger.Timeout)
			if err != nil {
				return job, fmt.Errorf("trigger timeout: %w", err)
			}
			tr.Timeout = d
		}
		job.Trigger = tr
	}
	return job, nil
}

func convertInputs(in map[string]rawInput) map[string]domain.WorkflowInput {
	out := make(map[string]domain.WorkflowInput, len(in))
	for k := range in {
		v := in[k]
		out[k] = convertInput(&v)
	}
	return out
}

func convertInput(in *rawInput) domain.WorkflowInput {
	return domain.WorkflowInput{Type: in.Type, Description: in.Description, Required: in.Required, Default: in.Default, Options: in.Options}
}

// truthy interprets a YAML trigger value: absent/null/false is off; a map,
// list, string, or true is on.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != "" && t != "false"
	default:
		return true
	}
}

// stringList normalizes a YAML scalar or list into []string.
func stringList(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	default:
		return nil
	}
}
