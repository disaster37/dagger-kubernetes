package pipeline

import (
	"fmt"
	"strings"

	"github.com/disaster37/dagger-library-go/lib/ci"
)

// Validate enforces required fields and applies defaults. Never panics.
func Validate(spec *PipelineSpec) error {
	// CI required and must be supported
	if spec.CI == ci.CI("") {
		return fmt.Errorf("CI is required")
	}
	supported := false
	for _, c := range Supported() {
		if c == spec.CI {
			supported = true
			break
		}
	}
	if !supported {
		return fmt.Errorf("unsupported CI: %s", spec.CI)
	}

	// ModuleRef required, must contain "@"
	if spec.ModuleRef == "" {
		return fmt.Errorf("ModuleRef is required")
	}
	if !strings.Contains(spec.ModuleRef, "@") {
		return fmt.Errorf("ModuleRef must contain '@' (e.g. 'github.com/org/repo/mod@2.0.3'), got: %s", spec.ModuleRef)
	}

	// Branches default ["main"]
	if len(spec.Branches) == 0 {
		spec.Branches = []string{"main"}
	}

	// DefaultBranch default "main"
	if spec.DefaultBranch == "" {
		spec.DefaultBranch = "main"
	}

	// Triggers: if all false, set defaults
	if !spec.Triggers.Push && !spec.Triggers.PullRequest && !spec.Triggers.Tag && !spec.Triggers.Release {
		spec.Triggers.Push = true
		spec.Triggers.PullRequest = true
		spec.Triggers.Tag = true
	}

	// Job.Function required
	if spec.Job.Function == "" {
		return fmt.Errorf("Job.Function is required")
	}

	// If Registry set, Repository required + three secret bindings required
	if spec.Registry != "" {
		if spec.Repository == "" {
			return fmt.Errorf("Repository is required when Registry is set")
		}
		// Check secret bindings
		requiredSecrets := []string{PhRegistryUser, PhRegistryPass, PhGitToken}
		for _, ph := range requiredSecrets {
			b, ok := spec.Job.Placeholders[ph]
			if !ok {
				return fmt.Errorf("Job.Placeholders[%q] is required when Registry is set (must be of kind BindingSecret)", ph)
			}
			if b.Kind != BindingSecret {
				return fmt.Errorf("Job.Placeholders[%q] must be of kind BindingSecret when Registry is set", ph)
			}
		}
	}

	// Apply VersionStrategy defaults
	if spec.VersionStrategy.BranchPattern == "" {
		spec.VersionStrategy.BranchPattern = "0.0.0-rc.{build}"
	}
	if spec.VersionStrategy.PRPattern == "" {
		spec.VersionStrategy.PRPattern = "0.0.0-pr.{pr}.{build}"
	}
	if spec.VersionStrategy.TagPattern == "" {
		spec.VersionStrategy.TagPattern = "{tag}"
	}

	return nil
}
