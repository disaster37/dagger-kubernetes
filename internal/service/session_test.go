package service

import (
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestStoreRegisterAndGet(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "")

	lease, err := s.Get("fp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if lease.Version != "v0.21.4" {
		t.Fatalf("expected v0.21.4, got %s", lease.Version)
	}
	if lease.ReplicaPod != "pod-0" {
		t.Fatalf("expected pod-0, got %s", lease.ReplicaPod)
	}
}

func TestStoreExpiry(t *testing.T) {
	s := NewStore(10 * time.Millisecond)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "")

	time.Sleep(20 * time.Millisecond)

	_, err := s.Get("fp1")
	if err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestStoreTouch(t *testing.T) {
	s := NewStore(50 * time.Millisecond)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "")

	time.Sleep(25 * time.Millisecond)
	s.Touch("fp1")

	time.Sleep(30 * time.Millisecond)

	_, err := s.Get("fp1")
	if err != nil {
		t.Fatalf("unexpected expiry after touch: %v", err)
	}
}

func TestStoreReapOrphans(t *testing.T) {
	s := NewStore(10 * time.Millisecond)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "")
	s.Register("fp2", "v0.21.4", "pod-1", "inst-2", "trace-2", "")

	time.Sleep(20 * time.Millisecond)

	expired := s.ReapOrphans()
	if len(expired) != 2 {
		t.Fatalf("expected 2 expired, got %d", len(expired))
	}
	if n := len(s.List()); n != 0 {
		t.Fatalf("expected 0 leases, got %d", n)
	}
}

func TestStorePinnedSessionsOnReplica(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "")
	s.Register("fp2", "v0.20.0", "pod-0", "inst-2", "trace-2", "")
	s.Register("fp3", "v0.21.4", "pod-1", "inst-3", "trace-3", "")

	if count := s.PinnedSessionsOnReplica("pod-0"); count != 2 {
		t.Fatalf("expected 2 on pod-0, got %d", count)
	}
	if count := s.PinnedSessionsOnReplica("pod-1"); count != 1 {
		t.Fatalf("expected 1 on pod-1, got %d", count)
	}
}

func TestStoreInFlight(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "")

	s.IncInFlight("fp1")
	s.IncInFlight("fp1")

	lease, _ := s.Get("fp1")
	if lease.InFlight != 2 {
		t.Fatalf("expected InFlight=2, got %d", lease.InFlight)
	}

	s.DecInFlight("fp1")
	lease, _ = s.Get("fp1")
	if lease.InFlight != 1 {
		t.Fatalf("expected InFlight=1, got %d", lease.InFlight)
	}
}

func TestDecInFlightAndGet(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "")

	s.IncInFlight("fp1")
	s.IncInFlight("fp1")

	remaining, err := s.DecInFlightAndGet("fp1")
	if err != nil {
		t.Fatalf("DecInFlightAndGet: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("remaining = %d, want 1", remaining)
	}

	remaining, err = s.DecInFlightAndGet("fp1")
	if err != nil {
		t.Fatalf("DecInFlightAndGet 2: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}

	// Missing lease returns error + 0.
	remaining, err = s.DecInFlightAndGet("nope")
	if err == nil {
		t.Fatal("expected error for missing lease")
	}
	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
}

// TestStoreListSnapshotNoRace verifies List returns copies so a caller reading
// fields (e.g. InFlight) does not race with concurrent Inc/DecInFlight writes.
// Run with -race.
func TestStoreListSnapshotNoRace(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.IncInFlight("fp1")
				_ = s.DecInFlight("fp1")
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				for _, l := range s.List() {
					_ = l.InFlight
					_ = l.ReplicaPod
				}
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestStoreSetGroupID(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "u1")

	s.SetGroupID("fp1", "g1")
	leases := s.List()
	if len(leases) != 1 || leases[0].GroupID != "g1" {
		t.Fatalf("GroupID = %q, want g1", leases[0].GroupID)
	}

	// Unknown certFP is a no-op (no panic).
	s.SetGroupID("nope", "g2")
}

// TestStoreSetGroupIDNoRace verifies SetGroupID is safe to call concurrently
// with List (the lease returned by Register is shared with the store). Run
// with -race.
func TestStoreSetGroupIDNoRace(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1", "u1")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.SetGroupID("fp1", "g1")
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				for _, l := range s.List() {
					_ = l.GroupID
				}
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// Compile-time assertion that Store satisfies domain.SessionStore.
var _ domain.SessionStore = (*Store)(nil)
