package pipeline

import (
	"fmt"
	"strings"

	"github.com/disaster37/dagger-library-go/lib/ci"
	"gopkg.in/yaml.v3"
)

// GitHubRenderer produces GitHub Actions workflow YAML.
type GitHubRenderer struct{}

func (r *GitHubRenderer) Supports(c ci.CI) bool {
	return c == ci.Github
}

func (r *GitHubRenderer) Render(spec PipelineSpec) (map[string]string, error) {
	// Compute secret references for env block
	phMap := spec.Job.Placeholders
	registryUsername := "${{ github.actor }}"
	if b, ok := phMap[PhRegistryUser]; ok && b.Ref != "" && b.Kind == BindingSecret {
		registryUsername = fmt.Sprintf("${{ secrets.%s }}", b.Ref)
	}

	registryPassword := "${{ secrets.GITHUB_TOKEN }}"
	if b, ok := phMap[PhRegistryPass]; ok && b.Ref != "" && b.Kind == BindingSecret {
		registryPassword = fmt.Sprintf("${{ secrets.%s }}", b.Ref)
	}

	// VERSION and BRANCH_NAME expressions matching old template behavior.
	// DefaultBranch is escaped so single quotes do not break GitHub Actions expression string literals.
	versionExpr := `${{ github.ref_type == 'tag' && github.ref_name || github.event_name == 'pull_request' && format('0.0.0-pr.{0}.{1}', github.event.number, github.run_number) || format('0.0.0-rc.{0}', github.run_number) }}`
	branchExpr := fmt.Sprintf(`${{ github.event_name == 'pull_request' && github.head_ref || github.ref_type == 'tag' && '%s' || github.ref_name }}`, ghExprQuote(spec.DefaultBranch))

	args := buildCIArgs(spec, func(ph string, b Binding) string {
		switch ph {
		case PhVersion:
			return "${VERSION}"
		case PhBranch:
			return "${BRANCH_NAME}"
		case PhGitRepoURL:
			return "${{ github.repositoryUrl }}"
		case PhGitToken:
			return "env:GITHUB_TOKEN"
		case PhRegistryUser:
			return "env:REGISTRY_USERNAME"
		case PhRegistryPass:
			return "env:REGISTRY_PASSWORD"
		default:
			switch b.Kind {
			case BindingSecret:
				return "env:" + strings.ToUpper(strings.ReplaceAll(ph, "-", "_"))
			case BindingExpr:
				return b.Ref
			case BindingLiteral:
				return b.Ref
			}
			return ""
		}
	})

	// Shell-quote ModuleRef, Job.Function, and literal arg values to prevent
	// command injection from user-controlled inputs.
	safeArgs := quoteArgsForShell(args)
	srcPrefix := ""
	if spec.SrcDir != "" {
		srcPrefix = fmt.Sprintf("--src %s ", shellQuote(spec.SrcDir))
	}
	fullCmd := fmt.Sprintf("dagger call -m %s %s%s %s export --path .", shellQuote(spec.ModuleRef), srcPrefix, shellQuote(spec.Job.Function), strings.Join(safeArgs, " "))

	type step struct {
		Uses string            `yaml:"uses,omitempty"`
		Name string            `yaml:"name,omitempty"`
		With map[string]string `yaml:"with,omitempty"`
		Env  map[string]string `yaml:"env,omitempty"`
	}

	type job struct {
		Permissions map[string]string `yaml:"permissions"`
		RunsOn      string            `yaml:"runs-on"`
		Steps       []step            `yaml:"steps"`
	}

	type trigger struct {
		Types    []string `yaml:"types,omitempty"`
		Branches []string `yaml:"branches,omitempty"`
		Tags     []string `yaml:"tags,omitempty"`
	}

	type workflow struct {
		Name string                 `yaml:"name"`
		On   map[string]interface{} `yaml:"on"`
		Jobs map[string]job         `yaml:"jobs"`
	}

	on := map[string]interface{}{}
	if spec.Triggers.Push {
		on["push"] = trigger{Branches: spec.Branches, Tags: []string{"*"}}
	}
	if spec.Triggers.PullRequest {
		on["pull_request"] = trigger{Branches: spec.Branches}
	}
	// Release trigger is always emitted for parity with old template behavior
	on["release"] = trigger{Types: []string{"created"}}

	wf := workflow{
		Name: "dagger",
		On:   on,
		Jobs: map[string]job{
			"dagger": {
				Permissions: map[string]string{
					"contents": "write",
					"packages": "write",
				},
				RunsOn: "ubuntu-latest",
				Steps: []step{
					{
						Uses: "actions/checkout@v2",
					},
					{
						Name: "Call Dagger Function",
						Uses: "dagger/dagger-for-github@v7",
						With: map[string]string{
							"version":     spec.DaggerVersion,
							"cloud-token": "${{ secrets.DAGGER_CLOUD_TOKEN }}",
							"verb":        "call",
							"module":      spec.ModuleRef,
							"args":        fullCmd,
						},
						Env: map[string]string{
							"GITHUB_TOKEN":      "${{ secrets.GITHUB_TOKEN }}",
							"REGISTRY_USERNAME": registryUsername,
							"REGISTRY_PASSWORD": registryPassword,
							"VERSION":           versionExpr,
							"BRANCH_NAME":       branchExpr,
						},
					},
				},
			},
		},
	}

	out, err := yaml.Marshal(wf)
	if err != nil {
		return nil, err
	}

	return map[string]string{
		".github/workflows/dagger.yaml": string(out),
	}, nil
}
