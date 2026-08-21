package service

import (
	"fmt"
	"regexp"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GroupMapper maps upstream provider group names to supervisor group names
// using an ordered list of regex rules. First-match-wins per group; a group
// matching no rule is dropped. Go regexp is RE2, so matching is linear-time
// and not susceptible to catastrophic backtracking (ReDoS).
type GroupMapper struct {
	rules []compiledRule
}

type compiledRule struct {
	pattern     *regexp.Regexp
	replacement string
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
		compiled = append(compiled, compiledRule{pattern: re, replacement: rule.Replacement})
	}
	return &GroupMapper{rules: compiled}, nil
}

// Active reports whether any mapping rules are configured. When false, the
// OAuth services skip the mapping/sync step entirely (backward compatible).
func (m *GroupMapper) Active() bool {
	return m != nil && len(m.rules) > 0
}

// mapIfActive applies Map when rules are configured and returns nil otherwise,
// so the OAuth services skip the membership-sync step when no mapping rules are
// configured (an empty rules list must produce no membership changes, not the
// identity mapping).
func (m *GroupMapper) mapIfActive(providerGroups []string) []string {
	if m.Active() {
		return m.Map(providerGroups)
	}
	return nil
}

// Map applies the ordered rules to each provider group. First-match-wins per
// group; the first rule whose pattern matches produces the replacement (with
// capture substitution). A group matching no rule is dropped. Output order is
// deterministic (input order, first match); duplicates are removed preserving
// first occurrence. With no rules it returns the input unchanged (identity).
func (m *GroupMapper) Map(providerGroups []string) []string {
	out := make([]string, 0, len(providerGroups))
	seen := make(map[string]struct{}, len(providerGroups))
	for _, group := range providerGroups {
		mapped, ok := m.mapGroup(group)
		if !ok {
			continue
		}
		// A replacement whose capture references resolve to nothing (e.g. an
		// out-of-range $9 or an unmatched ${name}) expands to the empty string.
		// Drop it: an empty supervisor group name can never match a real group
		// (group names are validated to ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$), so
		// passing it to GetByName would only produce a spurious lookup + log.
		if mapped == "" {
			continue
		}
		if _, dup := seen[mapped]; dup {
			continue
		}
		seen[mapped] = struct{}{}
		out = append(out, mapped)
	}
	return out
}

// mapGroup applies the first matching rule to group and reports whether any
// rule matched. With no rules configured it returns the group unchanged
// (identity).
func (m *GroupMapper) mapGroup(group string) (string, bool) {
	if m == nil || len(m.rules) == 0 {
		return group, true
	}
	for _, rule := range m.rules {
		if idx := rule.pattern.FindStringSubmatchIndex(group); idx != nil {
			return string(rule.pattern.ExpandString(nil, rule.replacement, group, idx)), true
		}
	}
	return "", false
}
