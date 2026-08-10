package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newLegacyForTest(t *testing.T) (*UserService, *TokenService, *GroupService, *repos) {
	t.Helper()
	r := newServiceDB(t)
	logger := testLogger()
	usvc := NewUserService(r.users, r.groups, logger)
	tsvc := NewTokenService(r.tokens, logger)
	gsvc := NewGroupService(r.groups, r.users, logger)
	return usvc, tsvc, gsvc, r
}

func writeTokensFile(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write tokens: %v", err)
	}
	return path
}

func TestImportTokensFile(t *testing.T) {
	usvc, tsvc, gsvc, _ := newLegacyForTest(t)
	ctx := context.Background()

	path := writeTokensFile(t, []string{"# comment", "", "token-one", "token-two"})
	res, err := ImportTokensFile(ctx, path, usvc, tsvc, gsvc, testLogger(), false)
	if err != nil {
		t.Fatalf("ImportTokensFile: %v", err)
	}
	if res.Imported != 2 {
		t.Fatalf("imported = %d, want 2", res.Imported)
	}
	if res.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", res.Skipped)
	}
	if len(res.Usernames) != 2 || res.Usernames[0] != "legacy-1" || res.Usernames[1] != "legacy-2" {
		t.Fatalf("usernames = %v", res.Usernames)
	}

	// Both tokens should now validate.
	if _, err := tsvc.Validate(ctx, "token-one"); err != nil {
		t.Fatalf("validate token-one: %v", err)
	}
	if _, err := tsvc.Validate(ctx, "token-two"); err != nil {
		t.Fatalf("validate token-two: %v", err)
	}

	// Users should be members of the "legacy" group.
	legacy, err := gsvc.GetByName(ctx, "legacy")
	if err != nil {
		t.Fatalf("get legacy group: %v", err)
	}
	members, _ := gsvc.Members(ctx, legacy.ID)
	if len(members) != 2 {
		t.Fatalf("legacy members = %d, want 2", len(members))
	}
}

func TestImportTokensFileIdempotent(t *testing.T) {
	usvc, tsvc, gsvc, _ := newLegacyForTest(t)
	ctx := context.Background()

	path := writeTokensFile(t, []string{"token-one", "token-two"})
	if _, err := ImportTokensFile(ctx, path, usvc, tsvc, gsvc, testLogger(), false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// Second run skips both (already present by hash).
	res, err := ImportTokensFile(ctx, path, usvc, tsvc, gsvc, testLogger(), false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if res.Imported != 0 || res.Skipped != 2 {
		t.Fatalf("second run: imported=%d skipped=%d, want 0/2", res.Imported, res.Skipped)
	}
}

func TestImportTokensFileDryRun(t *testing.T) {
	usvc, tsvc, gsvc, r := newLegacyForTest(t)
	ctx := context.Background()

	path := writeTokensFile(t, []string{"token-one"})
	res, err := ImportTokensFile(ctx, path, usvc, tsvc, gsvc, testLogger(), true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("imported = %d, want 1", res.Imported)
	}
	// Nothing should have been written.
	if n, _ := r.users.Count(ctx); n != 0 {
		t.Fatalf("dry-run should not create users, got %d", n)
	}
}

func TestImportTokensFileMissing(t *testing.T) {
	usvc, tsvc, gsvc, _ := newLegacyForTest(t)
	if _, err := ImportTokensFile(context.Background(), "/no/such/file", usvc, tsvc, gsvc, testLogger(), false); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestImportTokensFileEmptyAndComments(t *testing.T) {
	usvc, tsvc, gsvc, r := newLegacyForTest(t)
	ctx := context.Background()

	path := writeTokensFile(t, []string{"# only a comment", "", "   "})
	res, err := ImportTokensFile(ctx, path, usvc, tsvc, gsvc, testLogger(), false)
	if err != nil {
		t.Fatalf("ImportTokensFile: %v", err)
	}
	if res.Imported != 0 {
		t.Fatalf("imported = %d, want 0", res.Imported)
	}
	if n, _ := r.users.Count(ctx); n != 0 {
		t.Fatalf("no users should be created, got %d", n)
	}
}
