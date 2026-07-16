package auth

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/sirupsen/logrus"
)

type TokenValidator struct {
	TokensFile string
	Enabled    bool
	logger     *logrus.Logger
}

func NewTokenValidator(tokensFile string, enabled bool, logger *logrus.Logger) *TokenValidator {
	return &TokenValidator{TokensFile: tokensFile, Enabled: enabled, logger: logger}
}

func (v *TokenValidator) ValidateRequest(c *app.RequestContext) (string, error) {
	// Auth explicitly disabled: accept any request without extracting the token.
	if !v.Enabled {
		v.logger.Debug("auth disabled, accepting request")
		return "no-auth", nil
	}

	token, err := extractToken(c)
	if err != nil {
		return "", fmt.Errorf("unauthorized: %w", err)
	}
	return v.ValidateToken(token)
}

func (v *TokenValidator) ValidateToken(token string) (string, error) {
	// Auth explicitly disabled: accept any token including empty (dev / no-auth mode).
	if !v.Enabled {
		v.logger.Debug("auth disabled, accepting token")
		return token, nil
	}

	if token == "" {
		return "", fmt.Errorf("empty token")
	}

	if v.TokensFile == "" {
		return "", fmt.Errorf("auth enabled but no tokens file configured")
	}

	valid, err := v.checkTokenFile(token)
	if err != nil {
		return "", fmt.Errorf("token file error: %w", err)
	}
	if !valid {
		return "", fmt.Errorf("invalid token")
	}

	return token, nil
}

func (v *TokenValidator) checkTokenFile(token string) (bool, error) {
	data, err := os.ReadFile(v.TokensFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Auth is enabled but the configured tokens file is missing: fail
			// closed rather than silently accepting all tokens.
			return false, fmt.Errorf("tokens file not found: %s", v.TokensFile)
		}
		return false, fmt.Errorf("read tokens file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if line == token {
			return true, nil
		}
	}

	return false, nil
}

func extractToken(c *app.RequestContext) (string, error) {
	authHeader := string(c.GetHeader("Authorization"))
	if authHeader == "" {
		return "", fmt.Errorf("missing authorization")
	}

	// Bearer tokens are the primary contract (DAGGER_CLOUD_TOKEN).
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer "), nil
	}

	// Basic auth is kept as a fallback (username is treated as the token).
	if strings.HasPrefix(authHeader, "Basic ") {
		payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authHeader, "Basic "))
		if err != nil {
			return "", fmt.Errorf("decode basic auth: %w", err)
		}
		parts := strings.SplitN(string(payload), ":", 2)
		return parts[0], nil
	}

	return "", fmt.Errorf("unsupported auth scheme")
}
