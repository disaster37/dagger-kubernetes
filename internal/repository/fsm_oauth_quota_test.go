package repository

import (
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TestFSMOAuthLoginEnforcesMaxMembers verifies that an OAuth login that would
// add the user as a NEW distinct member of an org at its max_members hard cap
// is rejected atomically (no membership written), while the same mutation for
// an org under its cap succeeds.
func TestFSMOAuthLoginEnforcesMaxMembers(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-7777777777777777")

	const acmeID = "org-aaaaaaaaaaaaaaaa"
	// acme with max_members.hard = 1.
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{
		ID:     acmeID,
		Name:   "acme",
		Quotas: domain.OrgQuotas{MaxMembers: domain.QuotaThreshold{Hard: 1}},
	}, Create: true}); err != nil {
		t.Fatalf("create acme: %v", err)
	}
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g1", OrgID: acmeID, Name: "team"}, Create: true}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	// Existing member u2 fills the single allowed slot.
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u2", Username: "bob", Create: true}); err != nil {
		t.Fatalf("create u2: %v", err)
	}
	if err := applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: "g1", UserIDs: []string{"u2"}}); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// u1 is a brand-new OAuth user: joining acme's group would exceed the cap.
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "alice", OAuthProvider: "oidc", OAuthID: "sub1", Create: true}); err != nil {
		t.Fatalf("create u1: %v", err)
	}
	err := applyCmd(t, f, kindOAuthUserMutation, &cmdOAuthUserMutation{
		Kind:             domain.OAuthLoginCommit,
		User:             &cmdUser{ID: "u1", OAuthProvider: "oidc", OAuthID: "sub1", Role: domain.RoleUser},
		ExpectedRevision: 1,
		ManagedGroupIDs:  []string{"g1"},
	})
	if !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("login commit = %v, want ErrOrgQuotaExceeded", err)
	}
	if _, ok := f.state.orgMembers[acmeID]["u1"]; ok {
		t.Fatal("u1 became an acme member despite max_members rejection")
	}
	if _, ok := f.state.memberships["g1"]["u1"]; ok {
		t.Fatal("u1 joined group g1 despite max_members rejection")
	}

	// Raise the cap: the same login now succeeds and adds the membership.
	acme := f.state.orgs[acmeID]
	acme.Quotas.MaxMembers.Hard = 2
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: *acme, Create: false}); err != nil {
		t.Fatalf("raise cap: %v", err)
	}
	// The rejected commit did not advance the revision; retry with the same one.
	if _, err := applyResp(t, f, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind:             domain.OAuthLoginCommit,
		User:             &cmdUser{ID: "u1", OAuthProvider: "oidc", OAuthID: "sub1", Role: domain.RoleUser},
		ExpectedRevision: 1,
		ManagedGroupIDs:  []string{"g1"},
	}); err != nil {
		t.Fatalf("login commit after cap raise: %v", err)
	}
	if role := f.state.orgMembers[acmeID]["u1"]; role != domain.OrgMember {
		t.Fatalf("u1 org role = %q, want member", role)
	}
}
