package repository

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type StubProvider struct {
	mu       *sync.Mutex
	scope    string
	versions map[string]*stubSTS
}

type stubSTS struct {
	replicasM map[string]*domain.Replica
	nextIP    int
	idleSince time.Time       // zero = unset
	pvcs      map[string]bool // PVC names that exist (retained on scale-down)
}

var (
	_ domain.FleetProvider       = (*StubProvider)(nil)
	_ domain.ScopedFleetProvider = (*StubProvider)(nil)
)

func NewStubProvider() *StubProvider {
	return &StubProvider{
		mu:       &sync.Mutex{},
		versions: make(map[string]*stubSTS),
	}
}

// WithScope returns a scoped copy of the stub. Org-scoped instances key their
// fleets separately so per_org behavior (distinct STS names, scopes) is
// exercised identically to the K8s provider in tests. The scoped copy shares
// the backing map AND its mutex so scoped/unscoped instances never guard the
// same map with different locks.
func (p *StubProvider) WithScope(orgSlug string) domain.FleetProvider {
	cp := &StubProvider{
		mu:       p.mu,
		scope:    orgSlug,
		versions: p.versions, // share the backing map so scoped copies see each other
	}
	return cp
}

// stubKey is the internal map key for a (scope, version) pair.
func (p *StubProvider) stubKey(version string) string {
	return p.scope + "\x00" + version
}

// AllScopes returns the deployed (org, version) scopes.
func (p *StubProvider) AllScopes() ([]domain.FleetScope, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]domain.FleetScope, 0, len(p.versions))
	for k := range p.versions {
		org, version, _ := strings.Cut(k, "\x00")
		out = append(out, domain.FleetScope{OrgID: org, Version: version})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OrgID != out[j].OrgID {
			return out[i].OrgID < out[j].OrgID
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// Lookup through the shared map by (scope, version). Callers own p.mu.

func (p *StubProvider) EnsureStatefulSet(version, image string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := p.stubKey(version)
	if _, ok := p.versions[key]; !ok {
		p.versions[key] = &stubSTS{
			replicasM: make(map[string]*domain.Replica),
			pvcs:      make(map[string]bool),
		}
	}
	return nil
}

func (p *StubProvider) DeleteStatefulSet(version string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.versions, p.stubKey(version))
	return nil
}

func (p *StubProvider) EnsureService(version string) error {
	return nil
}

func (p *StubProvider) DeleteService(version string) error {
	return nil
}

func (p *StubProvider) GetReplicas(version string) ([]domain.Replica, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[p.stubKey(version)]
	if !ok {
		return nil, nil
	}

	var out []domain.Replica
	for _, r := range sts.replicasM {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Ordinal < out[j].Ordinal
	})
	return out, nil
}

func (p *StubProvider) ScaleUp(version string, targetReplicas int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[p.stubKey(version)]
	if !ok {
		return fmt.Errorf("statefulset not found for version %s", version)
	}

	for len(sts.replicasM) < targetReplicas {
		ordinal := len(sts.replicasM)
		podName := domain.EnginePodName(p.scope, version, ordinal)
		ip := fmt.Sprintf("10.0.0.%d", (sts.nextIP%254)+1)
		sts.nextIP++

		sts.replicasM[podName] = &domain.Replica{
			Name:      podName,
			Ordinal:   ordinal,
			Version:   version,
			PodIP:     ip,
			Ready:     true,
			StartedAt: time.Now(),
		}
		sts.pvcs[engineScopedPVCName(p.scope, version, ordinal)] = true
	}
	return nil
}

func (p *StubProvider) ScaleDown(version string, ordinal int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[p.stubKey(version)]
	if !ok {
		return fmt.Errorf("statefulset not found for version %s", version)
	}

	podName := domain.EnginePodName(p.scope, version, ordinal)
	delete(sts.replicasM, podName)
	return nil
}

func (p *StubProvider) GetReadyReplicaIP(version, podName string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[p.stubKey(version)]
	if !ok {
		return "", fmt.Errorf("statefulset not found for version %s", version)
	}

	r, ok := sts.replicasM[podName]
	if !ok {
		return "", fmt.Errorf("replica %s not found", podName)
	}

	return r.PodIP, nil
}

func (p *StubProvider) WaitForReady(version, podName string) error {
	return nil
}

func (p *StubProvider) GetEngineImage(version string) string {
	return fmt.Sprintf("registry.dagger.io/engine:%s", version)
}

func (p *StubProvider) AllVersions() ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	versions := make([]string, 0, len(p.versions))
	for k := range p.versions {
		org, version, _ := strings.Cut(k, "\x00")
		if org == p.scope {
			versions = append(versions, version)
		}
	}
	return versions, nil
}

func (p *StubProvider) VersionIdleSince(version string) (time.Time, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[p.stubKey(version)]
	if !ok || sts.idleSince.IsZero() {
		return time.Time{}, false, nil
	}
	return sts.idleSince, true, nil
}

func (p *StubProvider) SetVersionIdleSince(version string, idleSince time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[p.stubKey(version)]
	if !ok {
		return nil // version already gone
	}
	sts.idleSince = idleSince
	return nil
}

// ListPVCs returns the stub's per-pod PVCs for the version, sorted by
// ordinal. The stub only ever creates convention-compliant names
// (engineScopedPVCName), so every entry parses.
func (p *StubProvider) ListPVCs(version string) ([]domain.PVCInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[p.stubKey(version)]
	if !ok {
		return nil, nil
	}

	out := make([]domain.PVCInfo, 0, len(sts.pvcs))
	for name := range sts.pvcs {
		out = append(out, domain.PVCInfo{Name: name, Ordinal: extractScopedPVCOrdinal(name, domain.EngineStsName(p.scope, version))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out, nil
}

// DeletePVC removes a PVC from the stub. A missing PVC is treated as success
// (NotFound → idempotent).
func (p *StubProvider) DeletePVC(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, sts := range p.versions {
		delete(sts.pvcs, name)
	}
	return nil
}

// AddPVC adds a PVC to the stub for testing, creating the version's
// StatefulSet entry when needed.
func (p *StubProvider) AddPVC(version string, ordinal int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := p.stubKey(version)
	sts, ok := p.versions[key]
	if !ok {
		sts = &stubSTS{
			replicasM: make(map[string]*domain.Replica),
			pvcs:      make(map[string]bool),
		}
		p.versions[key] = sts
	}
	sts.pvcs[engineScopedPVCName(p.scope, version, ordinal)] = true
}

// enginePVCName is the StatefulSet PVC naming convention for the shared
// (unscoped) fleet (<volume-claim-template>-<stsName>-<ordinal>).
func enginePVCName(version string, ordinal int) string {
	return engineScopedPVCName("", version, ordinal)
}

// engineScopedPVCName is the StatefulSet PVC naming convention for an engine
// fleet (<volume-claim-template>-<stsName>-<ordinal>).
func engineScopedPVCName(orgSlug, version string, ordinal int) string {
	return fmt.Sprintf("%s-%s-%d", volumeDaggerKubernetes, domain.EngineStsName(orgSlug, version), ordinal)
}
