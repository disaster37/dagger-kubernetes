package session

import (
	"testing"
	"time"
)

func TestStoreRegisterAndGet(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1")

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
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1")

	time.Sleep(20 * time.Millisecond)

	_, err := s.Get("fp1")
	if err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestStoreTouch(t *testing.T) {
	s := NewStore(50 * time.Millisecond)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1")

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
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1")
	s.Register("fp2", "v0.21.4", "pod-1", "inst-2", "trace-2")

	time.Sleep(20 * time.Millisecond)

	expired := s.ReapOrphans()
	if len(expired) != 2 {
		t.Fatalf("expected 2 expired, got %d", len(expired))
	}
	if s.Count() != 0 {
		t.Fatalf("expected 0 leases, got %d", s.Count())
	}
}

func TestStorePinnedSessionsOnReplica(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1")
	s.Register("fp2", "v0.20.0", "pod-0", "inst-2", "trace-2")
	s.Register("fp3", "v0.21.4", "pod-1", "inst-3", "trace-3")

	if count := s.PinnedSessionsOnReplica("pod-0"); count != 2 {
		t.Fatalf("expected 2 on pod-0, got %d", count)
	}
	if count := s.PinnedSessionsOnReplica("pod-1"); count != 1 {
		t.Fatalf("expected 1 on pod-1, got %d", count)
	}
}

func TestStoreInFlight(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Register("fp1", "v0.21.4", "pod-0", "inst-1", "trace-1")

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
