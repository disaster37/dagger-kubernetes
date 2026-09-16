package service

import (
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func mustProjectMapper(t *testing.T, rules []domain.ProjectMappingRule) *ProjectMapper {
	t.Helper()
	m, err := NewProjectMapper(rules)
	if err != nil {
		t.Fatalf("NewProjectMapper: %v", err)
	}
	return m
}

func TestProjectMapperMap(t *testing.T) {
	tests := []struct {
		name    string
		rules   []domain.ProjectMappingRule
		in      string
		want    string
		matched bool
	}{
		{
			name: "first match wins",
			rules: []domain.ProjectMappingRule{
				{Pattern: `^github\.com/acme/.*`, Group: "first"},
				{Pattern: `^github\.com/acme/.*`, Group: "second"},
			},
			in:      "github.com/acme/api",
			want:    "first",
			matched: true,
		},
		{
			name: "no match",
			rules: []domain.ProjectMappingRule{
				{Pattern: `^github\.com/acme/.*`, Group: "acme"},
			},
			in:      "github.com/other/api",
			matched: false,
		},
		{
			name: "case sensitive by default",
			rules: []domain.ProjectMappingRule{
				{Pattern: `^Foo$`, Group: "matched"},
			},
			in:      "foo",
			matched: false,
		},
		{
			name: "case insensitive inline flag",
			rules: []domain.ProjectMappingRule{
				{Pattern: `(?i)^foo$`, Group: "matched"},
			},
			in:      "FOO",
			want:    "matched",
			matched: true,
		},
		{
			name: "unanchored matches substring",
			rules: []domain.ProjectMappingRule{
				{Pattern: `acme`, Group: "substring"},
			},
			in:      "github.com/acme/api",
			want:    "substring",
			matched: true,
		},
		{
			name: "second rule matches when first does not",
			rules: []domain.ProjectMappingRule{
				{Pattern: `^github\.com/acme/.*`, Group: "acme"},
				{Pattern: `^github\.com/.*`, Group: "github"},
			},
			in:      "github.com/other/api",
			want:    "github",
			matched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mustProjectMapper(t, tt.rules)
			got, matched := m.Map(tt.in)
			if matched != tt.matched {
				t.Fatalf("Map(%q) matched = %v, want %v", tt.in, matched, tt.matched)
			}
			if got != tt.want {
				t.Fatalf("Map(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestProjectMapperActive(t *testing.T) {
	active := mustProjectMapper(t, []domain.ProjectMappingRule{{Pattern: `.*`, Group: "g"}})
	tests := []struct {
		name string
		m    *ProjectMapper
		want bool
	}{
		{name: "nil mapper inactive", m: nil},
		{name: "empty rules inactive", m: &ProjectMapper{}},
		{name: "with rules active", m: active, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.Active(); got != tt.want {
				t.Fatalf("Active() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProjectMapperNewErrors(t *testing.T) {
	tests := []struct {
		name  string
		rules []domain.ProjectMappingRule
		want  string
	}{
		{
			name:  "empty pattern",
			rules: []domain.ProjectMappingRule{{Pattern: "", Group: "g"}},
			want:  "project_mappings[0] pattern must not be empty",
		},
		{
			name:  "bad regex",
			rules: []domain.ProjectMappingRule{{Pattern: "[", Group: "g"}},
			want:  "project_mappings[0] pattern",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProjectMapper(tt.rules)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewProjectMapper error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// TestProjectMapperLinearTime documents RE2 linear-time matching: a large
// project name matched against an anchored pattern must complete quickly (Go
// regexp is RE2, so no catastrophic backtracking / ReDoS).
func TestProjectMapperLinearTime(t *testing.T) {
	m := mustProjectMapper(t, []domain.ProjectMappingRule{{Pattern: `^a+b$`, Group: "matched"}})
	large := strings.Repeat("a", 100000) + "c"
	if _, matched := m.Map(large); matched {
		t.Fatal("Map(large no-match) matched, want no match")
	}
}
