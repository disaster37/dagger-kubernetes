package domain

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SecretScope is the scope at which a write-only secret is stored.
type SecretScope string

const (
	SecretScopeOrg         SecretScope = "org"
	SecretScopeProject     SecretScope = "project"
	SecretScopeEnvironment SecretScope = "environment"
)

// Secret is a write-only AES-256-GCM sealed value. Plaintext is never returned
// by the API.
type Secret struct {
	ID         string      `json:"id"`
	Scope      SecretScope `json:"scope"`
	ScopeID    string      `json:"scope_id"`
	Name       string      `json:"name"`
	Ciphertext string      `json:"-"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

// EnvironmentScopeID builds the tenant-isolated scope identifier
// "<orgID>/<envName>" for SecretScopeEnvironment so two organizations sharing
// an environment name (e.g. "prod") never share AAD or FSM storage keys.
func EnvironmentScopeID(orgID, envName string) (string, error) {
	orgID = strings.TrimSpace(orgID)
	envName = strings.TrimSpace(envName)
	if orgID == "" || envName == "" || strings.ContainsAny(orgID, "/|") || strings.ContainsAny(envName, "/|") {
		return "", fmt.Errorf("%w: invalid environment scope", ErrValidation)
	}
	return fmt.Sprintf("%s/%s", orgID, envName), nil
}

// SecretRepository persists sealed secrets.
type SecretRepository interface {
	Upsert(ctx context.Context, s *Secret) error
	Get(ctx context.Context, scope SecretScope, scopeID, name string) (*Secret, error)
	List(ctx context.Context, scope SecretScope, scopeID string) ([]*Secret, error)
	Delete(ctx context.Context, scope SecretScope, scopeID, name string) error
}
