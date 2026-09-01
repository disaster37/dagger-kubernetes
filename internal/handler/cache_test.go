package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestHandleCacheInfoNoAuth(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/cache", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCacheInfoOK(t *testing.T) {
	env := newTestEnv(t)
	env.server.cacheStats = &stubCacheStatsProvider{stats: &domain.CacheStats{Running: true}}
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cache", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCachePurgeNoAuth(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "POST", "/api/v1/cache/purge", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCachePurgeAdminOnly(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "POST", "/api/v1/cache/purge", &ut.Body{Body: strings.NewReader("{}"), Len: len("{}")}, ut.Header{Key: "Authorization", Value: auth}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCachePurgeDeleteDisabled(t *testing.T) {
	env := newTestEnv(t)
	env.server.cachePurger = &stubCachePurger{err: domain.ErrRegistryDeleteDisabled}
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "POST", "/api/v1/cache/purge", &ut.Body{Body: strings.NewReader("{}"), Len: len("{}")}, ut.Header{Key: "Authorization", Value: auth}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.Result().StatusCode())
	}
}
