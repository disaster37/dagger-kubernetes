package handler

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

// extractToken pulls the bearer token from the Authorization header (Bearer
// then Basic). The ?token= query-param fallback (D14) is intentionally NOT
// handled here: tokens in URLs leak via logs and referrers, so query-param
// auth is limited to the SSE /live route (see requireAuthWithQueryFallback).
func extractToken(c *app.RequestContext) (string, error) {
	authHeader := string(c.GetHeader("Authorization"))
	if authHeader != "" {
		// Bearer tokens are the primary contract (DAGGER_CLOUD_TOKEN).
		if token, ok := strings.CutPrefix(authHeader, "Bearer "); ok {
			return token, nil
		}
		// Basic auth is kept as a fallback (username is treated as the token).
		if payload, ok := strings.CutPrefix(authHeader, "Basic "); ok {
			decoded, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return "", fmt.Errorf("decode basic auth: %w", err)
			}
			user, _, _ := strings.Cut(string(decoded), ":")
			return user, nil
		}
	}

	if authHeader == "" {
		return "", fmt.Errorf("missing authorization")
	}
	return "", fmt.Errorf("unsupported auth scheme")
}
