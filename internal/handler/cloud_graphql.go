package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/sirupsen/logrus"

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
	started := time.Now()
	out := s.cloudGraphQL.Exec(execCtx, req.Query, req.OperationName, req.Variables)
	if s.cfg != nil && s.cfg.GraphQLLogQueries {
		// Never log the query text: it can contain repository names and tokens.
		// The hash plus the byte length identify a repeated query across a
		// capture session.
		s.logger.WithFields(logrus.Fields{
			"operation_name": req.OperationName,
			"query_bytes":    len(req.Query),
			"query_sha256":   fmt.Sprintf("%x", sha256.Sum256([]byte(req.Query))),
			"exec_ms":        time.Since(started).Milliseconds(),
			"errors":         graphqlResponseHasErrors(out),
		}).Debug("cloud graphql query")
	}
	c.Response.Header.SetContentTypeBytes([]byte("application/json"))
	c.SetStatusCode(consts.StatusOK)
	c.Response.SetBody(out)
}

// graphqlResponseHasErrors reports whether a GraphQL response body carries an
// "errors" member (a validation or resolver failure).
func graphqlResponseHasErrors(body []byte) bool {
	var resp struct {
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return true
	}
	return len(resp.Errors) > 0
}
