package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type Store struct {
	mu     sync.RWMutex
	leases map[string]*domain.Lease
	ttl    time.Duration
}

var _ domain.SessionStore = (*Store)(nil)

func NewStore(ttl time.Duration) *Store {
	return &Store{
		leases: make(map[string]*domain.Lease),
		ttl:    ttl,
	}
}

func (s *Store) Register(certFP, version, replicaPod, instanceID, traceID, userID string) *domain.Lease {
	s.mu.Lock()
	defer s.mu.Unlock()

	lease := &domain.Lease{
		CertFP:       certFP,
		Version:      version,
		ReplicaPod:   replicaPod,
		InstanceID:   instanceID,
		LastActivity: time.Now(),
		InFlight:     0,
		TraceID:      traceID,
		UserID:       userID,
	}
	s.leases[certFP] = lease
	return lease
}

func (s *Store) Get(certFP string) (*domain.Lease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	l, err := s.lease(certFP)
	if err != nil {
		return nil, err
	}
	if time.Since(l.LastActivity) > s.ttl {
		return nil, fmt.Errorf("lease expired for certFP %s", certFP)
	}
	return l, nil
}

func (s *Store) Touch(certFP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, err := s.lease(certFP)
	if err != nil {
		return err
	}
	l.LastActivity = time.Now()
	return nil
}

func (s *Store) IncInFlight(certFP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, err := s.lease(certFP)
	if err != nil {
		return err
	}
	l.InFlight++
	return nil
}

func (s *Store) DecInFlight(certFP string) error {
	_, err := s.DecInFlightAndGet(certFP)
	return err
}

// DecInFlightAndGet decrements the in-flight count for certFP and returns the
// resulting count. Returns 0 and a non-nil error when the lease is gone.
func (s *Store) DecInFlightAndGet(certFP string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, err := s.lease(certFP)
	if err != nil {
		return 0, err
	}
	if l.InFlight > 0 {
		l.InFlight--
	}
	return l.InFlight, nil
}

// lease returns the lease for certFP. Callers must hold the mutex.
func (s *Store) lease(certFP string) (*domain.Lease, error) {
	l, ok := s.leases[certFP]
	if !ok {
		return nil, fmt.Errorf("lease not found for certFP %s", certFP)
	}
	return l, nil
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

// CountByUser returns the number of active leases owned by the user. Used by
// the quota service (a multi-group user's lease counts against EACH of their
// groups — decision D3).
func (s *Store) CountByUser(userID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	for _, l := range s.leases {
		if l.UserID == userID {
			count++
		}
	}
	return count
}

// SetGroupID records the display-aid group on the lease for certFP. It is
// safe to call concurrently with other store operations (the lease returned
// by Register is shared with the store and must not be mutated directly).
func (s *Store) SetGroupID(certFP, groupID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if l, ok := s.leases[certFP]; ok {
		l.GroupID = groupID
	}
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

// List returns a snapshot of all leases. The returned leases are copies so
// callers may read fields (e.g. InFlight) without racing with concurrent
// Touch/IncInFlight/DecInFlight updates.
func (s *Store) List() []*domain.Lease {
	s.mu.RLock()
	defer s.mu.RUnlock()

	leases := make([]*domain.Lease, 0, len(s.leases))
	for _, l := range s.leases {
		cp := *l
		leases = append(leases, &cp)
	}
	return leases
}
