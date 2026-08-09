package pipeline

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/disaster37/dagger-library-go/lib/ci"
)

var update = flag.Bool("update", false, "update golden files")

// testSpec returns a PipelineSpec for golden file testing.
func testSpec(ciStr string) PipelineSpec {
	return PipelineSpec{
		CI:            parseCI(ciStr),
		ModuleRef:     "github.com/disaster37/dagger-library-go/helm@v2",
		Branches:      []string{"main"},
		DefaultBranch: "main",
		SrcDir:        ".",
		Triggers: Triggers{
			Push:        true,
			PullRequest: true,
			Tag:         true,
			Release:     true,
		},
		Job: Job{
			Function: "ci",
			Args: []string{
				"--ci", "github",
				"--version", "{{version}}",
				"--registry-username", "{{registry-username}}",
				"--registry-password", "{{registry-password}}",
				"--git-token", "{{git-token}}",
				"--git-repo-url", "{{git-repo-url}}",
				"--git-branch", "{{branch}}",
			},
			Placeholders: map[string]Binding{
				PhVersion:      {Kind: BindingExpr, Ref: ""},
				PhBranch:       {Kind: BindingExpr, Ref: ""},
				PhGitRepoURL:   {Kind: BindingExpr, Ref: ""},
				PhGitToken:     {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
				PhRegistryUser: {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
				PhRegistryPass: {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
			},
		},
		Registry:   "ghcr.io",
		Repository: "myorg/helm-charts",
		VersionStrategy: VersionStrategy{
			BranchPattern: "0.0.0-rc.{build}",
			PRPattern:     "0.0.0-pr.{pr}.{build}",
			TagPattern:    "{tag}",
		},
	}
}

// testSpecNoRegistry returns a PipelineSpec without registry (for testing).
func testSpecNoRegistry() PipelineSpec {
	return PipelineSpec{
		CI:            ci.Github,
		ModuleRef:     "github.com/disaster37/dagger-library-go/helm@v2",
		Branches:      []string{"main"},
		DefaultBranch: "main",
		SrcDir:        ".",
		Triggers: Triggers{
			Push:        true,
			PullRequest: true,
			Tag:         true,
		},
		Job: Job{
			Function:     "ci",
			Args:         []string{},
			Placeholders: map[string]Binding{},
		},
	}
}

func parseCI(s string) ci.CI {
	switch s {
	case "github":
		return ci.Github
	case "jenkins":
		return ci.Jenkins
	case "gitlab":
		return ci.Gitlab
	}
	return ci.CI(s)
}

func goldenPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

func writeGolden(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(goldenPath(t, name), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write golden file %s: %v", name, err)
	}
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(goldenPath(t, name))
	if err != nil {
		t.Fatalf("failed to read golden file %s: %v", name, err)
	}
	return string(data)
}

func compareOrUpdate(t *testing.T, name, got string) {
	t.Helper()
	if *update {
		writeGolden(t, name, got)
		return
	}
	want := readGolden(t, name)
	if got != want {
		t.Errorf("output mismatch for %s:\n=== GOT ===\n%s\n=== WANT ===\n%s", name, got, want)
	}
}
