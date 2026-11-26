package service

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// allowRuleKind classifies an allowlist entry. The three kinds differ in how
// strictly they match a candidate version:
//   - minorRule: a partial "MAJOR.MINOR" entry (e.g. "1.0") admits any version
//     in that minor series, including pre-releases.
//   - preReleasePrefixRule: a full version whose pre-release has no dot (e.g.
//     "1.0.0-beta") admits that pre-release track and its numbered builds
//     (beta.1, beta.28) but not other tracks or the stable release.
//   - exactRule: a full version with an empty or dotted pre-release (e.g.
//     "1.0.0", "v1.0.0-beta.28") matches that exact version only.
type allowRuleKind int

const (
	minorRule allowRuleKind = iota
	preReleasePrefixRule
	exactRule
)

// allowRule is a parsed allowlist entry.
type allowRule struct {
	kind       allowRuleKind
	major      int
	minor      int
	patch      int
	preRelease string
}

// matches reports whether v satisfies the rule.
func (rl allowRule) matches(v *domain.Version) bool {
	if v.Major != rl.major || v.Minor != rl.minor {
		return false
	}
	switch rl.kind {
	case minorRule:
		return true
	case preReleasePrefixRule:
		if v.Patch != rl.patch {
			return false
		}
		return v.PreRelease == rl.preRelease || strings.HasPrefix(v.PreRelease, fmt.Sprintf("%s.", rl.preRelease))
	default: // exactRule
		return v.Patch == rl.patch && v.PreRelease == rl.preRelease
	}
}

// newAllowRule classifies a raw allowlist entry. The caller has already
// validated that raw parses as a version.
//
// A partial "MAJOR.MINOR" entry is a minor-series rule. An entry containing a
// "-" but lacking an explicit patch (not a full version, e.g. "1.0-beta") is a
// malformed pre-release track and is rejected rather than silently treated as a
// minor rule (which would admit the entire stable minor series).
func newAllowRule(raw string, parsed *domain.Version) (allowRule, error) {
	rl := allowRule{
		major:      parsed.Major,
		minor:      parsed.Minor,
		patch:      parsed.Patch,
		preRelease: parsed.PreRelease,
	}
	switch {
	case !domain.IsFullVersion(raw):
		if strings.Contains(raw, "-") {
			return allowRule{}, fmt.Errorf("invalid allowlist version %s: pre-release entries require a full MAJOR.MINOR.PATCH version (e.g. \"1.0.0-beta\")", raw)
		}
		rl.kind = minorRule
	case parsed.PreRelease != "" && !strings.Contains(parsed.PreRelease, "."):
		rl.kind = preReleasePrefixRule
	default:
		rl.kind = exactRule
	}
	return rl, nil
}

type Resolver struct {
	mu       sync.RWMutex
	rules    []allowRule
	floor    *domain.Version
	releases map[string][]string
}

var _ domain.VersionResolver = (*Resolver)(nil)

func NewResolver(floor string, allowlist []string, releases map[string][]string) (*Resolver, error) {
	floorVer, err := domain.Parse(floor)
	if err != nil {
		return nil, fmt.Errorf("invalid floor version: %w", err)
	}

	rules := make([]allowRule, 0, len(allowlist))
	for _, v := range allowlist {
		parsed, err := domain.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("invalid allowlist version %s: %w", v, err)
		}
		rule, err := newAllowRule(v, parsed)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}

	if releases == nil {
		releases = make(map[string][]string)
	}

	return &Resolver{
		rules:    rules,
		floor:    floorVer,
		releases: releases,
	}, nil
}

func (r *Resolver) IsAllowed(v *domain.Version) bool {
	if v.Compare(r.floor) < 0 {
		return false
	}
	if len(r.rules) == 0 {
		return true
	}
	for _, rl := range r.rules {
		if rl.matches(v) {
			return true
		}
	}
	return false
}

func (r *Resolver) ResolveMinimal(raw string) (*domain.Version, error) {
	v, err := domain.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse version: %w", err)
	}
	if v.Patch != 0 || v.PreRelease != "" {
		return v, nil
	}

	r.mu.RLock()
	patches, ok := r.releases[v.MinorKey()]
	r.mu.RUnlock()

	if !ok || len(patches) == 0 {
		return v, nil
	}

	latestPatch := 0
	for _, p := range patches {
		ver, err := domain.Parse(p)
		if err != nil {
			continue
		}
		if ver.Major == v.Major && ver.Minor == v.Minor && ver.Patch > latestPatch {
			latestPatch = ver.Patch
		}
	}

	if latestPatch > 0 {
		v.Patch = latestPatch
		v.Raw = fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, latestPatch)
	}

	return v, nil
}

func (r *Resolver) Floor() *domain.Version {
	return r.floor
}

func (r *Resolver) AllReleases() []*domain.Version {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var versions []*domain.Version
	for _, patches := range r.releases {
		for _, p := range patches {
			v, err := domain.Parse(p)
			if err != nil {
				continue
			}
			if r.IsAllowed(v) {
				versions = append(versions, v)
			}
		}
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Compare(versions[j]) < 0
	})

	return versions
}
