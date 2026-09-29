package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// myTokenResponse carries the one-time plaintext token.
type myTokenResponse struct {
	Token string `json:"token,omitempty"`
}

// handleMyTokenMeta returns the caller's masked token metadata (404 if none).
func (s *Server) handleMyTokenMeta(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	t, err := s.tokens.Meta(ctx, id.UserID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, toTokenMeta(t))
}

// handleMyTokenCreate issues a new token for the caller (409 if one exists).
func (s *Server) handleMyTokenCreate(ctx context.Context, c *app.RequestContext) {
	s.issueMyToken(ctx, c, consts.StatusCreated, s.tokens.Generate)
}

// handleMyTokenRegenerate replaces the caller's token (old token invalid
// immediately) and returns the new plaintext.
func (s *Server) handleMyTokenRegenerate(ctx context.Context, c *app.RequestContext) {
	s.issueMyToken(ctx, c, consts.StatusOK, s.tokens.Regenerate)
}

// issueMyToken runs generate (create or regenerate) for the caller and writes
// the one-time plaintext with the given success status.
func (s *Server) issueMyToken(ctx context.Context, c *app.RequestContext, status int, generate func(ctx context.Context, userID string) (string, *domain.APIToken, error)) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	// Synthetic identities (legacy flat-file) have no users-table row; a token
	// row would violate the user_id foreign key.
	if id.Method == domain.AuthLegacyTok {
		writeError(c, consts.StatusBadRequest, "api tokens require a real user account")
		return
	}
	plaintext, _, err := generate(ctx, id.UserID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(status, myTokenResponse{Token: plaintext})
}

// handleMyTokenRevoke deletes the caller's token.
func (s *Server) handleMyTokenRevoke(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if err := s.tokens.Revoke(ctx, id.UserID); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}
