package pipeline

import (
	"testing"
)

func TestRender_GitHub_Default(t *testing.T) {
	spec := testSpec("github")
	files, err := Render(spec)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := files[".github/workflows/dagger.yaml"]
	compareOrUpdate(t, "github_default.yaml", got)
}

func TestRender_GitHub_Prerelease(t *testing.T) {
	spec := testSpec("github")
	spec.VersionStrategy.PrereleaseSuffix = "-rc"
	files, err := Render(spec)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := files[".github/workflows/dagger.yaml"]
	compareOrUpdate(t, "github_prerelease.yaml", got)
}

func TestRender_GitHub_NoRegistry(t *testing.T) {
	spec := testSpecNoRegistry()
	files, err := Render(spec)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := files[".github/workflows/dagger.yaml"]
	compareOrUpdate(t, "github_no_registry.yaml", got)
}

func TestRender_Jenkins_Default(t *testing.T) {
	spec := testSpec("jenkins")
	files, err := Render(spec)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := files["Jenkinsfile"]
	compareOrUpdate(t, "jenkins_default.groovy", got)
}

func TestRender_Jenkins_MultiBranch(t *testing.T) {
	spec := testSpec("jenkins")
	spec.Branches = []string{"main", "develop"}
	files, err := Render(spec)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := files["Jenkinsfile"]
	compareOrUpdate(t, "jenkins_multi_branch.groovy", got)
}

func TestRender_GitLab_Default(t *testing.T) {
	spec := testSpec("gitlab")
	files, err := Render(spec)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := files[".gitlab-ci.yml"]
	compareOrUpdate(t, "gitlab_default.yaml", got)
}

func TestRender_DaggerMd_Default(t *testing.T) {
	spec := testSpec("github")
	got := RenderDaggerMd(spec)
	compareOrUpdate(t, "dagger_md_default.md", got)
}
