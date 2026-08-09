package pipeline

import (
	"strings"
	"testing"
)

func TestAssembleShellCommand_Basic(t *testing.T) {
	spec := PipelineSpec{
		ModuleRef: "github.com/org/mod@v2",
		SrcDir:    ".",
		Job: Job{
			Function: "ci",
			Args:     []string{},
			Placeholders: map[string]Binding{
				PhVersion: {Kind: BindingExpr, Ref: ""},
			},
		},
	}

	cmd, decls, err := assembleShellCommand(spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "dagger call -m 'github.com/org/mod@v2' --src '.' 'ci'"
	if cmd != expected {
		t.Errorf("expected '%s', got '%s'", expected, cmd)
	}
	if len(decls) != 0 {
		t.Errorf("expected 0 envDecls, got %d", len(decls))
	}
}

func TestAssembleShellCommand_WithPlaceholders(t *testing.T) {
	spec := PipelineSpec{
		ModuleRef: "github.com/org/mod@v2",
		Job: Job{
			Function: "ci",
			Args: []string{
				"--version", "{{version}}",
				"--registry-username", "{{registry-username}}",
				"--git-token", "{{git-token}}",
			},
			Placeholders: map[string]Binding{
				PhVersion:      {Kind: BindingExpr, Ref: ""},
				PhRegistryUser: {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
				PhGitToken:     {Kind: BindingSecret, Ref: "GITHUB_TOKEN"},
			},
		},
	}

	cmd, decls, err := assembleShellCommand(spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(cmd, "$VERSION") {
		t.Errorf("expected $VERSION in command, got: %s", cmd)
	}
	if !strings.Contains(cmd, "$REGISTRY_USERNAME") {
		t.Errorf("expected $REGISTRY_USERNAME in command, got: %s", cmd)
	}
	if !strings.Contains(cmd, "$GIT_TOKEN") {
		t.Errorf("expected $GIT_TOKEN in command, got: %s", cmd)
	}
	if len(decls) != 3 {
		t.Errorf("expected 3 envDecls, got %d: %+v", len(decls), decls)
	}
}

func TestAssembleShellCommand_LiteralBinding(t *testing.T) {
	spec := PipelineSpec{
		ModuleRef: "github.com/org/mod@v2",
		Job: Job{
			Function: "ci",
			Args: []string{
				"--value", "{{my-value}}",
			},
			Placeholders: map[string]Binding{
				"my-value": {Kind: BindingLiteral, Ref: "hello"},
			},
		},
	}

	cmd, _, err := assembleShellCommand(spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// assembleShellCommand replaces with $ENV_VAR, not the literal value
	// The renderer then uses BindingLiteral to output the literal
	if !strings.Contains(cmd, "$MY_VALUE") {
		t.Errorf("expected $MY_VALUE in command, got: %s", cmd)
	}
}


