package pipeline

import (
	"testing"

	"github.com/disaster37/dagger-library-go/lib/ci"
)

func TestSpec_Fields(t *testing.T) {
	spec := PipelineSpec{
		CI:        ci.Github,
		ModuleRef: "github.com/foo/bar@v2",
		Branches:  []string{"main", "develop"},
		SrcDir:    ".",
		Triggers: Triggers{
			Push:    true,
			Release: true,
		},
		Job: Job{
			Function: "ci",
			Args:     []string{},
			Placeholders: map[string]Binding{
				PhVersion: {Kind: BindingExpr, Ref: "expression"},
			},
		},
	}
	if spec.CI != ci.Github {
		t.Error("CI mismatch")
	}
	if spec.ModuleRef != "github.com/foo/bar@v2" {
		t.Error("ModuleRef mismatch")
	}
	if len(spec.Branches) != 2 {
		t.Error("Branches count mismatch")
	}
}

func TestSupported(t *testing.T) {
	s := Supported()
	if len(s) != 3 {
		t.Fatalf("expected 3 supported CIs, got %d", len(s))
	}
	names := map[string]bool{}
	for _, c := range s {
		names[string(c)] = true
	}
	for _, want := range []string{"github", "jenkins", "gitlab"} {
		if !names[want] {
			t.Errorf("expected %s in Supported()", want)
		}
	}
}
