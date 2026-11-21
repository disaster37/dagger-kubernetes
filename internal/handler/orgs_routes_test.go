package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// newOrgRoutesEngine registers every organization route served by orgs.go.
func newOrgRoutesEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/api/v1/orgs", s.handleOrgsList)
	e.POST("/api/v1/orgs", s.handleOrgCreate)
	e.GET("/api/v1/orgs/:org", s.handleOrgGet)
	e.PUT("/api/v1/orgs/:org", s.handleOrgUpdate)
	e.DELETE("/api/v1/orgs/:org", s.handleOrgDelete)
	e.GET("/api/v1/orgs/:org/members", s.handleOrgMembers)
	e.PUT("/api/v1/orgs/:org/members", s.handleOrgSetMember)
	e.DELETE("/api/v1/orgs/:org/members/:userID", s.handleOrgRemoveMember)
	e.PUT("/api/v1/orgs/:org/members/:userID/groups", s.handleOrgMemberGroups)
	e.GET("/api/v1/orgs/:org/quotas", s.handleOrgQuotasGet)
	e.PUT("/api/v1/orgs/:org/quotas", s.handleOrgQuotasPut)
	e.GET("/api/v1/orgs/:org/token", s.handleOrgTokenMeta)
	e.POST("/api/v1/orgs/:org/token", s.handleOrgTokenGenerate)
	e.DELETE("/api/v1/orgs/:org/token", s.handleOrgTokenRevoke)
	e.GET("/api/v1/orgs/:org/environments", s.handleEnvironmentsList)
	e.PUT("/api/v1/orgs/:org/environments/:env", s.handleEnvironmentPut)
	e.DELETE("/api/v1/orgs/:org/environments/:env", s.handleEnvironmentDelete)
	e.GET("/api/v1/orgs/:org/environments/:env/secrets", s.handleEnvSecretsList)
	e.PUT("/api/v1/orgs/:org/environments/:env/secrets", s.handleEnvSecretPut)
	e.DELETE("/api/v1/orgs/:org/environments/:env/secrets/:name", s.handleEnvSecretDelete)
	e.NoRoute(s.handleNoRoute)
	return e
}

// wireOrgContentServices attaches the environment/secret services used by the
// org routes (the base test env leaves them nil).
func wireOrgContentServices(env *testEnv) {
	env.server.environments = service.NewEnvironmentService(repository.NewEnvironmentRepo(env.store), observ.NewTestLogger())
	env.server.secrets = service.NewSecretService(repository.NewSecretRepo(env.store), testTokenEncKey(), observ.NewTestLogger())
}

func orgReq(t *testing.T, e *route.Engine, method, path, bearer, body string) (status int, responseBody []byte) {
	t.Helper()
	opts := []ut.Header{{Key: "Authorization", Value: bearer}}
	if body != "" {
		opts = append(opts, ut.Header{Key: "Content-Type", Value: "application/json"})
	}
	resp := ut.PerformRequest(e, method, path,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)}, opts...)
	return resp.Result().StatusCode(), resp.Result().Body()
}

func TestOrgRoutesListAndGet(t *testing.T) {
	env := newTestEnv(t)
	e := newOrgRoutesEngine(env.server)
	admin := env.loginAsAdmin(t)

	status, body := orgReq(t, e, "GET", "/api/v1/orgs", admin, "")
	if status != http.StatusOK {
		t.Fatalf("list = %d", status)
	}
	var orgs []map[string]any
	if err := json.Unmarshal(body, &orgs); err != nil || len(orgs) != 1 {
		t.Fatalf("orgs = %s err=%v", body, err)
	}
	if orgs[0]["id"] != env.orgID {
		t.Fatalf("org id = %v, want %s", orgs[0]["id"], env.orgID)
	}

	if status, _ := orgReq(t, e, "GET", "/api/v1/orgs/"+env.orgID, admin, ""); status != http.StatusOK {
		t.Fatalf("get by id = %d", status)
	}
	if status, _ := orgReq(t, e, "GET", "/api/v1/orgs/default", admin, ""); status != http.StatusOK {
		t.Fatalf("get by slug = %d", status)
	}
	if status, _ := orgReq(t, e, "GET", "/api/v1/orgs/missing", admin, ""); status != http.StatusNotFound {
		t.Fatalf("get missing = %d, want 404", status)
	}

	// Non-admin with no membership sees an empty list and cannot read the org.
	ctx := context.Background()
	userBearer, alice := env.createUserAndToken(t)
	if err := env.server.orgs.RemoveMember(ctx, env.orgID, alice.ID); err != nil {
		t.Fatal(err)
	}
	status, body = orgReq(t, e, "GET", "/api/v1/orgs", userBearer, "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("non-admin list = %d %s", status, body)
	}
	if status, _ := orgReq(t, e, "GET", "/api/v1/orgs/"+env.orgID, userBearer, ""); status != http.StatusForbidden {
		t.Fatalf("foreign get = %d, want 403", status)
	}

	t.Run("nil org service", func(t *testing.T) {
		env.server.orgs = nil
		t.Cleanup(func() { env.server.orgs = envServerOrgs(env) })
		if status, _ := orgReq(t, e, "GET", "/api/v1/orgs", admin, ""); status != http.StatusOK {
			t.Fatalf("nil list = %d", status)
		}
		if status, _ := orgReq(t, e, "GET", "/api/v1/orgs/x", admin, ""); status != http.StatusNotFound {
			t.Fatalf("nil get = %d", status)
		}
	})
}

// envServerOrgs rebuilds the org service from the live store (used to restore a
// temporarily-nil dependency).
func envServerOrgs(env *testEnv) *service.OrgService {
	svc := service.NewOrgService(repository.NewOrgRepo(env.store), repository.NewGroupRepo(env.store), env.tokens, env.server.logger)
	return svc
}

func TestOrgRoutesCreateUpdateDelete(t *testing.T) {
	env := newTestEnv(t)
	e := newOrgRoutesEngine(env.server)
	admin := env.loginAsAdmin(t)
	userBearer, _ := env.createUserAndToken(t)

	if status, _ := orgReq(t, e, "POST", "/api/v1/orgs", userBearer, `{"name":"acme"}`); status != http.StatusForbidden {
		t.Fatalf("non-admin create = %d, want 403", status)
	}
	if status, _ := orgReq(t, e, "POST", "/api/v1/orgs", admin, `{bad`); status != http.StatusBadRequest {
		t.Fatalf("bad body create = %d, want 400", status)
	}
	status, body := orgReq(t, e, "POST", "/api/v1/orgs", admin, `{"name":"acme","display_name":"Acme"}`)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	acmeID, _ := created["id"].(string)
	if acmeID == "" {
		t.Fatalf("created org = %s", body)
	}
	if status, _ := orgReq(t, e, "POST", "/api/v1/orgs", admin, `{"name":"acme"}`); status != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", status)
	}

	if status, _ := orgReq(t, e, "PUT", "/api/v1/orgs/"+acmeID, admin, `{bad`); status != http.StatusBadRequest {
		t.Fatalf("bad body update = %d", status)
	}
	status, body = orgReq(t, e, "PUT", "/api/v1/orgs/"+acmeID, admin, `{"display_name":"Acme Corp"}`)
	if status != http.StatusOK {
		t.Fatalf("update = %d %s", status, body)
	}
	var updated map[string]any
	_ = json.Unmarshal(body, &updated)
	if updated["display_name"] != "Acme Corp" {
		t.Fatalf("display_name = %v", updated["display_name"])
	}
	if status, _ := orgReq(t, e, "PUT", "/api/v1/orgs/"+acmeID, admin, `{"name":"other"}`); status != http.StatusBadRequest {
		t.Fatalf("rename = %d, want 400", status)
	}

	if status, _ := orgReq(t, e, "DELETE", "/api/v1/orgs/"+env.orgID, admin, ""); status != http.StatusForbidden {
		t.Fatalf("delete bootstrap = %d, want 403", status)
	}
	if status, _ := orgReq(t, e, "DELETE", "/api/v1/orgs/"+acmeID+"?force=true", admin, ""); status != http.StatusNoContent {
		t.Fatalf("force delete = %d, want 204", status)
	}

	t.Run("nil org service", func(t *testing.T) {
		env.server.orgs = nil
		t.Cleanup(func() { env.server.orgs = envServerOrgs(env) })
		if status, _ := orgReq(t, e, "POST", "/api/v1/orgs", admin, `{"name":"acme"}`); status != http.StatusServiceUnavailable {
			t.Fatalf("nil create = %d", status)
		}
		if status, _ := orgReq(t, e, "DELETE", "/api/v1/orgs/x", admin, ""); status != http.StatusServiceUnavailable {
			t.Fatalf("nil delete = %d", status)
		}
	})
}

func TestOrgRoutesMembers(t *testing.T) {
	env := newTestEnv(t)
	e := newOrgRoutesEngine(env.server)
	admin := env.loginAsAdmin(t)
	ctx := context.Background()
	userBearer, alice := env.createUserAndToken(t)
	// New users are auto-assigned to the bootstrap default group (which implies
	// org membership); drop it so this user starts outside the org.
	if err := env.server.orgs.RemoveMember(ctx, env.orgID, alice.ID); err != nil {
		t.Fatal(err)
	}
	grp, err := env.groups.Create(ctx, env.orgID, service.GroupInput{Name: "devs", AgentAvailable: true})
	if err != nil {
		t.Fatal(err)
	}

	if status, _ := orgReq(t, e, "GET", env.orgPath("/members"), userBearer, ""); status != http.StatusForbidden {
		t.Fatalf("foreign members = %d, want 403", status)
	}
	if status, _ := orgReq(t, e, "GET", "/api/v1/orgs/"+env.orgID+"/members", admin, ""); status != http.StatusOK {
		t.Fatalf("members = %d", status)
	}
	if err := env.server.orgs.SetMember(ctx, env.orgID, alice.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}

	if status, _ := orgReq(t, e, "PUT", env.orgPath("/members"), admin, `{bad`); status != http.StatusBadRequest {
		t.Fatalf("bad body set member = %d", status)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/members"), admin, `{"role":"member"}`); status != http.StatusBadRequest {
		t.Fatalf("missing user = %d, want 400", status)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/members"), admin, `{"username":"ghost","role":"member"}`); status != http.StatusNotFound {
		t.Fatalf("unknown username = %d, want 404", status)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/members"), admin, `{"user_id":"`+alice.ID+`","role":"bogus"}`); status != http.StatusBadRequest {
		t.Fatalf("invalid role = %d, want 400", status)
	}

	status, body := orgReq(t, e, "PUT", env.orgPath("/members/%s/groups", alice.ID), admin, `{"group_ids":["`+grp.ID+`"]}`)
	if status != http.StatusOK {
		t.Fatalf("set groups = %d %s", status, body)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/members/%s/groups", alice.ID), admin, `{bad`); status != http.StatusBadRequest {
		t.Fatalf("bad body set groups = %d", status)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/members/ghost/groups"), admin, `{"group_ids":[]}`); status != http.StatusNotFound {
		t.Fatalf("unknown user groups = %d, want 404", status)
	}

	// An org_admin (not a global admin) may also manage members.
	if err := env.server.orgs.SetMember(ctx, env.orgID, alice.ID, domain.OrgAdmin); err != nil {
		t.Fatal(err)
	}
	if status, _ := orgReq(t, e, "DELETE", env.orgPath("/members/%s", alice.ID), userBearer, ""); status != http.StatusNoContent {
		t.Fatalf("org admin remove member = %d, want 204", status)
	}
	if status, _ := orgReq(t, e, "DELETE", env.orgPath("/members/%s", alice.ID), admin, ""); status != http.StatusNoContent {
		t.Fatalf("remove member = %d", status)
	}

	t.Run("nil org service", func(t *testing.T) {
		env.server.orgs = nil
		t.Cleanup(func() { env.server.orgs = envServerOrgs(env) })
		if status, _ := orgReq(t, e, "GET", env.orgPath("/members"), admin, ""); status != http.StatusOK {
			t.Fatalf("nil members = %d", status)
		}
		if status, _ := orgReq(t, e, "PUT", env.orgPath("/members"), admin, `{"user_id":"u"}`); status != http.StatusServiceUnavailable {
			t.Fatalf("nil set member = %d", status)
		}
		if status, _ := orgReq(t, e, "DELETE", env.orgPath("/members/x"), admin, ""); status != http.StatusServiceUnavailable {
			t.Fatalf("nil remove member = %d", status)
		}
	})
}

func TestOrgRoutesQuotas(t *testing.T) {
	env := newTestEnv(t)
	e := newOrgRoutesEngine(env.server)
	admin := env.loginAsAdmin(t)

	if status, _ := orgReq(t, e, "GET", env.orgPath("/quotas"), admin, ""); status != http.StatusOK {
		t.Fatalf("quotas get = %d", status)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/quotas"), admin, `{bad`); status != http.StatusBadRequest {
		t.Fatalf("bad body quotas = %d", status)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/quotas"), admin, `{"max_groups":{"soft":5,"hard":1}}`); status != http.StatusBadRequest {
		t.Fatalf("invalid quotas = %d, want 400", status)
	}
	status, body := orgReq(t, e, "PUT", env.orgPath("/quotas"), admin, `{"max_groups":{"soft":1,"hard":2}}`)
	if status != http.StatusOK {
		t.Fatalf("quotas put = %d %s", status, body)
	}
	var usage map[string]any
	if err := json.Unmarshal(body, &usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	eff, _ := usage["effective"].(map[string]any)
	maxGroups, _ := eff["max_groups"].(map[string]any)
	if maxGroups["hard"] != float64(2) {
		t.Fatalf("effective = %v", eff)
	}
}

func TestOrgRoutesTokens(t *testing.T) {
	env := newTestEnv(t)
	e := newOrgRoutesEngine(env.server)
	admin := env.loginAsAdmin(t)

	status, body := orgReq(t, e, "GET", env.orgPath("/token"), admin, "")
	if status != http.StatusOK || !strings.Contains(string(body), `"exists":false`) {
		t.Fatalf("token meta before = %d %s", status, body)
	}
	if status, _ := orgReq(t, e, "GET", env.orgPath("/token"), "", ""); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated token meta = %d", status)
	}
	status, body = orgReq(t, e, "POST", env.orgPath("/token"), admin, "")
	if status != http.StatusCreated || !strings.Contains(string(body), `"token"`) {
		t.Fatalf("token generate = %d %s", status, body)
	}
	status, body = orgReq(t, e, "GET", env.orgPath("/token"), admin, "")
	if status != http.StatusOK || !strings.Contains(string(body), `"prefix"`) {
		t.Fatalf("token meta after = %d %s", status, body)
	}
	if status, _ := orgReq(t, e, "DELETE", env.orgPath("/token"), admin, ""); status != http.StatusNoContent {
		t.Fatalf("token revoke = %d", status)
	}

	t.Run("nil org service", func(t *testing.T) {
		env.server.orgs = nil
		t.Cleanup(func() { env.server.orgs = envServerOrgs(env) })
		if status, _ := orgReq(t, e, "GET", env.orgPath("/token"), admin, ""); status != http.StatusNotFound {
			t.Fatalf("nil token meta = %d", status)
		}
		if status, _ := orgReq(t, e, "POST", env.orgPath("/token"), admin, ""); status != http.StatusServiceUnavailable {
			t.Fatalf("nil token generate = %d", status)
		}
		if status, _ := orgReq(t, e, "DELETE", env.orgPath("/token"), admin, ""); status != http.StatusServiceUnavailable {
			t.Fatalf("nil token revoke = %d", status)
		}
	})
}

func TestOrgRoutesEnvironmentsAndSecrets(t *testing.T) {
	env := newTestEnv(t)
	wireOrgContentServices(env)
	e := newOrgRoutesEngine(env.server)
	admin := env.loginAsAdmin(t)
	userBearer, alice := env.createUserAndToken(t)
	if err := env.server.orgs.RemoveMember(context.Background(), env.orgID, alice.ID); err != nil {
		t.Fatal(err)
	}

	if status, _ := orgReq(t, e, "GET", env.orgPath("/environments"), userBearer, ""); status != http.StatusForbidden {
		t.Fatalf("foreign environments = %d, want 403", status)
	}
	status, body := orgReq(t, e, "GET", env.orgPath("/environments"), admin, "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("environments = %d %s", status, body)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/environments/prod"), admin, `{bad`); status != http.StatusBadRequest {
		t.Fatalf("bad body env = %d", status)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/environments/prod"), admin, `{"approval_timeout":"nope"}`); status != http.StatusBadRequest {
		t.Fatalf("invalid timeout = %d", status)
	}
	status, body = orgReq(t, e, "PUT", env.orgPath("/environments/prod"), admin, `{"approver_group_ids":[],"approval_timeout":"1h"}`)
	if status != http.StatusOK {
		t.Fatalf("put env = %d %s", status, body)
	}
	status, body = orgReq(t, e, "GET", env.orgPath("/environments"), admin, "")
	if status != http.StatusOK || !strings.Contains(string(body), `"prod"`) {
		t.Fatalf("list env = %d %s", status, body)
	}

	status, body = orgReq(t, e, "GET", env.orgPath("/environments/prod/secrets"), admin, "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("env secrets = %d %s", status, body)
	}
	if status, _ := orgReq(t, e, "PUT", env.orgPath("/environments/prod/secrets"), admin, `{bad`); status != http.StatusBadRequest {
		t.Fatalf("bad body env secret = %d", status)
	}
	status, body = orgReq(t, e, "PUT", env.orgPath("/environments/prod/secrets"), admin, `{"name":"TOKEN","value":"s3cr3t"}`)
	if status != http.StatusCreated {
		t.Fatalf("put env secret = %d %s", status, body)
	}
	if strings.Contains(string(body), "s3cr3t") {
		t.Fatalf("secret value leaked: %s", body)
	}
	if status, _ := orgReq(t, e, "DELETE", env.orgPath("/environments/prod/secrets/TOKEN"), admin, ""); status != http.StatusNoContent {
		t.Fatalf("delete env secret = %d", status)
	}
	if status, _ := orgReq(t, e, "DELETE", env.orgPath("/environments/prod"), admin, ""); status != http.StatusNoContent {
		t.Fatalf("delete env = %d", status)
	}

	t.Run("nil services", func(t *testing.T) {
		env.server.environments = nil
		env.server.secrets = nil
		t.Cleanup(func() { wireOrgContentServices(env) })
		if status, _ := orgReq(t, e, "GET", env.orgPath("/environments"), admin, ""); status != http.StatusOK {
			t.Fatalf("nil env list = %d", status)
		}
		if status, _ := orgReq(t, e, "PUT", env.orgPath("/environments/prod"), admin, `{}`); status != http.StatusServiceUnavailable {
			t.Fatalf("nil env put = %d", status)
		}
		if status, _ := orgReq(t, e, "DELETE", env.orgPath("/environments/prod"), admin, ""); status != http.StatusServiceUnavailable {
			t.Fatalf("nil env delete = %d", status)
		}
		if status, _ := orgReq(t, e, "GET", env.orgPath("/environments/prod/secrets"), admin, ""); status != http.StatusOK {
			t.Fatalf("nil secret list = %d", status)
		}
		if status, _ := orgReq(t, e, "PUT", env.orgPath("/environments/prod/secrets"), admin, `{}`); status != http.StatusServiceUnavailable {
			t.Fatalf("nil secret put = %d", status)
		}
		if status, _ := orgReq(t, e, "DELETE", env.orgPath("/environments/prod/secrets/x"), admin, ""); status != http.StatusServiceUnavailable {
			t.Fatalf("nil secret delete = %d", status)
		}
	})
}

// TestOrgRoutesForbiddenAndErrorPaths covers the authorization failures and
// lookup errors that the happy-path routes do not reach.
func TestOrgRoutesForbiddenAndErrorPaths(t *testing.T) {
	env := newTestEnv(t)
	wireOrgContentServices(env)
	e := newOrgRoutesEngine(env.server)
	admin := env.loginAsAdmin(t)
	ctx := context.Background()
	userBearer, alice := env.createUserAndToken(t)
	if err := env.server.orgs.RemoveMember(ctx, env.orgID, alice.ID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		method string
		path   string
		bearer string
		body   string
		want   int
	}{
		{"members list forbidden", "GET", env.orgPath("/members"), userBearer, "", http.StatusForbidden},
		{"member groups forbidden", "PUT", env.orgPath("/members/%s/groups", alice.ID), userBearer, `{"group_ids":[]}`, http.StatusForbidden},
		{"set member forbidden", "PUT", env.orgPath("/members"), userBearer, `{"user_id":"x"}`, http.StatusForbidden},
		{"remove member forbidden", "DELETE", env.orgPath("/members/%s", alice.ID), userBearer, "", http.StatusForbidden},
		{"quotas get forbidden", "GET", env.orgPath("/quotas"), userBearer, "", http.StatusForbidden},
		{"quotas put missing org", "PUT", "/api/v1/orgs/missing/quotas", admin, `{}`, http.StatusNotFound},
		{"token meta forbidden", "GET", env.orgPath("/token"), userBearer, "", http.StatusForbidden},
		{"token generate forbidden", "POST", env.orgPath("/token"), userBearer, "", http.StatusForbidden},
		{"token revoke forbidden", "DELETE", env.orgPath("/token"), userBearer, "", http.StatusForbidden},
		{"env list forbidden", "GET", env.orgPath("/environments"), userBearer, "", http.StatusForbidden},
		{"env put forbidden", "PUT", env.orgPath("/environments/prod"), userBearer, `{}`, http.StatusForbidden},
		{"env delete forbidden", "DELETE", env.orgPath("/environments/prod"), userBearer, "", http.StatusForbidden},
		{"env secret list forbidden", "GET", env.orgPath("/environments/prod/secrets"), userBearer, "", http.StatusForbidden},
		{"env secret put forbidden", "PUT", env.orgPath("/environments/prod/secrets"), userBearer, `{"name":"X","value":"Y"}`, http.StatusForbidden},
		{"env secret delete forbidden", "DELETE", env.orgPath("/environments/prod/secrets/X"), userBearer, "", http.StatusForbidden},
		{"unauthenticated set member", "PUT", env.orgPath("/members"), "", `{"user_id":"x"}`, http.StatusUnauthorized},
		{"unauthenticated member list", "GET", env.orgPath("/members"), "", "", http.StatusUnauthorized},
		{"unauthenticated env list", "GET", env.orgPath("/environments"), "", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := orgReq(t, e, tc.method, tc.path, tc.bearer, tc.body)
			if status != tc.want {
				t.Fatalf("%s %s = %d (%s), want %d", tc.method, tc.path, status, body, tc.want)
			}
		})
	}

	t.Run("remove unknown member is idempotent", func(t *testing.T) {
		if status, _ := orgReq(t, e, "DELETE", env.orgPath("/members/ghost"), admin, ""); status != http.StatusNoContent {
			t.Fatalf("remove ghost = %d, want 204", status)
		}
	})

	t.Run("nil members are skipped", func(t *testing.T) {
		rows := env.server.orgMemberRows(context.Background(), env.orgID, []*domain.OrgMembership{nil})
		if len(rows) != 0 {
			t.Fatalf("rows = %+v, want none", rows)
		}
	})
}
