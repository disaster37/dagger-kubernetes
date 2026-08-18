package domain

import (
	"strings"
	"testing"
)

// TestValidTraceID verifies the trace-ID bounds enforced for the history-purge
// path. IDs are persisted as the trace_meta primary key and interpolated into
// Loki/VictoriaMetrics delete selectors, so they must be hex-only and bounded
// in length (CWE-20/CWE-94/CWE-400).
func TestValidTraceID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty rejected", "", false},
		{"too short", "abc", false},
		{"non-hex", "zzzzzzzzzzzzzzzz", false},
		{"contains quote", `abcdef0123456789"`, false},
		{"contains brace", "abcdef0123456789{", false},
		{"contains space", "abcdef01234567 89", false},
		{"16 hex ok", "aaaaaaaaaaaaaaaa", true},
		{"32 hex ok", "401ccb197124a8ff2028720fcb5eaa06", true},
		{"uppercase hex ok", "DEADBEEF0123456789ABCDEF", true},
		{"128 hex ok", strings.Repeat("a", 128), true},
		{"129 hex too long", strings.Repeat("a", 129), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidTraceID(tt.in); got != tt.want {
				t.Fatalf("ValidTraceID(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestValidTraceIDKey verifies the tolerant trace_meta primary-key charset
// used by the provision/ingest persistence path (NOT the hex-only
// delete-target charset enforced by ValidTraceID).
func TestValidTraceIDKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty rejected", "", false},
		{"simple alnum", "abc123", true},
		{"single char", "a", true},
		{"dashes", "test-trace-001", true},
		{"dots", "trace.id.1", true},
		{"underscores", "trace_id_1", true},
		{"mixed", "in-group_1.v2", true},
		{"leading dot rejected", ".abc", false},
		{"leading dash rejected", "-abc", false},
		{"leading underscore rejected", "_abc", false},
		{"contains quote", `bad"id`, false},
		{"contains space", "has space", false},
		{"contains slash", "a/b", false},
		{"contains brace", "a{b", false},
		{"128 chars ok", strings.Repeat("a", 128), true},
		{"129 chars too long", strings.Repeat("a", 129), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidTraceIDKey(tt.in); got != tt.want {
				t.Fatalf("ValidTraceIDKey(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
