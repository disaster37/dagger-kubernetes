package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// handleCIOIDC implements POST /v1/oidc?provider=github|gitlab.
func (s *Server) handleCIOIDC(ctx context.Context, c *app.RequestContext) {
	if s.ciOIDC == nil {
		writeError(c, consts.StatusNotFound, "ci oidc disabled")
		return
	}
	var req struct {
		Token   string `json:"token"`
		Project string `json:"project"`
		Org     string `json:"org"`
	}
	if !decodeBody(c, &req) {
		return
	}
	res, err := s.ciOIDC.Exchange(ctx, c.Query("provider"), req.Token, req.Org, req.Project)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, res)
}
