package service

import (
	"fmt"
	"regexp"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// ProjectMapper maps project names to supervisor group names using an ordered
// list of regex rules. First-match-wins; Go regexp is RE2 (linear-time, no
// ReDoS). The target group is a literal name (no capture substitution).
type ProjectMapper struct {
	rules []projectMappingRule
}

type projectMappingRule struct {
	pattern *regexp.Regexp
	group   string
}

// NewProjectMapper compiles the configured rules. A nil or empty list yields an
// inactive mapper. Errors name the offending rule index (defense-in-depth;
// config.Load already validated them).
func NewProjectMapper(rules []domain.ProjectMappingRule) (*ProjectMapper, error) {
	compiled := make([]projectMappingRule, 0, len(rules))
	for i, rule := range rules {
		if rule.Pattern == "" {
			return nil, fmt.Errorf("project_mappings[%d] pattern must not be empty", i)
		}
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return nil, fmt.Errorf("project_mappings[%d] pattern %q: %w", i, rule.Pattern, err)
		}
		compiled = append(compiled, projectMappingRule{pattern: re, group: rule.Group})
	}
	return &ProjectMapper{rules: compiled}, nil
}

// Active reports whether any rules are configured.
func (m *ProjectMapper) Active() bool {
	return m != nil && len(m.rules) > 0
}

// Map returns the target group of the first matching rule, or ("", false) when
// no rule matches.
func (m *ProjectMapper) Map(projectName string) (string, bool) {
	if m == nil {
		return "", false
	}
	for _, rule := range m.rules {
		if rule.pattern.MatchString(projectName) {
			return rule.group, true
		}
	}
	return "", false
}
