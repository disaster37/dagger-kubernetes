package repository

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type StubProvider struct {
	mu       sync.Mutex
	versions map[string]*stubSTS
}

type stubSTS struct {
	replicasM map[string]*domain.Replica
	nextIP    int
	idleSince time.Time       // zero = unset
	pvcs      map[string]bool // PVC names that exist (retained on scale-down)
}

var _ domain.FleetProvider = (*StubProvider)(nil)

func NewStubProvider() *StubProvider {
	return &StubProvider{
		versions: make(map[string]*stubSTS),
	}
}

func (p *StubProvider) EnsureStatefulSet(version, image string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.versions[version]; !ok {
		p.versions[version] = &stubSTS{
			replicasM: make(map[string]*domain.Replica),
			pvcs:      make(map[string]bool),
		}
	}
	return nil
}

func (p *StubProvider) DeleteStatefulSet(version string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.versions, version)
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

	sts, ok := p.versions[version]
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

	sts, ok := p.versions[version]
	if !ok {
		return fmt.Errorf("statefulset not found for version %s", version)
	}

	for len(sts.replicasM) < targetReplicas {
		ordinal := len(sts.replicasM)
		podName := domain.PodName(version, ordinal)
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
		sts.pvcs[enginePVCName(version, ordinal)] = true
	}
	return nil
}

func (p *StubProvider) ScaleDown(version string, ordinal int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[version]
	if !ok {
		return fmt.Errorf("statefulset not found for version %s", version)
	}

	podName := domain.PodName(version, ordinal)
	delete(sts.replicasM, podName)
	return nil
}

func (p *StubProvider) GetReadyReplicaIP(version, podName string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[version]
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
	for v := range p.versions {
		versions = append(versions, v)
	}
	return versions, nil
}

func (p *StubProvider) VersionIdleSince(version string) (time.Time, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[version]
	if !ok || sts.idleSince.IsZero() {
		return time.Time{}, false, nil
	}
	return sts.idleSince, true, nil
}

func (p *StubProvider) SetVersionIdleSince(version string, idleSince time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[version]
	if !ok {
		return nil // version already gone
	}
	sts.idleSince = idleSince
	return nil
}

// ListPVCs returns the stub's per-pod PVCs for the version, sorted by
// ordinal. The stub only ever creates convention-compliant names
// (enginePVCName), so every entry parses.
func (p *StubProvider) ListPVCs(version string) ([]domain.PVCInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sts, ok := p.versions[version]
	if !ok {
		return nil, nil
	}

	out := make([]domain.PVCInfo, 0, len(sts.pvcs))
	for name := range sts.pvcs {
		out = append(out, domain.PVCInfo{Name: name, Ordinal: extractPVCOrdinal(name, version)})
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

	sts, ok := p.versions[version]
	if !ok {
		sts = &stubSTS{
			replicasM: make(map[string]*domain.Replica),
			pvcs:      make(map[string]bool),
		}
		p.versions[version] = sts
	}
	sts.pvcs[enginePVCName(version, ordinal)] = true
}

// enginePVCName is the StatefulSet PVC naming convention
// (<volume-claim-template>-<stsName>-<ordinal>).
func enginePVCName(version string, ordinal int) string {
	return fmt.Sprintf("%s-%s-%d", volumeDaggerKubernetes, domain.StsName(version), ordinal)
}
