package service

import (
	"context"
	"io"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// capturingForgeProvider records the secret passed to RegisterWebhook.
type capturingForgeProvider struct {
	kind          domain.ForgeKind
	registerCalls int
	gotSecret     string
}

func (c *capturingForgeProvider) Kind() domain.ForgeKind { return c.kind }
func (c *capturingForgeProvider) RegisterWebhook(_ context.Context, _ *domain.ForgeSource, _, _, secret string) (string, error) {
	c.registerCalls++
	c.gotSecret = secret
	return "1", nil
}
func (c *capturingForgeProvider) DeleteWebhook(context.Context, *domain.ForgeSource, string, string) error {
	return nil
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
