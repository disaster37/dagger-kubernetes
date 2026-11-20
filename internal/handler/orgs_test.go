package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// newOrgMembersEngine registers the org membership routes used by these tests.
func newOrgMembersEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/api/v1/orgs/:org/members", s.handleOrgMembers)
	e.PUT("/api/v1/orgs/:org/members", s.handleOrgSetMember)
	e.NoRoute(s.handleNoRoute)
	return e
}

// orgMemberRowsFor decodes a members response and indexes it by user ID.
func orgMemberRowsFor(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode members: %v", err)
	}
	out := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		id, _ := row["user_id"].(string)
		out[id] = row
	}
	return out
}

// groupNames extracts the sorted group names from a member row.
func groupNames(t *testing.T, row map[string]any) []string {
	t.Helper()
	raw, ok := row["groups"].([]any)
	if !ok {
		t.Fatalf("groups = %v, want array", row["groups"])
	}
	names := make([]string, 0, len(raw))
	for _, g := range raw {
		group, ok := g.(map[string]any)
		if !ok {
			t.Fatalf("group = %v, want object", g)
		}
		name, _ := group["name"].(string)
		names = append(names, name)
	}
	return names
}

// TestOrgMembersListEnriched verifies the list carries the username and the
// user's org-scoped groups (sorted by name).
func TestOrgMembersListEnriched(t *testing.T) {
	env := newTestEnv(t)
	e := newOrgMembersEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	alice, err := env.users.Create(ctx, "alice", "password123", "user")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	beta, err := env.groups.Create(ctx, env.orgID, service.GroupInput{Name: "beta", AgentAvailable: true})
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}
	alpha, err := env.groups.Create(ctx, env.orgID, service.GroupInput{Name: "alpha", AgentAvailable: true})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if err := env.groups.SetMembers(ctx, env.orgID, beta.ID, []string{alice.ID}); err != nil {
		t.Fatalf("add alice to beta: %v", err)
	}
	if err := env.groups.SetMembers(ctx, env.orgID, alpha.ID, []string{alice.ID}); err != nil {
		t.Fatalf("add alice to alpha: %v", err)
	}
	if err := env.server.orgs.SetMember(ctx, env.orgID, alice.ID, domain.OrgMember); err != nil {
		t.Fatalf("set org member: %v", err)
	}

	resp := ut.PerformRequest(e, "GET", env.orgPath("/members"), nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("list members: %d", resp.Result().StatusCode())
	}
	rows := orgMemberRowsFor(t, resp.Result().Body())
	row, ok := rows[alice.ID]
	if !ok {
		t.Fatalf("alice missing from %v", rows)
	}
	if row["username"] != "alice" {
		t.Fatalf("username = %v, want alice", row["username"])
	}
	if row["role"] != string(domain.OrgMember) {
		t.Fatalf("role = %v", row["role"])
	}
	if row["org_id"] != env.orgID {
		t.Fatalf("org_id = %v, want %s", row["org_id"], env.orgID)
	}
	// created_at is the recorded membership time (never the zero year-1 value).
	createdAt, _ := row["created_at"].(string)
	if ts, err := time.Parse(time.RFC3339Nano, createdAt); err != nil || ts.IsZero() {
		t.Fatalf("created_at = %v, want a real timestamp (err=%v)", row["created_at"], err)
	}
	// Every user also lands in the bootstrap org's "default" group.
	names := groupNames(t, row)
	if len(names) != 3 || names[0] != "alpha" || names[1] != "beta" || names[2] != "default" {
		t.Fatalf("groups = %v, want [alpha beta default] (sorted)", names)
	}
}

// TestOrgMembersExcludesForeignOrgGroups verifies a membership's group list is
// scoped to the requested org: a group held only in another org is hidden.
func TestOrgMembersExcludesForeignOrgGroups(t *testing.T) {
	env := newTestEnv(t)
	e := newOrgMembersEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	alice, err := env.users.Create(ctx, "alice", "password123", "user")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	local, err := env.groups.Create(ctx, env.orgID, service.GroupInput{Name: "local", AgentAvailable: true})
	if err != nil {
		t.Fatalf("create local group: %v", err)
	}
	if err := env.groups.SetMembers(ctx, env.orgID, local.ID, []string{alice.ID}); err != nil {
		t.Fatalf("add alice to local: %v", err)
	}
	otherOrg, err := env.server.orgs.Create(ctx, service.OrgInput{Name: "acme", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("create second org: %v", err)
	}
	foreign, err := env.groups.Create(ctx, otherOrg.ID, service.GroupInput{Name: "foreign", AgentAvailable: true})
	if err != nil {
		t.Fatalf("create foreign group: %v", err)
	}
	if err := env.groups.SetMembers(ctx, otherOrg.ID, foreign.ID, []string{alice.ID}); err != nil {
		t.Fatalf("add alice to foreign: %v", err)
	}
	if err := env.server.orgs.SetMember(ctx, env.orgID, alice.ID, domain.OrgMember); err != nil {
		t.Fatalf("set org member: %v", err)
	}

	resp := ut.PerformRequest(e, "GET", env.orgPath("/members"), nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("list members: %d", resp.Result().StatusCode())
	}
	row, ok := orgMemberRowsFor(t, resp.Result().Body())[alice.ID]
	if !ok {
		t.Fatalf("alice missing from members")
	}
	names := groupNames(t, row)
	if len(names) != 2 || names[0] != "default" || names[1] != "local" {
		t.Fatalf("groups = %v, want [default local] (no foreign group)", names)
	}
	groups, _ := row["groups"].([]any)
	for _, g := range groups {
		group := g.(map[string]any)
		if group["org_id"] == otherOrg.ID {
			t.Fatalf("foreign org group leaked: %v", group)
		}
	}
}

// TestOrgSetMemberReturnsRow verifies PUT returns 200 with the enriched row
// (instead of the old empty 204).
func TestOrgSetMemberReturnsRow(t *testing.T) {
	env := newTestEnv(t)
	e := newOrgMembersEngine(env.server)
	ctx := context.Background()

	bearer := env.loginAsAdmin(t)
	alice, err := env.users.Create(ctx, "alice", "password123", "user")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	grp, err := env.groups.Create(ctx, env.orgID, service.GroupInput{Name: "devs", AgentAvailable: true})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := env.groups.SetMembers(ctx, env.orgID, grp.ID, []string{alice.ID}); err != nil {
		t.Fatalf("add alice to group: %v", err)
	}

	cases := []struct {
		name string
		body string
	}{
		{"by user id", `{"user_id":"` + alice.ID + `","role":"org_admin"}`},
		{"by username", `{"username":"alice","role":"member"}`},
	}
	var firstCreatedAt string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := ut.PerformRequest(e, "PUT", env.orgPath("/members"),
				&ut.Body{Body: strings.NewReader(tc.body), Len: len(tc.body)},
				ut.Header{Key: "Authorization", Value: bearer},
				ut.Header{Key: "Content-Type", Value: "application/json"})
			if resp.Result().StatusCode() != http.StatusOK {
				t.Fatalf("set member: %d, want 200", resp.Result().StatusCode())
			}
			var row map[string]any
			if err := json.Unmarshal(resp.Result().Body(), &row); err != nil {
				t.Fatalf("decode row: %v", err)
			}
			if row["user_id"] != alice.ID || row["username"] != "alice" {
				t.Fatalf("row user = %v / %v", row["user_id"], row["username"])
			}
			if row["org_id"] != env.orgID {
				t.Fatalf("org_id = %v, want %s", row["org_id"], env.orgID)
			}
			if names := groupNames(t, row); len(names) != 2 || names[0] != "default" || names[1] != "devs" {
				t.Fatalf("groups = %v, want [default devs]", names)
			}
			createdAt, _ := row["created_at"].(string)
			if ts, err := time.Parse(time.RFC3339Nano, createdAt); err != nil || ts.IsZero() {
				t.Fatalf("created_at = %v, want a real timestamp (err=%v)", row["created_at"], err)
			}
			// The second case is a role change: the original time must survive.
			if firstCreatedAt == "" {
				firstCreatedAt = createdAt
			} else if createdAt != firstCreatedAt {
				t.Fatalf("role change rewrote created_at: %v, want %v", createdAt, firstCreatedAt)
			}
		})
	}
}
