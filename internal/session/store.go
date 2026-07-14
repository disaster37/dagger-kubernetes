package session

import (
	"fmt"
	"sync"
	"time"
)

type Lease struct {
	CertFP       string
	Version      string
	ReplicaPod   string
	InstanceID   string
	LastActivity time.Time
	InFlight     int
	TraceID      string
}

type Store struct {
	mu     sync.RWMutex
	leases map[string]*Lease
	ttl    time.Duration
}

func NewStore(ttl time.Duration) *Store {
	return &Store{
		leases: make(map[string]*Lease),
		ttl:    ttl,
	}
}

func (s *Store) Register(certFP, version, replicaPod, instanceID, traceID string) *Lease {
	s.mu.Lock()
	defer s.mu.Unlock()

	lease := &Lease{
		CertFP:       certFP,
		Version:      version,
		ReplicaPod:   replicaPod,
		InstanceID:   instanceID,
		LastActivity: time.Now(),
		InFlight:     0,
		TraceID:      traceID,
	}
	s.leases[certFP] = lease
	return lease
}

func (s *Store) Get(certFP string) (*Lease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	l, ok := s.leases[certFP]
	if !ok {
		return nil, fmt.Errorf("lease not found for certFP %s", certFP)
	}
	if time.Since(l.LastActivity) > s.ttl {
		return nil, fmt.Errorf("lease expired for certFP %s", certFP)
	}
	return l, nil
}

func (s *Store) Touch(certFP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, ok := s.leases[certFP]
	if !ok {
		return fmt.Errorf("lease not found for certFP %s", certFP)
	}
	l.LastActivity = time.Now()
	return nil
}

func (s *Store) IncInFlight(certFP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, ok := s.leases[certFP]
	if !ok {
		return fmt.Errorf("lease not found for certFP %s", certFP)
	}
	l.InFlight++
	return nil
}

func (s *Store) DecInFlight(certFP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, ok := s.leases[certFP]
	if !ok {
		return fmt.Errorf("lease not found for certFP %s", certFP)
	}
	if l.InFlight > 0 {
		l.InFlight--
	}
	return nil
}

func (s *Store) Remove(certFP string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leases, certFP)
}

func (s *Store) PinnedSessionsOnReplica(podName string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	for _, l := range s.leases {
		if l.ReplicaPod == podName {
			count++
		}
	}
	return count
}

func (s *Store) ReapOrphans() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []string
	now := time.Now()
	for fp, l := range s.leases {
		if now.Sub(l.LastActivity) > s.ttl {
			expired = append(expired, fp)
			delete(s.leases, fp)
		}
	}
	return expired
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.leases)
}

func (s *Store) List() []*Lease {
	s.mu.RLock()
	defer s.mu.RUnlock()

	leases := make([]*Lease, 0, len(s.leases))
	for _, l := range s.leases {
		leases = append(leases, l)
	}
	return leases
}

func (s *Store) ListByVersion(version string) []*Lease {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var leases []*Lease
	for _, l := range s.leases {
		if l.Version == version {
			leases = append(leases, l)
		}
	}
	return leases
}
