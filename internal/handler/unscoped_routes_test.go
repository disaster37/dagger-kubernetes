package handler

import (
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
)

// TestUnscopedGroupProjectRoutesNotFound guards the permanent removal of the
// unscoped /api/v1/groups and /api/v1/projects routes (plan Revision 2 decision
// 3): a route table that mirrors production's org-scoped-only registration must
// 404 every legacy unscoped path rather than alias it into the bootstrap org.
func TestUnscopedGroupProjectRoutesNotFound(t *testing.T) {
	env := newTestEnv(t)
	e := route.NewEngine(config.NewOptions(nil))
	// Mirror production: only org-scoped group/project routes exist.
	e.GET("/api/v1/orgs/:org/groups", env.server.handleGroupsList)
	e.POST("/api/v1/orgs/:org/groups", env.server.handleGroupCreate)
	e.GET("/api/v1/orgs/:org/projects", env.server.handleProjectsList)
	e.POST("/api/v1/orgs/:org/projects", env.server.handleProjectCreate)
	e.NoRoute(env.server.handleNoRoute)

	for _, path := range []string{
		"/api/v1/groups",
		"/api/v1/projects",
	} {
		for _, method := range []string{"GET", "POST"} {
			resp := ut.PerformRequest(e, method, path, nil)
			if got := resp.Result().StatusCode(); got != http.StatusNotFound {
				t.Fatalf("%s %s = %d, want 404", method, path, got)
			}
		}
	}
}
