package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// handleConnectEnv returns the connection env-var snapshot for the caller.
// Auth-gated. ?reveal=true populates the DAGGER_CLOUD_TOKEN plaintext when the
// token is recoverable. The response is never cached (no-store).
func (s *Server) handleConnectEnv(_ context.Context, c *app.RequestContext) {
	if !s.requireAuth(c) {
		return
	}
	if s.connect == nil {
		writeError(c, consts.StatusInternalServerError, "connect env unavailable")
		return
	}
	id := identityOf(c)
	if id == nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}
	reveal := c.Query("reveal") == "true"
	version := c.Query("version")
	snap, err := s.connect.ConnectEnv(context.Background(), id.UserID, version, reveal)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	// Never cache a response that may contain the token plaintext.
	c.Response.Header.Set("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Response.Header.Set("Pragma", "no-cache")
	writeJSON(c, snap)
}
