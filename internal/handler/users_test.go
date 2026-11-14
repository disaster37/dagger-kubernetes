package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

func TestUsersListAdmin(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/users", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("list: %d", resp.Result().StatusCode())
	}
	var rows []map[string]any
	json.Unmarshal(resp.Result().Body(), &rows)
	if len(rows) != 1 || rows[0]["username"] != "admin" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestUserCreate(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	body := `{"username":"alice","password":"password123","role":"user"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/users", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusCreated {
		t.Fatalf("create: %d", resp.Result().StatusCode())
	}
	var u map[string]any
	json.Unmarshal(resp.Result().Body(), &u)
	if u["username"] != "alice" || u["role"] != "user" {
		t.Fatalf("user = %v", u)
	}
}

func TestUserCreateDuplicate(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	body := `{"username":"alice","password":"password123","role":"user"}`
	ut.PerformRequest(e, "POST", "/api/v1/users", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	resp := ut.PerformRequest(e, "POST", "/api/v1/users", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusConflict {
		t.Fatalf("duplicate: %d, want 409", resp.Result().StatusCode())
	}
}

func TestUserCreateValidation(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	cases := []string{
		`{"username":"a","password":"password123","role":"user"}`,      // short username
		`{"username":"alice","password":"short","role":"user"}`,        // short password
		`{"username":"alice","password":"password123","role":"super"}`, // bad role
	}
	for _, body := range cases {
		resp := ut.PerformRequest(e, "POST", "/api/v1/users", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Authorization", Value: bearer},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if resp.Result().StatusCode() != http.StatusBadRequest {
			t.Fatalf("validation: %d, want 400 (body=%s)", resp.Result().StatusCode(), body)
		}
	}
}

func TestUserGetAndUpdate(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	u, _ := env.users.Create(ctx, "alice", "password123", domain.RoleUser)

	resp := ut.PerformRequest(e, "GET", "/api/v1/users/"+u.ID, nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("get: %d", resp.Result().StatusCode())
	}

	body := `{"role":"admin"}`
	resp = ut.PerformRequest(e, "PUT", "/api/v1/users/"+u.ID, &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("update: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["role"] != "admin" {
		t.Fatalf("role = %v", out["role"])
	}
}

func TestUserGetMissing(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/users/nope", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("missing: %d, want 404", resp.Result().StatusCode())
	}
}

func TestUserDeleteSelf(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	admin, _ := env.users.GetByUsername(ctx, "admin")
	resp := ut.PerformRequest(e, "DELETE", "/api/v1/users/"+admin.ID, nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusConflict {
		t.Fatalf("self-delete: %d, want 409", resp.Result().StatusCode())
	}
}

func TestUserDelete(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	u, _ := env.users.Create(ctx, "alice", "password123", domain.RoleUser)
	resp := ut.PerformRequest(e, "DELETE", "/api/v1/users/"+u.ID, nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("delete: %d, want 204", resp.Result().StatusCode())
	}
}

func TestUserResetPassword(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	u, _ := env.users.Create(ctx, "alice", "password123", domain.RoleUser)
	body := `{"password":"newpassword123"}`
	resp := ut.PerformRequest(e, "PUT", "/api/v1/users/"+u.ID+"/password", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("reset pw: %d", resp.Result().StatusCode())
	}
}

func TestUserGroups(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	u, _ := env.users.Create(ctx, "alice", "password123", domain.RoleUser)
	g, _ := env.groups.Create(ctx, service.GroupInput{Name: "G1", AgentAvailable: true})

	body := `{"group_ids":["` + g.ID + `"]}`
	resp := ut.PerformRequest(e, "PUT", "/api/v1/users/"+u.ID+"/groups", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("set groups: %d", resp.Result().StatusCode())
	}
	var out []map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if len(out) != 1 || out[0]["id"] != g.ID {
		t.Fatalf("groups = %v", out)
	}
}

func TestUserTokenMetaAdmin(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	u, _ := env.users.Create(ctx, "alice", "password123", domain.RoleUser)
	env.tokens.Generate(ctx, u.ID)

	resp := ut.PerformRequest(e, "GET", "/api/v1/users/"+u.ID+"/token", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("token meta: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["prefix"] == nil || out["prefix"] == "" {
		t.Fatal("missing prefix")
	}
}

func TestUserTokenMetaMissing(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	u, _ := env.users.Create(ctx, "alice", "password123", domain.RoleUser)
	resp := ut.PerformRequest(e, "GET", "/api/v1/users/"+u.ID+"/token", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("token meta missing: %d, want 404", resp.Result().StatusCode())
	}
}

func TestUserTokenRevokeAdmin(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	u, _ := env.users.Create(ctx, "alice", "password123", domain.RoleUser)
	env.tokens.Generate(ctx, u.ID)
	resp := ut.PerformRequest(e, "DELETE", "/api/v1/users/"+u.ID+"/token", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp.Result().StatusCode())
	}
}

func TestUserStateLifecycle(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	bearer := env.loginAsAdmin(t)

	// Create a plain user to manage.
	body := `{"username":"bob","password":"password123","role":"user"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/users", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusCreated {
		t.Fatalf("create: %d", resp.Result().StatusCode())
	}
	var created map[string]any
	if err := json.Unmarshal(resp.Result().Body(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("created user has no id")
	}

	state := func(action string) (*ut.ResponseRecorder, map[string]any) {
		payload := `{"action":"` + action + `"}`
		r := ut.PerformRequest(e, "POST", "/api/v1/users/"+id+"/state",
			&ut.Body{Body: strings.NewReader(payload), Len: len(payload)},
			ut.Header{Key: "Authorization", Value: bearer},
			ut.Header{Key: "Content-Type", Value: "application/json; charset=utf-8"})
		var out map[string]any
		_ = json.Unmarshal(r.Result().Body(), &out)
		return r, out
	}

	// Disable -> disabled_at present.
	r, out := state("disable")
	if r.Result().StatusCode() != http.StatusOK {
		t.Fatalf("disable: %d", r.Result().StatusCode())
	}
	if out["disabled_at"] == nil {
		t.Fatalf("disable must set disabled_at: %v", out)
	}
	// Enable -> disabled_at absent.
	r, out = state("enable")
	if r.Result().StatusCode() != http.StatusOK || out["disabled_at"] != nil {
		t.Fatalf("enable: %d %v", r.Result().StatusCode(), out)
	}
	// Delete -> 204, tombstone listed with deleted_at.
	dr := ut.PerformRequest(e, "DELETE", "/api/v1/users/"+id, nil, ut.Header{Key: "Authorization", Value: bearer})
	if dr.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("delete: %d", dr.Result().StatusCode())
	}
	lr := ut.PerformRequest(e, "GET", "/api/v1/users", nil, ut.Header{Key: "Authorization", Value: bearer})
	var rows []map[string]any
	_ = json.Unmarshal(lr.Result().Body(), &rows)
	var tombstone map[string]any
	for _, row := range rows {
		if row["id"] == id {
			tombstone = row
		}
	}
	if tombstone == nil || tombstone["deleted_at"] == nil {
		t.Fatalf("deleted user must be listed as a tombstone: %v", rows)
	}
	// Enable on a tombstone -> 403 account blocked.
	r, _ = state("enable")
	if r.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("enable tombstone: %d, want 403", r.Result().StatusCode())
	}
	// Restore -> deleted_at absent, disabled_at present.
	r, out = state("restore")
	if r.Result().StatusCode() != http.StatusOK || out["deleted_at"] != nil || out["disabled_at"] == nil {
		t.Fatalf("restore: %d %v", r.Result().StatusCode(), out)
	}
}

func TestUserStateEndpointGuards(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	adminBearer := env.loginAsAdmin(t)

	admin, err := env.users.GetByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("load admin: %v", err)
	}

	// Self-disable is rejected (admin is the only admin).
	payload := `{"action":"disable"}`
	self := ut.PerformRequest(e, "POST", "/api/v1/users/"+admin.ID+"/state",
		&ut.Body{Body: strings.NewReader(payload), Len: len(payload)},
		ut.Header{Key: "Authorization", Value: adminBearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if self.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("self-disable: %d, want 403", self.Result().StatusCode())
	}

	// Wrong content type -> 415.
	other := ut.PerformRequest(e, "POST", "/api/v1/users/"+admin.ID+"/state",
		&ut.Body{Body: strings.NewReader(payload), Len: len(payload)},
		ut.Header{Key: "Authorization", Value: adminBearer},
		ut.Header{Key: "Content-Type", Value: "text/plain"})
	if other.Result().StatusCode() != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content type: %d, want 415", other.Result().StatusCode())
	}

	// Non-admin -> 403.
	userBearer, u := env.createUserAndToken(t)
	nonAdmin := ut.PerformRequest(e, "POST", "/api/v1/users/"+u.ID+"/state",
		&ut.Body{Body: strings.NewReader(payload), Len: len(payload)},
		ut.Header{Key: "Authorization", Value: userBearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if nonAdmin.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("non-admin state: %d, want 403", nonAdmin.Result().StatusCode())
	}

	// Unauthenticated -> 401.
	anon := ut.PerformRequest(e, "POST", "/api/v1/users/"+u.ID+"/state",
		&ut.Body{Body: strings.NewReader(payload), Len: len(payload)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if anon.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("anonymous state: %d, want 401", anon.Result().StatusCode())
	}
}
