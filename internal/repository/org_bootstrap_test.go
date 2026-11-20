package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func bootstrapOrgForTest(t *testing.T, f *FSM, id string) {
	t.Helper()
	err := applyCmd(t, f, kindBootstrapOrg, &cmdBootstrapOrg{
		Org: domain.Org{
			ID:          id,
			Name:        domain.DefaultOrgName,
			DisplayName: domain.DefaultOrgDisplayName,
			CreatedAt:   time.Unix(1, 0).UTC(),
		},
		DefaultGroupID:  "grp-bootstrap",
		DisplayNameMeta: domain.DefaultOrgDisplayName,
	})
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
}

func TestFSMBootstrapOrgDeterministic(t *testing.T) {
	f := newTestFSM(t)
	if _, err := f.BootstrapOrgID(); !errors.Is(err, domain.ErrBootstrapNotReady) {
		t.Fatalf("fresh FSM BootstrapOrgID = %v, want ErrBootstrapNotReady", err)
	}
	if n := len(f.state.orgs); n != 0 {
		t.Fatalf("fresh FSM orgs = %d, want 0", n)
	}

	bootstrapOrgForTest(t, f, "org-1111111111111111")
	id, err := f.BootstrapOrgID()
	if err != nil || id != "org-1111111111111111" {
		t.Fatalf("BootstrapOrgID = %q, %v", id, err)
	}
	org, err := f.bootstrapOrg()
	if err != nil || org.Name != domain.DefaultOrgName {
		t.Fatalf("bootstrap org = %+v, %v", org, err)
	}
	if _, ok := f.state.groupsByOrgName[orgEntityNameKey(id, domain.DefaultOrgName)]; !ok {
		t.Fatal("bootstrap default group missing")
	}

	// A second proposal (split election) must be a no-op preserving the first.
	bootstrapOrgForTest(t, f, "org-2222222222222222")
	id, _ = f.BootstrapOrgID()
	if id != "org-1111111111111111" {
		t.Fatalf("second bootstrap replaced UID: %q", id)
	}
	if n := len(f.state.orgs); n != 1 {
		t.Fatalf("orgs = %d, want 1", n)
	}
}

func TestFSMBootstrapNormalizesLegacyRows(t *testing.T) {
	f := newTestFSM(t)
	// Pre-upgrade (main) rows have empty OrgID and stage successfully.
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g1", Name: "legacy"}, Create: true}); err != nil {
		t.Fatalf("legacy group: %v", err)
	}
	if err := applyCmd(t, f, kindUpsertProject, &cmdProject{Project: domain.Project{ID: "p1", Name: "legacy/project"}, Create: true}); err != nil {
		t.Fatalf("legacy project: %v", err)
	}
	if err := applyCmd(t, f, kindUpsertTraceProvision, &cmdUpsertTraceProvision{TraceID: "trace-1", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("legacy trace: %v", err)
	}

	bootstrapOrgForTest(t, f, "org-3333333333333333")

	if g := f.state.groups["g1"]; g.OrgID != "org-3333333333333333" {
		t.Fatalf("legacy group org = %q", g.OrgID)
	}
	if p := f.state.projects["p1"]; p.OrgID != "org-3333333333333333" {
		t.Fatalf("legacy project org = %q", p.OrgID)
	}
	if m := f.state.traces["trace-1"]; m.OrgID != "org-3333333333333333" {
		t.Fatalf("legacy trace org = %q", m.OrgID)
	}

	// After bootstrap, empty OrgID writes are rejected.
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g2", Name: "nope"}, Create: true}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty org group = %v, want ErrValidation", err)
	}
}

func TestFSMCompositeNameUniquenessAndCrossOrgGroup(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-4444444444444444")
	for _, o := range []*domain.Org{
		{ID: "org-aaaaaaaaaaaaaaaa", Name: "acme"},
		{ID: "org-bbbbbbbbbbbbbbbb", Name: "globex"},
	} {
		if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: *o, Create: true}); err != nil {
			t.Fatalf("create org %s: %v", o.Name, err)
		}
	}

	for _, orgID := range []string{"org-aaaaaaaaaaaaaaaa", "org-bbbbbbbbbbbbbbbb"} {
		if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g-" + orgID, OrgID: orgID, Name: "platform"}, Create: true}); err != nil {
			t.Fatalf("group in %s: %v", orgID, err)
		}
	}
	// Duplicate name in the same org fails.
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "dup", OrgID: "org-aaaaaaaaaaaaaaaa", Name: "Platform"}, Create: true}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("same-org duplicate = %v, want ErrConflict", err)
	}
	// Cross-org group assignment fails.
	if err := applyCmd(t, f, kindUpsertProject, &cmdProject{Project: domain.Project{
		ID: "proj-cross", OrgID: "org-aaaaaaaaaaaaaaaa", Name: "repo", GroupID: "g-org-bbbbbbbbbbbbbbbb",
	}, Create: true}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("cross-org group = %v, want ErrValidation", err)
	}
}

func TestFSMDeleteOrgGuards(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-5555555555555555")
	if err := applyCmd(t, f, kindDeleteOrg, "org-5555555555555555"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("delete bootstrap = %v, want ErrForbidden", err)
	}
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-cccccccccccccccc", Name: "acme"}, Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindUpsertProject, &cmdProject{Project: domain.Project{ID: "p", OrgID: "org-cccccccccccccccc", Name: "repo"}, Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindDeleteOrg, "org-cccccccccccccccc"); !errors.Is(err, domain.ErrOrgNotEmpty) {
		t.Fatalf("non-empty delete = %v, want ErrOrgNotEmpty", err)
	}
	if err := applyCmd(t, f, kindDeleteOrgCascade, "org-cccccccccccccccc"); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}
	if _, ok := f.state.orgs["org-cccccccccccccccc"]; ok {
		t.Fatal("org still present after cascade")
	}
	if _, ok := f.state.projects["p"]; ok {
		t.Fatal("project still present after cascade")
	}
}

func TestFSMGroupMembershipImpliesOrgMember(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-6666666666666666")
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "alice", Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g1", OrgID: "org-6666666666666666", Name: "G1"}, Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: "g1", UserIDs: []string{"u1"}}); err != nil {
		t.Fatal(err)
	}
	if role := f.state.orgMembers["org-6666666666666666"]["u1"]; role != domain.OrgMember {
		t.Fatalf("org role = %q, want member", role)
	}
	if err := applyCmd(t, f, kindRemoveOrgMember, cmdOrgMember{OrgID: "org-6666666666666666", UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.state.memberships["g1"]["u1"]; ok {
		t.Fatal("org removal must cascade that org's group membership")
	}
}
