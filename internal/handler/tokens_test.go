package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestMyTokenCreateAndMeta(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer, _ := env.createUserAndToken(t)

	// alice already has a token (from createUserAndToken); meta returns it.
	resp := ut.PerformRequest(e, "GET", "/api/v1/tokens/me", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("meta: %d", resp.Result().StatusCode())
	}
	var meta map[string]any
	json.Unmarshal(resp.Result().Body(), &meta)
	if meta["prefix"] == nil || meta["prefix"] == "" {
		t.Fatal("missing prefix")
	}

	// Second create -> 409 (already exists).
	resp = ut.PerformRequest(e, "POST", "/api/v1/tokens/me", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusConflict {
		t.Fatalf("second create: %d, want 409", resp.Result().StatusCode())
	}
}

func TestMyTokenMetaMissing(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	// Create a user but log in via JWT (no API token).
	u, _ := env.users.Create(context.Background(), "alice", "password123", domain.RoleUser)
	access, _, _, _ := env.auth.Login(context.Background(), "alice", "password123")
	bearer := "Bearer " + access
	_ = u

	resp := ut.PerformRequest(e, "GET", "/api/v1/tokens/me", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("meta missing: %d, want 404", resp.Result().StatusCode())
	}
}

func TestMyTokenCreateFromJWT(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	env.users.Create(context.Background(), "alice", "password123", domain.RoleUser)
	access, _, _, _ := env.auth.Login(context.Background(), "alice", "password123")
	bearer := "Bearer " + access

	resp := ut.PerformRequest(e, "POST", "/api/v1/tokens/me", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusCreated {
		t.Fatalf("create: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["token"] == nil || out["token"] == "" {
		t.Fatal("missing token")
	}
	if tok, ok := out["token"].(string); !ok || len(tok) < 12 || tok[:4] != "dct_" {
		t.Fatalf("token format = %v", out["token"])
	}
}

func TestMyTokenRegenerate(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer, _ := env.createUserAndToken(t)

	resp := ut.PerformRequest(e, "PUT", "/api/v1/tokens/me/regenerate", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("regenerate: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["token"] == nil {
		t.Fatal("missing new token")
	}
}

func TestMyTokenRevoke(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer, _ := env.createUserAndToken(t)

	resp := ut.PerformRequest(e, "DELETE", "/api/v1/tokens/me", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp.Result().StatusCode())
	}

	// Token no longer validates.
	resp = ut.PerformRequest(e, "GET", "/api/v1/tokens/me", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d, want 401", resp.Result().StatusCode())
	}
}

func TestMyTokenUnauthenticated(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/tokens/me", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("unauth: %d, want 401", resp.Result().StatusCode())
	}
}
