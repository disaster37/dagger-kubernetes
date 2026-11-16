package repository

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkflowExprEvaluatorHelpers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "version.yaml"), []byte("version: 1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewWorkflowExprEvaluator(dir)
	out, err := e.Eval(`semver_bump("1.2.3", "minor")`, nil)
	if err != nil || out != "1.3.0" {
		t.Fatalf("bump: %v %v", out, err)
	}
	out, err = e.Eval(`file("version.yaml", "version")`, nil)
	if err != nil || out != "1.2.3" {
		t.Fatalf("file: %v %v", out, err)
	}
	out, err = e.Eval(`slug("Hello World!")`, nil)
	if err != nil || out != "hello-world" {
		t.Fatalf("slug: %v %v", out, err)
	}
	env := map[string]any{
		"git": map[string]any{"is_pr": true, "pr_number": 7},
		"run": map[string]any{"number": 3},
	}
	out, err = e.Eval(`next_semver("version.yaml", "version", "minor", false)`, env)
	if err != nil || out != "1.3.0-pr.7.3" {
		t.Fatalf("next_semver: %v %v", out, err)
	}
}
