package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// stubEngineCachePurger is a scriptable domain.EngineCachePurger for handler
// tests.
type stubEngineCachePurger struct {
	result *domain.EngineCachePurgeResult
	err    error
	status *domain.EngineCachePurgeResult
	ok     bool
}

func (s *stubEngineCachePurger) Purge(context.Context, string) (*domain.EngineCachePurgeResult, error) {
	return s.result, s.err
}

func (s *stubEngineCachePurger) Status(string) (*domain.EngineCachePurgeResult, bool) {
	return s.status, s.ok
}

// newPurgeTestEnv builds a test env with the given purger wired and returns the
// fully-configured route engine (all routes + middleware).
func newPurgeTestEnv(t *testing.T, purger domain.EngineCachePurger) (*testEnv, *route.Engine) {
	t.Helper()
	env := newTestEnv(t)
	env.server.engineCachePurger = purger
	h, err := env.server.configure()
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	return env, h.Engine
}

func TestFleetPurgeRequiresAdmin(t *testing.T) {
	env, e := newPurgeTestEnv(t, &stubEngineCachePurger{result: &domain.EngineCachePurgeResult{}})

	resp := ut.PerformRequest(e, "POST", "/api/v1/fleet/v0.19.0/purge-cache", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", resp.Result().StatusCode())
	}

	alice, _ := env.createUserAndToken(t)
	resp = ut.PerformRequest(e, "POST", "/api/v1/fleet/v0.19.0/purge-cache", nil, ut.Header{Key: "Authorization", Value: alice})
	if resp.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", resp.Result().StatusCode())
	}
}

func TestFleetPurgeInvalidVersion(t *testing.T) {
	env, e := newPurgeTestEnv(t, &stubEngineCachePurger{result: &domain.EngineCachePurgeResult{}})
	auth := env.loginAsAdmin(t)

	for _, version := range []string{"v", "1.2", "v0.19"} {
		resp := ut.PerformRequest(e, "POST", fmt.Sprintf("/api/v1/fleet/%s/purge-cache", version), nil,
			ut.Header{Key: "Authorization", Value: auth})
		if resp.Result().StatusCode() != http.StatusBadRequest {
			t.Fatalf("version %q = %d, want 400", version, resp.Result().StatusCode())
		}
	}
}

func TestFleetPurgeSuccess(t *testing.T) {
	want := &domain.EngineCachePurgeResult{
		Version:  "v0.19.0",
		State:    "completed",
		Replicas: 2,
		Pods: []domain.EnginePodPurgeResult{
			{PodName: "p0", Ordinal: 0, Pruned: true},
			{PodName: "p1", Ordinal: 1, Pruned: true},
		},
	}
	env, e := newPurgeTestEnv(t, &stubEngineCachePurger{result: want})
	auth := env.loginAsAdmin(t)

	resp := ut.PerformRequest(e, "POST", "/api/v1/fleet/v0.19.0/purge-cache", nil,
		ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
	}
	var got domain.EngineCachePurgeResult
	if err := json.Unmarshal(resp.Result().Body(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != "completed" || len(got.Pods) != 2 {
		t.Fatalf("got = %+v, want completed/2 pods", got)
	}
}

func TestFleetPurgeErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"fleet not found", domain.ErrEngineFleetNotFound, http.StatusNotFound},
		{"in progress", domain.ErrPurgeInProgress, http.StatusConflict},
		{"generic", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, e := newPurgeTestEnv(t, &stubEngineCachePurger{err: tc.err})
			auth := env.loginAsAdmin(t)
			resp := ut.PerformRequest(e, "POST", "/api/v1/fleet/v0.19.0/purge-cache", nil,
				ut.Header{Key: "Authorization", Value: auth})
			if resp.Result().StatusCode() != tc.want {
				t.Fatalf("status = %d, want %d", resp.Result().StatusCode(), tc.want)
			}
		})
	}
}

func TestFleetPurgeStatusNotFound(t *testing.T) {
	env, e := newPurgeTestEnv(t, &stubEngineCachePurger{ok: false})
	auth := env.loginAsAdmin(t)

	resp := ut.PerformRequest(e, "GET", "/api/v1/fleet/v0.19.0/purge-cache", nil,
		ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Result().StatusCode())
	}
}

func TestFleetPurgeStatusOK(t *testing.T) {
	want := &domain.EngineCachePurgeResult{Version: "v0.19.0", State: "completed"}
	env, e := newPurgeTestEnv(t, &stubEngineCachePurger{status: want, ok: true})
	auth := env.loginAsAdmin(t)

	resp := ut.PerformRequest(e, "GET", "/api/v1/fleet/v0.19.0/purge-cache", nil,
		ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
	}
}

func TestFleetPurgeNilPurger(t *testing.T) {
	env, e := newPurgeTestEnv(t, nil)
	auth := env.loginAsAdmin(t)

	resp := ut.PerformRequest(e, "POST", "/api/v1/fleet/v0.19.0/purge-cache", nil,
		ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.Result().StatusCode())
	}
}
