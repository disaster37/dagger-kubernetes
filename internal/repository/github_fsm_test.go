package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestGitHubFSMSnapshotRestore(t *testing.T) {
	f := newTestFSM(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	conn := &domain.UserGitHubConnection{
		UserID: "user-a", GitHubUserID: 7, GitHubLogin: "alice", BaseURL: "https://api.github.com",
		Scopes: []string{"repo"}, TokenCiphertext: "conn-ct", Status: domain.GitHubConnectionActive,
		ConnectedAt: now, UpdatedAt: now,
	}
	if err := applyCmd(t, f, kindUpsertGitHubConnection, cmdGitHubConnectionFrom(conn)); err != nil {
		t.Fatalf("upsert connection: %v", err)
	}
	acct := &domain.GitHubAccountSource{
		ID: "ghacct-1", OrgID: "org-a", Owner: "Acme", OwnerType: "User", BaseURL: "https://api.github.com",
		TokenCiphertext: "acct-ct", Status: domain.GitHubConnectionActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := applyCmd(t, f, kindUpsertGitHubAccountSource, cmdGitHubAccountSourceFrom(acct)); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	src := &domain.ForgeSource{ID: "ghrepo-1", OrgID: "org-a", Provider: domain.ForgeGitHub, Repo: "acme/api", AccountSourceID: "ghacct-1", TokenCiphertext: "repo-ct"}
	if err := applyCmd(t, f, kindBatchUpsertRepoSettings, cmdBatchUpsertRepoSettings{
		OrgID: "org-a",
		Writes: []cmdRepoSettingWrite{{
			Setting:     domain.RepoSetting{OrgID: "org-a", Repo: "github.com/acme/api", Provider: domain.ForgeGitHub, SourceID: "ghacct-1", ForgeSourceID: "ghrepo-1", RunChecksOnDefaultBranch: true},
			ForgeSource: cmdForgeSourceFrom(src),
		}},
	}); err != nil {
		t.Fatalf("batch settings: %v", err)
	}
	state := &domain.GitHubOAuthState{StateID: strings.Repeat("a", 64), UserID: "user-a", OrgID: "org-a", RedirectURI: "https://example.com/github/callback", PKCEVerifier: "verifier", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	if err := applyCmd(t, f, kindCreateGitHubOAuthState, cmdGitHubOAuthStateFrom(state)); err != nil {
		t.Fatalf("create state: %v", err)
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
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	gotConn, err := restored.readGitHubConnection("user-a")
	if err != nil || gotConn.TokenCiphertext != "conn-ct" || gotConn.GitHubLogin != "alice" {
		t.Fatalf("connection after restore = %+v err=%v", gotConn, err)
	}
	gotAcct, err := restored.readGitHubAccountSourceByOwner("org-a", "acme")
	if err != nil || gotAcct.ID != "ghacct-1" || gotAcct.TokenCiphertext != "acct-ct" {
		t.Fatalf("account after restore = %+v err=%v", gotAcct, err)
	}
	gotSetting, err := restored.readRepoSettingByCanonical("github.com/acme/api")
	if err != nil || gotSetting.OrgID != "org-a" || !gotSetting.RunChecksOnDefaultBranch {
		t.Fatalf("setting after restore = %+v err=%v", gotSetting, err)
	}
	gotSrc, err := restored.readForgeSource("ghrepo-1")
	if err != nil || gotSrc.AccountSourceID != "ghacct-1" || gotSrc.TokenCiphertext != "repo-ct" {
		t.Fatalf("materialized source after restore = %+v err=%v", gotSrc, err)
	}
	gotState, err := restored.readGitHubOAuthState(state.StateID)
	if err != nil || gotState.PKCEVerifier != "verifier" {
		t.Fatalf("oauth state after restore = %+v err=%v", gotState, err)
	}
	consumed, err := consumedStateAt(restored, state.StateID, now.Add(time.Minute))
	if err != nil || consumed == nil || !consumed.Consumed {
		t.Fatalf("consume after restore = %+v err=%v", consumed, err)
	}
	if _, err := restored.readGitHubOAuthState(state.StateID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("consumed state must be deleted, err=%v", err)
	}
	if _, err := consumedStateAt(restored, state.StateID, now.Add(time.Minute)); !errors.Is(err, domain.ErrOAuthStateInvalid) {
		t.Fatalf("replay must be invalid, err=%v", err)
	}
}

func TestGitHubFSMClaimConflictIsAtomic(t *testing.T) {
	f := newTestFSM(t)
	first := domain.RepoSetting{OrgID: "org-a", Repo: "github.com/acme/api", Provider: domain.ForgeGitHub, SourceID: "ghacct-a", ForgeSourceID: "ghrepo-a"}
	if err := applyCmd(t, f, kindBatchUpsertRepoSettings, cmdBatchUpsertRepoSettings{OrgID: "org-a", Writes: []cmdRepoSettingWrite{{Setting: first}}}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// A batch from another org must fail before applying its first write.
	err := applyCmd(t, f, kindBatchUpsertRepoSettings, cmdBatchUpsertRepoSettings{
		OrgID: "org-b",
		Writes: []cmdRepoSettingWrite{
			{Setting: domain.RepoSetting{OrgID: "org-b", Repo: "github.com/acme/web", Provider: domain.ForgeGitHub, SourceID: "ghacct-b", ForgeSourceID: "ghrepo-b"}},
			{Setting: domain.RepoSetting{OrgID: "org-b", Repo: "github.com/acme/api", Provider: domain.ForgeGitHub, SourceID: "ghacct-b", ForgeSourceID: "ghrepo-b2"}},
		},
	})
	if !errors.Is(err, domain.ErrRepoClaimedByOtherOrg) {
		t.Fatalf("conflict err = %v", err)
	}
	if _, err := f.readRepoSetting("org-b", "github.com/acme/web"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("batch must be all-or-nothing, err=%v", err)
	}
	// Same org may update its own claim.
	if err := applyCmd(t, f, kindBatchUpsertRepoSettings, cmdBatchUpsertRepoSettings{OrgID: "org-a", Writes: []cmdRepoSettingWrite{{Setting: first}}}); err != nil {
		t.Fatalf("same-org update: %v", err)
	}
}

func TestGitHubFSMDeleteForgeSourceRemovesClaim(t *testing.T) {
	f := newTestFSM(t)
	src := &domain.ForgeSource{ID: "ghrepo-1", OrgID: "org-a", Provider: domain.ForgeGitHub, Repo: "acme/api"}
	if err := applyCmd(t, f, kindBatchUpsertRepoSettings, cmdBatchUpsertRepoSettings{
		OrgID:  "org-a",
		Writes: []cmdRepoSettingWrite{{Setting: domain.RepoSetting{OrgID: "org-a", Repo: "github.com/acme/api", Provider: domain.ForgeGitHub, ForgeSourceID: "ghrepo-1"}, ForgeSource: cmdForgeSourceFrom(src)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindDeleteForgeSource, cmdForgeSource{ID: "ghrepo-1", OrgID: "org-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.readRepoSetting("org-a", "github.com/acme/api"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("claim must be removed with its source, err=%v", err)
	}
	if _, taken := f.state.repoClaimsByRepo[repoClaimKey("github.com/acme/api")]; taken {
		t.Fatal("global claim index must be cleared")
	}
}

func TestGitHubFSMOAuthStateSweepCapAndExpiry(t *testing.T) {
	f := newTestFSM(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	expired := &domain.GitHubOAuthState{StateID: "expired", UserID: "u", ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-2 * time.Minute)}
	if err := applyCmd(t, f, kindCreateGitHubOAuthState, cmdGitHubOAuthStateFrom(expired)); err != nil {
		t.Fatal(err)
	}
	valid := &domain.GitHubOAuthState{StateID: "valid", UserID: "u", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	if err := applyCmd(t, f, kindCreateGitHubOAuthState, cmdGitHubOAuthStateFrom(valid)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.readGitHubOAuthState("expired"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired state must be swept, err=%v", err)
	}
	if _, err := consumedStateAt(f, "valid", now.Add(2*time.Hour)); !errors.Is(err, domain.ErrOAuthStateInvalid) {
		t.Fatalf("expired consume must be invalid, err=%v", err)
	}

	// Fill the bounded map and prove deterministic eviction of the earliest
	// ExpiresAt (tie broken by the lexicographically smallest StateID).
	f2 := newTestFSM(t)
	for i := 0; i < maxPendingGitHubOAuthStates; i++ {
		id := newID()
		st := &domain.GitHubOAuthState{StateID: id, UserID: "u", CreatedAt: now, ExpiresAt: now.Add(time.Duration(i+1) * time.Second)}
		if err := applyCmd(t, f2, kindCreateGitHubOAuthState, cmdGitHubOAuthStateFrom(st)); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if got := len(f2.state.githubOAuthStates); got != maxPendingGitHubOAuthStates {
		t.Fatalf("state cap = %d", got)
	}
	extra := &domain.GitHubOAuthState{StateID: "extra", UserID: "u", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := applyCmd(t, f2, kindCreateGitHubOAuthState, cmdGitHubOAuthStateFrom(extra)); err != nil {
		t.Fatal(err)
	}
	if got := len(f2.state.githubOAuthStates); got != maxPendingGitHubOAuthStates {
		t.Fatalf("state cap after eviction = %d", got)
	}
	if _, ok := f2.state.githubOAuthStates["extra"]; !ok {
		t.Fatal("new state must be retained")
	}
}

func consumedStateAt(f *FSM, stateID string, at time.Time) (*domain.GitHubOAuthState, error) {
	cmd, err := newCommand(kindConsumeGitHubOAuthState, cmdConsumeGitHubOAuthState{StateID: stateID, At: at})
	if err != nil {
		return nil, err
	}
	resp, err := f.applyCommand(cmd)
	if err == nil {
		if respErr, ok := resp.(error); ok {
			err = respErr
		}
	}
	if err != nil {
		return nil, err
	}
	st, _ := resp.(*domain.GitHubOAuthState)
	return st, nil
}

func TestGitHubFSMCascades(t *testing.T) {
	f := newTestFSM(t)
	now := time.Now().UTC()
	if err := applyCmd(t, f, kindUpsertOrg, &cmdOrg{Org: domain.Org{ID: "org-a", Name: "acme", CreatedAt: now}, Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "user-a", Username: "alice", Create: true}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindUpsertGitHubAccountSource, cmdGitHubAccountSourceFrom(&domain.GitHubAccountSource{ID: "acct", OrgID: "org-a", Owner: "acme"})); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindBatchUpsertRepoSettings, cmdBatchUpsertRepoSettings{OrgID: "org-a", Writes: []cmdRepoSettingWrite{{Setting: domain.RepoSetting{OrgID: "org-a", Repo: "github.com/acme/api", SourceID: "acct", ForgeSourceID: "repo"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindCreateGitHubOAuthState, cmdGitHubOAuthStateFrom(&domain.GitHubOAuthState{StateID: "state", UserID: "user-a", OrgID: "org-a", ExpiresAt: now.Add(time.Hour), CreatedAt: now})); err != nil {
		t.Fatal(err)
	}
	if err := applyCmd(t, f, kindUpsertGitHubConnection, cmdGitHubConnectionFrom(&domain.UserGitHubConnection{UserID: "user-a", TokenCiphertext: "ct"})); err != nil {
		t.Fatal(err)
	}
	// User deletion erases the connection and the user's pending state.
	if err := applyCmd(t, f, kindSoftDeleteUser, cmdSoftDeleteUser{ID: "user-a", At: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.readGitHubConnection("user-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("connection must be erased, err=%v", err)
	}
	if _, err := f.readGitHubOAuthState("state"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("user state must be erased, err=%v", err)
	}
	// Org cascade erases account sources and claims.
	if err := applyCmd(t, f, kindDeleteOrgCascade, "org-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.readGitHubAccountSource("acct"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("account must be erased, err=%v", err)
	}
	if _, err := f.readRepoSetting("org-a", "github.com/acme/api"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("setting must be erased, err=%v", err)
	}
}

func TestGitHubSnapshotBackwardCompatible(t *testing.T) {
	legacy := `{"users":[],"groups":[],"tokens":[],"projects":[],"orgs":[{"id":"org-a","name":"acme"}],"secrets":[],"forge_sources":[],"meta":{}}`
	f := NewFSM()
	if err := f.Restore(io.NopCloser(strings.NewReader(legacy))); err != nil {
		t.Fatalf("restore legacy snapshot: %v", err)
	}
	o := f.state.orgs["org-a"]
	if o == nil {
		t.Fatal("org must be restored")
	}
	if len(o.Features) != len(domain.DefaultOrgFeatures) {
		t.Fatalf("features = %v", o.Features)
	}
	if f.state.githubConnections == nil || f.state.repoSettings == nil || f.state.githubOAuthStates == nil {
		t.Fatal("new maps must initialize non-nil after legacy restore")
	}
}

func TestGitHubRepositoriesRoundTrip(t *testing.T) {
	store := newTestRaftStore(t)
	ctx := context.Background()
	connRepo := NewGitHubConnectionRepo(store)
	acctRepo := NewGitHubAccountSourceRepo(store)
	settingRepo := NewRepoSettingRepo(store)
	stateRepo := NewGitHubOAuthStateRepo(store)

	conn := &domain.UserGitHubConnection{UserID: "u1", GitHubLogin: "alice", TokenCiphertext: "ct"}
	if err := connRepo.Upsert(ctx, conn); err != nil {
		t.Fatal(err)
	}
	got, err := connRepo.GetByUser(ctx, "u1")
	if err != nil || got.TokenCiphertext != "ct" || got.ConnectedAt.IsZero() {
		t.Fatalf("connection = %+v err=%v", got, err)
	}
	acct := &domain.GitHubAccountSource{ID: "acct", OrgID: "org-a", Owner: "acme", TokenCiphertext: "act"}
	if err := acctRepo.Upsert(ctx, acct); err != nil {
		t.Fatal(err)
	}
	if got, err := acctRepo.GetByOrgAndOwner(ctx, "org-a", "ACME"); err != nil || got.ID != "acct" {
		t.Fatalf("account by owner = %+v err=%v", got, err)
	}
	if got, err := acctRepo.ListByOrg(ctx, "org-a"); err != nil || len(got) != 1 {
		t.Fatalf("account list = %v err=%v", got, err)
	}
	if err := settingRepo.BatchUpsert(ctx, "org-a", []domain.RepoSettingWrite{{
		Setting:     &domain.RepoSetting{OrgID: "org-a", Repo: "github.com/acme/api", SourceID: "acct", ForgeSourceID: "repo"},
		ForgeSource: &domain.ForgeSource{ID: "repo", OrgID: "org-a", Provider: domain.ForgeGitHub, Repo: "acme/api"},
	}}); err != nil {
		t.Fatal(err)
	}
	if got, err := settingRepo.Get(ctx, "org-a", "github.com/acme/api"); err != nil || got.SourceID != "acct" {
		t.Fatalf("setting = %+v err=%v", got, err)
	}
	if got, err := settingRepo.GetByCanonicalRepo(ctx, "github.com/acme/api"); err != nil || got.ForgeSourceID != "repo" {
		t.Fatalf("setting by canonical = %+v err=%v", got, err)
	}
	if got, err := settingRepo.ListBySource(ctx, "acct"); err != nil || len(got) != 1 {
		t.Fatalf("settings by source = %v err=%v", got, err)
	}
	if err := settingRepo.Delete(ctx, "org-a", "github.com/acme/api", "repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := settingRepo.Get(ctx, "org-a", "github.com/acme/api"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted setting err=%v", err)
	}

	now := time.Now().UTC()
	st := &domain.GitHubOAuthState{StateID: newID(), UserID: "u1", ExpiresAt: now.Add(time.Minute), CreatedAt: now, PKCEVerifier: "v"}
	if err := stateRepo.Create(ctx, st); err != nil {
		t.Fatal(err)
	}
	if got, err := stateRepo.Get(ctx, st.StateID); err != nil || got.PKCEVerifier != "v" {
		t.Fatalf("state = %+v err=%v", got, err)
	}
	consumed, err := stateRepo.Consume(ctx, st.StateID, now.Add(time.Second))
	if err != nil || consumed.UserID != "u1" {
		t.Fatalf("consume = %+v err=%v", consumed, err)
	}
	if _, err := stateRepo.Consume(ctx, st.StateID, now.Add(time.Second)); !errors.Is(err, domain.ErrOAuthStateInvalid) {
		t.Fatalf("replay err=%v", err)
	}
}
