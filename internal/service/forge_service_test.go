package service

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// errForgeTest is an injected provider failure.
var errForgeTest = errors.New("forge test error")

// capturingForgeProvider records the secret passed to RegisterWebhook and the
// DeleteWebhook calls, with optional error injection.
type capturingForgeProvider struct {
	kind          domain.ForgeKind
	registerCalls int
	gotSecret     string
	deleteCalls   int
	gotDeleteID   string
	deleteErr     error
}

func (c *capturingForgeProvider) Kind() domain.ForgeKind { return c.kind }
func (c *capturingForgeProvider) RegisterWebhook(_ context.Context, _ *domain.ForgeSource, _, _, secret string) (string, error) {
	c.registerCalls++
	c.gotSecret = secret
	return "1", nil
}
func (c *capturingForgeProvider) DeleteWebhook(_ context.Context, _ *domain.ForgeSource, _, webhookID string) error {
	c.deleteCalls++
	c.gotDeleteID = webhookID
	return c.deleteErr
}
func (c *capturingForgeProvider) VerifyWebhook(*domain.ForgeSource, string, map[string]string, []byte) bool {
	return true
}
func (c *capturingForgeProvider) SetCommitStatus(context.Context, *domain.ForgeSource, string, *domain.CheckRun) error {
	return nil
}
func (c *capturingForgeProvider) CreateCheckRun(context.Context, *domain.ForgeSource, string, *domain.CheckRun) error {
	return nil
}

func newForgeServiceForTest(t *testing.T) (*ForgeService, *capturingForgeProvider, *SecretService, domain.ForgeSourceRepository) {
	t.Helper()
	store := newServiceStore(t)
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logger)
	sources := repository.NewForgeSourceRepo(store)
	provider := &capturingForgeProvider{kind: domain.ForgeGitHub}
	svc := NewForgeService(sources, secretSvc, []domain.ForgeProvider{provider}, "https://example.com", logger)
	return svc, provider, secretSvc, sources
}

func TestForgeServiceGitHubSync(t *testing.T) {
	store := newServiceStore(t)
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logger)
	sources := repository.NewForgeSourceRepo(store)
	accounts := repository.NewGitHubAccountSourceRepo(store)
	repoSets := repository.NewRepoSettingRepo(store)
	provider := &capturingForgeProvider{kind: domain.ForgeGitHub}
	svc := NewForgeService(sources, secretSvc, []domain.ForgeProvider{provider}, "https://example.com", logger).
		WithGitHubSync(accounts, repoSets)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, &ForgeSourceInput{OrgID: "acme", Repo: "acme/api", Provider: domain.ForgeGitHub, Token: "ghp_token"})
	if err != nil {
		t.Fatal(err)
	}
	wantAcct := domain.GitHubAccountSourceID("acme", "https://api.github.com", "acme")
	if src.AccountSourceID != wantAcct {
		t.Fatalf("account source id = %q want %q", src.AccountSourceID, wantAcct)
	}
	acct, err := accounts.Get(ctx, wantAcct)
	if err != nil || acct.Owner != "acme" || acct.TokenCiphertext == "" {
		t.Fatalf("account = %+v err=%v", acct, err)
	}
	setting, err := repoSets.Get(ctx, "acme", "github.com/acme/api")
	if err != nil || setting.SourceID != wantAcct || !setting.RunChecksOnDefaultBranch || !setting.RunChecksOnPullRequest || !setting.RunChecksOnBranchPush {
		t.Fatalf("setting = %+v err=%v", setting, err)
	}
	if err := svc.Delete(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repoSets.Get(ctx, "acme", "github.com/acme/api"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("setting must be deleted with the source, err=%v", err)
	}
}

func TestForgeServiceCreateSourceWebhookSecret(t *testing.T) {
	cases := []struct {
		name         string
		provided     string
		wantProvided bool
	}{
		{name: "generates a secret when omitted", provided: "", wantProvided: false},
		{name: "keeps a provided secret", provided: "provided-secret", wantProvided: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, provider, secretSvc, sources := newForgeServiceForTest(t)
			src, err := svc.CreateSource(context.Background(), &ForgeSourceInput{
				OrgID: "acme", Repo: "acme/repo", Provider: domain.ForgeGitHub,
				Token: "forge-token", WebhookSecret: tc.provided,
			})
			if err != nil {
				t.Fatal(err)
			}
			if provider.registerCalls != 1 {
				t.Fatalf("RegisterWebhook calls = %d", provider.registerCalls)
			}
			if provider.gotSecret == "" {
				t.Fatal("registered webhook secret must not be empty")
			}
			if tc.wantProvided && provider.gotSecret != tc.provided {
				t.Fatalf("registered secret = %q, want %q", provider.gotSecret, tc.provided)
			}
			if !tc.wantProvided && provider.gotSecret == tc.provided {
				t.Fatal("expected a generated secret, got the empty input")
			}
			stored, err := sources.Get(context.Background(), src.ID)
			if err != nil {
				t.Fatal(err)
			}
			opened, err := secretSvc.OpenRaw(domain.SecretScopeOrg, "acme", stored.WebhookSecretCiphertext)
			if err != nil {
				t.Fatal(err)
			}
			if opened != provider.gotSecret {
				t.Fatalf("sealed secret = %q, want registered %q", opened, provider.gotSecret)
			}
		})
	}
}

func TestForgeServiceCreateSourcePreCleansWebhook(t *testing.T) {
	svc, provider, _, _ := newForgeServiceForTest(t)
	if _, err := svc.CreateSource(context.Background(), &ForgeSourceInput{
		OrgID: "acme", Repo: "acme/repo", Provider: domain.ForgeGitHub, Token: "forge-token",
	}); err != nil {
		t.Fatal(err)
	}
	if provider.deleteCalls != 1 {
		t.Fatalf("DeleteWebhook calls = %d, want 1 pre-clean", provider.deleteCalls)
	}
	if provider.gotDeleteID != "" {
		t.Fatalf("pre-clean should look up by URL, got id %q", provider.gotDeleteID)
	}
	if provider.registerCalls != 1 {
		t.Fatalf("RegisterWebhook calls = %d", provider.registerCalls)
	}
}

func TestForgeServiceCreateSourcePreCleanFailureStillRegisters(t *testing.T) {
	svc, provider, _, _ := newForgeServiceForTest(t)
	provider.deleteErr = errForgeTest
	if _, err := svc.CreateSource(context.Background(), &ForgeSourceInput{
		OrgID: "acme", Repo: "acme/repo", Provider: domain.ForgeGitHub, Token: "forge-token",
	}); err != nil {
		t.Fatal(err)
	}
	if provider.registerCalls != 1 {
		t.Fatalf("RegisterWebhook calls = %d, want 1 despite pre-clean failure", provider.registerCalls)
	}
}

func TestForgeServiceDeleteCleansRemoteWebhook(t *testing.T) {
	cases := []struct {
		name      string
		deleteErr error
	}{
		{name: "cleanup succeeds"},
		{name: "cleanup failure still deletes locally", deleteErr: errForgeTest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, provider, _, sources := newForgeServiceForTest(t)
			src, err := svc.CreateSource(context.Background(), &ForgeSourceInput{
				OrgID: "acme", Repo: "acme/repo", Provider: domain.ForgeGitHub, Token: "forge-token",
			})
			if err != nil {
				t.Fatal(err)
			}
			provider.deleteCalls = 0
			provider.deleteErr = tc.deleteErr
			if err := svc.Delete(context.Background(), src.ID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if provider.deleteCalls != 1 {
				t.Fatalf("DeleteWebhook calls = %d, want 1", provider.deleteCalls)
			}
			if provider.gotDeleteID != "" {
				t.Fatalf("delete should look up by URL, got id %q", provider.gotDeleteID)
			}
			if _, err := sources.Get(context.Background(), src.ID); err == nil {
				t.Fatal("source must be deleted locally even when cleanup fails")
			}
		})
	}
}

func TestForgeServiceDeleteIsIdempotent(t *testing.T) {
	svc, provider, _, _ := newForgeServiceForTest(t)
	src, err := svc.CreateSource(context.Background(), &ForgeSourceInput{
		OrgID: "acme", Repo: "acme/repo", Provider: domain.ForgeGitHub, Token: "forge-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), src.ID); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	provider.deleteCalls = 0
	// A second delete of a missing source must be a no-op success, not 404.
	if err := svc.Delete(context.Background(), src.ID); err != nil {
		t.Fatalf("second Delete must be idempotent, got %v", err)
	}
	if provider.deleteCalls != 0 {
		t.Fatalf("cleanup must not run for a missing source, got %d calls", provider.deleteCalls)
	}
}
