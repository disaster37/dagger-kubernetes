package service

import (
	"crypto/subtle"
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type TokenValidator struct {
	TokensFile string
	logger     *logrus.Logger
}

var _ domain.TokenValidator = (*TokenValidator)(nil)

func NewTokenValidator(tokensFile string, logger *logrus.Logger) *TokenValidator {
	return &TokenValidator{TokensFile: tokensFile, logger: logger}
}

func (v *TokenValidator) ValidateToken(token string) (string, error) {
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

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(line), []byte(token)) == 1 {
			return true, nil
		}
	}

	return false, nil
}
