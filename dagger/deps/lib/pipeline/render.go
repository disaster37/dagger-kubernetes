package pipeline

import (
	"fmt"
	"strings"

	"github.com/disaster37/dagger-library-go/lib/ci"
)

// Renderer turns a PipelineSpec into a map of output-path -> content.
type Renderer interface {
	Render(spec PipelineSpec) (map[string]string, error)
	Supports(c ci.CI) bool
}

// Renderers maps CI types to their Renderers.
var Renderers = map[ci.CI]Renderer{
	ci.Github: &GitHubRenderer{},
	ci.Jenkins: &JenkinsRenderer{},
	ci.Gitlab:  &GitLabRenderer{},
}

// Render validates the spec and dispatches to the registered renderer.
func Render(spec PipelineSpec) (map[string]string, error) {
	if err := Validate(&spec); err != nil {
		return nil, err
	}

	r, ok := Renderers[spec.CI]
	if !ok {
		return nil, fmt.Errorf("unsupported CI: %s", spec.CI)
	}

	files, err := r.Render(spec)
	if err != nil {
		return nil, err
	}

	// Always include DAGGER.md
	files["DAGGER.md"] = RenderDaggerMd(spec)

	// Include any extra files
	for name, content := range spec.ExtraFiles {
		files[name] = content
	}

	return files, nil
}

// buildCIArgs processes a PipelineSpec's Job.Args, substituting each placeholder
// using the provided resolver function. Unknown placeholders are left as-is.
func buildCIArgs(spec PipelineSpec, resolve func(ph string, b Binding) string) []string {
	var args []string
	for _, arg := range spec.Job.Args {
		s := arg
		for ph, b := range spec.Job.Placeholders {
			token := "{{" + ph + "}}"
			if strings.Contains(s, token) {
				replacement := resolve(ph, b)
				s = strings.ReplaceAll(s, token, replacement)
			}
		}
		args = append(args, s)
	}
	return args
}
