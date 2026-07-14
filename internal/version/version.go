package version

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Version struct {
	Major int
	Minor int
	Patch int
	Raw   string
}

type Resolver struct {
	mu        sync.RWMutex
	allowlist map[string]bool
	floor     *Version
	releases  map[string][]string
	lastFetch time.Time
	cacheTTL  time.Duration
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?$`)

func Parse(raw string) (*Version, error) {
	raw = strings.TrimPrefix(raw, "v")
	matches := versionRe.FindStringSubmatch(raw)
	if matches == nil {
		return nil, fmt.Errorf("invalid version format: %s", raw)
	}

	major, _ := strconv.Atoi(matches[1])
	minor, _ := strconv.Atoi(matches[2])
	patch := 0
	if matches[3] != "" {
		patch, _ = strconv.Atoi(matches[3])
	}

	return &Version{Major: major, Minor: minor, Patch: patch, Raw: fmt.Sprintf("v%d.%d.%d", major, minor, patch)}, nil
}

func NewResolver(floor string, allowlist []string, releases map[string][]string) (*Resolver, error) {
	floorVer, err := Parse(floor)
	if err != nil {
		return nil, fmt.Errorf("invalid floor version: %w", err)
	}

	al := make(map[string]bool)
	for _, v := range allowlist {
		parsed, err := Parse(v)
		if err != nil {
			return nil, fmt.Errorf("invalid allowlist version %s: %w", v, err)
		}
		al[parsed.MinorKey()] = true
	}

	if releases == nil {
		releases = make(map[string][]string)
	}

	return &Resolver{
		allowlist: al,
		floor:     floorVer,
		releases:  releases,
		cacheTTL:  1 * time.Hour,
	}, nil
}

func (r *Resolver) IsAllowed(v *Version) bool {
	if v.Compare(r.floor) < 0 {
		return false
	}
	if len(r.allowlist) > 0 {
		if !r.allowlist[v.MinorKey()] {
			return false
		}
	}
	return true
}

func (r *Resolver) ResolveMinimal(raw string) (*Version, error) {
	v, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if v.Patch != 0 {
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
		ver, err := Parse(p)
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

func (v *Version) Compare(other *Version) int {
	if v.Major != other.Major {
		return v.Major - other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor - other.Minor
	}
	return v.Patch - other.Patch
}

func (v *Version) MinorKey() string {
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

func (v *Version) Slug() string {
	return strings.ReplaceAll(v.Raw, ".", "-")
}

func (v *Version) CacheRefTag() string {
	return v.Slug()
}

func (v *Version) String() string {
	return v.Raw
}

func (r *Resolver) SetReleases(releases map[string][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases = releases
	r.lastFetch = time.Now()
}

func (r *Resolver) NeedsRefresh() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return time.Since(r.lastFetch) > r.cacheTTL
}

func (r *Resolver) Floor() *Version {
	return r.floor
}

func (r *Resolver) AllReleases() []*Version {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var versions []*Version
	for _, patches := range r.releases {
		for _, p := range patches {
			v, err := Parse(p)
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
