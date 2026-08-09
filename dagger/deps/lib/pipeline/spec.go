package pipeline

import "github.com/disaster37/dagger-library-go/lib/ci"

// PipelineSpec is a CI-agnostic description of a release/CI pipeline.
type PipelineSpec struct {
	CI              ci.CI
	ModuleRef       string            // e.g. "github.com/disaster37/dagger-library-go/helm@2.0.3" (required)
	DaggerVersion   string            // empty = engine default
	Branches        []string          // default ["main"]
	DefaultBranch   string            // default "main"; branch commits land here on tag
	Triggers        Triggers
	Job             Job
	Registry        string            // empty = no push; renderer omits push flags
	Repository      string            // required when Registry != ""
	SrcDir          string            // directory to pass as --src for the constructor (default ".")
	VersionStrategy VersionStrategy
	TimeoutMinutes  int               // 0 = renderer default (GitHub: none, Jenkins: 10, GitLab: 30)
	ExtraFiles      map[string]string // additional filename->content emitted alongside
}

// Triggers control which events cause the pipeline to run.
type Triggers struct {
	Push        bool // default true
	PullRequest bool // default true
	Tag         bool // default true
	Release     bool // default false (GitHub only)
}

// Job describes the `dagger call` invocation.
type Job struct {
	Function     string              // e.g. "ci", "release" (required)
	Args         []string            // ordered flags
	Placeholders map[string]Binding  // canonical + custom placeholder bindings
}

// BindingKind selects how a placeholder value is sourced at CI runtime.
type BindingKind int

const (
	BindingSecret  BindingKind = iota // Ref = CI secret/credential name
	BindingExpr                       // Ref = CI-native expression (renderer must support)
	BindingLiteral                    // Ref = plain string, emitted verbatim
)

// Binding maps a placeholder name to its runtime source.
type Binding struct {
	Kind BindingKind
	Ref  string
}

// Canonical placeholder names.
const (
	PhVersion      = "version"
	PhBranch       = "branch"
	PhGitRepoURL   = "git-repo-url"
	PhGitToken     = "git-token"
	PhRegistryUser = "registry-username"
	PhRegistryPass = "registry-password"
)
