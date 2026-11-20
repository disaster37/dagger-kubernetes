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
	// Only wcprof.op.kind names an operation kind. dagger.io/cache.type is the
	// GraphQL return type ("Container", "Directory", "String", ...) in the
	// 1.0-beta CLI, not an op kind, so it must never be classified here.
	opKind: []string{"wcprof.op.kind"},
	// The real engine (v0.21.8+) carries the operation and its arguments in a
	// base64 callpbv1.Call protobuf, not in the argv keys above.
	call: []string{"dagger.io/dag.call"},
}

// execNameMarkers identify exec-like span names when no command attribute is
// present (e.g. a publish/export operation span).
var execNameMarkers = []string{"exec", "publish", "push", "export", "import", "processrun"}

// DeriveExec walks the span tree rooted at node and sets node.Exec (and
// recursively its children) from the span's flattened attributes + events.
// After the per-span derivation it rolls up execution facts that Dagger splits
// across parent/child spans:
//   - a child exec.processRun's command/args onto a parent exec.run that has
//     none (the 1.0-beta CLI nests the argv one level down);
//   - a container's exit code from a "resume withExec"/exec.run span onto the
//     related Container.withExec span when the pairing is unambiguous.
//
// It is a pure transform; safe to call on every GET trace detail. Returns the
// same node for chaining.
func DeriveExec(node *domain.SpanNode) *domain.SpanNode {
	deriveExecTree(node)
	propagateExecExitCodes(node)
	return node
}

// deriveExecTree sets node.Exec (and recursively its children) from the span's
// flattened attributes + events, then merges process-run argv upward.
func deriveExecTree(node *domain.SpanNode) {
	if node == nil {
		return
	}
	node.Exec = execFromSpan(node)
	for _, child := range node.Children {
		deriveExecTree(child)
	}
	mergeProcessRunArgs(node)
}

// propagateExecExitCodes finds a scope (a subtree) in which a container's
// withExec definition and its actual execution are separate sibling/descendant
// spans, and copies the exit code onto the definition span when the pairing is
// unambiguous (exactly one withExec and one resolved exec result in the scope).
// Older traces without such a split are left untouched.
func propagateExecExitCodes(node *domain.SpanNode) {
	if node == nil {
		return
	}
	var defs []*domain.SpanNode
	var execs []*domain.SpanNode
	collectExecSpans(node, &defs, &execs)
	if len(defs) == 1 && len(execs) == 1 && defs[0].Exec != nil && execs[0].Exec != nil {
		if defs[0].Exec.ExitCode == nil && execs[0].Exec.ExitCode != nil {
			defs[0].Exec.ExitCode = execs[0].Exec.ExitCode
		}
	}
	for _, child := range node.Children {
		propagateExecExitCodes(child)
	}
}

// collectExecSpans gathers the withExec definition spans and the resolved
// execution spans in a subtree: defs are "Container.withExec" spans, execs are
// the spans that actually ran (exec.run / resume withExec) carrying an exit
// code.
func collectExecSpans(node *domain.SpanNode, defs, execs *[]*domain.SpanNode) {
	if node.Exec != nil {
		lower := strings.ToLower(node.Name)
		switch {
		case strings.Contains(lower, "exec.run") || strings.Contains(lower, "resume withexec"):
			if node.Exec.ExitCode != nil {
				*execs = append(*execs, node)
			}
		case strings.Contains(lower, "withexec"):
			*defs = append(*defs, node)
		}
	}
	for _, child := range node.Children {
		collectExecSpans(child, defs, execs)
	}
}

// mergeProcessRunArgs copies a sole child exec.processRun's command/args onto a
// parent span (exec.run) that carries no argv of its own but is an exec op.
// The child keeps its own Exec so it can be deduplicated by the UI.
func mergeProcessRunArgs(node *domain.SpanNode) {
	if node.Exec == nil || len(node.Exec.Args) > 0 || node.Exec.Kind != "exec" {
		return
	}
	for _, child := range node.Children {
		if !isProcessRunSpan(child) || child.Exec == nil || len(child.Exec.Args) == 0 {
			continue
		}
		node.Exec.Command = child.Exec.Args[0]
		node.Exec.Args = child.Exec.Args
		if node.Exec.ExitCode == nil {
			node.Exec.ExitCode = child.Exec.ExitCode
		}
		return
	}
}

// isProcessRunSpan reports whether a span is Dagger's inner process-run record
// (name "exec.processRun" in the 1.0-beta CLI).
func isProcessRunSpan(node *domain.SpanNode) bool {
	return strings.Contains(strings.ToLower(node.Name), "processrun")
}

// execSignals is the per-span exec signal gathered from attributes, dag.calls,
// and events before the nil/suppression decision.
type execSignals struct {
	argv []string
	cwd  string
	user string
	env  []domain.ExecEnvVar
	kind string
	exit *int
}

// execFromSpan builds the ExecInfo for a single span, or nil when the span
// carries no exec-like signal. Pure + table-testable.
func execFromSpan(node *domain.SpanNode) *domain.ExecInfo {
	if node == nil {
		return nil
	}
	sig := gatherExecSignals(node)
	if suppressExecInfo(node, &sig) {
		return nil
	}
	info := &domain.ExecInfo{
		Cwd:      sig.cwd,
		User:     sig.user,
		Env:      sig.env,
		ExitCode: sig.exit,
		Kind:     sig.kind,
	}
	if len(sig.argv) > 0 {
		info.Command = sig.argv[0]
		info.Args = sig.argv
	}
	return info
}

// gatherExecSignals collects the exec facts for a span from its OTLP attributes
// and its dagger.io/dag.call, refining the coarse kind when a call supplies the
// real signal (env, container setup, or image source).
func gatherExecSignals(node *domain.SpanNode) execSignals {
	sig := execSignals{
		argv: parseArgv(node.Attributes),
		cwd:  firstAttr(node.Attributes, execAttrKeys.cwd),
		user: firstAttr(node.Attributes, execAttrKeys.user),
		env:  parseEnv(node.Attributes),
		kind: classifyExecKind(node),
	}
	if len(sig.argv) == 0 {
		sig.argv = ioOperationArgv(node.Attributes)
	}
	sig.applyCallEnv(node.Attributes)
	sig.applyCallSetup(node.Attributes)
	if len(sig.argv) == 0 {
		if imgArgv := imageSourceArgv(node.Attributes); len(imgArgv) > 0 {
			sig.argv = imgArgv
			sig.refineKind("image")
		}
	}
	sig.exit = parseExitCode(node)
	return sig
}

// applyCallEnv fills env from a withEnvVariable/withSecretVariable call when the
// span emits no dagger.io/exec.env attribute.
func (s *execSignals) applyCallEnv(attrs map[string]string) {
	if len(s.env) > 0 {
		return
	}
	if callEnvVars := callEnv(attrs); len(callEnvVars) > 0 {
		s.env = callEnvVars
		s.refineKind("env")
	}
}

// applyCallSetup fills cwd/user from a withWorkdir/withUser call.
func (s *execSignals) applyCallSetup(attrs map[string]string) {
	if s.cwd != "" && s.user != "" {
		return
	}
	callCwd, callUser, ok := callContainerSetup(attrs)
	if !ok {
		return
	}
	if s.cwd == "" {
		s.cwd = callCwd
	}
	if s.user == "" {
		s.user = callUser
	}
	s.refineKind("config")
}

// refineKind promotes an unresolved kind ("other") to kind.
func (s *execSignals) refineKind(kind string) {
	if s.kind == "other" {
		s.kind = kind
	}
}

// suppressExecInfo reports whether a span should get no ExecInfo: it carries no
// exec facts and either its name is not exec-like, or its only signal is a
// non-exec op kind (internal/lazy/exec_phase — engine bookkeeping spans such as
// dagql.publishResult or exec.containerStart).
func suppressExecInfo(node *domain.SpanNode, sig *execSignals) bool {
	if len(sig.argv) > 0 || sig.cwd != "" || sig.user != "" || len(sig.env) > 0 || sig.exit != nil {
		return false
	}
	return !isExecLikeName(node.Name) || sig.kind == "other"
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

// imageSourceArgv synthesizes a command line for a container image source
// (from) from its dagger.io/dag.call, e.g. ["from","alpine:3.20"]. Returns nil
// when the span carries no image-source call.
func imageSourceArgv(attrs map[string]string) []string {
	for _, key := range execAttrKeys.call {
		raw := attrs[key]
		if raw == "" {
			continue
		}
		field, args, ok := parseDagCall(raw)
		if !ok || !isImageSourceField(field) {
			continue
		}
		return capArgv(append([]string{field}, args["address"]...))
	}
	return nil
}

// callContainerSetup derives the workdir (withWorkdir path) and user (withUser
// name) from a dagger.io/dag.call. It returns ok=true when the call is one of
// those setup fields, with the corresponding value (empty when the other field
// was set).
func callContainerSetup(attrs map[string]string) (cwd, user string, ok bool) {
	for _, key := range execAttrKeys.call {
		raw := attrs[key]
		if raw == "" {
			continue
		}
		field, args, parsed := parseDagCall(raw)
		if !parsed {
			continue
		}
		switch {
		case isWorkdirCallField(field):
			return firstString(args["path"]), "", true
		case isUserCallField(field):
			return "", firstString(args["name"]), true
		}
	}
	return "", "", false
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

// callEnv derives env entries from a dagql call that sets an environment
// variable (withEnvVariable/withEnvironmentVariable) or a secret variable
// (withSecretVariable). The real engine carries these in dagger.io/dag.call;
// secret values are never in telemetry, so they render as is_secret.
func callEnv(attrs map[string]string) []domain.ExecEnvVar {
	for _, key := range execAttrKeys.call {
		raw := attrs[key]
		if raw == "" {
			continue
		}
		field, args, ok := parseDagCall(raw)
		if !ok {
			continue
		}
		name := firstString(args["name"])
		if name == "" {
			continue
		}
		switch strings.ToLower(field) {
		case "withenvvariable", "withenvironmentvariable":
			return []domain.ExecEnvVar{envVar(name, firstString(args["value"]), false)}
		case "withsecretvariable":
			return []domain.ExecEnvVar{{Name: name, IsSecret: true}}
		}
	}
	return nil
}

// firstString returns the first element of values, or "" when empty.
func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
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

// normalizeKind maps a raw op-kind attribute value to a coarse kind label. Only
// wcprof.op.kind values describe an operation; engine bookkeeping values
// ("internal", "lazy", "exec_phase", "call_exec") map to "other" so a span
// carrying only one of them is not reported as an exec operation.
func normalizeKind(v string) string {
	switch strings.ToLower(v) {
	case "exec":
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
