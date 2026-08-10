package service

import (
	"context"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newQuotaForTest(t *testing.T, groups []*domain.Group, memberships map[string][]string, sessions *stubSessionStore) *QuotaService {
	t.Helper()
	grepo := newStubGroupRepo(groups...)
	for gid, uids := range memberships {
		grepo.SetMembers(context.Background(), gid, uids)
	}
	return NewQuotaService(sessions, grepo, testLogger())
}

func TestQuotaAdminBypass(t *testing.T) {
	q := newQuotaForTest(t, nil, nil, &stubSessionStore{})
	if err := q.CheckEngineAccess(context.Background(), &domain.Identity{UserID: "a", Role: domain.RoleAdmin}); err != nil {
		t.Fatalf("admin bypass: %v", err)
	}
}

func TestQuotaNoGroups(t *testing.T) {
	q := newQuotaForTest(t, nil, nil, &stubSessionStore{})
	err := q.CheckEngineAccess(context.Background(), &domain.Identity{UserID: "u", Role: domain.RoleUser})
	if err != domain.ErrNoGroups {
		t.Fatalf("no groups: %v, want ErrNoGroups", err)
	}
}

func TestQuotaAgentUnavailable(t *testing.T) {
	g := &domain.Group{ID: "g1", Name: "G1", AgentAvailable: false}
	q := newQuotaForTest(t, []*domain.Group{g}, map[string][]string{"g1": {"u"}}, &stubSessionStore{})
	err := q.CheckEngineAccess(context.Background(), &domain.Identity{UserID: "u", Role: domain.RoleUser, GroupIDs: []string{"g1"}})
	if err != domain.ErrAgentUnavailable {
		t.Fatalf("agent unavailable: %v, want ErrAgentUnavailable", err)
	}
}

func TestQuotaUnlimited(t *testing.T) {
	g := &domain.Group{ID: "g1", Name: "G1", AgentAvailable: true, MaxRunnerSessions: 0}
	sessions := &stubSessionStore{}
	// Many existing sessions for this user — still admitted (0 = unlimited).
	for i := 0; i < 100; i++ {
		sessions.Register("fp", "v", "pod", "inst", "trace", "u")
	}
	q := newQuotaForTest(t, []*domain.Group{g}, map[string][]string{"g1": {"u"}}, sessions)
	if err := q.CheckEngineAccess(context.Background(), &domain.Identity{UserID: "u", Role: domain.RoleUser, GroupIDs: []string{"g1"}}); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
}

func TestQuotaExhausted(t *testing.T) {
	g := &domain.Group{ID: "g1", Name: "G1", AgentAvailable: true, MaxRunnerSessions: 2}
	sessions := &stubSessionStore{}
	sessions.Register("fp1", "v", "pod", "inst", "trace", "u")
	sessions.Register("fp2", "v", "pod", "inst", "trace", "u")
	q := newQuotaForTest(t, []*domain.Group{g}, map[string][]string{"g1": {"u"}}, sessions)
	err := q.CheckEngineAccess(context.Background(), &domain.Identity{UserID: "u", Role: domain.RoleUser, GroupIDs: []string{"g1"}})
	if err != domain.ErrQuotaExhausted {
		t.Fatalf("exhausted: %v, want ErrQuotaExhausted", err)
	}
}

func TestQuotaAdmittedUnderLimit(t *testing.T) {
	g := &domain.Group{ID: "g1", Name: "G1", AgentAvailable: true, MaxRunnerSessions: 8}
	sessions := &stubSessionStore{}
	sessions.Register("fp1", "v", "pod", "inst", "trace", "u")
	q := newQuotaForTest(t, []*domain.Group{g}, map[string][]string{"g1": {"u"}}, sessions)
	if err := q.CheckEngineAccess(context.Background(), &domain.Identity{UserID: "u", Role: domain.RoleUser, GroupIDs: []string{"g1"}}); err != nil {
		t.Fatalf("under limit: %v", err)
	}
}

func TestQuotaMultiGroupDoubleCount(t *testing.T) {
	// User in g1 (max 1) and g2 (max 1). One lease counts against BOTH groups.
	g1 := &domain.Group{ID: "g1", Name: "G1", AgentAvailable: true, MaxRunnerSessions: 1}
	g2 := &domain.Group{ID: "g2", Name: "G2", AgentAvailable: true, MaxRunnerSessions: 1}
	sessions := &stubSessionStore{}
	sessions.Register("fp1", "v", "pod", "inst", "trace", "u")
	q := newQuotaForTest(t, []*domain.Group{g1, g2}, map[string][]string{"g1": {"u"}, "g2": {"u"}}, sessions)
	// One lease fills both groups (each max 1) -> exhausted.
	err := q.CheckEngineAccess(context.Background(), &domain.Identity{UserID: "u", Role: domain.RoleUser, GroupIDs: []string{"g1", "g2"}})
	if err != domain.ErrQuotaExhausted {
		t.Fatalf("multi-group double-count: %v, want ErrQuotaExhausted", err)
	}
}

func TestQuotaAnyAvailableGroupAdmits(t *testing.T) {
	// g1 full (max 1, 1 lease), g2 has capacity (max 5, 0 leases) -> admitted via g2.
	g1 := &domain.Group{ID: "g1", Name: "G1", AgentAvailable: true, MaxRunnerSessions: 1}
	g2 := &domain.Group{ID: "g2", Name: "G2", AgentAvailable: true, MaxRunnerSessions: 5}
	sessions := &stubSessionStore{}
	sessions.Register("fp1", "v", "pod", "inst", "trace", "u")
	q := newQuotaForTest(t, []*domain.Group{g1, g2}, map[string][]string{"g1": {"u"}, "g2": {"u"}}, sessions)
	if err := q.CheckEngineAccess(context.Background(), &domain.Identity{UserID: "u", Role: domain.RoleUser, GroupIDs: []string{"g1", "g2"}}); err != nil {
		t.Fatalf("any-available: %v", err)
	}
}

func TestQuotaUsageByGroup(t *testing.T) {
	g1 := &domain.Group{ID: "g1", Name: "G1", AgentAvailable: true}
	g2 := &domain.Group{ID: "g2", Name: "G2", AgentAvailable: true}
	sessions := &stubSessionStore{}
	sessions.Register("fp1", "v", "pod", "inst", "trace", "u1")
	sessions.Register("fp2", "v", "pod", "inst", "trace", "u1")
	sessions.Register("fp3", "v", "pod", "inst", "trace", "u2")
	q := newQuotaForTest(t, []*domain.Group{g1, g2}, map[string][]string{
		"g1": {"u1", "u2"},
		"g2": {"u1"},
	}, sessions)
	usage, err := q.UsageByGroup(context.Background())
	if err != nil {
		t.Fatalf("UsageByGroup: %v", err)
	}
	// g1 has u1 (2 leases) + u2 (1 lease) = 3; g2 has u1 (2 leases) = 2.
	if usage["g1"] != 3 {
		t.Fatalf("g1 usage = %d, want 3", usage["g1"])
	}
	if usage["g2"] != 2 {
		t.Fatalf("g2 usage = %d, want 2", usage["g2"])
	}
}
