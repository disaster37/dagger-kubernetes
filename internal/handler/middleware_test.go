package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// newAuthEngine builds a route engine with the admin + auth routes registered
// (mirroring configure(), including the adminOnly gate on admin routes).
func newAuthEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.POST("/api/v1/auth/login", s.handleLogin)
	e.POST("/api/v1/auth/refresh", s.handleRefresh)
	e.GET("/api/v1/auth/providers", s.handleProviders)
	e.GET("/api/v1/auth/me", s.handleMe)
	e.PUT("/api/v1/auth/password", s.handleChangePassword)
	e.GET("/api/v1/users", s.adminOnly(s.handleUsersList))
	e.POST("/api/v1/users", s.adminOnly(s.handleUserCreate))
	e.GET("/api/v1/users/:id", s.adminOnly(s.handleUserGet))
	e.PUT("/api/v1/users/:id", s.adminOnly(s.handleUserUpdate))
	e.DELETE("/api/v1/users/:id", s.adminOnly(s.handleUserDelete))
	e.PUT("/api/v1/users/:id/password", s.adminOnly(s.handleUserResetPassword))
	e.PUT("/api/v1/users/:id/groups", s.adminOnly(s.handleUserGroups))
	e.GET("/api/v1/users/:id/token", s.adminOnly(s.handleUserTokenMeta))
	e.DELETE("/api/v1/users/:id/token", s.adminOnly(s.handleUserTokenRevoke))
	e.GET("/api/v1/groups", s.adminOnly(s.handleGroupsList))
	e.POST("/api/v1/groups", s.adminOnly(s.handleGroupCreate))
	e.GET("/api/v1/groups/:id", s.adminOnly(s.handleGroupGet))
	e.PUT("/api/v1/groups/:id", s.adminOnly(s.handleGroupUpdate))
	e.DELETE("/api/v1/groups/:id", s.adminOnly(s.handleGroupDelete))
	e.GET("/api/v1/groups/:id/members", s.adminOnly(s.handleGroupMembers))
	e.PUT("/api/v1/groups/:id/members", s.adminOnly(s.handleGroupSetMembers))
	e.GET("/api/v1/projects", s.adminOnly(s.handleProjectsList))
	e.POST("/api/v1/projects", s.adminOnly(s.handleProjectCreate))
	e.PUT("/api/v1/projects/:id", s.adminOnly(s.handleProjectUpdate))
	e.DELETE("/api/v1/projects/:id", s.adminOnly(s.handleProjectDelete))
	e.GET("/api/v1/tokens/me", s.handleMyTokenMeta)
	e.POST("/api/v1/tokens/me", s.handleMyTokenCreate)
	e.PUT("/api/v1/tokens/me/regenerate", s.handleMyTokenRegenerate)
	e.DELETE("/api/v1/tokens/me", s.handleMyTokenRevoke)
	e.GET("/api/v1/connect/env", s.handleConnectEnv)
	e.GET("/api/v1/traces", s.handleTracesList)
	e.GET("/api/v1/traces/:traceID", s.handleTracesDetail)
	e.GET("/api/v1/traces/:traceID/logs", s.handleTracesLogs)
	e.GET("/api/v1/logs/:traceID", s.handleLogsRoutes)
	e.POST("/v1/engines", s.handleEngines)
	return e
}

func TestMiddlewareRequireAdminRejectsUser(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	bearer, _ := env.createUserAndToken(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/users", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("user hitting admin route: %d, want 403", resp.Result().StatusCode())
	}
}

func TestMiddlewareRequireAdminRejectsAnonymous(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/users", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("anonymous hitting admin route: %d, want 401", resp.Result().StatusCode())
	}
}

func TestMiddlewareRequireAdminAllowsAdmin(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/users", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("admin hitting admin route: %d, want 200", resp.Result().StatusCode())
	}
}

func TestAuthorizeTraceOwnerSeesOwn(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer, u := env.createUserAndToken(t)
	// Seed a trace owned by alice (unassigned).
	if err := env.server.traceMeta.UpsertProvision(ctx, "trace-own", u.ID, ""); err != nil {
		t.Fatalf("upsert provision: %v", err)
	}

	// Alice can list it (own unassigned fallback).
	resp := ut.PerformRequest(e, "GET", "/api/v1/traces", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("list: %d", resp.Result().StatusCode())
	}
	var rows []map[string]any
	if err := json.Unmarshal(resp.Result().Body(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0]["trace_id"] != "trace-own" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestAuthorizeTraceNonMember404(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	// Create a group + admin-owned trace in that group.
	g, _ := env.groups.Create(ctx, service.GroupInput{Name: "G1", AgentAvailable: true})
	admin, _ := env.users.GetByUsername(ctx, "admin")
	env.groups.SetMembers(ctx, g.ID, []string{admin.ID})
	env.server.traceMeta.UpsertProvision(ctx, "trace-group", admin.ID, "")
	env.server.traceMeta.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "trace-group", GroupID: g.ID, UserID: admin.ID})

	// Non-member user gets 404 on detail.
	bearer, _ := env.createUserAndToken(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/trace-group", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("non-member: %d, want 404", resp.Result().StatusCode())
	}
}

func TestAuthorizeTraceAdminSeesAll(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	env.server.traceMeta.UpsertProvision(ctx, "trace-x", "admin", "")
	// Admin sees unknown traces too (Tempo-only).
	bearer := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/unknown-trace", nil, ut.Header{Key: "Authorization", Value: bearer})
	// Unknown trace -> 404 from Tempo (not from authorizeTrace).
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("admin unknown trace: %d, want 404 (from Tempo)", resp.Result().StatusCode())
	}
}

func TestAuthorizeTraceNonMemberUnknownTrace404(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	bearer, _ := env.createUserAndToken(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/unknown-trace", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("non-member unknown trace: %d, want 404 (fail-closed)", resp.Result().StatusCode())
	}
}

func TestTracesListAdminUnassignedFilter(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	// Seed one grouped trace and one unassigned trace.
	g, _ := env.groups.Create(ctx, service.GroupInput{Name: "G1", AgentAvailable: true})
	admin, _ := env.users.GetByUsername(ctx, "admin")
	env.server.traceMeta.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "in-group", GroupID: g.ID, UserID: admin.ID})
	env.server.traceMeta.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "loose", UserID: admin.ID})

	bearer := env.loginAsAdmin(t)

	// ?group_id=unassigned returns only unassigned traces.
	resp := ut.PerformRequest(e, "GET", "/api/v1/traces?group_id=unassigned", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("unassigned filter: %d", resp.Result().StatusCode())
	}
	var rows []map[string]any
	if err := json.Unmarshal(resp.Result().Body(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0]["trace_id"] != "loose" {
		t.Fatalf("unassigned filter rows = %v, want only loose", rows)
	}

	// Without the filter the admin sees both.
	resp = ut.PerformRequest(e, "GET", "/api/v1/traces", nil, ut.Header{Key: "Authorization", Value: bearer})
	json.Unmarshal(resp.Result().Body(), &rows)
	if len(rows) != 2 {
		t.Fatalf("admin all rows = %d, want 2", len(rows))
	}
}

func TestWriteServiceErrorMapping(t *testing.T) {
	env := newTestEnv(t, false)
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"not found", domain.ErrNotFound, http.StatusNotFound},
		{"token exists", domain.ErrTokenExists, http.StatusConflict},
		{"invalid cred", domain.ErrInvalidCredential, http.StatusUnauthorized},
		{"forbidden", domain.ErrForbidden, http.StatusForbidden},
		{"no groups", domain.ErrNoGroups, http.StatusForbidden},
		{"agent unavailable", domain.ErrAgentUnavailable, http.StatusForbidden},
		{"quota exhausted", domain.ErrQuotaExhausted, http.StatusTooManyRequests},
		{"unauthenticated", domain.ErrUnauthenticated, http.StatusUnauthorized},
		{"unique violation", errors.New("UNIQUE constraint failed: users"), http.StatusConflict},
		{"other", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := route.NewEngine(config.NewOptions(nil))
			e.GET("/err", func(_ context.Context, c *app.RequestContext) {
				env.server.writeServiceError(c, tc.err)
			})
			resp := ut.PerformRequest(e, "GET", "/err", nil)
			if resp.Result().StatusCode() != tc.status {
				t.Fatalf("%s: %d, want %d", tc.name, resp.Result().StatusCode(), tc.status)
			}
		})
	}
}
