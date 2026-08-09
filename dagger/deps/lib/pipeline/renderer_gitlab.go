package pipeline

import (
	"fmt"
	"strings"

	"github.com/disaster37/dagger-library-go/lib/ci"
	"gopkg.in/yaml.v3"
)

// GitLabRenderer produces GitLab CI YAML (.gitlab-ci.yml).
type GitLabRenderer struct{}

func (r *GitLabRenderer) Supports(c ci.CI) bool {
	return c == ci.Gitlab
}

func (r *GitLabRenderer) Render(spec PipelineSpec) (map[string]string, error) {
	// Compute command args with GitLab-specific resolver
	args := buildCIArgs(spec, func(ph string, b Binding) string {
		switch ph {
		case PhVersion:
			return "${VERSION}"
		case PhBranch:
			return "${BRANCH_NAME}"
		case PhGitRepoURL:
			return "${CI_REPOSITORY_URL}"
		case PhGitToken:
			return "env:GIT_TOKEN"
		case PhRegistryUser:
			return "env:REGISTRY_USERNAME"
		case PhRegistryPass:
			return "env:REGISTRY_PASSWORD"
		default:
			envName := strings.ToUpper(strings.ReplaceAll(ph, "-", "_"))
			switch b.Kind {
			case BindingSecret:
				return "env:" + envName
			case BindingExpr:
				return b.Ref
			case BindingLiteral:
				return b.Ref
			}
			return ""
		}
	})

	// Build VERSION computation matching current template behavior,
	// translated to GitLab CI variable expressions.
	// GitLab does not support inline if/else like GitHub; we use shell.
	// DefaultBranch is shell-escaped to prevent injection via branch name.
	versionScript := fmt.Sprintf(
		`if [ -n "$CI_COMMIT_TAG" ]; then VERSION="$CI_COMMIT_TAG"; elif [ "$CI_PIPELINE_SOURCE" = "merge_request_event" ]; then VERSION="0.0.0-pr.${CI_MERGE_REQUEST_IID}.${CI_PIPELINE_IID}"; else VERSION="0.0.0-rc.${CI_PIPELINE_IID}"; fi`+
			"\nif [ -n \"$CI_COMMIT_TAG\" ]; then BRANCH_NAME=%s; elif [ \"$CI_PIPELINE_SOURCE\" = \"merge_request_event\" ]; then BRANCH_NAME=\"$CI_MERGE_REQUEST_SOURCE_BRANCH_NAME\"; else BRANCH_NAME=\"$CI_COMMIT_BRANCH\"; fi",
		shellQuote(spec.DefaultBranch),
	)

	// Shell-quote ModuleRef, Job.Function, and literal arg values to prevent
	// command injection from user-controlled inputs.
	safeArgs := quoteArgsForShell(args)
	srcPrefix := ""
	if spec.SrcDir != "" {
		srcPrefix = fmt.Sprintf("--src %s ", shellQuote(spec.SrcDir))
	}
	fullCmd := fmt.Sprintf("dagger call -m %s %s%s %s export --path .", shellQuote(spec.ModuleRef), srcPrefix, shellQuote(spec.Job.Function), strings.Join(safeArgs, " "))

	defaultTimeout := spec.TimeoutMinutes
	if defaultTimeout <= 0 {
		defaultTimeout = 30
	}

	// Build rules for triggers
	var rules []map[string]interface{}

	if spec.Triggers.Tag {
		rules = append(rules, map[string]interface{}{
			"if": "$CI_COMMIT_TAG",
		})
	}
	if spec.Triggers.Push {
		pushRule := map[string]interface{}{
			"if":     "$CI_COMMIT_BRANCH",
			"changes": []string{"**/*"},
		}
		rules = append(rules, pushRule)
	}
	if spec.Triggers.PullRequest {
		rules = append(rules, map[string]interface{}{
			"if": "$CI_PIPELINE_SOURCE == \"merge_request_event\"",
		})
	}

	type gitlabJob struct {
		Stage   string                   `yaml:"stage"`
		Image   string                   `yaml:"image"`
		Script  []string                 `yaml:"script"`
		Timeout string                   `yaml:"timeout,omitempty"`
		Rules   []map[string]interface{} `yaml:"rules,omitempty"`
	}

	gitlabWf := map[string]interface{}{
		"stages": []string{"dagger"},
		"dagger": gitlabJob{
			Stage:   "dagger",
			Image:   "docker:stable-dind",
			Timeout: fmt.Sprintf("%dm", defaultTimeout),
			Rules:   rules,
			Script: []string{
				versionScript,
				fullCmd,
			},
		},
	}

	out, err := yaml.Marshal(gitlabWf)
	if err != nil {
		return nil, err
	}

	return map[string]string{
		".gitlab-ci.yml": string(out),
	}, nil
}
