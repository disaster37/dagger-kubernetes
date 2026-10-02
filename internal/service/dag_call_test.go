package service

import (
	"encoding/base64"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// Verbatim dagger.io/dag.call captures from a live engine v0.21.8 trace
// (service dagger-cli). dagCallWithExec is the Container.withExec span whose
// args are `sh -c "echo hello-from-issue60; exit 0"`; dagCallFrom is the
// Container.from span whose address argument is `alpine`. Both are the exact
// base64 attribute values emitted by the engine, unmodified.
const (
	dagCallWithExec = "ChV4eGgzOmI0M2Q2MWM2YWI0YTE5NDkSDQoJQ29udGFpbmVyGAEaCHdpdGhFeGVjIjkKBGFyZ3MSMUIvCgQ6AnNoCgQ6Ai1jCiE6H2VjaG8gaGVsbG8tZnJvbS1pc3N1ZTYwOyBleGl0IDBKFXh4aDM6ZTNiYTg1ZjU1NzFkN2I4MlIHdjAuMjEuOA=="
	dagCallFrom     = "ChV4eGgzOmZjNjZkZjg0YWRmOWM1YjUSDQoJQ29udGFpbmVyGAEaBGZyb20iEwoHYWRkcmVzcxIIOgZhbHBpbmVKFXh4aDM6Njk1Y2EzMGE5YjE3ZmUxM3ovChBmcm9tU2Vzc2lvblNjb3BlEhs6GTl6Y2lweDNyY3p5cGtvMGVrcmJ1ZHMzemw="

	// Deliberate mutations of dagCallWithExec (not captures), used to prove the
	// parser rejects corrupt input safely rather than panicking.
	dagCallWithExecTruncated = "ChV4eGgzOmI0M2Q2MWM2YWI0YTE5NDkSDQoJQ29udGFpbmVyGAEaCHdpdGhFeGVjIjkKBGFyZ3MSMUIvCgQ6AnNoCgQ6Ai1jCiE6H2VjaG8gaGVsbG8tZnJvbS1pc3N1ZTYwOyBleGl0IDBKFXh4aDM6ZTNiYTg1ZjU1NzFkN2I4MlIHdjAuMjEu"
	dagCallWithExecBadLength = "ChZ4eGgzOmI0M2Q2MWM2YWI0YTE5NDkSDQoJQ29udGFpbmVyGAEaCHdpdGhFeGVjIjkKBGFyZ3MSMUIvCgQ6AnNoCgQ6Ai1jCiE6H2VjaG8gaGVsbG8tZnJvbS1pc3N1ZTYwOyBleGl0IDBKFXh4aDM6ZTNiYTg1ZjU1NzFkN2I4MlIHdjAuMjEuOA=="
)

func TestParseDagCall(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantField string
		wantArgs  map[string][]string
	}{
		{
			name:      "withExec",
			raw:       dagCallWithExec,
			wantField: "withExec",
			wantArgs:  map[string][]string{"args": {"sh", "-c", "echo hello-from-issue60; exit 0"}},
		},
		{
			name:      "from",
			raw:       dagCallFrom,
			wantField: "from",
			wantArgs:  map[string][]string{"address": {"alpine"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			field, args, ok := parseDagCall(tc.raw)
			if !ok {
				t.Fatalf("parseDagCall(%s) ok = false, want true", tc.name)
			}
			if field != tc.wantField {
				t.Fatalf("field = %q, want %q", field, tc.wantField)
			}
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("args = %v, want %v", args, tc.wantArgs)
			}
			for name, want := range tc.wantArgs {
				got := args[name]
				if len(got) != len(want) {
					t.Fatalf("args[%q] = %v, want %v", name, got, want)
				}
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("args[%q] = %v, want %v", name, got, want)
					}
				}
			}
		})
	}
}

func TestParseDagCallMalformed(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"invalid base64", "not base64!!!"},
		{"truncated length", base64.StdEncoding.EncodeToString([]byte{0x0a, 0x05, 0x01})},
		{"field number zero", base64.StdEncoding.EncodeToString([]byte{0x07})},
		{"unknown wire type", base64.StdEncoding.EncodeToString([]byte{0x0f})},
		{"over-long varint", base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})},
		{"non-proto bytes", base64.StdEncoding.EncodeToString([]byte("hello world"))},
		{"truncated real withExec mutation", dagCallWithExecTruncated},
		{"real withExec with corrupted digest length", dagCallWithExecBadLength},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			field, args, ok := parseDagCall(tc.raw)
			if ok {
				t.Fatalf("parseDagCall(%q) ok = true (field=%q args=%v), want false", tc.name, field, args)
			}
		})
	}
}

func TestParseProtoFields(t *testing.T) {
	// field 1 varint 150, field 2 bytes "hi", field 3 fixed64, field 4 fixed32.
	data := []byte{
		0x08, 0x96, 0x01,
		0x12, 0x02, 'h', 'i',
		0x19, 1, 2, 3, 4, 5, 6, 7, 8,
		0x25, 9, 10, 11, 12,
	}
	fields, ok := parseProtoFields(data)
	if !ok {
		t.Fatal("parseProtoFields ok = false, want true")
	}
	if len(fields) != 4 {
		t.Fatalf("fields = %d, want 4", len(fields))
	}
	if got := protoString(fields, 2); got != "hi" {
		t.Fatalf("protoString = %q, want hi", got)
	}
	if got := protoBytes(fields, 2); string(got) != "hi" {
		t.Fatalf("protoBytes = %q, want hi", got)
	}
	if got := protoString(fields, 99); got != "" {
		t.Fatalf("missing protoString = %q, want empty", got)
	}
	if got := protoBytes(fields, 99); got != nil {
		t.Fatalf("missing protoBytes = %v, want nil", got)
	}
}

func TestParseLiteralStrings(t *testing.T) {
	// Scalar string at field 7.
	scalar := []byte{0x3a, 0x02, 'h', 'i'}
	if got := parseLiteralStrings(scalar); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("scalar = %v, want [hi]", got)
	}
	// List at field 8 -> repeated field 1 -> each item's field 7 string.
	list := []byte{
		0x42, 0x08,
		0x0a, 0x02, 0x3a, 0x00, // item: field 7 empty string
		0x0a, 0x02, 0x3a, 0x00,
	}
	if got := parseLiteralStrings(list); len(got) != 2 {
		t.Fatalf("list = %v, want 2 items", got)
	}
	// Non-string literal (int at field 5) yields nothing.
	if got := parseLiteralStrings([]byte{0x28, 0x01}); got != nil {
		t.Fatalf("int literal = %v, want nil", got)
	}
	// Malformed input yields nothing.
	if got := parseLiteralStrings([]byte{0x3a, 0x05, 'h'}); got != nil {
		t.Fatalf("malformed = %v, want nil", got)
	}
}

func TestIsExecCallField(t *testing.T) {
	tests := []struct {
		field string
		want  bool
	}{
		{"withExec", true},
		{"WithExec", true},
		{"exec", true},
		{"EXEC", true},
		{"from", false},
		{"publish", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isExecCallField(tc.field); got != tc.want {
			t.Fatalf("isExecCallField(%q) = %v, want %v", tc.field, got, tc.want)
		}
	}
}

func TestIsIOOperationField(t *testing.T) {
	tests := []struct {
		field string
		want  bool
	}{
		{"publish", true},
		{"Publish", true},
		{"export", true},
		{"import", true},
		{"push", true},
		{"withExec", false},
		{"from", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isIOOperationField(tc.field); got != tc.want {
			t.Fatalf("isIOOperationField(%q) = %v, want %v", tc.field, got, tc.want)
		}
	}
}

func TestParseArgvDagCall(t *testing.T) {
	// dag.call with withExec yields the argv.
	got := parseArgv(map[string]string{"dagger.io/dag.call": dagCallWithExec})
	want := []string{"sh", "-c", "echo hello-from-issue60; exit 0"}
	if len(got) != len(want) {
		t.Fatalf("parseArgv = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("parseArgv = %v, want %v", got, want)
		}
	}
	// dag.call with a non-exec field yields nil.
	if got := parseArgv(map[string]string{"dagger.io/dag.call": dagCallFrom}); got != nil {
		t.Fatalf("parseArgv(from) = %v, want nil", got)
	}
	// The dedicated argv attribute keeps precedence over dag.call.
	got = parseArgv(map[string]string{
		"dagger.io/exec.args": `["go","build"]`,
		"dagger.io/dag.call":  dagCallWithExec,
	})
	if len(got) != 2 || got[0] != "go" {
		t.Fatalf("parseArgv precedence = %v, want [go build]", got)
	}
}

func TestExecFromSpanIOOperation(t *testing.T) {
	// A publish call with an address synthesizes "publish <address>".
	publishCall := encodeDagCall(t, "publish", "address", "ghcr.io/example/app:1.0")
	node := &domain.SpanNode{
		Name:       "Container.publish",
		Attributes: map[string]string{"dagger.io/dag.call": publishCall},
	}
	got := execFromSpan(node)
	if got == nil {
		t.Fatal("execFromSpan = nil, want io operation")
	}
	if got.Command != "publish" {
		t.Fatalf("command = %q, want publish", got.Command)
	}
	if len(got.Args) != 2 || got.Args[1] != "ghcr.io/example/app:1.0" {
		t.Fatalf("args = %v, want [publish ghcr.io/example/app:1.0]", got.Args)
	}
	if got.Kind != "io" {
		t.Fatalf("kind = %q, want io", got.Kind)
	}

	// Without an address the argv is just the field name.
	noAddr := encodeDagCall(t, "export", "", "")
	node = &domain.SpanNode{
		Name:       "Container.export",
		Attributes: map[string]string{"dagger.io/dag.call": noAddr},
	}
	got = execFromSpan(node)
	if got == nil || got.Command != "export" || len(got.Args) != 1 {
		t.Fatalf("execFromSpan(no address) = %+v, want command export with one arg", got)
	}
}

// encodeDagCall builds a minimal callpbv1.Call for tests: field name plus one
// optional string argument.
func encodeDagCall(t *testing.T, field, argName, argValue string) string {
	t.Helper()
	var arg []byte
	if argName != "" {
		arg = append(arg, 0x0a, byte(len(argName)))
		arg = append(arg, argName...)
		literal := append([]byte{0x3a, byte(len(argValue))}, argValue...)
		arg = append(arg, 0x12, byte(len(literal)))
		arg = append(arg, literal...)
	}
	var msg []byte
	msg = append(msg, 0x1a, byte(len(field)))
	msg = append(msg, field...)
	if len(arg) > 0 {
		msg = append(msg, 0x22, byte(len(arg)))
		msg = append(msg, arg...)
	}
	return base64.StdEncoding.EncodeToString(msg)
}
