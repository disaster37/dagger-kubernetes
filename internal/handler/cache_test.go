package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestHandleCacheInfoAuthGating(t *testing.T) {
	env := newTestEnv(t, false)
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/cache", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Result().StatusCode())
	}

	auth := env.loginAsAdmin(t)
	resp = ut.PerformRequest(e, "GET", "/api/v1/cache", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCacheInfoShape(t *testing.T) {
	env := newTestEnv(t, false)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cache", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}

	var info domain.CacheStats
	if err := json.Unmarshal(resp.Result().Body(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Backend != "registry" || info.Registry != "cache.reg/dagger-cache" {
		t.Fatalf("info = %+v", info)
	}
}

func TestHandleCachePurgeAdminOnly(t *testing.T) {
	env := newTestEnv(t, false)
	e := newTestEngine(env.server)

	body := `{"version":"v0.21.4"}`
	// No token → 401.
	resp := ut.PerformRequest(e, "POST", "/api/v1/cache/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Result().StatusCode())
	}

	// Regular user → 403.
	alice, _ := env.createUserAndToken(t)
	resp = ut.PerformRequest(e, "POST", "/api/v1/cache/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Authorization", Value: alice}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("expected 403 for user, got %d", resp.Result().StatusCode())
	}

	// Admin → 200 with stub result.
	auth := env.loginAsAdmin(t)
	resp = ut.PerformRequest(e, "POST", "/api/v1/cache/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Authorization", Value: auth}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200 for admin, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCachePurgeInvalidVersion(t *testing.T) {
	env := newTestEnv(t, false)
	e := newTestEngine(env.server)
	auth := env.loginAsAdmin(t)

	body := `{"version":"not-a-version"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/cache/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Authorization", Value: auth}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCachePurgeDeleteDisabled(t *testing.T) {
	env := newTestEnv(t, false)
	env.server.cachePurger = &stubCachePurger{err: domain.ErrRegistryDeleteDisabled}
	e := newTestEngine(env.server)
	auth := env.loginAsAdmin(t)

	body := `{"version":"v0.21.4"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/cache/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Authorization", Value: auth}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCachePurgeAllAdminOnly(t *testing.T) {
	env := newTestEnv(t, false)
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "POST", "/api/v1/cache/purge-all", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Result().StatusCode())
	}

	auth := env.loginAsAdmin(t)
	resp = ut.PerformRequest(e, "POST", "/api/v1/cache/purge-all", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}
