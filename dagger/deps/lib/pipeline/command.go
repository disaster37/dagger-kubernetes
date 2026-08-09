package pipeline

import "strings"

// envDecl pairs an environment variable name with its Binding for renderer emission.
type envDecl struct {
	EnvVar  string
	Binding Binding
}

// assembleShellCommand builds the `dagger call -m <ref> [--src <dir>] <function> <args...>` shell string
// with {{placeholder}} replaced by $ENV_VAR references. Used by RenderDaggerMd.
func assembleShellCommand(spec PipelineSpec) (cmd string, envDecls []envDecl, err error) {
	var parts []string
	parts = append(parts, "dagger", "call", "-m", shellQuote(spec.ModuleRef))
	if spec.SrcDir != "" {
		parts = append(parts, "--src", shellQuote(spec.SrcDir))
	}
	parts = append(parts, shellQuote(spec.Job.Function))

	var decls []envDecl

	for _, arg := range spec.Job.Args {
		processed := arg
		for placeholder, binding := range spec.Job.Placeholders {
			token := "{{" + placeholder + "}}"
			envName := strings.ToUpper(strings.ReplaceAll(placeholder, "-", "_"))
			envRef := "$" + envName
			if strings.Contains(processed, token) {
				processed = strings.ReplaceAll(processed, token, envRef)
				found := false
				for _, d := range decls {
					if d.EnvVar == envName {
						found = true
						break
					}
				}
				if !found {
					decls = append(decls, envDecl{EnvVar: envName, Binding: binding})
				}
			}
		}
		parts = append(parts, processed)
	}

	return strings.Join(parts, " "), decls, nil
}
