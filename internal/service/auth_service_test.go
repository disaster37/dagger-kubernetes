package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newAuthForTest(t *testing.T, cfg AuthServiceConfig, legacy domain.TokenValidator) (*AuthService, *UserService, *stubUserRepo, *stubGroupRepo) {
	t.Helper()
	urepo := newStubUserRepo()
	grepo := newStubGroupRepo()
	trepo := newStubTokenRepo()
	logger := testLogger()
	usvc := NewUserService(urepo, grepo, logger)
	tsvc := NewTokenService(trepo, logger, nil)
	jwtSvc := NewJWTService([]byte("test-secret-32-bytes-long-enough!!"), 15*time.Minute, 168*time.Hour)
	asvc := NewAuthService(cfg, usvc, grepo, tsvc, jwtSvc, legacy, logger)
	return asvc, usvc, urepo, grepo
}

func TestAuthResolveDisabled(t *testing.T) {
	asvc, _, _, _ := newAuthForTest(t, AuthServiceConfig{Disabled: true}, nil)
	id, err := asvc.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !id.IsAdmin() || id.Method != domain.AuthNone {
		t.Fatalf("disabled identity = %+v", id)
	}
}

func TestAuthResolveEmpty(t *testing.T) {
	asvc, _, _, _ := newAuthForTest(t, AuthServiceConfig{}, nil)
	if _, err := asvc.Resolve(context.Background(), ""); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("empty bearer: %v", err)
	}
}

func TestAuthResolveAPIToken(t *testing.T) {
	asvc, usvc, _, grepo := newAuthForTest(t, AuthServiceConfig{}, nil)
	ctx := context.Background()
	u, _ := usvc.Create(ctx, "alice", "password123", domain.RoleUser)
	g := &domain.Group{ID: "g1", Name: "G1", AgentAvailable: true}
	grepo.Create(ctx, g)
	grepo.SetMembers(ctx, g.ID, []string{u.ID})

	plaintext, _, _ := asvc.tokens.Generate(ctx, u.ID)
	id, err := asvc.Resolve(ctx, plaintext)
	if err != nil {
		t.Fatalf("Resolve api token: %v", err)
	}
	if id.UserID != u.ID || id.Method != domain.AuthAPIToken {
		t.Fatalf("identity = %+v", id)
	}
	if len(id.GroupIDs) != 1 || id.GroupIDs[0] != "g1" {
		t.Fatalf("groups = %v", id.GroupIDs)
	}
}

func TestAuthResolveJWT(t *testing.T) {
	asvc, usvc, _, grepo := newAuthForTest(t, AuthServiceConfig{}, nil)
	ctx := context.Background()
	u, _ := usvc.Create(ctx, "alice", "password123", domain.RoleAdmin)
	g := &domain.Group{ID: "g1", Name: "G1"}
	grepo.Create(ctx, g)
	grepo.SetMembers(ctx, g.ID, []string{u.ID})

	access, _, _ := asvc.jwt.IssuePair(u, []string{"g1"})
	id, err := asvc.Resolve(ctx, access)
	if err != nil {
		t.Fatalf("Resolve jwt: %v", err)
	}
	if id.UserID != u.ID || id.Method != domain.AuthJWT || !id.IsAdmin() {
		t.Fatalf("identity = %+v", id)
	}
}

func TestAuthResolveLegacy(t *testing.T) {
	legacy := &stubLegacyValidator{valid: map[string]bool{"legacy-token": true}}
	asvc, _, _, _ := newAuthForTest(t, AuthServiceConfig{}, legacy)
	id, err := asvc.Resolve(context.Background(), "legacy-token")
	if err != nil {
		t.Fatalf("Resolve legacy: %v", err)
	}
	if id.UserID != "legacy" || id.Method != domain.AuthLegacyTok || !id.IsAdmin() {
		t.Fatalf("identity = %+v", id)
	}
}

func TestAuthResolveLegacyMiss(t *testing.T) {
	legacy := &stubLegacyValidator{valid: map[string]bool{"legacy-token": true}}
	asvc, _, _, _ := newAuthForTest(t, AuthServiceConfig{}, legacy)
	if _, err := asvc.Resolve(context.Background(), "not-a-token"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("legacy miss: %v", err)
	}
}

func TestAuthResolveDeletedUserJWT(t *testing.T) {
	asvc, usvc, urepo, _ := newAuthForTest(t, AuthServiceConfig{}, nil)
	ctx := context.Background()
	u, _ := usvc.Create(ctx, "alice", "password123", domain.RoleUser)
	access, _, _ := asvc.jwt.IssuePair(u, nil)
	// Delete the user; the JWT is still valid cryptographically but the user is gone.
	urepo.Delete(ctx, u.ID)
	if _, err := asvc.Resolve(ctx, access); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("deleted user jwt: %v", err)
	}
}

func TestAuthResolveOrphanedTokenHash(t *testing.T) {
	asvc, usvc, urepo, _ := newAuthForTest(t, AuthServiceConfig{}, nil)
	ctx := context.Background()
	u, _ := usvc.Create(ctx, "alice", "password123", domain.RoleUser)
	plaintext, _, _ := asvc.tokens.Generate(ctx, u.ID)
	// Delete the user but leave the token hash in place (orphaned token).
	urepo.Delete(ctx, u.ID)
	if _, err := asvc.Resolve(ctx, plaintext); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("orphaned token: %v", err)
	}
}

func TestAuthLogin(t *testing.T) {
	asvc, usvc, _, _ := newAuthForTest(t, AuthServiceConfig{}, nil)
	ctx := context.Background()
	usvc.Create(ctx, "alice", "password123", domain.RoleUser)

	access, refresh, u, err := asvc.Login(ctx, "alice", "password123")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if access == "" || refresh == "" || u == nil {
		t.Fatal("bad login result")
	}
	if _, _, _, err := asvc.Login(ctx, "alice", "wrong"); err != domain.ErrInvalidCredential {
		t.Fatalf("login wrong pw: %v", err)
	}
	if _, _, _, err := asvc.Login(ctx, "nobody", "x"); err != domain.ErrInvalidCredential {
		t.Fatalf("login nobody: %v", err)
	}
}

func TestAuthRefresh(t *testing.T) {
	asvc, usvc, _, _ := newAuthForTest(t, AuthServiceConfig{}, nil)
	ctx := context.Background()
	u, _ := usvc.Create(ctx, "alice", "password123", domain.RoleUser)
	_, refresh, _, _ := asvc.Login(ctx, "alice", "password123")
	_ = u

	newAccess, newRefresh, err := asvc.Refresh(ctx, refresh)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if newAccess == "" || newRefresh == "" {
		t.Fatal("bad refresh result")
	}
	// Old refresh still cryptographically valid (stateless) but rotation issues new.
	_ = u
	// Bad refresh token.
	if _, _, err := asvc.Refresh(ctx, "not-a-jwt"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("bad refresh: %v", err)
	}
}
