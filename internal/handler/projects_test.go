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

func TestProjectsList(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	env.projects.Create(ctx, "github.com/acme/api", "")

	resp := ut.PerformRequest(e, "GET", "/api/v1/projects", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("list: %d", resp.Result().StatusCode())
	}
	var rows []map[string]any
	json.Unmarshal(resp.Result().Body(), &rows)
	if len(rows) != 1 || rows[0]["name"] != "github.com/acme/api" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestProjectCreate(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	body := `{"name":"github.com/acme/api","group_id":""}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/projects", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusCreated {
		t.Fatalf("create: %d", resp.Result().StatusCode())
	}
}

func TestProjectCreateWithGroup(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	g, _ := env.groups.Create(ctx, service.GroupInput{Name: "G1", AgentAvailable: true})
	body := `{"name":"github.com/acme/api","group_id":"` + g.ID + `"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/projects", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusCreated {
		t.Fatalf("create with group: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["group_id"] != g.ID {
		t.Fatalf("group_id = %v", out["group_id"])
	}
}

func TestProjectUpdateAssign(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	g, _ := env.groups.Create(ctx, service.GroupInput{Name: "G1", AgentAvailable: true})
	p, _ := env.projects.Create(ctx, "github.com/acme/api", "")

	body := `{"group_id":"` + g.ID + `"}`
	resp := ut.PerformRequest(e, "PUT", "/api/v1/projects/"+p.ID, &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("update: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["group_id"] != g.ID {
		t.Fatalf("group_id = %v", out["group_id"])
	}

	// Unassign.
	body = `{"group_id":""}`
	resp = ut.PerformRequest(e, "PUT", "/api/v1/projects/"+p.ID, &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("unassign: %d", resp.Result().StatusCode())
	}
	json.Unmarshal(resp.Result().Body(), &out)
	if out["group_id"] != "" {
		t.Fatalf("group_id = %v, want empty", out["group_id"])
	}
}

func TestProjectDelete(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	p, _ := env.projects.Create(ctx, "github.com/acme/api", "")
	resp := ut.PerformRequest(e, "DELETE", "/api/v1/projects/"+p.ID, nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.Result().StatusCode())
	}
}
