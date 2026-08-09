package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/golang/internal/dagger"

	"emperror.dev/errors"
	cimodule "github.com/disaster37/dagger-library-go/lib/ci"
	"github.com/disaster37/dagger-library-go/lib/pipeline"
)

type CI cimodule.CI

// Ci runs lint, test, and build as a single CI entrypoint.
func (m *Golang) Ci(
	ctx context.Context,
	// Path to main.go
	// +optional
	main string,
	// Binary output name
	// +optional
	out string,
	// Target OS
	// +optional
	os string,
	// Target arch
	// +optional
	arch string,
	// ldflags
	// +optional
	ldflags []string,
) (*dagger.Directory, error) {
	if _, err := m.Lint(ctx); err != nil {
		return nil, errors.Wrap(err, "Error when lint")
	}

	// Test returns a *dagger.File; force DAG evaluation to catch test failures.
	if _, err := m.Test(ctx, false, false, "", "", false, "").Sync(ctx); err != nil {
		return nil, errors.Wrap(err, "Error when test")
	}

	dir := m.Build(main, out, os, arch, ldflags)
	return dir, nil
}

// GenerateCi generates CI pipeline files for the given CI system.
func (m *Golang) GenerateCi(
	ctx context.Context,

	// The CI runner: github, jenkins, or gitlab
	// +required
	ci CI,

	// Branches that trigger the pipeline
	// +optional
	// +default=["main"]
	branches []string,

	// Dagger CLI version to use in CI (empty = engine default)
	// +optional
	daggerVersion string,

	// Branch commits land here when running on a tag.
	// +optional
	// +default="main"
	defaultBranch string,

	// Configurable dagger module reference.
	// Defaults to this module's current version (auto-detected).
	// +optional
	moduleRef string,

	// GitHub: secret name for registry username (empty = github.actor).
	// +optional
	registryUsernameKey string,

	// GitHub: secret name for registry password (empty = GITHUB_TOKEN).
	// +optional
	registryPasswordKey string,

	// Jenkins: credential id for registry username/password.
	// +optional
	registryCredential string,

	// Jenkins: credential id for git token.
	// +optional
	gitTokenCredential string,

	// GitLab: CI/CD variable name for registry username.
	// +optional
	registryUsernameVar string,

	// GitLab: CI/CD variable name for registry password.
	// +optional
	registryPasswordVar string,

	// GitLab: CI/CD variable name for git token.
	// +optional
	gitTokenVar string,
) (*dagger.Directory, error) {
	var err error

	if len(branches) == 0 {
		branches = []string{"main"}
	}
	if daggerVersion == "" {
		daggerVersion, err = dag.Version(ctx)
		if err != nil {
			return nil, err
		}
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if moduleRef == "" {
		moduleRef = fmt.Sprintf("github.com/disaster37/dagger-library-go/golang@%s", strings.TrimSpace(ModuleVersion))
	}

	// Determine placeholder bindings based on CI
	registryUserBinding, registryPassBinding, gitTokenBinding, err := pipeline.ResolveCredentialBindings(cimodule.CI(ci), pipeline.CredentialConfig{
		RegistryUsernameKey: registryUsernameKey,
		RegistryPasswordKey: registryPasswordKey,
		RegistryCredential:  registryCredential,
		GitTokenCredential:  gitTokenCredential,
		RegistryUsernameVar: registryUsernameVar,
		RegistryPasswordVar: registryPasswordVar,
		GitTokenVar:         gitTokenVar,
	})
	if err != nil {
		return nil, errors.Wrap(err, "Error when resolve credential bindings")
	}

	// Build PipelineSpec
	spec := pipeline.PipelineSpec{
		CI:            cimodule.CI(ci),
		ModuleRef:     moduleRef,
		DaggerVersion: daggerVersion,
		Branches:      branches,
		DefaultBranch: defaultBranch,
		SrcDir:        ".",
		Triggers: pipeline.Triggers{
			Push:        true,
			PullRequest: true,
			Tag:         true,
		},
		Job: pipeline.Job{
			Function: "ci",
			Args:     []string{},
			Placeholders: map[string]pipeline.Binding{
				pipeline.PhVersion:      {Kind: pipeline.BindingExpr, Ref: ""},
				pipeline.PhBranch:       {Kind: pipeline.BindingExpr, Ref: ""},
				pipeline.PhRegistryUser: registryUserBinding,
				pipeline.PhRegistryPass: registryPassBinding,
				pipeline.PhGitToken:     gitTokenBinding,
				pipeline.PhGitRepoURL:  {Kind: pipeline.BindingExpr, Ref: ""},
			},
		},
	}

	files, err := pipeline.Render(spec)
	if err != nil {
		return nil, errors.Wrap(err, "Error when render CI pipeline")
	}

	dir := dag.Directory()
	for path, content := range files {
		dir = dir.WithNewFile(path, content)
	}

	return dir, nil
}
