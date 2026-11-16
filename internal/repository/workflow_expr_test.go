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
	e := NewWorkflowExprEvaluator()
	out, err := e.Eval(`semver_bump("1.2.3", "minor")`, nil, dir)
	if err != nil || out != "1.3.0" {
		t.Fatalf("bump: %v %v", out, err)
	}
	out, err = e.Eval(`file("version.yaml", "version")`, nil, dir)
	if err != nil || out != "1.2.3" {
		t.Fatalf("file: %v %v", out, err)
	}
	out, err = e.Eval(`slug("Hello World!")`, nil, dir)
	if err != nil || out != "hello-world" {
		t.Fatalf("slug: %v %v", out, err)
	}
	env := map[string]any{
		"git": map[string]any{"is_pr": true, "pr_number": 7},
		"run": map[string]any{"number": 3},
	}
	out, err = e.Eval(`next_semver("version.yaml", "version", "minor", false)`, env, dir)
	if err != nil || out != "1.3.0-pr.7.3" {
		t.Fatalf("next_semver: %v %v", out, err)
	}
}

func TestWorkflowExprFileJailAndFormats(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"VERSION":   "1.2.3\n",
		"data.json": `{"version":"2.0.0"}`,
		"data.yaml": "version: 3.0.0\n",
		"data.toml": "version = \"4.0.0\"\n",
		"data.ini":  "[app]\nversion=5.0.0\n",
		"data.env":  "VERSION=6.0.0\n",
		"bad.json":  `{not json}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := NewWorkflowExprEvaluator()
	cases := []struct {
		name string
		expr string
		want any
	}{
		{"plain", `file("VERSION")`, "1.2.3"},
		{"json", `file("data.json", "version")`, "2.0.0"},
		{"yaml", `file("data.yaml", "version")`, "3.0.0"},
		{"toml", `file("data.toml", "version")`, "4.0.0"},
		{"ini", `file("data.ini", "app.version")`, "5.0.0"},
		{"env", `file("data.env", "VERSION")`, "6.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.Eval(tc.expr, nil, dir)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := e.Eval(`file("bad.json", "version")`, nil, dir); err == nil {
		t.Fatal("invalid json must fail")
	}
	if _, err := e.Eval(`file("VERSION")`, nil, ""); err == nil {
		t.Fatal("empty repo root must fail")
	}
	if _, err := e.Eval(`file("VERSION")`, nil, "."); err == nil {
		t.Fatal("dot repo root must fail")
	}
	if _, err := e.Eval(`file("../VERSION")`, nil, dir); err == nil {
		t.Fatal("traversal must fail")
	}
}

func TestWorkflowExprNextSemverContexts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewWorkflowExprEvaluator()
	cases := []struct {
		name string
		git  map[string]any
		expr string
		want string
	}{
		{"tag", map[string]any{"is_tag": true, "tag_version": "9.9.9"}, `next_semver("VERSION")`, "9.9.9"},
		{"release", map[string]any{"is_release": true, "tag_version": "8.8.8"}, `next_semver("VERSION")`, "8.8.8"},
		{"pr", map[string]any{"is_pr": true, "pr_number": 4}, `next_semver("VERSION")`, "1.2.4-pr.4.2"},
		{"default-rc", map[string]any{"is_default_branch": true}, `next_semver("VERSION")`, "1.2.4-rc.2"},
		{"default-release", map[string]any{"is_default_branch": true}, `next_semver("VERSION", "", "patch", true)`, "1.2.4"},
		{"branch", map[string]any{"branch": "Feature/X"}, `next_semver("VERSION")`, "1.2.4-feature-x.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]any{"git": tc.git, "run": map[string]any{"number": 2}}
			got, err := e.Eval(tc.expr, env, dir)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
