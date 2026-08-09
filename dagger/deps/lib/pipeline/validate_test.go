package pipeline

import (
	"testing"

	"github.com/disaster37/dagger-library-go/lib/ci"
)

func TestValidate_Defaults(t *testing.T) {
	spec := &PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar@v1",
		Job: Job{
			Function: "ci",
			Args:     []string{"--src", "."},
		},
	}
	if err := Validate(spec); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(spec.Branches) != 1 || spec.Branches[0] != "main" {
		t.Errorf("expected branches to be ['main'], got %v", spec.Branches)
	}
	if spec.DefaultBranch != "main" {
		t.Errorf("expected DefaultBranch to be 'main', got %s", spec.DefaultBranch)
	}
	if !spec.Triggers.Push || !spec.Triggers.PullRequest || !spec.Triggers.Tag {
		t.Errorf("expected all triggers to be true, got %+v", spec.Triggers)
	}
}

func TestValidate_MissingCI(t *testing.T) {
	spec := &PipelineSpec{
		ModuleRef: "github.com/foo/bar@v1",
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("expected error for missing CI")
	}
}

func TestValidate_UnsupportedCI(t *testing.T) {
	spec := &PipelineSpec{
		CI:        "circleci",
		ModuleRef: "github.com/foo/bar@v1",
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("expected error for unsupported CI")
	}
}

func TestValidate_MissingModuleRef(t *testing.T) {
	spec := &PipelineSpec{
		CI: ci.Github,
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("expected error for missing ModuleRef")
	}
}

func TestValidate_ModuleRefNoAt(t *testing.T) {
	spec := &PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar",
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("expected error for ModuleRef without @")
	}
}

func TestValidate_MissingJobFunction(t *testing.T) {
	spec := &PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar@v1",
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("expected error for missing Job.Function")
	}
}

func TestValidate_RegistryNoRepository(t *testing.T) {
	spec := &PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar@v1",
		Job: Job{
			Function: "ci",
		},
		Registry: "ghcr.io",
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("expected error for Registry without Repository")
	}
}

func TestValidate_RegistryMissingSecretBindings(t *testing.T) {
	spec := &PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar@v1",
		Job: Job{
			Function:     "ci",
			Placeholders: map[string]Binding{},
		},
		Registry:   "ghcr.io",
		Repository: "myorg/repo",
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("expected error for missing secret bindings")
	}
}

func TestValidate_RegistryValidSecretBindings(t *testing.T) {
	spec := &PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar@v1",
		Job: Job{
			Function: "ci",
			Placeholders: map[string]Binding{
				PhRegistryUser: {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
				PhRegistryPass: {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
				PhGitToken:     {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
			},
		},
		Registry:   "ghcr.io",
		Repository: "myorg/repo",
	}
	if err := Validate(spec); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidate_RegistryBindingNotSecret(t *testing.T) {
	spec := &PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar@v1",
		Job: Job{
			Function: "ci",
			Placeholders: map[string]Binding{
				PhRegistryUser: {Kind: BindingExpr, Ref: "some_expr"},
				PhRegistryPass: {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
				PhGitToken:     {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
			},
		},
		Registry:   "ghcr.io",
		Repository: "myorg/repo",
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("expected error for non-secret binding")
	}
}

func TestValidate_DefaultsAllFalseTriggers(t *testing.T) {
	spec := &PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar@v1",
		Job: Job{
			Function: "ci",
		},
		Triggers: Triggers{Push: false, PullRequest: false, Tag: false, Release: false},
	}
	if err := Validate(spec); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !spec.Triggers.Push || !spec.Triggers.PullRequest || !spec.Triggers.Tag {
		t.Errorf("expected Push, PullRequest, Tag to be true, got %+v", spec.Triggers)
	}
}

func TestValidate_DefaultsVersionStrategy(t *testing.T) {
	spec := &PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar@v1",
		Job: Job{
			Function: "ci",
		},
	}
	if err := Validate(spec); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if spec.VersionStrategy.BranchPattern != "0.0.0-rc.{build}" {
		t.Errorf("expected BranchPattern default, got %s", spec.VersionStrategy.BranchPattern)
	}
	if spec.VersionStrategy.PRPattern != "0.0.0-pr.{pr}.{build}" {
		t.Errorf("expected PRPattern default, got %s", spec.VersionStrategy.PRPattern)
	}
	if spec.VersionStrategy.TagPattern != "{tag}" {
		t.Errorf("expected TagPattern default, got %s", spec.VersionStrategy.TagPattern)
	}
}
