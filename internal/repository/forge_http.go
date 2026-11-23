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
	neturl "net/url"
	"strings"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
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

// webhookCallbackSuffix is the path suffix of a source's webhook callback URL
// (the public base is unknown to the provider, so matching is suffix-only).
func webhookCallbackSuffix(source *domain.ForgeSource) string {
	return fmt.Sprintf("/v1/webhooks/%s/%s", source.Provider, source.ID)
}

// matchesWebhookCallback reports whether a hook's configured URL targets this
// source. Matching on the path suffix avoids depending on the public base URL.
func matchesWebhookCallback(rawURL string, source *domain.ForgeSource) bool {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.Path, webhookCallbackSuffix(source))
}

// forgeHook is the common shape of a listed webhook across providers.
type forgeHook struct {
	ID     int64  `json:"id"`
	URL    string `json:"url"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

// hookURL returns the callback URL a hook is configured with. GitHub/Gitea nest
// the callback under config.url (their top-level url is the hook's API URL);
// GitLab has no config object and exposes the callback as the top-level url.
func (h forgeHook) hookURL() string {
	if h.Config.URL != "" {
		return h.Config.URL
	}
	return h.URL
}

// deleteMatchingHooks lists a repo's hooks and deletes every one whose URL
// matches the source's callback suffix. deleteURL builds the DELETE target from
// a hook ID. No matches is a no-op (idempotent).
func deleteMatchingHooks(ctx context.Context, listURL, authHeader, authValue string, source *domain.ForgeSource, deleteURL func(int64) string) error {
	data, err := forgeRequest(ctx, "GET", listURL, authHeader, authValue, nil, "")
	if err != nil {
		return err
	}
	var hooks []forgeHook
	if err := json.Unmarshal(data, &hooks); err != nil {
		return fmt.Errorf("decode forge hooks: %w", err)
	}
	for _, h := range hooks {
		if !matchesWebhookCallback(h.hookURL(), source) {
			continue
		}
		if _, err := forgeRequest(ctx, "DELETE", deleteURL(h.ID), authHeader, authValue, nil, ""); err != nil {
			return err
		}
	}
	return nil
}
