package repository

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// forgeHTTPClient is the shared outbound HTTP client for forge APIs (ADR-007:
// outbound net/http lives in repository).
var forgeHTTPClient = &http.Client{Timeout: 30 * time.Second}

// forgeRequest performs an authenticated JSON/form request and returns the
// response body.
func forgeRequest(ctx context.Context, method, url, authHeader, authValue string, body []byte, contentType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build forge request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if authHeader != "" {
		req.Header.Set(authHeader, authValue)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := forgeHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forge request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read forge response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return data, fmt.Errorf("forge API %s %s: status %d", method, url, resp.StatusCode)
	}
	return data, nil
}

// verifyHMACSHA256 verifies a hex HMAC-SHA256 signature.
func verifyHMACSHA256(secret string, body []byte, signature string) bool {
	signature = strings.TrimPrefix(signature, "sha256=")
	if secret == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// jsonBody marshals v.
func jsonBody(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal forge payload: %w", err)
	}
	return data, nil
}
