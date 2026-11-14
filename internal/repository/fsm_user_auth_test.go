package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func applyResp(t *testing.T, f *FSM, kind commandKind, payload any) (interface{}, error) {
	t.Helper()
	return f.applyCommand(mustCommand(t, kind, payload))
}

// seedFSMUser creates a user via the create command.
func seedFSMUser(t *testing.T, f *FSM, c *cmdUser) {
	t.Helper()
	c.Create = true
	if _, err := applyResp(t, f, kindUpsertUser, c); err != nil {
		t.Fatalf("seed user %s: %v", c.ID, err)
	}
}

func seedFSMGroup(t *testing.T, f *FSM, id, name string) {
	t.Helper()
	if _, err := applyResp(t, f, kindUpsertGroup, cmdGroup{Group: domain.Group{ID: id, Name: name}, Create: true}); err != nil {
		t.Fatalf("seed group %s: %v", id, err)
	}
}

func TestFSMUserCASUpdate(t *testing.T) {
	f := newTestFSM(t)
	now := time.Now().UTC()
	seedFSMUser(t, f, &cmdUser{ID: "u1", Username: "Alice", Role: domain.RoleUser, OAuthProvider: "oidc", OAuthID: "s1", OAuthLoginVersion: 4, TokenVersion: 2})

	// Stale precondition (pointer to 0 != stored 1) must have zero effect.
	stale := uint64(0)
	_, err := applyResp(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "Alice", Role: domain.RoleAdmin, ExpectedRevision: &stale})
	if !errors.Is(err, domain.ErrStaleUser) {
		t.Fatalf("stale update: %v, want ErrStaleUser", err)
	}
	got, _ := f.readUserByID("u1")
	if got.Role != domain.RoleUser || got.Revision != 1 {
		t.Fatalf("stale update changed state: %+v", got)
	}

	// Matching precondition succeeds, increments revision, preserves identity
	// and dedicated-command-only fields.
	rev := uint64(1)
	_, err = applyResp(t, f, kindUpsertUser, &cmdUser{
		ID: "u1", Username: "Alice", Role: domain.RoleAdmin, ExpectedRevision: &rev,
		OAuthLoginVersion: 999, OAuthProvider: "evil", OAuthID: "evil", DisabledAt: &now,
	})
	if err != nil {
		t.Fatalf("matching update: %v", err)
	}
	got, _ = f.readUserByID("u1")
	if got.Role != domain.RoleAdmin || got.Revision != 2 {
		t.Fatalf("update result: %+v", got)
	}
	if got.OAuthProvider != "oidc" || got.OAuthID != "s1" || got.OAuthLoginVersion != 4 || got.DisabledAt != nil {
		t.Fatalf("general update must not change identity/timestamps/login version: %+v", got)
	}
}

func TestFSMUserLoginCommitWhitelist(t *testing.T) {
	f := newTestFSM(t)
	now := time.Now().UTC()
	deactivated := now.Add(-time.Hour)
	seedFSMUser(t, f, &cmdUser{
		ID: "u1", Username: "Alice", Role: domain.RoleUser, OAuthProvider: "oidc", OAuthID: "s1",
		PasswordHash: "hash", TokenVersion: 3, OAuthLoginVersion: 5, DeactivatedAt: &deactivated,
	})
	rev := uint64(1) // read after seed

	res, err := applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind: domain.OAuthLoginCommit,
		User: &cmdUser{ID: "u1", OAuthProvider: "oidc", OAuthID: "s1", Role: domain.RoleAdmin,
			OAuthAdmin: true, OAuthTokenCiphertext: "ct2", OAuthGroups: []string{"devs"}},
		ExpectedRevision: rev,
	})
	if err != nil {
		t.Fatalf("login commit: %v", err)
	}
	out := res.(*domain.OAuthUserResult)
	if out.User.OAuthLoginVersion != 6 {
		t.Fatalf("login version = %d, want 6", out.User.OAuthLoginVersion)
	}
	if out.User.DeactivatedAt != nil {
		t.Fatal("login must clear IdP deactivation")
	}
	if out.User.PasswordHash != "hash" || out.User.TokenVersion != 3 {
		t.Fatalf("login must preserve password/token version: %+v", out.User)
	}
	if out.User.Role != domain.RoleAdmin || !out.User.OAuthAdmin || out.User.OAuthTokenCiphertext != "ct2" {
		t.Fatalf("login whitelist fields not applied: %+v", out.User)
	}
	// The returned result must be a copy: mutating it must not change state.
	out.User.OAuthGroups[0] = "mutated"
	stored, _ := f.readUserByID("u1")
	if stored.OAuthGroups[0] != "devs" {
		t.Fatal("FSM state mutated through returned slice")
	}
}

func TestFSMUserLoginRejectsBlocked(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		u    *cmdUser
		want error
	}{
		{"disabled", &cmdUser{ID: "u1", Username: "a", OAuthProvider: "oidc", OAuthID: "s1", DisabledAt: &now}, domain.ErrAccountBlocked},
		{"deleted", &cmdUser{ID: "u1", Username: "a", OAuthProvider: "oidc", OAuthID: "s1", DeletedAt: &now}, domain.ErrAccountBlocked},
		{"non-oauth", &cmdUser{ID: "u1", Username: "a"}, domain.ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFSM(t)
			seedFSMUser(t, f, tc.u)
			_, err := applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
				Kind:             domain.OAuthLoginCommit,
				User:             &cmdUser{ID: "u1", OAuthProvider: "oidc", OAuthID: "s1", Role: domain.RoleUser},
				ExpectedRevision: 1,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFSMUserFractionalMembershipAndFallback(t *testing.T) {
	f := newTestFSM(t)
	seedFSMGroup(t, f, "g1", "one")
	seedFSMGroup(t, f, "g2", "two")
	seedFSMUser(t, f, &cmdUser{ID: "u1", Username: "a", OAuthProvider: "oidc", OAuthID: "s1"})

	res, err := applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind:             domain.OAuthLoginCommit,
		User:             &cmdUser{ID: "u1", OAuthProvider: "oidc", OAuthID: "s1", Role: domain.RoleUser},
		ExpectedRevision: 1,
		ManagedGroupIDs:  []string{"g1"},
		FallbackGroupID:  "g2",
	})
	if err != nil {
		t.Fatalf("login with managed group: %v", err)
	}
	got := res.(*domain.OAuthUserResult)
	if len(got.GroupIDs) != 1 || got.GroupIDs[0] != "g1" {
		t.Fatalf("fallback must not apply with a non-empty membership: %v", got.GroupIDs)
	}

	// Second login with no managed groups: the fallback applies.
	res, err = applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind:             domain.OAuthLoginCommit,
		User:             &cmdUser{ID: "u1", OAuthProvider: "oidc", OAuthID: "s1", Role: domain.RoleUser},
		ExpectedRevision: got.User.Revision,
		FallbackGroupID:  "g2",
	})
	if err != nil {
		t.Fatalf("login fallback: %v", err)
	}
	got = res.(*domain.OAuthUserResult)
	if len(got.GroupIDs) != 1 || got.GroupIDs[0] != "g2" {
		t.Fatalf("fallback membership = %v, want [g2]", got.GroupIDs)
	}
}

func TestFSMUserMissingGroupRollback(t *testing.T) {
	f := newTestFSM(t)
	seedFSMUser(t, f, &cmdUser{ID: "u1", Username: "a", OAuthProvider: "oidc", OAuthID: "s1"})
	_, err := applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind:             domain.OAuthMembershipCommit,
		User:             &cmdUser{ID: "u1", Role: domain.RoleUser},
		ExpectedRevision: 1,
		ManagedGroupIDs:  []string{"missing"},
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	got, _ := f.readUserByID("u1")
	if got.Revision != 1 {
		t.Fatalf("failed mutation advanced revision to %d", got.Revision)
	}
}

func TestFSMUserCredentialCommitPreservesEverything(t *testing.T) {
	f := newTestFSM(t)
	seedFSMUser(t, f, &cmdUser{ID: "u1", Username: "a", OAuthProvider: "oidc", OAuthID: "s1",
		Role: domain.RoleAdmin, OAuthAdmin: true, OAuthLoginVersion: 7, TokenVersion: 2, OAuthTokenCiphertext: "old"})
	res, err := applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind:             domain.OAuthCredentialCommit,
		User:             &cmdUser{ID: "u1", OAuthTokenCiphertext: "new"},
		ExpectedRevision: 1,
	})
	if err != nil {
		t.Fatalf("credential commit: %v", err)
	}
	out := res.(*domain.OAuthUserResult)
	if out.User.OAuthTokenCiphertext != "new" || out.User.OAuthLoginVersion != 7 || out.User.TokenVersion != 2 || out.User.Role != domain.RoleAdmin {
		t.Fatalf("credential-only commit must preserve everything else: %+v", out.User)
	}
}

func TestFSMUserRevokeAtomic(t *testing.T) {
	f := newTestFSM(t)
	seedFSMUser(t, f, &cmdUser{ID: "u1", Username: "a", OAuthProvider: "oidc", OAuthID: "s1", TokenVersion: 2})
	if _, err := applyResp(t, f, kindUpsertToken, cmdToken{ID: "t1", UserID: "u1", TokenHash: "h1", Prefix: "dct_x"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	res, err := applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind:             domain.OAuthRevocationCommit,
		User:             &cmdUser{ID: "u1"},
		ExpectedRevision: 1,
	})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	out := res.(*domain.OAuthUserResult)
	if out.User.DeactivatedAt == nil || out.User.TokenVersion != 3 {
		t.Fatalf("revoke must deactivate and bump token version: %+v", out.User)
	}
	if _, err := f.readTokenByUser("u1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revoke must delete the API token, got %v", err)
	}

	// A stale revoke can never delete a token created after it.
	seedFSMUser(t, f, &cmdUser{ID: "u2", Username: "b", OAuthProvider: "oidc", OAuthID: "s2"})
	if _, err := applyResp(t, f, kindUpsertToken, cmdToken{ID: "t2", UserID: "u2", TokenHash: "h2", Prefix: "dct_y"}); err != nil {
		t.Fatalf("seed token2: %v", err)
	}
	if _, err := applyResp(t, f, kindUpsertUser, &cmdUser{ID: "u2", Username: "b", Role: domain.RoleUser}); err != nil {
		t.Fatalf("legacy update u2: %v", err)
	}
	_, err = applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind:             domain.OAuthRevocationCommit,
		User:             &cmdUser{ID: "u2"},
		ExpectedRevision: 1, // stale: the update above advanced it to 2
	})
	if !errors.Is(err, domain.ErrStaleUser) {
		t.Fatalf("stale revoke = %v, want ErrStaleUser", err)
	}
	if _, err := f.readTokenByUser("u2"); err != nil {
		t.Fatalf("stale revoke must not delete the new token: %v", err)
	}
}

func TestFSMUserLifecycleState(t *testing.T) {
	f := newTestFSM(t)
	tokenAt := time.Now().UTC()
	seedFSMUser(t, f, &cmdUser{ID: "u1", Username: "a", Role: domain.RoleUser, PasswordHash: "hash"})
	if _, err := applyResp(t, f, kindUpsertToken, cmdToken{ID: "t1", UserID: "u1", TokenHash: "h1", Prefix: "dct_x"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	// Disable: sets timestamp + bumps token version + deletes token.
	res, err := applyResp(t, f, kindUserState, cmdUserState{ActorID: "admin", ID: "u1", Action: domain.UserDisable, At: tokenAt})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if res.(*domain.User).DisabledAt == nil || res.(*domain.User).TokenVersion != 1 {
		t.Fatalf("disable result: %+v", res.(*domain.User))
	}
	if _, err := f.readTokenByUser("u1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("disable must delete the API token, got %v", err)
	}
	// Idempotent: a second disable does not bump counters again.
	before, _ := f.readUserByID("u1")
	res, err = applyResp(t, f, kindUserState, cmdUserState{ActorID: "admin", ID: "u1", Action: domain.UserDisable, At: tokenAt})
	if err != nil {
		t.Fatalf("second disable: %v", err)
	}
	if res.(*domain.User).Revision != before.Revision || res.(*domain.User).TokenVersion != before.TokenVersion {
		t.Fatal("idempotent disable must not bump counters")
	}

	// Enable clears the timestamp.
	res, err = applyResp(t, f, kindUserState, cmdUserState{ActorID: "admin", ID: "u1", Action: domain.UserEnable, At: tokenAt})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if res.(*domain.User).DisabledAt != nil {
		t.Fatal("enable must clear DisabledAt")
	}

	// Delete: tombstone reserves identity, clears credentials, removes membership.
	seedFSMGroup(t, f, "g1", "one")
	if _, err := applyResp(t, f, kindSetMembers, cmdSetMembers{GroupID: "g1", UserIDs: []string{"u1"}}); err != nil {
		t.Fatalf("set members: %v", err)
	}
	if _, err := applyResp(t, f, kindSoftDeleteUser, cmdSoftDeleteUser{ActorID: "admin", ID: "u1", At: tokenAt}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	got, err := f.readUserByID("u1")
	if err != nil {
		t.Fatalf("tombstone must be readable: %v", err)
	}
	if got.DeletedAt == nil || got.DisabledAt == nil || got.PasswordHash != "" || len(got.OAuthGroupIDs) != 0 {
		t.Fatalf("tombstone state: %+v", got)
	}
	if members := f.members("g1"); len(members) != 0 {
		t.Fatalf("delete must remove memberships: %v", members)
	}
	// Deleted rows are reserved: enable rejects, restore re-disables.
	if _, err := applyResp(t, f, kindUserState, cmdUserState{ActorID: "admin", ID: "u1", Action: domain.UserEnable, At: tokenAt}); !errors.Is(err, domain.ErrAccountBlocked) {
		t.Fatalf("enable deleted = %v, want ErrAccountBlocked", err)
	}
	res, err = applyResp(t, f, kindUserState, cmdUserState{ActorID: "admin", ID: "u1", Action: domain.UserRestore, At: tokenAt})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored := res.(*domain.User)
	if restored.DeletedAt != nil || restored.DisabledAt == nil || restored.PasswordHash != "" {
		t.Fatalf("restore must clear deletion, keep disabled, restore no credentials: %+v", restored)
	}
}

func TestFSMUserLastAdminAndSelfGuards(t *testing.T) {
	f := newTestFSM(t)
	now := time.Now().UTC()
	seedFSMUser(t, f, &cmdUser{ID: "admin1", Username: "root", Role: domain.RoleAdmin})

	if _, err := applyResp(t, f, kindUserState, cmdUserState{ActorID: "admin1", ID: "admin1", Action: domain.UserDisable, At: now}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("self-disable = %v, want ErrForbidden", err)
	}
	if _, err := applyResp(t, f, kindSoftDeleteUser, cmdSoftDeleteUser{ActorID: "admin1", ID: "admin1", At: now}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("self-delete = %v, want ErrForbidden", err)
	}
	rev := uint64(1)
	if _, err := applyResp(t, f, kindUpsertUser, &cmdUser{ID: "admin1", Username: "root", Role: domain.RoleUser, ExpectedRevision: &rev}); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("last-admin demotion = %v, want ErrLastAdmin", err)
	}
	if _, err := applyResp(t, f, kindUserState, cmdUserState{ActorID: "other", ID: "admin1", Action: domain.UserDisable, At: now}); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("last-admin disable = %v, want ErrLastAdmin", err)
	}
	if _, err := applyResp(t, f, kindSoftDeleteUser, cmdSoftDeleteUser{ActorID: "other", ID: "admin1", At: now}); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("last-admin delete = %v, want ErrLastAdmin", err)
	}

	// With a second active admin the same delete is allowed.
	seedFSMUser(t, f, &cmdUser{ID: "admin2", Username: "root2", Role: domain.RoleAdmin})
	if _, err := applyResp(t, f, kindSoftDeleteUser, cmdSoftDeleteUser{ActorID: "admin2", ID: "admin1", At: now}); err != nil {
		t.Fatalf("delete with a peer admin: %v", err)
	}
}

func TestFSMMembershipAdvancesRevision(t *testing.T) {
	f := newTestFSM(t)
	seedFSMUser(t, f, &cmdUser{ID: "u1", Username: "a", Role: domain.RoleUser})
	seedFSMGroup(t, f, "g1", "one")

	if _, err := applyResp(t, f, kindSetMembers, cmdSetMembers{GroupID: "g1", UserIDs: []string{"u1"}}); err != nil {
		t.Fatalf("set members: %v", err)
	}
	u1, _ := f.readUserByID("u1")
	if u1.Revision != 2 {
		t.Fatalf("membership change must advance u1 revision: %d", u1.Revision)
	}
	// An in-flight mutation using the pre-change revision conflicts.
	_, err := applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind:             domain.OAuthMembershipCommit,
		User:             &cmdUser{ID: "u1", Role: domain.RoleUser},
		ExpectedRevision: 1,
	})
	if !errors.Is(err, domain.ErrStaleUser) {
		t.Fatalf("stale membership commit = %v, want ErrStaleUser", err)
	}
}

func TestFSMTokenOwnerChecks(t *testing.T) {
	f := newTestFSM(t)
	if _, err := applyResp(t, f, kindUpsertToken, cmdToken{ID: "t1", UserID: "ghost", TokenHash: "h1", Prefix: "dct_x"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("token for missing owner = %v, want ErrNotFound", err)
	}
	seedFSMUser(t, f, &cmdUser{ID: "u1", Username: "a", Role: domain.RoleUser})
	if _, err := applyResp(t, f, kindUpsertToken, cmdToken{ID: "t1", UserID: "u1", TokenHash: "h1", Prefix: "dct_x"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	// Disable then try to replace the token: blocked.
	if _, err := applyResp(t, f, kindUserState, cmdUserState{ActorID: "admin", ID: "u1", Action: domain.UserDisable, At: time.Now().UTC()}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := applyResp(t, f, kindUpsertToken, cmdToken{ID: "t2", UserID: "u1", TokenHash: "h2", Prefix: "dct_y"}); !errors.Is(err, domain.ErrAccountBlocked) {
		t.Fatalf("token for blocked owner = %v, want ErrAccountBlocked", err)
	}
}

func TestFSMReadUserDeepCopy(t *testing.T) {
	f := newTestFSM(t)
	seedFSMUser(t, f, &cmdUser{ID: "u1", Username: "a", Role: domain.RoleUser, OAuthGroups: []string{"devs"}, OAuthGroupIDs: []string{"g1"}})
	got, _ := f.readUserByID("u1")
	got.OAuthGroups[0] = "mutated"
	got.OAuthGroupIDs[0] = "mutated"
	again, _ := f.readUserByID("u1")
	if again.OAuthGroups[0] != "devs" || again.OAuthGroupIDs[0] != "g1" {
		t.Fatalf("read helpers must return deep copies: %+v", again)
	}
}
