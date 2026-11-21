package service

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type fakeSecretRepo struct{ secrets map[string]*domain.Secret }

func (f *fakeSecretRepo) Upsert(_ context.Context, s *domain.Secret) error {
	cp := *s
	f.secrets[s.ID] = &cp
	return nil
}
func (f *fakeSecretRepo) Get(_ context.Context, scope domain.SecretScope, scopeID, name string) (*domain.Secret, error) {
	s, ok := f.secrets[string(scope)+":"+scopeID+":"+name]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *s
	return &cp, nil
}
func (f *fakeSecretRepo) List(_ context.Context, scope domain.SecretScope, scopeID string) ([]*domain.Secret, error) {
	var out []*domain.Secret
	for _, s := range f.secrets {
		if s.Scope == scope && s.ScopeID == scopeID {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (f *fakeSecretRepo) Delete(context.Context, domain.SecretScope, string, string) error {
	return nil
}

func TestSecretServiceSealReveal(t *testing.T) {
	repo := &fakeSecretRepo{secrets: map[string]*domain.Secret{}}
	svc := NewSecretService(repo, []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	if _, err := svc.Put(context.Background(), domain.SecretScopeOrg, "acme", "TOKEN", "s3cr3t"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Reveal(context.Background(), domain.SecretScopeOrg, "acme", "TOKEN")
	if err != nil || got != "s3cr3t" {
		t.Fatalf("reveal = %q err=%v", got, err)
	}
	if _, err := svc.Reveal(context.Background(), domain.SecretScopeOrg, "other", "TOKEN"); err == nil {
		t.Fatal("scope-bound AAD must reject a different scope id")
	}
}

func TestEnvironmentScopeIsolation(t *testing.T) {
	repo := &fakeSecretRepo{secrets: map[string]*domain.Secret{}}
	svc := NewSecretService(repo, []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	ctx := context.Background()
	if _, err := svc.PutEnvironment(ctx, "org-a", "prod", "KEY", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutEnvironment(ctx, "org-b", "prod", "KEY", "b"); err != nil {
		t.Fatal(err)
	}
	a, err := svc.RevealEnvironment(ctx, "org-a", "prod", "KEY")
	if err != nil || a != "a" {
		t.Fatalf("org-a = %q err=%v", a, err)
	}
	b, err := svc.RevealEnvironment(ctx, "org-b", "prod", "KEY")
	if err != nil || b != "b" {
		t.Fatalf("org-b = %q err=%v", b, err)
	}
	if _, err := svc.Put(ctx, domain.SecretScopeEnvironment, "prod", "KEY", "x"); err == nil {
		t.Fatal("raw environment scope must be rejected")
	}
}

func TestResolvePrecedence(t *testing.T) {
	repo := &fakeSecretRepo{secrets: map[string]*domain.Secret{}}
	svc := NewSecretService(repo, []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	ctx := context.Background()
	if _, err := svc.Put(ctx, domain.SecretScopeOrg, "acme", "KEY", "org"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Put(ctx, domain.SecretScopeProject, "p1", "KEY", "project"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutEnvironment(ctx, "acme", "prod", "KEY", "env"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Resolve(ctx, "acme", "p1", "prod", "KEY")
	if err != nil || got != "env" {
		t.Fatalf("env precedence = %q err=%v", got, err)
	}
	got, err = svc.Resolve(ctx, "acme", "p1", "", "KEY")
	if err != nil || got != "project" {
		t.Fatalf("project precedence = %q err=%v", got, err)
	}
	got, err = svc.Resolve(ctx, "acme", "", "", "KEY")
	if err != nil || got != "org" {
		t.Fatalf("org precedence = %q err=%v", got, err)
	}
}

func TestResolveJobSecretsAndAllForCheck(t *testing.T) {
	repo := &fakeSecretRepo{secrets: map[string]*domain.Secret{}}
	svc := NewSecretService(repo, []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	ctx := context.Background()
	if _, err := svc.Put(ctx, domain.SecretScopeOrg, "acme", "A", "org-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Put(ctx, domain.SecretScopeOrg, "acme", "B", "org-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Put(ctx, domain.SecretScopeProject, "p1", "B", "proj-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutEnvironment(ctx, "acme", "prod", "C", "env-c"); err != nil {
		t.Fatal(err)
	}
	all, err := svc.ResolveAllForCheck(ctx, "acme", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if all["A"] != "org-a" || all["B"] != "proj-b" {
		t.Fatalf("all = %+v", all)
	}
	if _, ok := all["C"]; ok {
		t.Fatal("check secrets must never include environment secrets")
	}
	job, err := svc.ResolveJobSecrets(ctx, "acme", "p1", "prod", []string{"A", "C"})
	if err != nil {
		t.Fatal(err)
	}
	if job["A"] != "org-a" || job["C"] != "env-c" {
		t.Fatalf("job = %+v", job)
	}
	if _, err := svc.ResolveJobSecrets(ctx, "acme", "p1", "", []string{"MISSING"}); err == nil {
		t.Fatal("missing secret must fail")
	}
}
