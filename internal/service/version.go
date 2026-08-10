package service

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type Resolver struct {
	mu        sync.RWMutex
	allowlist map[string]bool
	floor     *domain.Version
	releases  map[string][]string
	lastFetch time.Time
	cacheTTL  time.Duration
}

var _ domain.VersionResolver = (*Resolver)(nil)

func NewResolver(floor string, allowlist []string, releases map[string][]string) (*Resolver, error) {
	floorVer, err := domain.Parse(floor)
	if err != nil {
		return nil, fmt.Errorf("invalid floor version: %w", err)
	}

	al := make(map[string]bool)
	for _, v := range allowlist {
		parsed, err := domain.Parse(v)
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

func (r *Resolver) IsAllowed(v *domain.Version) bool {
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

func (r *Resolver) ResolveMinimal(raw string) (*domain.Version, error) {
	v, err := domain.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse version: %w", err)
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
