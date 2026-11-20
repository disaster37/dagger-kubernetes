package service

import (
	"fmt"
	"regexp"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GroupMapper maps upstream provider group names to supervisor group names
// using an ordered list of regex rules. First-match-wins per group; a group
// matching no rule is dropped; Go regexp is RE2, so matching is linear-time
// (ReDoS-safe). Each rule may also carry a target organization (Org) so a
// mapped group is auto-created inside the organization the operator assigned.
type GroupMapper struct {
	rules []compiledRule
}

type compiledRule struct {
	pattern     *regexp.Regexp
	replacement string
	org         string
}

// mappedGroup is one mapped supervisor group name plus its target org slug/ID
// (empty = bootstrap organization).
type mappedGroup struct {
	Name string
	Org  string
}

// NewGroupMapper compiles the configured rules. A nil or empty list yields the
// identity mapper (Map returns its input unchanged, de-duplicated). It returns
// an error naming the offending rule when a pattern is empty or fails to
// compile (defense-in-depth; config.Load already validated them).
func NewGroupMapper(rules []domain.GroupMappingRule) (*GroupMapper, error) {
	compiled := make([]compiledRule, 0, len(rules))
	for i, rule := range rules {
		if rule.Pattern == "" {
			return nil, fmt.Errorf("group_mappings[%d] pattern must not be empty", i)
		}
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return nil, fmt.Errorf("group_mappings[%d] pattern %q: %w", i, rule.Pattern, err)
		}
		compiled = append(compiled, compiledRule{pattern: re, replacement: rule.Replacement, org: rule.Org})
	}
	return &GroupMapper{rules: compiled}, nil
}

// Active reports whether any mapping rules are configured. When false, the
// OAuth services skip the mapping/sync step entirely (backward compatible).
func (m *GroupMapper) Active() bool {
	return m != nil && len(m.rules) > 0
}

// mapIfActiveTargets applies the mapping when rules are configured, returning
// nil otherwise (an empty rules list must produce no membership changes, not
// the identity mapping). Each result carries the matched rule's target org.
func (m *GroupMapper) mapIfActiveTargets(providerGroups []string) []mappedGroup {
	if !m.Active() {
		return nil
	}
	return m.mapTargets(providerGroups)
}

// Map applies the ordered rules to each provider group. First-match-wins per
// group; a group matching no rule is dropped. Output order is deterministic
// (input order, first match); duplicates are removed preserving first
// occurrence. With no rules it returns the input unchanged (identity).
func (m *GroupMapper) Map(providerGroups []string) []string {
	out := make([]string, 0, len(providerGroups))
	seen := make(map[string]struct{}, len(providerGroups))
	for _, group := range providerGroups {
		name, _, ok := m.mapGroupTarget(group)
		if !ok || name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func (m *GroupMapper) mapTargets(providerGroups []string) []mappedGroup {
	out := make([]mappedGroup, 0, len(providerGroups))
	seen := make(map[string]struct{}, len(providerGroups))
	for _, group := range providerGroups {
		name, org, ok := m.mapGroupTarget(group)
		if !ok || name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, mappedGroup{Name: name, Org: org})
	}
	return out
}

// mapGroupTarget maps one provider group, returning the target name and the
// matched rule's org. The (name, "", true) identity result is returned when no
// rules are configured (nil mapper).
func (m *GroupMapper) mapGroupTarget(group string) (name, org string, ok bool) {
	if m == nil || len(m.rules) == 0 {
		return group, "", true
	}
	for _, rule := range m.rules {
		if idx := rule.pattern.FindStringSubmatchIndex(group); idx != nil {
			return string(rule.pattern.ExpandString(nil, rule.replacement, group, idx)), rule.org, true
		}
	}
	return "", "", false
}
