package repository

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestOrgRepoLifecycle(t *testing.T) {
	store := newTestRaftStore(t)
	ctx := context.Background()
	repo := NewOrgRepo(store)

	boot, err := repo.Bootstrap(ctx, &domain.Org{
		ID:          "org-0000000000000001",
		Name:        domain.DefaultOrgName,
		DisplayName: domain.DefaultOrgDisplayName,
	}, "grp-0000000000000001", domain.DefaultOrgDisplayName)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if boot.CreatedAt.IsZero() {
		t.Fatal("bootstrap org CreatedAt was not assigned")
	}
	// Idempotent: the second proposal returns the committed org.
	again, err := repo.Bootstrap(ctx, &domain.Org{ID: "org-0000000000000002", Name: domain.DefaultOrgName}, "", "")
	if err != nil || again.ID != boot.ID {
		t.Fatalf("second bootstrap = %+v err=%v", again, err)
	}
	if _, err := repo.Bootstrap(ctx, nil, "", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil bootstrap = %v, want ErrValidation", err)
	}

	if id, err := repo.BootstrapOrgID(ctx); err != nil || id != boot.ID {
		t.Fatalf("BootstrapOrgID = %q err=%v", id, err)
	}
	if got, err := repo.GetBootstrap(ctx); err != nil || got.ID != boot.ID {
		t.Fatalf("GetBootstrap = %+v err=%v", got, err)
	}
	if got, err := repo.Get(ctx, boot.ID); err != nil || got.Name != domain.DefaultOrgName {
		t.Fatalf("Get = %+v err=%v", got, err)
	}
	if _, err := repo.Get(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if got, err := repo.GetByName(ctx, "DEFAULT"); err != nil || got.ID != boot.ID {
		t.Fatalf("GetByName = %+v err=%v", got, err)
	}

	acme := &domain.Org{ID: "org-1111111111111111", Name: "acme"}
	if err := repo.Create(ctx, acme); err != nil {
		t.Fatalf("create acme: %v", err)
	}
	if acme.UpdatedAt.IsZero() || len(acme.Features) == 0 {
		t.Fatalf("create defaults = %+v", acme)
	}
	acme.DisplayName = "Acme Corp"
	if err := repo.Update(ctx, acme); err != nil {
		t.Fatalf("update acme: %v", err)
	}
	acme.Name = "renamed"
	if err := repo.Update(ctx, acme); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("rename = %v, want ErrValidation", err)
	}

	user := seedUser(t, store, "acme-member")
	if err := repo.SetMember(ctx, acme.ID, user.ID, domain.OrgAdmin); err != nil {
		t.Fatalf("set member: %v", err)
	}
	if role, ok, err := repo.GetMemberRole(ctx, acme.ID, user.ID); err != nil || !ok || role != domain.OrgAdmin {
		t.Fatalf("GetMemberRole = %q %v %v", role, ok, err)
	}
	if n, err := repo.DistinctMemberCount(ctx, acme.ID); err != nil || n != 1 {
		t.Fatalf("DistinctMemberCount = %d err=%v", n, err)
	}
	if orgs, err := repo.OrgsForUser(ctx, user.ID); err != nil || len(orgs) != 1 {
		t.Fatalf("OrgsForUser = %+v err=%v", orgs, err)
	}
	all, err := repo.AllMemberships(ctx)
	if err != nil || len(all[acme.ID]) != 1 {
		t.Fatalf("AllMemberships = %+v err=%v", all, err)
	}
	if err := repo.RemoveMember(ctx, acme.ID, user.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if ms, err := repo.Members(ctx, acme.ID); err != nil || len(ms) != 0 {
		t.Fatalf("Members after remove = %+v err=%v", ms, err)
	}

	if err := repo.Delete(ctx, acme.ID); err != nil {
		t.Fatalf("delete acme: %v", err)
	}
	if _, err := repo.Get(ctx, acme.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("acme still present: %v", err)
	}
	if err := repo.DeleteCascade(ctx, boot.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cascade bootstrap = %v, want ErrForbidden", err)
	}

	if copyOrg(nil) != nil {
		t.Fatal("copyOrg(nil) must be nil")
	}
}

func TestFSMOrgQuotaDefaults(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-0000000000000001")
	f.SetOrgQuotaDefaults(domain.OrgQuotas{
		MaxGroups:   domain.QuotaThreshold{Hard: 1},
		MaxProjects: domain.QuotaThreshold{Hard: 1},
		MaxMembers:  domain.QuotaThreshold{Hard: 1},
	})
	if f.state.quotaDefaults.MaxGroups.Hard != 1 {
		t.Fatalf("quota defaults = %+v", f.state.quotaDefaults)
	}

	orgID := "org-1111111111111111"
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: orgID, Name: "acme"}, Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g1", OrgID: orgID, Name: "g1"}, Create: true}); err != nil {
		t.Fatalf("first group: %v", err)
	}
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g2", OrgID: orgID, Name: "g2"}, Create: true}); !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("second group = %v, want ErrOrgQuotaExceeded", err)
	}

	if err := applyCmd(t, f, kindUpsertProject, &cmdProject{Project: domain.Project{ID: "p1", OrgID: orgID, Name: "p1"}, Create: true}); err != nil {
		t.Fatalf("first project: %v", err)
	}
	if err := applyCmd(t, f, kindUpsertProject, &cmdProject{Project: domain.Project{ID: "p2", OrgID: orgID, Name: "p2"}, Create: true}); !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("second project = %v, want ErrOrgQuotaExceeded", err)
	}

	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "alice", Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindSetOrgMember, cmdOrgMember{OrgID: orgID, UserID: "u1", Role: domain.OrgMember, At: time.Now().UTC()}); err != nil {
		t.Fatalf("first member: %v", err)
	}
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u2", Username: "bob", Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindSetOrgMember, cmdOrgMember{OrgID: orgID, UserID: "u2", Role: domain.OrgMember, At: time.Now().UTC()}); !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("second member = %v, want ErrOrgQuotaExceeded", err)
	}

	// Unknown org / unknown resource are no-ops.
	if err := f.state.checkOrgQuotaLocked("org-missing0000000000", domain.QuotaGroups, 99); err != nil {
		t.Fatalf("unknown org quota = %v", err)
	}
	if err := f.state.checkOrgQuotaLocked(orgID, domain.QuotaEngines, 99); err != nil {
		t.Fatalf("unchecked resource = %v", err)
	}
	if got := f.state.orgUsageLocked(orgID, domain.QuotaEngines); got != 0 {
		t.Fatalf("engine usage = %d, want 0", got)
	}
	if got := f.state.orgUsageLocked(orgID, domain.QuotaGroups); got != 1 {
		t.Fatalf("group usage = %d, want 1", got)
	}
	if got := f.state.orgUsageLocked(orgID, domain.QuotaProjects); got != 1 {
		t.Fatalf("project usage = %d, want 1", got)
	}

	if role, ok := f.memberRole(orgID, "u1"); !ok || role != domain.OrgMember {
		t.Fatalf("memberRole = %q %v", role, ok)
	}
	if n := f.distinctMemberCount(orgID); n < 1 {
		t.Fatalf("distinctMemberCount = %d", n)
	}
}

func TestFSMApplyBootstrapOrgValidation(t *testing.T) {
	cases := []struct {
		name    string
		payload *cmdBootstrapOrg
		want    error
	}{
		{"nil payload", nil, domain.ErrValidation},
		{"empty id", &cmdBootstrapOrg{Org: domain.Org{Name: domain.DefaultOrgName}}, domain.ErrValidation},
		{"wrong slug", &cmdBootstrapOrg{Org: domain.Org{ID: "org-1111111111111111", Name: "acme"}}, domain.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFSM(t)
			if _, err := f.state.applyBootstrapOrg(tc.payload); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("existing meta with missing org", func(t *testing.T) {
		f := newTestFSM(t)
		f.state.meta[domain.BootstrapOrgMetaKey] = "org-2222222222222222"
		if _, err := f.state.applyBootstrapOrg(&cmdBootstrapOrg{Org: domain.Org{ID: "org-3333333333333333", Name: domain.DefaultOrgName}}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("display and features defaulted and hook fired", func(t *testing.T) {
		f := newTestFSM(t)
		var hookKey, hookValue string
		f.SetMetaHook(func(key, value string) { hookKey, hookValue = key, value })
		org, err := f.state.applyBootstrapOrg(&cmdBootstrapOrg{
			Org:            domain.Org{ID: "org-4444444444444444", Name: domain.DefaultOrgName, UpdatedAt: time.Unix(2, 0).UTC()},
			DefaultGroupID: "grp-4444",
		})
		if err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if org.DisplayName != domain.DefaultOrgDisplayName || len(org.Features) == 0 {
			t.Fatalf("org defaults = %+v", org)
		}
		if hookKey != domain.BootstrapOrgMetaKey || hookValue != org.ID {
			t.Fatalf("hook = %q %q", hookKey, hookValue)
		}
		if _, ok := f.state.groupsByOrgName[orgEntityNameKey(org.ID, domain.DefaultOrgName)]; !ok {
			t.Fatal("default group not created")
		}
	})
}

// TestFSMRestoreLegacyRowsNormalization restores a pre-bootstrap-style snapshot
// whose rows carry empty OrgIDs and verifies normalizeLegacyRowsLocked rewrites
// every entity type, backfills admin membership and member timestamps.
func TestFSMRestoreLegacyRowsNormalization(t *testing.T) {
	const orgID = "org-5555555555555555"
	payload := `{
	  "users": [
	    {"id":"admin","username":"root","role":"admin","create":false},
	    {"id":"member","username":"member","role":"user","create":false}
	  ],
	  "groups": [{"id":"g1","name":"legacy"}],
	  "memberships": [{"group_id":"g1","user_id":"member"}],
	  "tokens": [],
	  "projects": [{"id":"p1","name":"repo"}],
	  "orgs": [{"id":"` + orgID + `","name":"default","display_name":"Default",
	            "created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z"}],
	  "org_members": [],
	  "secrets": [{"id":"s1","scope":"org","scope_id":"","name":"TOKEN","ciphertext":"x"}],
	  "forge_sources": [{"id":"f1","org_id":"","provider":"github","base_url":"https://x","repo":"a/b"}],
	  "environments": [],
	  "check_runs": [{"id":"c1","org_id":"","project_id":"p1"}],
	  "workflow_defs": [{"id":"w1","project_id":"p1","workflow":{"name":"build"}}],
	  "workflow_runs": [{"run":{"id":"r1","org_id":"","project_id":"p1","workflow":"build"}}],
	  "workflow_counters": {"/p1/build": 4},
	  "traces": [],
	  "meta": {"bootstrap_org_id":"` + orgID + `"}
	}`
	f := NewFSM()
	if err := f.Restore(io.NopCloser(strings.NewReader(payload))); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if g := f.state.groups["g1"]; g.OrgID != orgID {
		t.Fatalf("group org = %q", g.OrgID)
	}
	if p := f.state.projects["p1"]; p.OrgID != orgID {
		t.Fatalf("project org = %q", p.OrgID)
	}
	if n, ok := f.state.workflowCounters[orgID+"/p1/build"]; !ok || n != 4 {
		t.Fatalf("workflow counters = %+v", f.state.workflowCounters)
	}
	if src := f.state.forgeSources["f1"]; src.OrgID != orgID {
		t.Fatalf("forge org = %q", src.OrgID)
	}
	rec, ok := f.state.checkRuns["c1"]
	if !ok || rec == nil {
		t.Fatalf("check run missing: %+v", f.state.checkRuns)
	}
	if rec.OrgID != orgID {
		t.Fatalf("check run org = %q", rec.OrgID)
	}
	if def := f.state.workflowDefs[workflowDefKey(orgID, "p1", "build")]; def == nil {
		t.Fatalf("workflow def not reindexed: %+v", f.state.workflowDefs)
	}
	if run := f.state.workflowRuns["r1"]; run == nil || run.OrgID != orgID {
		t.Fatalf("workflow run = %+v", run)
	}
	if _, ok := f.state.secrets[secretKey(domain.SecretScopeOrg, orgID, "TOKEN")]; !ok {
		t.Fatalf("org secret not rescoped: %+v", f.state.secrets)
	}
	if role := f.state.orgMembers[orgID]["member"]; role != domain.OrgMember {
		t.Fatalf("group-derived org role = %q", role)
	}
	if role := f.state.orgMembers[orgID]["admin"]; role != domain.OrgAdmin {
		t.Fatalf("admin org role = %q", role)
	}
	// Legacy memberships are backfilled with the bootstrap org CreatedAt.
	want := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, uid := range []string{"admin", "member"} {
		if got := f.state.orgMemberSince[orgID][uid]; !got.Equal(want) {
			t.Fatalf("%s created_at = %v, want %v", uid, got, want)
		}
	}
}

func TestFSMDeleteOrgNonCascade(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-0000000000000001")
	orgID := "org-6666666666666666"
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: orgID, Name: "acme"}, Create: true}); err != nil {
		t.Fatal(err)
	}
	grp := &domain.Group{ID: "grp-acme", OrgID: orgID, Name: "default"}
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: *grp, Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "alice", Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindSetOrgMember, cmdOrgMember{OrgID: orgID, UserID: "u1", Role: domain.OrgMember, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindDeleteOrg, orgID); err != nil {
		t.Fatalf("delete org: %v", err)
	}
	if _, ok := f.state.orgs[orgID]; ok {
		t.Fatal("org still present")
	}
	if _, ok := f.state.orgMembers[orgID]; ok {
		t.Fatalf("org memberships leaked: %+v", f.state.orgMembers)
	}
	if _, ok := f.state.orgMemberSince[orgID]; ok {
		t.Fatalf("membership timestamps leaked: %+v", f.state.orgMemberSince)
	}
	if _, ok := f.state.groups[grp.ID]; ok {
		t.Fatal("default group of the deleted org still present")
	}
	if _, ok := f.state.orgMembersByUser["u1"]; ok {
		t.Fatalf("reverse membership index leaked: %+v", f.state.orgMembersByUser)
	}
}

func TestFSMUpsertOrgValidation(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-0000000000000001")

	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{Name: "x"}, Create: true}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty id = %v", err)
	}
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-1111111111111111"}, Create: true}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty name = %v", err)
	}
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-1111111111111111", Name: "acme"}, Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-1111111111111111", Name: "acme"}, Create: true}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate id = %v, want ErrConflict", err)
	}
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-2222222222222222", Name: "ACME"}, Create: true}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate name = %v, want ErrConflict", err)
	}
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-3333333333333333", Name: "beta"}, Create: false}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update missing = %v, want ErrNotFound", err)
	}
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-2222222222222222", Name: "beta"}, Create: true}); err != nil {
		t.Fatal(err)
	}
	// A name-index collision with another org's slug (staged directly: the
	// create path already prevents it).
	f.state.orgs["org-2222222222222222"].Name = "acme"
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-2222222222222222", Name: "acme"}, Create: false}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("name collision = %v, want ErrConflict", err)
	}
}

func TestS3OrgKeyBuilders(t *testing.T) {
	cases := []struct {
		name  string
		store *S3RemoteCacheStore
		want  map[string]string
	}{
		{
			name:  "with prefix",
			store: &S3RemoteCacheStore{prefix: "remote-cache"},
			want: map[string]string{
				"org":        "orgs/org-1/remote-cache",
				"bundle":     "orgs/org-1/remote-cache/bundles/abc.json",
				"blob":       "orgs/org-1/remote-cache/blobs/sha256:abc",
				"pack":       "orgs/org-1/remote-cache/packs/p1.pack",
				"checkpoint": "orgs/org-1/remote-cache/index/checkpoint.json",
			},
		},
		{
			name:  "without prefix",
			store: &S3RemoteCacheStore{},
			want: map[string]string{
				"org":        "orgs/org-1",
				"bundle":     "orgs/org-1/bundles/abc.json",
				"blob":       "orgs/org-1/blobs/sha256:abc",
				"pack":       "orgs/org-1/packs/p1.pack",
				"checkpoint": "orgs/org-1/index/checkpoint.json",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{
				"org":        tc.store.orgPrefix("org-1"),
				"bundle":     tc.store.bundleKey("org-1", "abc"),
				"blob":       tc.store.blobKey("org-1", "sha256:abc"),
				"pack":       tc.store.packKey("org-1", "p1"),
				"checkpoint": tc.store.checkpointKey("org-1"),
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("%s = %q, want %q", key, got[key], want)
				}
			}
			if tc.store.PackKey("org-1", "p1") != tc.want["pack"] {
				t.Fatalf("PackKey = %q", tc.store.PackKey("org-1", "p1"))
			}
		})
	}

	if got := (&S3ArtifactStore{prefix: "art"}).key("org-1", "proj", "run", "out.txt"); got != "art/orgs/org-1/artifacts/proj/run/out.txt" {
		t.Fatalf("artifact key = %q", got)
	}
	if got := (&S3ArtifactStore{}).key("org-1", "proj", "run", "out.txt"); got != "orgs/org-1/artifacts/proj/run/out.txt" {
		t.Fatalf("artifact key without prefix = %q", got)
	}
	if got := (&S3OTLPArchive{prefix: "arch"}).key("org-1", "trace-1", "traces", 7); got != "arch/orgs/org-1/otlp-archive/trace-1/traces/00000000000000000007.bin" {
		t.Fatalf("otlp key = %q", got)
	}
	if got := joinS3Key("", "/a/", "b", ""); got != "a/b" {
		t.Fatalf("joinS3Key = %q", got)
	}

	resolver := NewSharedBucketResolver("bucket")
	target, err := resolver.ResolveS3(context.Background(), "org-1")
	if err != nil || target.Bucket != "bucket" {
		t.Fatalf("ResolveS3 = %+v err=%v", target, err)
	}
	if _, err := NewSharedBucketResolver("").ResolveS3(context.Background(), "org-1"); err == nil {
		t.Fatal("empty bucket must fail")
	}
}

func TestFSMOrgSealedStateSnapshot(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-0000000000000001")
	orgID := "org-7777777777777777"
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: orgID, Name: "acme"}, Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindUpsertSecret, &cmdSecret{
		ID: "s1", Scope: domain.SecretScopeOrg, ScopeID: orgID, Name: "TOKEN", Ciphertext: "sealed",
	}); err != nil {
		t.Fatalf("upsert secret: %v", err)
	}
	if err := applyCmd(t, f, kindUpsertForgeSource, &cmdForgeSource{
		ID: "f1", OrgID: orgID, Provider: domain.ForgeGitHub, BaseURL: "https://forge", Repo: "a/b", TokenCiphertext: "sealed-token",
	}); err != nil {
		t.Fatalf("upsert forge: %v", err)
	}
	if err := applyCmd(t, f, kindUpsertEnvironment, &cmdEnvironment{Environment: domain.Environment{OrgID: orgID, Name: "prod"}}); err != nil {
		t.Fatalf("upsert environment: %v", err)
	}

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	sink := &memSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	snap.Release()

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(strings.NewReader(sink.String()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	sec := restored.state.secrets[secretKey(domain.SecretScopeOrg, orgID, "TOKEN")]
	if sec == nil || sec.Ciphertext != "sealed" {
		t.Fatalf("secret lost: %+v", sec)
	}
	forge := restored.state.forgeSources["f1"]
	if forge == nil || forge.TokenCiphertext != "sealed-token" {
		t.Fatalf("forge source lost: %+v", forge)
	}
	if env := restored.state.environments[environmentKey(orgID, "prod")]; env == nil {
		t.Fatal("environment lost")
	}
}

func TestFSMDeleteOrgCascadeRemovesEveryChild(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-0000000000000001")
	orgID := "org-8888888888888888"
	steps := []struct {
		kind    commandKind
		payload any
	}{
		{kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: orgID, Name: "acme"}, Create: true}},
		{kindUpsertUser, &cmdUser{ID: "u1", Username: "alice", Create: true}},
		{kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g1", OrgID: orgID, Name: "team"}, Create: true}},
		{kindSetMembers, cmdSetMembers{GroupID: "g1", UserIDs: []string{"u1"}, At: time.Now().UTC()}},
		{kindUpsertProject, &cmdProject{Project: domain.Project{ID: "p1", OrgID: orgID, Name: "repo"}, Create: true}},
		{kindUpsertSecret, &cmdSecret{ID: "s1", Scope: domain.SecretScopeOrg, ScopeID: orgID, Name: "TOKEN", Ciphertext: "x"}},
		{kindUpsertForgeSource, &cmdForgeSource{ID: "f1", OrgID: orgID, Provider: domain.ForgeGitHub}},
		{kindUpsertEnvironment, &cmdEnvironment{Environment: domain.Environment{OrgID: orgID, Name: "prod"}}},
		{kindUpsertCheckRun, &cmdCheckRun{Record: domain.CheckRunRecord{ID: "c1", OrgID: orgID}}},
		{kindSyncProjectWorkflowDefs, &cmdSyncProjectWorkflowDefs{OrgID: orgID, ProjectID: "p1", Defs: []*domain.WorkflowDef{{OrgID: orgID, ProjectID: "p1", Workflow: domain.Workflow{Name: "build"}}}}},
		{kindCreateWorkflowRun, &cmdWorkflowRun{Run: domain.WorkflowRun{OrgID: orgID, ProjectID: "p1", Workflow: "build"}}},
		{kindUpsertTraceProvision, &cmdUpsertTraceProvision{TraceID: "t1", OrgID: orgID, UserID: "u1", UpdatedAt: time.Now().UTC()}},
		{kindUpsertToken, &cmdToken{ID: "tok", UserID: "org:" + orgID, TokenHash: "h"}},
	}
	for _, step := range steps {
		if err := applyCmd(t, f, step.kind, step.payload); err != nil {
			t.Fatalf("seed kind %d: %v", step.kind, err)
		}
	}
	if err := applyCmd(t, f, kindDeleteOrgCascade, orgID); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}

	checks := []struct {
		name string
		cond bool
	}{
		{"org", len(f.state.orgs) == 1},
		{"groups", len(f.state.groups) == 1}, // bootstrap default group only
		{"projects", len(f.state.projects) == 0},
		{"secrets", len(f.state.secrets) == 0},
		{"forge sources", len(f.state.forgeSources) == 0},
		{"environments", len(f.state.environments) == 0},
		{"check runs", len(f.state.checkRuns) == 0},
		{"workflow defs", len(f.state.workflowDefs) == 0},
		{"workflow runs", len(f.state.workflowRuns) == 0},
		{"workflow counters", len(f.state.workflowCounters) == 0},
		{"traces", len(f.state.traces) == 0},
		{"org members", len(f.state.orgMembers[orgID]) == 0},
		{"org member since", len(f.state.orgMemberSince[orgID]) == 0},
		{"org token", len(f.state.tokens) == 0},
	}
	for _, c := range checks {
		if !c.cond {
			t.Fatalf("cascade left %s behind: orgs=%d groups=%d projects=%d secrets=%d forges=%d envs=%d checks=%d defs=%d runs=%d counters=%d traces=%d tokens=%d",
				c.name, len(f.state.orgs), len(f.state.groups), len(f.state.projects), len(f.state.secrets), len(f.state.forgeSources),
				len(f.state.environments), len(f.state.checkRuns), len(f.state.workflowDefs), len(f.state.workflowRuns),
				len(f.state.workflowCounters), len(f.state.traces), len(f.state.tokens))
		}
	}
}

func TestFSMSetOrgMemberValidation(t *testing.T) {
	f := newTestFSM(t)
	bootstrapOrgForTest(t, f, "org-0000000000000001")
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "alice", Create: true}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cmd  cmdOrgMember
		want error
	}{
		{"unknown org", cmdOrgMember{OrgID: "org-9999999999999999", UserID: "u1", Role: domain.OrgMember}, domain.ErrNotFound},
		{"unknown user", cmdOrgMember{OrgID: "org-0000000000000001", UserID: "ghost", Role: domain.OrgMember}, domain.ErrNotFound},
		{"invalid role", cmdOrgMember{OrgID: "org-0000000000000001", UserID: "u1", Role: domain.OrgRole("root")}, domain.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyCmd(t, f, kindSetOrgMember, tc.cmd); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestS3Constructors(t *testing.T) {
	store, err := NewS3RemoteCacheStore("127.0.0.1:9000", "us-east-1", "key", "secret", false, "", "bucket", "/prefix/", 0)
	if err != nil || store.prefix != "prefix" || store.presignTTL != time.Hour {
		t.Fatalf("store = %+v err=%v", store, err)
	}
	if _, err := NewS3RemoteCacheStore("127.0.0.1:9000", "us-east-1", "key", "secret", false, "other:9000", "bucket", "", time.Minute); err != nil {
		t.Fatalf("engine endpoint store: %v", err)
	}
	if _, err := NewS3RemoteCacheStore("://bad", "us-east-1", "key", "secret", false, "", "bucket", "", time.Minute); err == nil {
		t.Fatal("invalid S3 endpoint must fail")
	}
	if got := NewS3ArtifactStore(nil, "bucket", "/art"); got.prefix != "art" || got.bucket != "bucket" {
		t.Fatalf("artifact store = %+v", got)
	}
	if got := NewS3OTLPArchive(nil, "bucket", "/arch", testLogger()); got.prefix != "arch" {
		t.Fatalf("otlp archive = %+v", got)
	}
}
