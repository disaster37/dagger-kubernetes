package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// fakeRevalidateProvider is an OAuthProvider stub for revalidator tests.
type fakeRevalidateProvider struct {
	mu        sync.Mutex
	groups    []string
	err       error
	callCount int
}

func (f *fakeRevalidateProvider) LoginURL(state string) string { return "" }
func (f *fakeRevalidateProvider) Complete(_ context.Context, _ string) (access, refresh string, u *domain.User, err error) {
	return "", "", &domain.User{ID: "u1", Username: "alice"}, nil
}
func (f *fakeRevalidateProvider) Revalidate(ctx context.Context, u *domain.User) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]string, len(f.groups))
	copy(out, f.groups)
	return out, nil
}

func newTestRevalidator(t *testing.T, provider *fakeRevalidateProvider, cfg OAuthRevalidatorConfig) *OAuthRevalidator {
	t.Helper()
	r := newServiceDB(t)
	logger := testLogger()
	usersSvc := NewUserService(r.users, r.groups, logger)
	revalidator := NewOAuthRevalidator(provider, nil, usersSvc, r.groups, nil, logger, cfg)
	// Override clock for deterministic testing.
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	revalidator.clock = func() time.Time { return now }
	return revalidator
}

func TestRevalidatorCacheHit(t *testing.T) {
	provider := &fakeRevalidateProvider{groups: []string{"g1"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}

	// Create a shared serviceDB so the revalidator and test share the same groups.
	r := newServiceDB(t)

	// Create the user FIRST (membership validation requires user to exist).
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	g := &domain.Group{ID: "g1", Name: "group1"}
	if err := r.groups.Create(context.Background(), g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	r.groups.SetMembers(context.Background(), g.ID, []string{"u1"})
	usersSvc := NewUserService(r.users, r.groups, testLogger())

	rv := NewOAuthRevalidator(provider, nil, usersSvc, r.groups, nil, testLogger(), cfg)
	rv.clock = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	gids, err := rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("first check: %v", err)
	}
	if len(gids) == 0 {
		t.Fatal("expected group IDs from cache hit")
	}
	if provider.callCount != 1 {
		t.Fatalf("expected 1 provider call, got %d", provider.callCount)
	}

	// Second check within TTL should be a cache hit.
	gids2, err := rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if len(gids2) == 0 {
		t.Fatal("expected group IDs from cache")
	}
	if provider.callCount != 1 {
		t.Fatalf("expected still 1 provider call (cache hit), got %d", provider.callCount)
	}
	// Third check guards against the mutex being left locked on the cache-hit
	// path (a regression that would deadlock all subsequent Checks).
	gids3, err := rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("third check: %v", err)
	}
	if len(gids3) == 0 {
		t.Fatal("expected group IDs from cache")
	}
	if provider.callCount != 1 {
		t.Fatalf("expected still 1 provider call after third check, got %d", provider.callCount)
	}
	_ = gids // same as gids2/gids3
}

func TestRevalidatorRevokes(t *testing.T) {
	provider := &fakeRevalidateProvider{err: domain.ErrForbidden}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	rv := newTestRevalidator(t, provider, cfg)

	// Create the user in the DB.
	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rv.users = NewUserService(r.users, r.groups, testLogger())

	_, err := rv.Check(context.Background(), u)
	if !errors.Is(err, domain.ErrSessionRevoked) {
		t.Fatalf("expected ErrSessionRevoked, got %v", err)
	}
	if !u.Deactivated() {
		t.Fatal("expected user to be deactivated")
	}
}

func TestRevalidatorReLoginAfterRevocation(t *testing.T) {
	provider := &fakeRevalidateProvider{groups: []string{"g1"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}

	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	g := &domain.Group{ID: "g1", Name: "group1"}
	if err := r.groups.Create(context.Background(), g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := r.groups.SetMembers(context.Background(), g.ID, []string{"u1"}); err != nil {
		t.Fatalf("set members: %v", err)
	}
	usersSvc := NewUserService(r.users, r.groups, testLogger())
	rv := NewOAuthRevalidator(provider, nil, usersSvc, r.groups, nil, testLogger(), cfg)
	rv.clock = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	// Revoke: the IdP now denies.
	provider.err = domain.ErrForbidden
	if _, err := rv.Check(context.Background(), u); !errors.Is(err, domain.ErrSessionRevoked) {
		t.Fatalf("expected revocation, got %v", err)
	}
	if !u.Deactivated() {
		t.Fatal("expected user to be deactivated")
	}

	// Re-login: DB deactivation is cleared and the IdP allows again. The
	// revoked cache entry must be re-checked rather than denying forever.
	u.DeactivatedAt = nil
	provider.err = nil
	provider.groups = []string{"g1"}

	gids, err := rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("expected allow after re-login, got %v", err)
	}
	if len(gids) == 0 {
		t.Fatal("expected group IDs after re-login")
	}
	if u.Deactivated() {
		t.Fatal("user should remain active after re-login")
	}
}

func TestRevalidatorReconcileAddRemove(t *testing.T) {
	// Create two groups: one existing (admin-managed), one new.
	r := newServiceDB(t)
	logger := testLogger()
	usersSvc := NewUserService(r.users, r.groups, logger)

	// Create the user in the DB first.
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Create the groups first using domain.Group directly.
	gAdmin := &domain.Group{ID: "g-admin", Name: "admins"}
	if err := r.groups.Create(context.Background(), gAdmin); err != nil {
		t.Fatalf("create admins group: %v", err)
	}
	gNew := &domain.Group{ID: "g-new", Name: "newteam"}
	if err := r.groups.Create(context.Background(), gNew); err != nil {
		t.Fatalf("create newteam group: %v", err)
	}

	// Add user to admin group (admin-managed).
	r.groups.SetMembers(context.Background(), gAdmin.ID, []string{"u1"})

	// Use a mapper that maps "newteam" → "newteam".
	mapper, err := NewGroupMapper([]domain.GroupMappingRule{{Pattern: "^newteam$", Replacement: "newteam"}})
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}

	provider := &fakeRevalidateProvider{groups: []string{"newteam"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	rv := NewOAuthRevalidator(provider, mapper, usersSvc, r.groups, nil, logger, cfg)
	rv.clock = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	_, err = rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	// User should now be in newteam (reconciled).
	gids, _ := r.groups.GroupsForUser(context.Background(), "u1")
	hasNew := false
	for _, g := range gids {
		if g.ID == gNew.ID {
			hasNew = true
		}
	}
	if !hasNew {
		t.Fatal("expected user to be added to newteam via reconciliation")
	}
}

func TestRevalidatorFailClosed(t *testing.T) {
	provider := &fakeRevalidateProvider{err: errors.New("transport error")}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Second, FailOpen: false}
	rv := newTestRevalidator(t, provider, cfg)

	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rv.users = NewUserService(r.users, r.groups, testLogger())

	_, err := rv.Check(context.Background(), u)
	if err == nil {
		t.Fatal("expected denial with fail-closed and no prior good")
	}
}

func TestRevalidatorFailOpen(t *testing.T) {
	provider := &fakeRevalidateProvider{err: errors.New("transport error")}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Second, FailOpen: true}
	rv := newTestRevalidator(t, provider, cfg)

	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rv.users = NewUserService(r.users, r.groups, testLogger())

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rv.cache["u1"] = &revalidationEntry{
		state:     stateUnavailable,
		groupIDs:  []string{"g1"},
		lastGood:  base.Add(-2 * time.Second), // past grace (grace=1s)
		checkedAt: base,
		expiresAt: base.Add(5 * time.Minute), // fresh: serve directly
	}
	rv.clock = func() time.Time { return base }

	if _, err := rv.Check(context.Background(), u); err != nil {
		t.Fatalf("expected allow with fail-open and last-known-good, got: %v", err)
	}
}

func TestRevalidatorGraceWindow(t *testing.T) {
	provider := &fakeRevalidateProvider{err: errors.New("transport error")}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: 10 * time.Minute, FailOpen: false}
	rv := newTestRevalidator(t, provider, cfg)

	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rv.users = NewUserService(r.users, r.groups, testLogger())

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rv.cache["u1"] = &revalidationEntry{
		state:     stateUnavailable,
		groupIDs:  []string{"g1"},
		lastGood:  base.Add(-30 * time.Second), // within grace (grace=10m)
		checkedAt: base,
		expiresAt: base.Add(5 * time.Minute), // fresh: serve directly
	}
	rv.clock = func() time.Time { return base }

	gids, err := rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("expected allow within grace window, got: %v", err)
	}
	if len(gids) == 0 {
		t.Fatal("expected cached group IDs within grace")
	}
}

func TestRevalidatorNoCredential(t *testing.T) {
	provider := &fakeRevalidateProvider{}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	rv := newTestRevalidator(t, provider, cfg)

	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rv.users = NewUserService(r.users, r.groups, testLogger())

	// User with no stored credential; the provider reports no credential.
	u.OAuthTokenCiphertext = ""
	provider.err = errOAuthNoCredential

	_, err := rv.Check(context.Background(), u)
	// Should NOT revoke when there's no credential — allow instead.
	if err != nil {
		t.Fatalf("expected allow for no-credential user, got: %v", err)
	}
	if u.Deactivated() {
		t.Fatal("user should not be deactivated when no credential exists")
	}
}

func TestRevalidatorSingleFlight(t *testing.T) {
	provider := &fakeRevalidateProvider{groups: []string{"g1"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	rv := newTestRevalidator(t, provider, cfg)

	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rv.users = NewUserService(r.users, r.groups, testLogger())

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rv.Check(context.Background(), u)
		}()
	}
	wg.Wait()

	if provider.callCount != 1 {
		t.Fatalf("expected exactly 1 provider call (single-flight), got %d", provider.callCount)
	}
}

func TestRevalidatorDeactivatedSkipsIDP(t *testing.T) {
	provider := &fakeRevalidateProvider{groups: []string{"g1"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	rv := newTestRevalidator(t, provider, cfg)

	r := newServiceDB(t)
	now := time.Now()
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "github", DeactivatedAt: &now}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rv.users = NewUserService(r.users, r.groups, testLogger())

	_, err := rv.Check(context.Background(), u)
	if !errors.Is(err, domain.ErrSessionRevoked) {
		t.Fatalf("expected ErrSessionRevoked for already-deactivated user, got: %v", err)
	}
	if provider.callCount != 0 {
		t.Fatalf("expected 0 provider calls (deactivated skips IDP), got %d", provider.callCount)
	}
}
