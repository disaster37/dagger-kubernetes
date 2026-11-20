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
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	env.groups.Create(ctx, env.orgID, service.GroupInput{Name: "G1", AgentAvailable: true, MaxRunnerSessions: 8})

	resp := ut.PerformRequest(e, "GET", env.orgPath("/groups"), nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("list: %d", resp.Result().StatusCode())
	}
	var rows []map[string]any
	json.Unmarshal(resp.Result().Body(), &rows)
	var g1 map[string]any
	for _, row := range rows {
		if row["name"] == "G1" {
			g1 = row
		}
	}
	if g1 == nil {
		t.Fatalf("rows = %v", rows)
	}
	if g1["max_runner_sessions"] != float64(8) {
		t.Fatalf("max = %v", g1["max_runner_sessions"])
	}
}

func TestGroupCreate(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	body := `{"name":"G1","description":"team","max_runner_sessions":4,"agent_available":true,"auto_assign_pattern":"^github.*"}`
	resp := ut.PerformRequest(e, "POST", env.orgPath("/groups"), &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusCreated {
		t.Fatalf("create: %d", resp.Result().StatusCode())
	}
}

func TestGroupCreateBadPattern(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	body := `{"name":"G1","auto_assign_pattern":"["}`
	resp := ut.PerformRequest(e, "POST", env.orgPath("/groups"), &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("bad pattern: %d, want 400", resp.Result().StatusCode())
	}
}

func TestGroupCreateDuplicate(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	env.groups.Create(ctx, env.orgID, service.GroupInput{Name: "G1", AgentAvailable: true})
	body := `{"name":"G1"}`
	resp := ut.PerformRequest(e, "POST", env.orgPath("/groups"), &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusConflict {
		t.Fatalf("duplicate: %d, want 409", resp.Result().StatusCode())
	}
}

func TestGroupGetUpdateDelete(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	g, _ := env.groups.Create(ctx, env.orgID, service.GroupInput{Name: "G1", AgentAvailable: true, MaxRunnerSessions: 4})

	resp := ut.PerformRequest(e, "GET", env.orgPath("/groups/%s", g.ID), nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("get: %d", resp.Result().StatusCode())
	}

	body := `{"name":"G2","max_runner_sessions":8,"agent_available":false}`
	resp = ut.PerformRequest(e, "PUT", env.orgPath("/groups/%s", g.ID), &ut.Body{Body: strings.NewReader(body), Len: len(body)},
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

	resp = ut.PerformRequest(e, "DELETE", env.orgPath("/groups/%s", g.ID), nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.Result().StatusCode())
	}
}

func TestGroupMembers(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	g, _ := env.groups.Create(ctx, env.orgID, service.GroupInput{Name: "G1", AgentAvailable: true})
	u, _ := env.users.Create(ctx, "alice", "password123", "user")

	body := `{"user_ids":["` + u.ID + `"]}`
	resp := ut.PerformRequest(e, "PUT", env.orgPath("/groups/%s/members", g.ID), &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("set members: %d", resp.Result().StatusCode())
	}

	resp = ut.PerformRequest(e, "GET", env.orgPath("/groups/%s/members", g.ID), nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("members: %d", resp.Result().StatusCode())
	}
	var rows []map[string]any
	json.Unmarshal(resp.Result().Body(), &rows)
	if len(rows) != 1 || rows[0]["username"] != "alice" {
		t.Fatalf("members = %v", rows)
	}
}
