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
func (s *Server) handleMyTokenMeta(_ context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(c)
	if !ok {
		return
	}
	t, err := s.tokens.Meta(context.Background(), id.UserID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, toTokenMeta(t))
}

// handleMyTokenCreate issues a new token for the caller (409 if one exists).
func (s *Server) handleMyTokenCreate(_ context.Context, c *app.RequestContext) {
	s.issueMyToken(c, consts.StatusCreated, s.tokens.Generate)
}

// handleMyTokenRegenerate replaces the caller's token (old token invalid
// immediately) and returns the new plaintext.
func (s *Server) handleMyTokenRegenerate(_ context.Context, c *app.RequestContext) {
	s.issueMyToken(c, consts.StatusOK, s.tokens.Regenerate)
}

// issueMyToken runs generate (create or regenerate) for the caller and writes
// the one-time plaintext with the given success status.
func (s *Server) issueMyToken(c *app.RequestContext, status int, generate func(ctx context.Context, userID string) (string, *domain.APIToken, error)) {
	id, ok := s.resolveIdentity(c)
	if !ok {
		return
	}
	// Synthetic identities (legacy flat-file) have no users-table row; a token
	// row would violate the user_id foreign key.
	if id.Method == domain.AuthLegacyTok {
		writeError(c, consts.StatusBadRequest, "api tokens require a real user account")
		return
	}
	plaintext, _, err := generate(context.Background(), id.UserID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(status, myTokenResponse{Token: plaintext})
}

// handleMyTokenRevoke deletes the caller's token.
func (s *Server) handleMyTokenRevoke(_ context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(c)
	if !ok {
		return
	}
	if err := s.tokens.Revoke(context.Background(), id.UserID); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}
