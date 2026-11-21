package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// handleCloudGraphQL implements POST /query using graphql-go's schema.Exec
// directly from the Hertz request body (no net/http in this package).
func (s *Server) handleCloudGraphQL(ctx context.Context, c *app.RequestContext) {
	if s.cloudGraphQL == nil {
		writeError(c, consts.StatusNotFound, "graphql disabled")
		return
	}
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	var req struct {
		Query         string         `json:"query"`
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if !decodeBody(c, &req) {
		return
	}
	execCtx := repository.WithGraphQLIdentity(ctx, repository.GraphQLIdentity{UserID: id.UserID, OrgID: id.OrgID})
	out := s.cloudGraphQL.Exec(execCtx, req.Query, req.OperationName, req.Variables)
	c.Response.Header.SetContentTypeBytes([]byte("application/json"))
	c.SetStatusCode(consts.StatusOK)
	c.Response.SetBody(out)
}
