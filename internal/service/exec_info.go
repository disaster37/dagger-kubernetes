package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// Bounds on the derived ExecInfo. They cap the response size (CWE-400) without
// hiding the command; the raw span Attributes map is unchanged.
const (
	maxExecArgv     = 256
	maxExecEnv      = 200
	maxExecValueLen = 4096
)

// execAttrKeys centralizes the Dagger/OTel attribute keys that carry exec
// facts, in priority order (first present wins), so a future engine rename is
// a one-line change here. The keys are asserted by a table-driven unit test.
var execAttrKeys = struct {
	argv      []string
	cwd       []string
	user      []string
	env       []string
	secretEnv []string
	exitCode  []string
	opKind    []string
	call      []string
}{
	argv: []string{
		"dagger.io/exec.args",
		"process.command_args",
		"wcprof.exec.argv",
		"dagger.io/exec.argv",
	},
	cwd:       []string{"dagger.io/exec.cwd", "process.cwd"},
	user:      []string{"dagger.io/exec.user", "process.owner"},
	env:       []string{"dagger.io/exec.env"},
	secretEnv: []string{"dagger.io/exec.secret.env"},
	exitCode:  []string{"dagger.io/exec.exitCode", "exit.code"},
	opKind:    []string{"dagger.io/cache.type", "wcprof.op.kind"},
	// The real engine (v0.21.8) carries the operation and its arguments in a
	// base64 callpbv1.Call protobuf, not in the argv keys above.
	call: []string{"dagger.io/dag.call"},
}

// execNameMarkers identify exec-like span names when no command attribute is
// present (e.g. a publish/export operation span).
var execNameMarkers = []string{"exec", "publish", "push", "export", "import", "processrun"}

// DeriveExec walks the span tree rooted at node and sets node.Exec (and
// recursively its children) from the span's flattened attributes + events.
// It is a pure transform; safe to call on every GET trace detail. Returns the
// same node for chaining.
func DeriveExec(node *domain.SpanNode) *domain.SpanNode {
	if node == nil {
		return nil
	}
	node.Exec = execFromSpan(node)
	for _, child := range node.Children {
		DeriveExec(child)
	}
	return node
}

// execFromSpan builds the ExecInfo for a single span, or nil when the span
// carries no exec-like signal. Pure + table-testable.
func execFromSpan(node *domain.SpanNode) *domain.ExecInfo {
	if node == nil {
		return nil
	}
	argv := parseArgv(node.Attributes)
	if len(argv) == 0 {
		argv = ioOperationArgv(node.Attributes)
	}
	cwd := firstAttr(node.Attributes, execAttrKeys.cwd)
	env := parseEnv(node.Attributes)
	exit := parseExitCode(node)

	if len(argv) == 0 && cwd == "" && len(env) == 0 && exit == nil && !isExecLikeName(node.Name) {
		return nil
	}

	info := &domain.ExecInfo{
		Cwd:      cwd,
		User:     firstAttr(node.Attributes, execAttrKeys.user),
		Env:      env,
		ExitCode: exit,
		Kind:     classifyExecKind(node),
	}
	if len(argv) > 0 {
		info.Command = argv[0]
		info.Args = argv
	}
	return info
}

// parseArgv extracts the argv slice from a span's attributes, trying the
// execAttrKeys table in priority order and decoding JSON-array values.
func parseArgv(attrs map[string]string) []string {
	for _, key := range execAttrKeys.argv {
		raw := attrs[key]
		if raw == "" {
			continue
		}
		if argv := decodeArgv(raw); len(argv) > 0 {
			return capArgv(argv)
		}
	}
	// The real engine carries exec argv inside the base64 callpbv1.Call in
	// dagger.io/dag.call rather than in a dedicated argv attribute.
	for _, key := range execAttrKeys.call {
		raw := attrs[key]
		if raw == "" {
			continue
		}
		field, args, ok := parseDagCall(raw)
		if !ok || !isExecCallField(field) {
			continue
		}
		if argv := args["args"]; len(argv) > 0 {
			return capArgv(argv)
		}
	}
	return nil
}

// ioOperationArgv synthesizes a command line for an io operation (publish,
// export, import, push) from its dagger.io/dag.call, e.g.
// ["publish","ghcr.io/org/image:tag"]. It returns nil when the span carries no
// io-operation call; when the call has no address the argv is just the field
// name.
func ioOperationArgv(attrs map[string]string) []string {
	for _, key := range execAttrKeys.call {
		raw := attrs[key]
		if raw == "" {
			continue
		}
		field, args, ok := parseDagCall(raw)
		if !ok || !isIOOperationField(field) {
			continue
		}
		return capArgv(append([]string{field}, args["address"]...))
	}
	return nil
}

// decodeArgv decodes a JSON string array (the flattening rule for OTLP string
// arrays). A non-JSON value yields nil rather than a fabricated command.
func decodeArgv(raw string) []string {
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		return nil
	}
	return argv
}

// capArgv bounds the argv length, replacing the tail with a truncation marker.
func capArgv(argv []string) []string {
	if len(argv) <= maxExecArgv {
		return argv
	}
	capped := make([]string, 0, maxExecArgv)
	capped = append(capped, argv[:maxExecArgv-1]...)
	capped = append(capped, "…")
	return capped
}

// parseEnv extracts non-secret + secret env entries, redacting secret values.
// Secret names come from dagger.io/exec.secret.env; their values are never in
// telemetry, so they render as is_secret with an empty value. A non-secret
// value whose name looks secret-ish is redacted as defense-in-depth.
func parseEnv(attrs map[string]string) []domain.ExecEnvVar {
	secretNames := make(map[string]bool)
	for _, key := range execAttrKeys.secretEnv {
		for _, name := range decodeArgv(attrs[key]) {
			secretNames[name] = true
		}
	}

	var env []domain.ExecEnvVar
	for _, key := range execAttrKeys.env {
		for _, kv := range decodeArgv(attrs[key]) {
			name, value, ok := strings.Cut(kv, "=")
			if !ok || name == "" {
				continue
			}
			env = append(env, envVar(name, value, secretNames[name]))
		}
	}

	// OTel semconv carries each variable as its own attribute; sort the names
	// so the derived output is deterministic.
	const prefix = "process.environment_variable."
	var names []string
	for key := range attrs {
		if strings.HasPrefix(key, prefix) && strings.TrimPrefix(key, prefix) != "" {
			names = append(names, key)
		}
	}
	sort.Strings(names)
	for _, key := range names {
		name := strings.TrimPrefix(key, prefix)
		env = append(env, envVar(name, attrs[key], secretNames[name]))
	}

	// Secret names are declared separately from the non-secret env array; add
	// an entry for any secret not already present so it renders as <secret>.
	seen := make(map[string]bool, len(env))
	for _, e := range env {
		seen[e.Name] = true
	}
	var missing []string
	for name := range secretNames {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		env = append(env, domain.ExecEnvVar{Name: name, IsSecret: true})
	}

	if len(env) == 0 {
		return nil
	}
	return capEnv(env)
}

// envVar builds one env entry, applying secret redaction and the value cap.
func envVar(name, value string, secret bool) domain.ExecEnvVar {
	if secret {
		return domain.ExecEnvVar{Name: name, IsSecret: true}
	}
	if isSecretName(name) {
		return domain.ExecEnvVar{Name: name, Value: "<redacted>"}
	}
	return domain.ExecEnvVar{Name: name, Value: capValue(value)}
}

// isSecretName reports whether an env var name looks secret-ish (case
// insensitive), used to redact values that leaked into a non-secret key.
func isSecretName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "KEY", "AUTH"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// capValue bounds a single env value, trimming to a valid UTF-8 boundary and
// appending a truncation marker.
func capValue(value string) string {
	if len(value) <= maxExecValueLen {
		return value
	}
	truncated := value[:maxExecValueLen]
	for truncated != "" {
		r, size := utf8.DecodeLastRuneInString(truncated)
		if r != utf8.RuneError || size > 1 {
			break
		}
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + "…"
}

// capEnv bounds the number of env entries, replacing the tail with a marker.
func capEnv(env []domain.ExecEnvVar) []domain.ExecEnvVar {
	if len(env) <= maxExecEnv {
		return env
	}
	capped := make([]domain.ExecEnvVar, 0, maxExecEnv)
	capped = append(capped, env[:maxExecEnv-1]...)
	capped = append(capped, domain.ExecEnvVar{
		Name:  "…",
		Value: fmt.Sprintf("%d more", len(env)-maxExecEnv+1),
	})
	return capped
}

// parseExitCode resolves the exit code from span events (Container exited /
// exit.code) then attribute fallbacks; nil when unknown.
func parseExitCode(node *domain.SpanNode) *int {
	if node == nil {
		return nil
	}
	for _, ev := range node.Events {
		if ev.Name != "Container exited" {
			continue
		}
		if code, ok := parseCode(ev.Attributes["exit.code"]); ok {
			return &code
		}
	}
	for _, key := range execAttrKeys.exitCode {
		if code, ok := parseCode(node.Attributes[key]); ok {
			return &code
		}
	}
	return nil
}

// parseCode parses an exit code string; ok is false for empty/non-numeric.
func parseCode(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// classifyExecKind maps span name + op-kind attributes to a coarse kind label.
func classifyExecKind(node *domain.SpanNode) string {
	if node == nil {
		return ""
	}
	for _, key := range execAttrKeys.opKind {
		if v := node.Attributes[key]; v != "" {
			return normalizeKind(v)
		}
	}
	name := strings.ToLower(node.Name)
	switch {
	case strings.Contains(name, "publish"), strings.Contains(name, "push"),
		strings.Contains(name, "export"), strings.Contains(name, "import"):
		return "io"
	case strings.Contains(name, "service"), strings.Contains(name, "tunnel"):
		return "service_start"
	case strings.Contains(name, "exec"), strings.Contains(name, "processrun"):
		return "exec"
	case strings.Contains(name, "call"):
		return "call"
	default:
		return "other"
	}
}

// normalizeKind maps a raw op-kind attribute value to a coarse kind label.
func normalizeKind(v string) string {
	switch strings.ToLower(v) {
	case "exec", "call_exec":
		return "exec"
	case "service_start":
		return "service_start"
	case "io":
		return "io"
	case "call":
		return "call"
	default:
		return "other"
	}
}

// isExecLikeName reports whether a span name matches an exec/publish pattern.
func isExecLikeName(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range execNameMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// firstAttr returns the first non-empty value among keys, in order.
func firstAttr(attrs map[string]string, keys []string) string {
	for _, key := range keys {
		if v := attrs[key]; v != "" {
			return v
		}
	}
	return ""
}
