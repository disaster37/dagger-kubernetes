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
	mu sync.Mutex
	// rotateTo, when set, mutates u.OAuthTokenCiphertext on every successful
	// Revalidate — emulating the OIDC refreshingSource rotating and persisting
	// the credential mid-check.
	rotateTo  string
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
	if f.rotateTo != "" {
		u.OAuthTokenCiphertext = f.rotateTo
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
	revalidator := NewOAuthRevalidator(provider, nil, nil, 0, usersSvc, r.groups, nil, logger, cfg)
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

	rv := NewOAuthRevalidator(provider, nil, nil, 0, usersSvc, r.groups, nil, testLogger(), cfg)
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
	tokenSvc := NewTokenService(r.tokens, testLogger(), nil)
	rv.tokens = tokenSvc
	plaintext, _, err := tokenSvc.Generate(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("generate api token: %v", err)
	}

	if _, err := rv.Check(context.Background(), u); !errors.Is(err, domain.ErrSessionRevoked) {
		t.Fatalf("expected ErrSessionRevoked, got %v", err)
	}
	if !u.Deactivated() {
		t.Fatal("expected user to be deactivated")
	}
	// Regression (issue #30): positive revocation (ErrForbidden) still deletes
	// the API token — only credential expiry became non-destructive.
	if _, err := tokenSvc.Validate(context.Background(), plaintext); err == nil {
		t.Fatal("expected API token to be revoked on ErrForbidden")
	}
}

// newExpiredFixture builds a revalidator with a healthy baseline: a user that
// is a member of group g1, an API token, and one successful cached check at
// base. clockVar is captured by the returned revalidator's clock so tests can
// advance time by reassigning it.
func newExpiredFixture(t *testing.T, provider *fakeRevalidateProvider, cfg OAuthRevalidatorConfig, now *time.Time) (*OAuthRevalidator, *domain.User, *TokenService) {
	t.Helper()
	r := newServiceDB(t)
	logger := testLogger()
	usersSvc := NewUserService(r.users, r.groups, logger)
	ctx := context.Background()

	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "oidc", OAuthTokenCiphertext: "cred-v1"}
	if err := r.users.Create(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	g := &domain.Group{ID: "g1", Name: "group1"}
	if err := r.groups.Create(ctx, g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := r.groups.SetMembers(ctx, g.ID, []string{u.ID}); err != nil {
		t.Fatalf("set members: %v", err)
	}
	tokenSvc := NewTokenService(r.tokens, logger, nil)
	rv := NewOAuthRevalidator(provider, nil, nil, 0, usersSvc, r.groups, tokenSvc, logger, cfg)
	rv.clock = func() time.Time { return *now }
	return rv, u, tokenSvc
}

// TestRevalidatorExpiredGracePolicy covers stateExpired (issue #30): an
// unusable credential serves cached groups within revalidate_grace, then
// fail-open/fail-closed applies — the user is never deactivated and the API
// token is never deleted.
func TestRevalidatorExpiredGracePolicy(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		at       time.Time
		failOpen bool
		wantDeny bool
	}{
		{name: "within grace serves cached groups", at: base.Add(10 * time.Minute), failOpen: false, wantDeny: false},
		{name: "past grace fail-closed denies", at: base.Add(2 * time.Hour), failOpen: false, wantDeny: true},
		{name: "past grace fail-open serves cached groups", at: base.Add(2 * time.Hour), failOpen: true, wantDeny: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &fakeRevalidateProvider{groups: []string{"g1"}}
			cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour, FailOpen: tt.failOpen}
			now := base
			rv, u, tokenSvc := newExpiredFixture(t, provider, cfg, &now)
			ctx := context.Background()

			plaintext, _, err := tokenSvc.Generate(ctx, u.ID)
			if err != nil {
				t.Fatalf("generate api token: %v", err)
			}

			// 1) Healthy baseline: cache one successful check.
			gids, err := rv.Check(ctx, u)
			if err != nil || len(gids) == 0 {
				t.Fatalf("baseline check: gids=%v err=%v", gids, err)
			}

			// 2) The stored credential becomes unusable.
			provider.err = errOAuthCredentialExpired
			provider.groups = nil
			now = tt.at
			gids, err = rv.Check(ctx, u)
			if tt.wantDeny {
				if !errors.Is(err, domain.ErrUnauthenticated) {
					t.Fatalf("err = %v, want ErrUnauthenticated past grace (fail-closed)", err)
				}
			} else {
				if err != nil {
					t.Fatalf("err = %v, want cached groups served", err)
				}
				if len(gids) == 0 {
					t.Fatal("expected cached group IDs")
				}
			}
			if entry := rv.cache[u.ID]; entry.state != stateExpired {
				t.Fatalf("state = %v, want stateExpired", entry.state)
			}
			if u.DeactivatedAt != nil {
				t.Fatal("user must not be deactivated on stateExpired")
			}
			if _, err := tokenSvc.Validate(ctx, plaintext); err != nil {
				t.Fatalf("API token must not be revoked on stateExpired: %v", err)
			}
		})
	}
}

// TestRevalidatorExpiredRecoversAfterRelogin: re-login stores a fresh
// credential, which must force an immediate re-check (fresh() == false) instead
// of serving the stateExpired entry until revalidate_interval elapses.
func TestRevalidatorExpiredRecoversAfterRelogin(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	provider := &fakeRevalidateProvider{groups: []string{"g1"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	now := base
	rv, u, _ := newExpiredFixture(t, provider, cfg, &now)
	ctx := context.Background()

	// 1) Healthy baseline (1 provider call).
	if gids, err := rv.Check(ctx, u); err != nil || len(gids) == 0 {
		t.Fatalf("baseline check: gids=%v err=%v", gids, err)
	}

	// 2) Credential expires -> stateExpired, still within its TTL (2 calls).
	provider.err = errOAuthCredentialExpired
	provider.groups = nil
	now = base.Add(10 * time.Minute)
	if gids, err := rv.Check(ctx, u); err != nil || len(gids) == 0 {
		t.Fatalf("expired check: gids=%v err=%v", gids, err)
	}

	// 3) Re-login writes a fresh credential: the very next check must re-query
	// the IdP (within the stateExpired entry TTL) and recover to stateOK.
	u.OAuthTokenCiphertext = "cred-v2-after-relogin"
	provider.err = nil
	provider.groups = []string{"g1"}
	now = base.Add(11 * time.Minute)
	gids, err := rv.Check(ctx, u)
	if err != nil {
		t.Fatalf("check after re-login: %v", err)
	}
	if len(gids) == 0 {
		t.Fatal("expected group IDs after re-login")
	}
	if provider.callCount != 3 {
		t.Fatalf("provider calls = %d, want 3 (re-login must force a re-check)", provider.callCount)
	}
	if entry := rv.cache[u.ID]; entry.state != stateOK {
		t.Fatalf("state = %v, want stateOK after re-login", entry.state)
	}
	if u.DeactivatedAt != nil {
		t.Fatal("user must stay active")
	}
}

// TestRevalidatorRefreshResnapshotsRotatedCredential: Revalidate's
// refreshingSource may rotate and persist the credential mid-check; the
// success block must re-snapshot entry.credential so a later stateExpired
// entry does not look like it has a "changed credential" and force one
// spurious re-check (issue #30 follow-up).
func TestRevalidatorRefreshResnapshotsRotatedCredential(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	provider := &fakeRevalidateProvider{groups: []string{"g1"}, rotateTo: "rotated"}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	now := base
	rv, u, _ := newExpiredFixture(t, provider, cfg, &now)
	ctx := context.Background()

	// The successful Revalidate rotated the credential from the fixture's
	// "cred-v1" to "rotated"; the entry must snapshot the rotated value.
	gids, err := rv.Check(ctx, u)
	if err != nil || len(gids) == 0 {
		t.Fatalf("check: gids=%v err=%v", gids, err)
	}
	if u.OAuthTokenCiphertext != "rotated" {
		t.Fatalf("user credential = %q, want %q", u.OAuthTokenCiphertext, "rotated")
	}
	if got := rv.cache[u.ID].credential; got != "rotated" {
		t.Fatalf("entry.credential = %q, want %q (re-snapshot after mid-check rotation)", got, "rotated")
	}

	// A same-TTL follow-up Check must be a cache hit: the rotated credential
	// must not be mistaken for a re-login and force a second IdP call.
	now = base.Add(time.Minute)
	if gids, err := rv.Check(ctx, u); err != nil || len(gids) == 0 {
		t.Fatalf("second check: gids=%v err=%v", gids, err)
	}
	if provider.callCount != 1 {
		t.Fatalf("provider calls = %d, want 1 (rotated credential must not force a re-check)", provider.callCount)
	}
}

// TestRevalidatorColdStartExpiredFailOpenDenies: a cold-start stateExpired
// entry (never a successful check → zero lastGood, no cached groups) must be
// denied even with FailOpen=true — fail-open can only serve last-known-good,
// and there is none (issue #30 residual-risk test).
func TestRevalidatorColdStartExpiredFailOpenDenies(t *testing.T) {
	provider := &fakeRevalidateProvider{err: errors.New("idp down")}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour, FailOpen: true}
	rv := newTestRevalidator(t, provider, cfg)

	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "oidc", OAuthTokenCiphertext: "cred-v1"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rv.users = NewUserService(r.users, r.groups, testLogger())

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rv.cache["u1"] = &revalidationEntry{
		state:      stateExpired,
		groupIDs:   nil,
		lastGood:   time.Time{},
		checkedAt:  base,
		expiresAt:  base.Add(5 * time.Minute), // fresh: served directly
		credential: u.OAuthTokenCiphertext,
	}
	rv.clock = func() time.Time { return base }

	_, err := rv.Check(context.Background(), u)
	if !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated (cold-start fail-open denies: no last-known-good)", err)
	}
	if provider.callCount != 0 {
		t.Fatalf("provider calls = %d, want 0 (fresh entry is served, not refreshed)", provider.callCount)
	}
}

// TestJitteredTTLStateExpiredUsesFullInterval: stateExpired retries on the full
// revalidate_interval (never min(interval, grace)) so a permanently-dead
// credential is not hammered against the IdP token endpoint; stateUnavailable
// keeps the shorter degraded retry window.
func TestJitteredTTLStateExpiredUsesFullInterval(t *testing.T) {
	tests := []struct {
		name     string
		state    revalidationState
		interval time.Duration
		grace    time.Duration
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{
			name:     "stateExpired uses full interval",
			state:    stateExpired,
			interval: 5 * time.Minute, grace: time.Minute,
			wantMin: 4*time.Minute + 30*time.Second, wantMax: 5*time.Minute + 30*time.Second,
		},
		{
			name:     "stateUnavailable shrinks to grace",
			state:    stateUnavailable,
			interval: 5 * time.Minute, grace: time.Minute,
			wantMin: 54 * time.Second, wantMax: 66 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jitteredTTL(tt.state, tt.interval, tt.grace)
			if got < tt.wantMin || got > tt.wantMax {
				t.Fatalf("jitteredTTL(%v) = %v, want within [%v, %v]", tt.state, got, tt.wantMin, tt.wantMax)
			}
		})
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
	rv := NewOAuthRevalidator(provider, nil, nil, 0, usersSvc, r.groups, nil, testLogger(), cfg)
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
	rv := NewOAuthRevalidator(provider, mapper, nil, 0, usersSvc, r.groups, nil, logger, cfg)
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

func TestOAuthRevalidatorRefreshAutoCreatesMappedGroup(t *testing.T) {
	r := newServiceDB(t)
	logger := testLogger()
	usersSvc := NewUserService(r.users, r.groups, logger)
	ctx := context.Background()

	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "oidc"}
	if err := r.users.Create(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	mapper, err := NewGroupMapper([]domain.GroupMappingRule{{Pattern: "^devs$", Replacement: "auto-rev"}})
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	provider := &fakeRevalidateProvider{groups: []string{"devs"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	rv := NewOAuthRevalidator(provider, mapper, nil, 4, usersSvc, r.groups, nil, logger, cfg)
	rv.clock = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	if _, err := rv.Check(ctx, u); err != nil {
		t.Fatalf("check: %v", err)
	}

	created, err := r.groups.GetByName(ctx, "auto-rev")
	if err != nil {
		t.Fatalf("auto-created group not persisted: %v", err)
	}
	if created.MaxRunnerSessions != 4 || !created.AgentAvailable {
		t.Fatalf("auto-created group = %+v, want quota 4 and agent available", created)
	}
	member, _ := r.groups.GroupsForUser(ctx, u.ID)
	if len(member) != 1 || member[0].ID != created.ID {
		t.Fatalf("memberships = %v, want only the auto-created group", member)
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

func TestNormalizeGroupList(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{name: "nil", input: nil, want: nil},
		{name: "empty", input: []string{}, want: nil},
		{name: "single", input: []string{"a"}, want: []string{"a"}},
		{name: "duplicates", input: []string{"b", "a", "b"}, want: []string{"a", "b"}},
		{name: "empty strings dropped", input: []string{"", "a", ""}, want: []string{"a"}},
		{name: "sort", input: []string{"c", "a", "b"}, want: []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeGroupList(tt.input)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("normalizeGroupList = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("normalizeGroupList = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestApplyOAuthAdminRole(t *testing.T) {
	tests := []struct {
		name           string
		initialRole    domain.Role
		initialOAAdmin bool
		adminGroups    []string
		upstreamGroups []string
		wantRole       domain.Role
		wantOAAdmin    bool
		wantChanged    bool
	}{
		{
			name:        "promote user on match",
			initialRole: domain.RoleUser, initialOAAdmin: false,
			adminGroups: []string{"platform-admins"}, upstreamGroups: []string{"platform-admins"},
			wantRole: domain.RoleAdmin, wantOAAdmin: true, wantChanged: true,
		},
		{
			name:        "demote OAuth admin on non-match",
			initialRole: domain.RoleAdmin, initialOAAdmin: true,
			adminGroups: []string{"platform-admins"}, upstreamGroups: []string{"other"},
			wantRole: domain.RoleUser, wantOAAdmin: false, wantChanged: true,
		},
		{
			name:        "preserve manual admin on non-match",
			initialRole: domain.RoleAdmin, initialOAAdmin: false,
			adminGroups: []string{"platform-admins"}, upstreamGroups: []string{"other"},
			wantRole: domain.RoleAdmin, wantOAAdmin: false, wantChanged: false,
		},
		{
			name:        "leave unchanged when already admin + match",
			initialRole: domain.RoleAdmin, initialOAAdmin: false,
			adminGroups: []string{"platform-admins"}, upstreamGroups: []string{"platform-admins"},
			wantRole: domain.RoleAdmin, wantOAAdmin: false, wantChanged: false,
		},
		{
			name:        "no change when empty admin_groups",
			initialRole: domain.RoleUser, initialOAAdmin: false,
			adminGroups: []string{}, upstreamGroups: []string{"platform-admins"},
			wantRole: domain.RoleUser, wantOAAdmin: false, wantChanged: false,
		},
		{
			name:        "no change when nil admin_groups",
			initialRole: domain.RoleUser, initialOAAdmin: false,
			adminGroups: nil, upstreamGroups: []string{"platform-admins"},
			wantRole: domain.RoleUser, wantOAAdmin: false, wantChanged: false,
		},
		{
			name:        "no change when empty upstream groups",
			initialRole: domain.RoleUser, initialOAAdmin: false,
			adminGroups: []string{"platform-admins"}, upstreamGroups: nil,
			wantRole: domain.RoleUser, wantOAAdmin: false, wantChanged: false,
		},
		{
			name:        "case sensitive match fails",
			initialRole: domain.RoleUser, initialOAAdmin: false,
			adminGroups: []string{"Platform-Admins"}, upstreamGroups: []string{"platform-admins"},
			wantRole: domain.RoleUser, wantOAAdmin: false, wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &domain.User{Role: tt.initialRole, OAuthAdmin: tt.initialOAAdmin}
			changed := applyOAuthAdminRole(u, tt.adminGroups, tt.upstreamGroups)
			if changed != tt.wantChanged {
				t.Fatalf("applyOAuthAdminRole changed = %v, want %v", changed, tt.wantChanged)
			}
			if u.Role != tt.wantRole {
				t.Fatalf("role = %v, want %v", u.Role, tt.wantRole)
			}
			if u.OAuthAdmin != tt.wantOAAdmin {
				t.Fatalf("OAuthAdmin = %v, want %v", u.OAuthAdmin, tt.wantOAAdmin)
			}
		})
	}
}

func TestRevalidatorAdminGroupPromotion(t *testing.T) {
	provider := &fakeRevalidateProvider{groups: []string{"platform-admins"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}

	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "oidc"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	usersSvc := NewUserService(r.users, r.groups, testLogger())

	rv := NewOAuthRevalidator(provider, nil, []string{"platform-admins"}, 0, usersSvc, r.groups, nil, testLogger(), cfg)
	rv.clock = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	_, err := rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	got, _ := r.users.Get(context.Background(), u.ID)
	if got.Role != domain.RoleAdmin {
		t.Fatalf("role = %v, want admin", got.Role)
	}
	if !got.OAuthAdmin {
		t.Fatal("expected OAuthAdmin = true")
	}
}

func TestRevalidatorAdminGroupDemotion(t *testing.T) {
	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleAdmin, OAuthProvider: "oidc", OAuthAdmin: true}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	usersSvc := NewUserService(r.users, r.groups, testLogger())

	provider := &fakeRevalidateProvider{groups: []string{"other-group"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	rv := NewOAuthRevalidator(provider, nil, []string{"platform-admins"}, 0, usersSvc, r.groups, nil, testLogger(), cfg)
	rv.clock = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	_, err := rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	got, _ := r.users.Get(context.Background(), u.ID)
	if got.Role != domain.RoleUser {
		t.Fatalf("role = %v, want user", got.Role)
	}
	if got.OAuthAdmin {
		t.Fatal("expected OAuthAdmin = false")
	}
}

func TestRevalidatorManualAdminNotDemoted(t *testing.T) {
	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleAdmin, OAuthProvider: "oidc", OAuthAdmin: false}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	usersSvc := NewUserService(r.users, r.groups, testLogger())

	provider := &fakeRevalidateProvider{groups: []string{"other-group"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}
	rv := NewOAuthRevalidator(provider, nil, []string{"platform-admins"}, 0, usersSvc, r.groups, nil, testLogger(), cfg)
	rv.clock = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	_, err := rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	got, _ := r.users.Get(context.Background(), u.ID)
	if got.Role != domain.RoleAdmin {
		t.Fatalf("role = %v, want admin (manual admin not demoted)", got.Role)
	}
	if got.OAuthAdmin {
		t.Fatal("expected OAuthAdmin = false (manual promotion not taken over)")
	}
}

func TestRevalidatorOAuthGroupsPersisted(t *testing.T) {
	provider := &fakeRevalidateProvider{groups: []string{"g1", "g2"}}
	cfg := OAuthRevalidatorConfig{Interval: 5 * time.Minute, Grace: time.Hour}

	r := newServiceDB(t)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "oidc"}
	if err := r.users.Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	usersSvc := NewUserService(r.users, r.groups, testLogger())

	rv := NewOAuthRevalidator(provider, nil, nil, 0, usersSvc, r.groups, nil, testLogger(), cfg)
	rv.clock = func() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

	_, err := rv.Check(context.Background(), u)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	got, _ := r.users.Get(context.Background(), u.ID)
	if len(got.OAuthGroups) != 2 {
		t.Fatalf("OAuthGroups = %v, want [\"g1\", \"g2\"]", got.OAuthGroups)
	}
	if got.OAuthGroups[0] != "g1" || got.OAuthGroups[1] != "g2" {
		t.Fatalf("OAuthGroups = %v, want [\"g1\", \"g2\"]", got.OAuthGroups)
	}
}

func TestValidateGroupName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "valid simple", input: "admin", want: true},
		{name: "valid with dash", input: "acme-eng", want: true},
		{name: "valid with dot", input: "acme.eng", want: true},
		{name: "starts with number ok", input: "9bad", want: true},
		{name: "with space", input: "with space", want: false},
		{name: "empty", input: "", want: false},
		{name: "65 chars too long", input: "a1234567890123456789012345678901234567890123456789012345678901234", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateGroupName(tt.input)
			if got != tt.want {
				t.Fatalf("ValidateGroupName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
