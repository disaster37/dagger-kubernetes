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
