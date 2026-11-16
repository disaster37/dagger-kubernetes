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
func (f *fakeSecretRepo) List(context.Context, domain.SecretScope, string) ([]*domain.Secret, error) {
	return nil, nil
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
