package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const memberSinceOrgID = "org-7777777777777777"

// memberSinceFSM builds a bootstrapped FSM with one user and one org group.
func memberSinceFSM(t *testing.T) (*FSM, *domain.Group) {
	t.Helper()
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, memberSinceOrgID)
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "alice", Create: true}); err != nil {
		t.Fatalf("user u1: %v", err)
	}
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u2", Username: "bob", Create: true}); err != nil {
		t.Fatalf("user u2: %v", err)
	}
	g := &domain.Group{ID: "g1", OrgID: memberSinceOrgID, Name: "devs"}
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: *g, Create: true}); err != nil {
		t.Fatalf("group: %v", err)
	}
	return f, g
}

func TestFSMOrgMemberSincePreservedAcrossRoleChange(t *testing.T) {
	f, _ := memberSinceFSM(t)
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	if err := applyCmd(t, f, kindSetOrgMember, cmdOrgMember{OrgID: memberSinceOrgID, UserID: "u1", Role: domain.OrgMember, At: first}); err != nil {
		t.Fatalf("set member: %v", err)
	}
	if got := f.state.orgMemberSince[memberSinceOrgID]["u1"]; !got.Equal(first) {
		t.Fatalf("created_at = %v, want %v", got, first)
	}
	if got := f.orgMemberships(memberSinceOrgID); len(got) != 1 || !got[0].CreatedAt.Equal(first) {
		t.Fatalf("Members = %+v, want created_at %v", got, first)
	}
	if all := f.allOrgMemberships()[memberSinceOrgID]; len(all) != 1 || !all[0].CreatedAt.Equal(first) {
		t.Fatalf("AllMemberships = %+v, want created_at %v", all, first)
	}

	// A role change carries a later At but must preserve the original time.
	roleChange := first.Add(48 * time.Hour)
	if err := applyCmd(t, f, kindSetOrgMember, cmdOrgMember{OrgID: memberSinceOrgID, UserID: "u1", Role: domain.OrgAdmin, At: roleChange}); err != nil {
		t.Fatalf("change role: %v", err)
	}
	if got := f.state.orgMembers[memberSinceOrgID]["u1"]; got != domain.OrgAdmin {
		t.Fatalf("role = %q, want org_admin", got)
	}
	if got := f.state.orgMemberSince[memberSinceOrgID]["u1"]; !got.Equal(first) {
		t.Fatalf("role change rewrote created_at: %v, want %v", got, first)
	}

	// Changing back preserves it again.
	if err := applyCmd(t, f, kindSetOrgMember, cmdOrgMember{OrgID: memberSinceOrgID, UserID: "u1", Role: domain.OrgMember, At: roleChange.Add(time.Hour)}); err != nil {
		t.Fatalf("change role back: %v", err)
	}
	if got := f.state.orgMemberSince[memberSinceOrgID]["u1"]; !got.Equal(first) {
		t.Fatalf("second role change rewrote created_at: %v, want %v", got, first)
	}
}

func TestFSMGroupMembershipSinceUsesCommandTimestamp(t *testing.T) {
	f, g := memberSinceFSM(t)
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

	if err := applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: g.ID, UserIDs: []string{"u2"}, At: at}); err != nil {
		t.Fatalf("set group members: %v", err)
	}
	if got := f.state.orgMembers[memberSinceOrgID]["u2"]; got != domain.OrgMember {
		t.Fatalf("implied org role = %q, want member", got)
	}
	if got := f.state.orgMemberSince[memberSinceOrgID]["u2"]; !got.Equal(at) {
		t.Fatalf("implied created_at = %v, want the command timestamp %v", got, at)
	}

	// Re-applying the same set with a later command timestamp keeps the
	// original membership time.
	later := at.Add(time.Hour)
	if err := applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: g.ID, UserIDs: []string{"u2"}, At: later}); err != nil {
		t.Fatalf("re-apply group members: %v", err)
	}
	if got := f.state.orgMemberSince[memberSinceOrgID]["u2"]; !got.Equal(at) {
		t.Fatalf("re-apply rewrote created_at: %v, want %v", got, at)
	}
}

func TestFSMOrgMemberSinceSnapshotRoundTrip(t *testing.T) {
	f, _ := memberSinceFSM(t)
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := applyCmd(t, f, kindSetOrgMember, cmdOrgMember{OrgID: memberSinceOrgID, UserID: "u1", Role: domain.OrgMember, At: at}); err != nil {
		t.Fatalf("set member: %v", err)
	}

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	sink := &memSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := restored.state.orgMemberSince[memberSinceOrgID]["u1"]; !got.Equal(at) {
		t.Fatalf("created_at after restore = %v, want %v", got, at)
	}
	if ms := restored.orgMemberships(memberSinceOrgID); len(ms) != 1 || !ms[0].CreatedAt.Equal(at) {
		t.Fatalf("Members after restore = %+v, want created_at %v", ms, at)
	}
}

// TestFSMRestoreLegacySnapshotBackfillsMemberSince simulates a Helm revision 128
// snapshot: org_members exists, org_member_since does not. The restore must
// backfill the bootstrap org CreatedAt instead of exposing year 1.
func TestFSMRestoreLegacySnapshotBackfillsMemberSince(t *testing.T) {
	cases := []struct {
		name  string
		since string
	}{
		{"field absent", ""},
		{"field empty", `"org_member_since": {},`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{
			  "users": [{"id":"u1","username":"alice","role":"user","create":false}],
			  "groups": [{"id":"g1","org_id":"` + memberSinceOrgID + `","name":"devs"}],
			  "memberships": [],
			  "tokens": [],
			  "projects": [],
			  "orgs": [{"id":"` + memberSinceOrgID + `","name":"default","display_name":"Default",
			            "created_at":"2025-06-01T10:00:00Z","updated_at":"2025-06-01T10:00:00Z"}],
			  "org_members": [{"org_id":"` + memberSinceOrgID + `","user_id":"u1","role":"org_admin"}],
			  ` + tc.since + `
			  "traces": [],
			  "meta": {"bootstrap_org_id":"` + memberSinceOrgID + `"}
			}`
			f := NewFSM()
			if err := f.Restore(io.NopCloser(strings.NewReader(payload))); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			want := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
			if got := f.state.orgMemberSince[memberSinceOrgID]["u1"]; !got.Equal(want) {
				t.Fatalf("backfilled created_at = %v, want %v", got, want)
			}
			if ms := f.orgMemberships(memberSinceOrgID); len(ms) != 1 || !ms[0].CreatedAt.Equal(want) {
				t.Fatalf("Members after restore = %+v, want created_at %v", ms, want)
			}
		})
	}
}

func TestFSMOrgMemberSinceRemovedWithMembership(t *testing.T) {
	cases := []struct {
		name  string
		orgID string
		apply func(t *testing.T, f *FSM)
	}{
		{
			name:  "remove member",
			orgID: memberSinceOrgID,
			apply: func(t *testing.T, f *FSM) {
				if err := applyCmd(t, f, kindRemoveOrgMember, cmdOrgMember{OrgID: memberSinceOrgID, UserID: "u1"}); err != nil {
					t.Fatalf("remove member: %v", err)
				}
			},
		},
		{
			name:  "hard delete user",
			orgID: memberSinceOrgID,
			apply: func(t *testing.T, f *FSM) {
				if err := applyCmd(t, f, kindDeleteUser, "u1"); err != nil {
					t.Fatalf("delete user: %v", err)
				}
			},
		},
		{
			name:  "soft delete user",
			orgID: memberSinceOrgID,
			apply: func(t *testing.T, f *FSM) {
				if err := applyCmd(t, f, kindSoftDeleteUser, cmdSoftDeleteUser{ID: "u1", At: time.Now().UTC()}); err != nil {
					t.Fatalf("soft delete user: %v", err)
				}
			},
		},
		{
			name:  "cascade delete org",
			orgID: "org-8888888888888888",
			apply: func(t *testing.T, f *FSM) {
				if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-8888888888888888", Name: "acme"}, Create: true}); err != nil {
					t.Fatalf("create org: %v", err)
				}
				if err := applyCmd(t, f, kindSetOrgMember, cmdOrgMember{OrgID: "org-8888888888888888", UserID: "u1", Role: domain.OrgMember, At: time.Now().UTC()}); err != nil {
					t.Fatalf("membership in acme: %v", err)
				}
				if err := applyCmd(t, f, kindDeleteOrgCascade, "org-8888888888888888"); err != nil {
					t.Fatalf("delete org cascade: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := memberSinceFSM(t)
			if err := applyCmd(t, f, kindSetOrgMember, cmdOrgMember{OrgID: memberSinceOrgID, UserID: "u1", Role: domain.OrgMember, At: time.Now().UTC()}); err != nil {
				t.Fatalf("set member: %v", err)
			}
			tc.apply(t, f)
			if n := len(f.state.orgMemberSince[tc.orgID]); n != 0 {
				t.Fatalf("orgMemberSince[%s] has %d entries after delete: %+v", tc.orgID, n, f.state.orgMemberSince)
			}
		})
	}
}

func TestOrgRepoMemberCreatedAt(t *testing.T) {
	store := newTestRaftStore(t)
	ctx := context.Background()
	repo := NewOrgRepo(store)
	user := seedUser(t, store, "member-since")
	if err := repo.Create(ctx, &domain.Org{ID: "org-9999999999999999", Name: "acme"}); err != nil {
		t.Fatalf("create org: %v", err)
	}

	if err := repo.SetMember(ctx, "org-9999999999999999", user.ID, domain.OrgMember); err != nil {
		t.Fatalf("set member: %v", err)
	}
	members, err := repo.Members(ctx, "org-9999999999999999")
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	first := members[0].CreatedAt
	if first.IsZero() {
		t.Fatal("created_at is zero for a new membership")
	}

	if err := repo.SetMember(ctx, "org-9999999999999999", user.ID, domain.OrgAdmin); err != nil {
		t.Fatalf("change role: %v", err)
	}
	members, err = repo.Members(ctx, "org-9999999999999999")
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(members) != 1 || members[0].Role != domain.OrgAdmin {
		t.Fatalf("members after role change = %+v", members)
	}
	if !members[0].CreatedAt.Equal(first) {
		t.Fatalf("role change rewrote created_at: %v, want %v", members[0].CreatedAt, first)
	}

	all, err := repo.AllMemberships(ctx)
	if err != nil {
		t.Fatalf("all memberships: %v", err)
	}
	if ms := all["org-9999999999999999"]; len(ms) != 1 || !ms[0].CreatedAt.Equal(first) {
		t.Fatalf("AllMemberships = %+v, want created_at %v", ms, first)
	}
}

// TestFSMSnapshotOmitsEmptyMemberSince keeps the optional field out of legacy
// state: an FSM with no memberships must not emit an empty org_member_since.
func TestFSMSnapshotOmitsEmptyMemberSince(t *testing.T) {
	payload, err := json.Marshal(fsmSnapshotPayload{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(payload, []byte("org_member_since")) {
		t.Fatalf("empty payload serialized org_member_since: %s", payload)
	}
}
