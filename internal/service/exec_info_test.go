package service

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// Verbatim dagger.io/dag.call values emitted by engine v0.21.8, extracted from
// live traces (base64 callpbv1.Call protobufs).
const (
	dagCallWithEnvVariable    = "ChV4eGgzOmI0M2Q2MWM2YWI0YTE5NDkSDQoJQ29udGFpbmVyGAEaD3dpdGhFbnZWYXJpYWJsZSINCgRuYW1lEgU6A0ZPTyIOCgV2YWx1ZRIFOgNiYXJKFXh4aDM6ZDE1YjFjYTJiYWZjNDhmMA=="
	dagCallWithSecretVariable = "ChV4eGgzOmI0M2Q2MWM2YWI0YTE5NDkSDQoJQ29udGFpbmVyGAEaEndpdGhTZWNyZXRWYXJpYWJsZSIUCgRuYW1lEgw6CkFQSV9TRUNSRVQiIQoGc2VjcmV0EhcKFXh4aDM6MTJhZjc0MTZiYmQxMWZhOUoVeHhoMzo5N2MzNTYxZTExZTdjMzU5"
	// Synthetic variant of dagCallWithEnvVariable with a secret-ish name and a
	// value, to prove defense-in-depth redaction.
	dagCallWithTokenVariable = "ChV4eGgzOmI0M2Q2MWM2YWI0YTE5NDkSDQoJQ29udGFpbmVyGAEaD3dpdGhFbnZWYXJpYWJsZSITCgRuYW1lEgs6CUFQSV9UT0tFTiIRCgV2YWx1ZRIIOgZsZWFrZWRKFXh4aDM6ZDE1YjFjYTJiYWZjNDhmMA=="
)

func TestParseArgv(t *testing.T) {
	tests := []struct {
		name  string
		attrs map[string]string
		want  []string
	}{
		{
			name:  "dagger exec args array",
			attrs: map[string]string{"dagger.io/exec.args": `["go","build","./..."]`},
			want:  []string{"go", "build", "./..."},
		},
		{
			name:  "process command args",
			attrs: map[string]string{"process.command_args": `["sh","-c","echo hi"]`},
			want:  []string{"sh", "-c", "echo hi"},
		},
		{
			name:  "wcprof exec argv",
			attrs: map[string]string{"wcprof.exec.argv": `["docker","push","img"]`},
			want:  []string{"docker", "push", "img"},
		},
		{
			name:  "legacy dagger argv",
			attrs: map[string]string{"dagger.io/exec.argv": `["ls"]`},
			want:  []string{"ls"},
		},
		{
			name: "priority order",
			attrs: map[string]string{
				"dagger.io/exec.args":  `["first"]`,
				"process.command_args": `["second"]`,
			},
			want: []string{"first"},
		},
		{
			name:  "non-json skipped",
			attrs: map[string]string{"dagger.io/exec.args": "go build"},
			want:  nil,
		},
		{
			name:  "empty array skipped",
			attrs: map[string]string{"dagger.io/exec.args": `[]`},
			want:  nil,
		},
		{
			name:  "no keys",
			attrs: map[string]string{},
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseArgv(tc.attrs)
			if len(got) != len(tc.want) {
				t.Fatalf("parseArgv = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseArgv = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestParseEnv(t *testing.T) {
	tests := []struct {
		name  string
		attrs map[string]string
		want  []domain.ExecEnvVar
	}{
		{
			name:  "non-secret split",
			attrs: map[string]string{"dagger.io/exec.env": `["FOO=bar","BAZ=qux"]`},
			want: []domain.ExecEnvVar{
				{Name: "FOO", Value: "bar"},
				{Name: "BAZ", Value: "qux"},
			},
		},
		{
			name: "secret name redacted",
			attrs: map[string]string{
				"dagger.io/exec.env":        `["API_TOKEN=leaked"]`,
				"dagger.io/exec.secret.env": `["DB_PASSWORD"]`,
			},
			want: []domain.ExecEnvVar{
				{Name: "API_TOKEN", Value: "<redacted>"},
				{Name: "DB_PASSWORD", IsSecret: true},
			},
		},
		{
			name: "secret entry has no value",
			attrs: map[string]string{
				"dagger.io/exec.env":        `["DB_PASSWORD=leaked"]`,
				"dagger.io/exec.secret.env": `["DB_PASSWORD"]`,
			},
			want: []domain.ExecEnvVar{
				{Name: "DB_PASSWORD", IsSecret: true},
			},
		},
		{
			name: "semconv variables sorted",
			attrs: map[string]string{
				"process.environment_variable.ZED": "z",
				"process.environment_variable.ABC": "a",
			},
			want: []domain.ExecEnvVar{
				{Name: "ABC", Value: "a"},
				{Name: "ZED", Value: "z"},
			},
		},
		{
			name:  "malformed entries skipped",
			attrs: map[string]string{"dagger.io/exec.env": `["NOEQUALS","=novalue","OK=1"]`},
			want:  []domain.ExecEnvVar{{Name: "OK", Value: "1"}},
		},
		{
			name:  "none",
			attrs: map[string]string{},
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseEnv(tc.attrs)
			if len(got) != len(tc.want) {
				t.Fatalf("parseEnv = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseEnv[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCallEnv(t *testing.T) {
	tests := []struct {
		name  string
		attrs map[string]string
		want  []domain.ExecEnvVar
	}{
		{
			name:  "withEnvVariable",
			attrs: map[string]string{"dagger.io/dag.call": dagCallWithEnvVariable},
			want:  []domain.ExecEnvVar{{Name: "FOO", Value: "bar"}},
		},
		{
			name:  "withSecretVariable",
			attrs: map[string]string{"dagger.io/dag.call": dagCallWithSecretVariable},
			want:  []domain.ExecEnvVar{{Name: "API_SECRET", IsSecret: true}},
		},
		{
			name:  "non-env call",
			attrs: map[string]string{"dagger.io/dag.call": dagCallWithExec},
			want:  nil,
		},
		{
			name:  "secret-ish name redacted",
			attrs: map[string]string{"dagger.io/dag.call": dagCallWithTokenVariable},
			want:  []domain.ExecEnvVar{{Name: "API_TOKEN", Value: "<redacted>"}},
		},
		{
			name:  "no call",
			attrs: map[string]string{},
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := callEnv(tc.attrs)
			if len(got) != len(tc.want) {
				t.Fatalf("callEnv = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("callEnv[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
			// The secret id must never surface in the derived env.
			for _, e := range got {
				if strings.Contains(e.Value, "xxh3:") {
					t.Fatalf("secret id leaked into env value: %+v", e)
				}
			}
		})
	}
}

func TestParseExitCode(t *testing.T) {
	code := func(n int) *int { return &n }

	tests := []struct {
		name string
		node *domain.SpanNode
		want *int
	}{
		{
			name: "from event",
			node: &domain.SpanNode{
				Events: []domain.SpanEvent{
					{Name: "Container exited", Attributes: map[string]string{"exit.code": "1"}},
				},
			},
			want: code(1),
		},
		{
			name: "event wins over attribute",
			node: &domain.SpanNode{
				Attributes: map[string]string{"exit.code": "9"},
				Events: []domain.SpanEvent{
					{Name: "Container exited", Attributes: map[string]string{"exit.code": "2"}},
				},
			},
			want: code(2),
		},
		{
			name: "from attribute",
			node: &domain.SpanNode{Attributes: map[string]string{"dagger.io/exec.exitCode": "0"}},
			want: code(0),
		},
		{
			name: "absent",
			node: &domain.SpanNode{},
			want: nil,
		},
		{
			name: "non-numeric ignored",
			node: &domain.SpanNode{Attributes: map[string]string{"exit.code": "boom"}},
			want: nil,
		},
		{
			name: "event without exit code falls through",
			node: &domain.SpanNode{
				Attributes: map[string]string{"exit.code": "3"},
				Events:     []domain.SpanEvent{{Name: "Container exited"}},
			},
			want: code(3),
		},
		{
			name: "unrelated event skipped",
			node: &domain.SpanNode{
				Attributes: map[string]string{"exit.code": "5"},
				Events:     []domain.SpanEvent{{Name: "something else"}},
			},
			want: code(5),
		},
		{
			name: "nil node",
			node: nil,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseExitCode(tc.node)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("parseExitCode = %v, want %v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Fatalf("parseExitCode = %d, want %d", *got, *tc.want)
			}
		})
	}
}

func TestExecFromSpan(t *testing.T) {
	tests := []struct {
		name string
		node *domain.SpanNode
		want *domain.ExecInfo
	}{
		{
			name: "nil for plain span",
			node: &domain.SpanNode{Name: "compile", Attributes: map[string]string{"foo": "bar"}},
			want: nil,
		},
		{
			name: "nil node",
			node: nil,
			want: nil,
		},
		{
			name: "full exec",
			node: &domain.SpanNode{
				Name: "exec.run",
				Attributes: map[string]string{
					"dagger.io/exec.args": `["go","build"]`,
					"dagger.io/exec.cwd":  "/src",
					"dagger.io/exec.user": "root",
					"dagger.io/exec.env":  `["FOO=bar"]`,
				},
				Events: []domain.SpanEvent{
					{Name: "Container exited", Attributes: map[string]string{"exit.code": "0"}},
				},
			},
			want: &domain.ExecInfo{
				Command:  "go",
				Args:     []string{"go", "build"},
				Cwd:      "/src",
				User:     "root",
				Env:      []domain.ExecEnvVar{{Name: "FOO", Value: "bar"}},
				ExitCode: intPtr(0),
				Kind:     "exec",
			},
		},
		{
			name: "name-only exec-like span",
			node: &domain.SpanNode{Name: "Container.publish"},
			want: &domain.ExecInfo{Kind: "io"},
		},
		{
			name: "cwd only",
			node: &domain.SpanNode{Name: "compile", Attributes: map[string]string{"process.cwd": "/work"}},
			want: &domain.ExecInfo{Cwd: "/work", Kind: "other"},
		},
		{
			name: "withEnvVariable call",
			node: &domain.SpanNode{
				Name:       "Container.withEnvVariable",
				Attributes: map[string]string{"dagger.io/dag.call": dagCallWithEnvVariable},
			},
			want: &domain.ExecInfo{
				Env:  []domain.ExecEnvVar{{Name: "FOO", Value: "bar"}},
				Kind: "env",
			},
		},
		{
			name: "withSecretVariable call",
			node: &domain.SpanNode{
				Name:       "Container.withSecretVariable",
				Attributes: map[string]string{"dagger.io/dag.call": dagCallWithSecretVariable},
			},
			want: &domain.ExecInfo{
				Env:  []domain.ExecEnvVar{{Name: "API_SECRET", IsSecret: true}},
				Kind: "env",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := execFromSpan(tc.node)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("execFromSpan = %+v, want %+v", got, tc.want)
			}
			if got == nil {
				return
			}
			if got.Command != tc.want.Command || got.Cwd != tc.want.Cwd || got.User != tc.want.User || got.Kind != tc.want.Kind {
				t.Fatalf("execFromSpan = %+v, want %+v", got, tc.want)
			}
			if len(got.Args) != len(tc.want.Args) {
				t.Fatalf("args = %v, want %v", got.Args, tc.want.Args)
			}
			if len(got.Env) != len(tc.want.Env) {
				t.Fatalf("env = %v, want %v", got.Env, tc.want.Env)
			}
			for i := range got.Env {
				if got.Env[i] != tc.want.Env[i] {
					t.Fatalf("env[%d] = %+v, want %+v", i, got.Env[i], tc.want.Env[i])
				}
			}
			if (got.ExitCode == nil) != (tc.want.ExitCode == nil) {
				t.Fatalf("exit = %v, want %v", got.ExitCode, tc.want.ExitCode)
			}
			if got.ExitCode != nil && *got.ExitCode != *tc.want.ExitCode {
				t.Fatalf("exit = %d, want %d", *got.ExitCode, *tc.want.ExitCode)
			}
		})
	}
}

func TestDeriveExec(t *testing.T) {
	child := &domain.SpanNode{Name: "exec.run", Attributes: map[string]string{"dagger.io/exec.args": `["ls"]`}}
	plain := &domain.SpanNode{Name: "compile"}
	root := &domain.SpanNode{Name: "build", Children: []*domain.SpanNode{child, plain}}

	got := DeriveExec(root)
	if got != root {
		t.Fatal("DeriveExec should return the same node")
	}
	if root.Exec != nil {
		t.Fatalf("root exec = %+v, want nil", root.Exec)
	}
	if child.Exec == nil || child.Exec.Command != "ls" {
		t.Fatalf("child exec = %+v, want ls", child.Exec)
	}
	if plain.Exec != nil {
		t.Fatalf("plain exec = %+v, want nil", plain.Exec)
	}
	if DeriveExec(nil) != nil {
		t.Fatal("DeriveExec(nil) should be nil")
	}
}

func TestCapArgv(t *testing.T) {
	short := []string{"a", "b"}
	if got := capArgv(short); len(got) != 2 {
		t.Fatalf("short argv changed: %v", got)
	}
	long := make([]string, maxExecArgv+10)
	for i := range long {
		long[i] = "x"
	}
	got := capArgv(long)
	if len(got) != maxExecArgv {
		t.Fatalf("capped argv len = %d, want %d", len(got), maxExecArgv)
	}
	if got[len(got)-1] != "…" {
		t.Fatalf("last argv = %q, want truncation marker", got[len(got)-1])
	}
}

func TestCapEnv(t *testing.T) {
	short := []domain.ExecEnvVar{{Name: "A"}}
	if got := capEnv(short); len(got) != 1 {
		t.Fatalf("short env changed: %v", got)
	}
	long := make([]domain.ExecEnvVar, maxExecEnv+5)
	for i := range long {
		long[i] = domain.ExecEnvVar{Name: "X"}
	}
	got := capEnv(long)
	if len(got) != maxExecEnv {
		t.Fatalf("capped env len = %d, want %d", len(got), maxExecEnv)
	}
	if got[len(got)-1].Name != "…" {
		t.Fatalf("last env = %+v, want truncation marker", got[len(got)-1])
	}
}

func TestCapValue(t *testing.T) {
	short := "hello"
	if got := capValue(short); got != short {
		t.Fatalf("short value changed: %q", got)
	}
	long := strings.Repeat("a", maxExecValueLen+100)
	got := capValue(long)
	if len(got) != maxExecValueLen+len("…") {
		t.Fatalf("capped value len = %d, want %d", len(got), maxExecValueLen+len("…"))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("capped value = %q, want truncation marker", got)
	}
	// A multibyte rune straddling the cap must not be split.
	multi := strings.Repeat("a", maxExecValueLen-1) + "é" + "b"
	got = capValue(multi)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("multibyte capped value = %q, want truncation marker", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("capped value is not valid UTF-8: %q", got)
	}
}

func TestIsSecretName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"API_TOKEN", true},
		{"db_password", true},
		{"MY_SECRET", true},
		{"SSH_KEY", true},
		{"AUTH_HEADER", true},
		{"FOO", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isSecretName(tc.name); got != tc.want {
			t.Fatalf("isSecretName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyExecKind(t *testing.T) {
	tests := []struct {
		name string
		node *domain.SpanNode
		want string
	}{
		{"nil", nil, ""},
		{"op kind exec", &domain.SpanNode{Attributes: map[string]string{"wcprof.op.kind": "exec"}}, "exec"},
		{"op kind call_exec", &domain.SpanNode{Attributes: map[string]string{"wcprof.op.kind": "call_exec"}}, "exec"},
		{"op kind service_start", &domain.SpanNode{Attributes: map[string]string{"wcprof.op.kind": "service_start"}}, "service_start"},
		{"op kind io", &domain.SpanNode{Attributes: map[string]string{"wcprof.op.kind": "io"}}, "io"},
		{"op kind call", &domain.SpanNode{Attributes: map[string]string{"wcprof.op.kind": "call"}}, "call"},
		{"op kind unknown", &domain.SpanNode{Attributes: map[string]string{"wcprof.op.kind": "weird"}}, "other"},
		{"cache type", &domain.SpanNode{Attributes: map[string]string{"dagger.io/cache.type": "exec"}}, "exec"},
		{"name publish", &domain.SpanNode{Name: "Container.publish"}, "io"},
		{"name push", &domain.SpanNode{Name: "docker push"}, "io"},
		{"name export", &domain.SpanNode{Name: "Export"}, "io"},
		{"name import", &domain.SpanNode{Name: "Import"}, "io"},
		{"name service", &domain.SpanNode{Name: "Service.Up"}, "service_start"},
		{"name tunnel", &domain.SpanNode{Name: "host.tunnel"}, "service_start"},
		{"name exec", &domain.SpanNode{Name: "exec.run"}, "exec"},
		{"name processRun", &domain.SpanNode{Name: "processRun"}, "exec"},
		{"name call", &domain.SpanNode{Name: "Container.call"}, "call"},
		{"name other", &domain.SpanNode{Name: "compile"}, "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyExecKind(tc.node); got != tc.want {
				t.Fatalf("classifyExecKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeArgv(t *testing.T) {
	if got := decodeArgv(`["a","b"]`); len(got) != 2 {
		t.Fatalf("decodeArgv = %v, want 2 elements", got)
	}
	if got := decodeArgv("not json"); got != nil {
		t.Fatalf("decodeArgv = %v, want nil", got)
	}
}

func TestFirstAttr(t *testing.T) {
	attrs := map[string]string{"b": "2", "a": "1"}
	if got := firstAttr(attrs, []string{"missing", "a", "b"}); got != "1" {
		t.Fatalf("firstAttr = %q, want 1", got)
	}
	if got := firstAttr(attrs, []string{"missing"}); got != "" {
		t.Fatalf("firstAttr = %q, want empty", got)
	}
}

func intPtr(n int) *int { return &n }
