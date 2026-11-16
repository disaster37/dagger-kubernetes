package repository

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/expr-lang/expr"
	"github.com/pelletier/go-toml/v2"
	"github.com/subosito/gotenv"
	"github.com/tidwall/gjson"
	"gopkg.in/ini.v1"
	"gopkg.in/yaml.v3"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// WorkflowExprEvaluator evaluates ${{ ... }} expressions with a sandboxed
// file() reader jailed under root.
type WorkflowExprEvaluator struct {
	root string
}

var _ domain.WorkflowExprEvaluator = (*WorkflowExprEvaluator)(nil)

// NewWorkflowExprEvaluator returns an evaluator jailed under root.
func NewWorkflowExprEvaluator(root string) *WorkflowExprEvaluator {
	return &WorkflowExprEvaluator{root: root}
}

// Eval compiles and evaluates expr with env plus the built-in helpers.
func (e *WorkflowExprEvaluator) Eval(expression string, env map[string]any) (any, error) {
	expression = strings.TrimSpace(expression)
	expression = strings.TrimPrefix(expression, "${{")
	expression = strings.TrimSuffix(expression, "}}")
	expression = strings.TrimSpace(expression)
	scope := make(map[string]any, len(env)+8)
	for k, v := range env {
		scope[k] = v
	}
	scope["file"] = e.file
	scope["semver_parse"] = semverParse
	scope["semver_bump"] = semverBump
	scope["semver_with_prerelease"] = semverWithPrerelease
	scope["semver_with_build"] = semverWithBuild
	scope["slug"] = slug
	scope["coalesce"] = coalesce
	scope["next_semver"] = func(filePath, dotPath, bump string, releaseOnDefault bool) (string, error) {
		return e.nextSemver(env, filePath, dotPath, bump, releaseOnDefault)
	}
	program, err := expr.Compile(expression, expr.Env(scope), expr.AllowUndefinedVariables())
	if err != nil {
		return nil, fmt.Errorf("compile workflow expression: %w", err)
	}
	out, err := expr.Run(program, scope)
	if err != nil {
		return nil, fmt.Errorf("evaluate workflow expression: %w", err)
	}
	return out, nil
}

// file reads a structured file under root and queries an optional dotted path.
func (e *WorkflowExprEvaluator) file(path, dotPath string) (any, error) {
	root, err := os.OpenRoot(e.root)
	if err != nil {
		return nil, fmt.Errorf("open workflow root: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("open workflow file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	data := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		data = append(data, buf[:n]...)
		if len(data) > 1<<20 {
			return nil, fmt.Errorf("workflow file %s exceeds 1 MiB", path)
		}
		if err != nil {
			break
		}
	}
	normalized, err := normalizeStructured(path, data)
	if err != nil {
		return nil, err
	}
	if dotPath == "" {
		return strings.TrimSpace(string(data)), nil
	}
	res := gjson.GetBytes(normalized, dotPath)
	if !res.Exists() {
		return nil, nil
	}
	return res.Value(), nil
}

// normalizeStructured converts YAML/TOML/INI/dotenv into JSON for gjson.
func normalizeStructured(path string, data []byte) ([]byte, error) {
	ext := strings.ToLower(filepath.Ext(path))
	var v any
	switch ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("parse yaml: %w", err)
		}
	case ".toml":
		if err := toml.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("parse toml: %w", err)
		}
	case ".ini", ".properties":
		cfg, err := ini.Load(data)
		if err != nil {
			return nil, fmt.Errorf("parse ini: %w", err)
		}
		m := map[string]any{}
		for _, section := range cfg.Sections() {
			sm := map[string]string{}
			for _, key := range section.Keys() {
				sm[key.Name()] = key.Value()
			}
			m[section.Name()] = sm
		}
		v = m
	case ".env":
		m := map[string]string{}
		for k, val := range gotenv.Parse(strings.NewReader(string(data))) {
			m[k] = val
		}
		v = m
	default:
		return nil, fmt.Errorf("unsupported workflow file type %s", ext)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("normalize workflow file: %w", err)
	}
	return out, nil
}

func semverParse(v string) (*semver.Version, error) {
	return semver.NewVersion(v)
}

func semverBump(v, part string) (string, error) {
	ver, err := semver.NewVersion(v)
	if err != nil {
		return "", err
	}
	var out semver.Version
	switch part {
	case "major":
		out = ver.IncMajor()
	case "minor":
		out = ver.IncMinor()
	default:
		out = ver.IncPatch()
	}
	return out.String(), nil
}

func semverWithPrerelease(v, pre string) (string, error) {
	ver, err := semver.NewVersion(v)
	if err != nil {
		return "", err
	}
	out, err := ver.SetPrerelease(pre)
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

func semverWithBuild(v, meta string) (string, error) {
	ver, err := semver.NewVersion(v)
	if err != nil {
		return "", err
	}
	out, err := ver.SetMetadata(meta)
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func coalesce(values ...any) any {
	for _, v := range values {
		if v != nil && v != "" {
			return v
		}
	}
	return nil
}

// nextSemver computes the next version for a workflow run using the git
// context (tag/PR/default branch/branch) and the run number.
func (e *WorkflowExprEvaluator) nextSemver(env map[string]any, filePath, dotPath, bump string, releaseOnDefault bool) (string, error) {
	raw, err := e.file(filePath, dotPath)
	if err != nil {
		return "", err
	}
	next, err := semverBump(fmt.Sprintf("%v", raw), bump)
	if err != nil {
		return "", err
	}
	git, _ := env["git"].(map[string]any)
	run, _ := env["run"].(map[string]any)
	runNumber := fmt.Sprintf("%v", run["number"])
	if truthy(git["is_tag"]) {
		if tag, ok := git["tag_version"].(string); ok && tag != "" {
			return tag, nil
		}
	}
	if truthy(git["is_pr"]) {
		return fmt.Sprintf("%s-pr.%v.%s", next, git["pr_number"], runNumber), nil
	}
	if truthy(git["is_default_branch"]) {
		if releaseOnDefault {
			return next, nil
		}
		return fmt.Sprintf("%s-rc.%s", next, runNumber), nil
	}
	if branch, ok := git["branch"].(string); ok && branch != "" {
		return fmt.Sprintf("%s-%s.%s", next, slug(branch), runNumber), nil
	}
	return next, nil
}
