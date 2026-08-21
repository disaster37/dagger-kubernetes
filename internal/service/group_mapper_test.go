package service

import (
	"reflect"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func mustMapper(t *testing.T, rules []domain.GroupMappingRule) *GroupMapper {
	t.Helper()
	m, err := NewGroupMapper(rules)
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	return m
}

func TestGroupMapperMap(t *testing.T) {
	tests := []struct {
		name  string
		rules []domain.GroupMappingRule
		in    []string
		want  []string
	}{
		{
			name: "nil rules identity",
			in:   []string{"a", "b", "a"},
			want: []string{"a", "b"},
		},
		{
			name:  "empty rules identity",
			rules: []domain.GroupMappingRule{},
			in:    []string{"x", "y", "x"},
			want:  []string{"x", "y"},
		},
		{
			name: "first match wins",
			rules: []domain.GroupMappingRule{
				{Pattern: "^acme", Replacement: "first"},
				{Pattern: "^acme", Replacement: "second"},
			},
			in:   []string{"acme-corp"},
			want: []string{"first"},
		},
		{
			name: "no match dropped",
			rules: []domain.GroupMappingRule{
				{Pattern: "^dev-", Replacement: "$1"},
			},
			in:   []string{"acme"},
			want: []string{},
		},
		{
			name: "capture substitution dollar",
			rules: []domain.GroupMappingRule{
				{Pattern: `^github\.com/acme-(.*)$`, Replacement: `acme-$1`},
			},
			in:   []string{"github.com/acme-eng", "github.com/acme-ops"},
			want: []string{"acme-eng", "acme-ops"},
		},
		{
			name: "capture substitution named",
			rules: []domain.GroupMappingRule{
				{Pattern: `^ldap/(?P<team>.+)$`, Replacement: `${team}`},
			},
			in:   []string{"ldap/platform"},
			want: []string{"platform"},
		},
		{
			name: "literal dollar via double dollar",
			rules: []domain.GroupMappingRule{
				{Pattern: `^acme$`, Replacement: `$$acme`},
			},
			in:   []string{"acme"},
			want: []string{"$acme"},
		},
		{
			name: "case sensitive by default",
			rules: []domain.GroupMappingRule{
				{Pattern: `^Acme$`, Replacement: "matched"},
			},
			in:   []string{"acme"},
			want: []string{},
		},
		{
			name: "case insensitive inline flag",
			rules: []domain.GroupMappingRule{
				{Pattern: `(?i)^acme$`, Replacement: "matched"},
			},
			in:   []string{"Acme", "ACME"},
			want: []string{"matched"},
		},
		{
			name: "anchored vs unanchored",
			rules: []domain.GroupMappingRule{
				{Pattern: `org`, Replacement: "substring"},
			},
			in:   []string{"myorgname"},
			want: []string{"substring"},
		},
		{
			name: "dedupe preserving first occurrence",
			rules: []domain.GroupMappingRule{
				{Pattern: `^a$`, Replacement: "mapped"},
				{Pattern: `^b$`, Replacement: "mapped"},
				{Pattern: `^c$`, Replacement: "other"},
			},
			in:   []string{"a", "b", "c"},
			want: []string{"mapped", "other"},
		},
		{
			name: "deterministic input order",
			rules: []domain.GroupMappingRule{
				{Pattern: `.*`, Replacement: "$0"},
			},
			in:   []string{"z", "a", "m"},
			want: []string{"z", "a", "m"},
		},
		{
			name: "unmatched capture expands to empty and is dropped",
			rules: []domain.GroupMappingRule{
				{Pattern: `.*`, Replacement: "$9"},
			},
			in:   []string{"acme"},
			want: []string{},
		},
		{
			name: "unmatched named capture expands to empty and is dropped",
			rules: []domain.GroupMappingRule{
				{Pattern: `.*`, Replacement: "${nope}"},
			},
			in:   []string{"acme", "devs"},
			want: []string{},
		},
		{
			name: "empty expansion dropped but real matches kept",
			rules: []domain.GroupMappingRule{
				{Pattern: `^dev-(.*)$`, Replacement: "dev-$1"},
				{Pattern: `.*`, Replacement: "$9"},
			},
			in:   []string{"dev-eng", "other"},
			want: []string{"dev-eng"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mustMapper(t, tt.rules)
			got := m.Map(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Map(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestGroupMapperActive(t *testing.T) {
	active := mustMapper(t, []domain.GroupMappingRule{{Pattern: ".*", Replacement: "$0"}})
	tests := []struct {
		name string
		m    *GroupMapper
		want bool
	}{
		{name: "nil mapper inactive", m: nil},
		{name: "empty rules inactive", m: &GroupMapper{}},
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

func TestGroupMapperMapIfActive(t *testing.T) {
	t.Run("inactive returns nil", func(t *testing.T) {
		m := mustMapper(t, nil)
		if got := m.mapIfActive([]string{"a", "b"}); got != nil {
			t.Fatalf("mapIfActive inactive = %v, want nil", got)
		}
	})
	t.Run("active maps", func(t *testing.T) {
		m := mustMapper(t, []domain.GroupMappingRule{{Pattern: `^dev-(.*)$`, Replacement: `$1`}})
		got := m.mapIfActive([]string{"dev-eng"})
		if !reflect.DeepEqual(got, []string{"eng"}) {
			t.Fatalf("mapIfActive active = %v, want [eng]", got)
		}
	})
}

func TestGroupMapperNewErrors(t *testing.T) {
	tests := []struct {
		name  string
		rules []domain.GroupMappingRule
		want  string
	}{
		{
			name:  "bad regex",
			rules: []domain.GroupMappingRule{{Pattern: "[", Replacement: "x"}},
			want:  "group_mappings[0] pattern",
		},
		{
			name:  "empty pattern",
			rules: []domain.GroupMappingRule{{Pattern: "", Replacement: "x"}},
			want:  "group_mappings[0] pattern",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewGroupMapper(tt.rules)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewGroupMapper error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// TestGroupMapperLinearTime documents RE2 linear-time matching: a large group
// name matched against an anchored pattern must complete quickly (Go regexp is
// RE2, so no catastrophic backtracking / ReDoS).
func TestGroupMapperLinearTime(t *testing.T) {
	m := mustMapper(t, []domain.GroupMappingRule{{Pattern: `^a+b$`, Replacement: "matched"}})
	large := strings.Repeat("a", 100000) + "c"
	if got := m.Map([]string{large}); len(got) != 0 {
		t.Fatalf("Map(large no-match) = %v, want empty", got)
	}
}
