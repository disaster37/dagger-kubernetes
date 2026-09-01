package service

import (
	"encoding/json"
	"fmt"
	"time"
)

// oauthCredential is the upstream OAuth credential captured at login and
// persisted encrypted on domain.User.OAuthTokenCiphertext.
type oauthCredential struct {
	Provider     string    `json:"provider"` // "github" | "oidc"
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"` // OIDC only
	ExpiresAt    time.Time `json:"expires_at,omitempty"`    // zero = unknown/does not expire
}

// encryptOAuthCredential JSON-encodes c and AES-256-GCM-encrypts it with
// encKey (reuses encryptToken). A nil/empty key returns ("", nil): credential
// persistence is disabled (pre-config dev mode).
func encryptOAuthCredential(encKey []byte, c *oauthCredential) (string, error) {
	if c == nil || len(encKey) == 0 {
		return "", nil
	}
	b, err := json.Marshal(c) //nolint:gosec // G117: marshaling the credential solely to AES-256-GCM-encrypt it at rest; never logged or returned.
	if err != nil {
		return "", fmt.Errorf("marshal oauth credential: %w", err)
	}
	return encryptToken(encKey, string(b))
}

// decryptOAuthCredential reverses encryptOAuthCredential. Returns (nil, nil)
// when ct is empty (no stored credential).
func decryptOAuthCredential(encKey []byte, ct string) (*oauthCredential, error) {
	if ct == "" {
		return nil, nil
	}
	if len(encKey) == 0 {
		return nil, fmt.Errorf("oauth credential stored but no encryption key configured")
	}
	pt, err := decryptToken(encKey, ct)
	if err != nil {
		return nil, fmt.Errorf("decrypt oauth credential: %w", err)
	}
	var c oauthCredential
	if err := json.Unmarshal([]byte(pt), &c); err != nil {
		return nil, fmt.Errorf("decode oauth credential: %w", err)
	}
	return &c, nil
}
