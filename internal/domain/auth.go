package domain

// TokenValidator validates a flat-file bearer token. Retained as the legacy
// fallback path used by AuthService when auth.internal.tokens_file is still
// configured (see ADR-010). New code should resolve identities via
// AuthService.Resolve instead.
type TokenValidator interface {
	ValidateToken(token string) (string, error)
}
