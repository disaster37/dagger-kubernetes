package service

import (
	goauthoauth "github.com/disaster37/go-auth/oauth"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GroupMapper maps upstream provider group names to supervisor group names
// using an ordered list of regex rules. It wraps go-auth's GroupMapper
// (first-match-wins per group; a group matching no rule is dropped; Go regexp
// is RE2, so matching is linear-time and ReDoS-safe).
type GroupMapper struct {
	mapper *goauthoauth.GroupMapper
}

// NewGroupMapper compiles the configured rules. A nil or empty list yields the
// identity mapper (Map returns its input unchanged, de-duplicated). It returns
// an error naming the offending rule when a pattern is empty or fails to
// compile (defense-in-depth; config.Load already validated them).
func NewGroupMapper(rules []domain.GroupMappingRule) (*GroupMapper, error) {
	converted := make([]goauthoauth.GroupMappingRule, 0, len(rules))
	for _, rule := range rules {
		converted = append(converted, goauthoauth.GroupMappingRule{
			Pattern:     rule.Pattern,
			Replacement: rule.Replacement,
		})
	}
	m, err := goauthoauth.NewGroupMapper(converted)
	if err != nil {
		return nil, err
	}
	return &GroupMapper{mapper: m}, nil
}

// Active reports whether any mapping rules are configured. When false, the
// OAuth services skip the mapping/sync step entirely (backward compatible).
func (m *GroupMapper) Active() bool {
	return m != nil && m.mapper.Active()
}

// mapIfActive applies Map when rules are configured and returns nil otherwise,
// so the OAuth services skip the membership-sync step when no mapping rules are
// configured (an empty rules list must produce no membership changes, not the
// identity mapping).
func (m *GroupMapper) mapIfActive(providerGroups []string) []string {
	if m == nil {
		return nil
	}
	return m.mapper.MapIfActive(providerGroups)
}

// Map applies the ordered rules to each provider group. First-match-wins per
// group; a group matching no rule is dropped. Output order is deterministic
// (input order, first match); duplicates are removed preserving first
// occurrence. With no rules it returns the input unchanged (identity).
func (m *GroupMapper) Map(providerGroups []string) []string {
	if m == nil {
		return providerGroups
	}
	return m.mapper.Map(providerGroups)
}
