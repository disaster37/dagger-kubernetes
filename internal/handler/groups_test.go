package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/service"
)

func TestGroupsList(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	env.groups.Create(ctx, service.GroupInput{Name: "G1", AgentAvailable: true, MaxRunnerSessions: 8})

	resp := ut.PerformRequest(e, "GET", "/api/v1/groups", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("list: %d", resp.Result().StatusCode())
	}
	var rows []map[string]any
	json.Unmarshal(resp.Result().Body(), &rows)
	if len(rows) != 1 || rows[0]["name"] != "G1" {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0]["max_runner_sessions"] != float64(8) {
		t.Fatalf("max = %v", rows[0]["max_runner_sessions"])
	}
}

func TestGroupCreate(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	body := `{"name":"G1","description":"team","max_runner_sessions":4,"agent_available":true,"auto_assign_pattern":"^github.*"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/groups", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusCreated {
		t.Fatalf("create: %d", resp.Result().StatusCode())
	}
}

func TestGroupCreateBadPattern(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	body := `{"name":"G1","auto_assign_pattern":"["}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/groups", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("bad pattern: %d, want 400", resp.Result().StatusCode())
	}
}

func TestGroupCreateDuplicate(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	env.groups.Create(ctx, service.GroupInput{Name: "G1", AgentAvailable: true})
	body := `{"name":"G1"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/groups", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusConflict {
		t.Fatalf("duplicate: %d, want 409", resp.Result().StatusCode())
	}
}

func TestGroupGetUpdateDelete(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	g, _ := env.groups.Create(ctx, service.GroupInput{Name: "G1", AgentAvailable: true, MaxRunnerSessions: 4})

	resp := ut.PerformRequest(e, "GET", "/api/v1/groups/"+g.ID, nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("get: %d", resp.Result().StatusCode())
	}

	body := `{"name":"G2","max_runner_sessions":8,"agent_available":false}`
	resp = ut.PerformRequest(e, "PUT", "/api/v1/groups/"+g.ID, &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("update: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["name"] != "G2" || out["agent_available"] != false {
		t.Fatalf("group = %v", out)
	}

	resp = ut.PerformRequest(e, "DELETE", "/api/v1/groups/"+g.ID, nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.Result().StatusCode())
	}
}

func TestGroupMembers(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	g, _ := env.groups.Create(ctx, service.GroupInput{Name: "G1", AgentAvailable: true})
	u, _ := env.users.Create(ctx, "alice", "password123", "user")

	body := `{"user_ids":["` + u.ID + `"]}`
	resp := ut.PerformRequest(e, "PUT", "/api/v1/groups/"+g.ID+"/members", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("set members: %d", resp.Result().StatusCode())
	}

	resp = ut.PerformRequest(e, "GET", "/api/v1/groups/"+g.ID+"/members", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("members: %d", resp.Result().StatusCode())
	}
	var rows []map[string]any
	json.Unmarshal(resp.Result().Body(), &rows)
	if len(rows) != 1 || rows[0]["username"] != "alice" {
		t.Fatalf("members = %v", rows)
	}
}
