package service

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newJWTService() *JWTService {
	return NewJWTService([]byte("test-secret-32-bytes-long-enough!!"), 15*time.Minute, 168*time.Hour)
}

func TestJWTIssueAndParse(t *testing.T) {
	s := newJWTService()
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleAdmin}

	access, refresh, err := s.IssuePair(u, []string{"g1", "g2"})
	if err != nil {
		t.Fatalf("IssuePair: %v", err)
	}
	if access == "" || refresh == "" || access == refresh {
		t.Fatal("bad tokens")
	}

	claims, err := s.ParseAccess(access)
	if err != nil {
		t.Fatalf("ParseAccess: %v", err)
	}
	if claims.UserID != "u1" || claims.Username != "alice" || claims.Role != "admin" {
		t.Fatalf("claims = %+v", claims)
	}
	if len(claims.GroupIDs) != 2 || claims.GroupIDs[0] != "g1" {
		t.Fatalf("groups = %v", claims.GroupIDs)
	}

	rclaims, err := s.ParseRefresh(refresh)
	if err != nil {
		t.Fatalf("ParseRefresh: %v", err)
	}
	if rclaims.UserID != "u1" {
		t.Fatalf("refresh claims = %+v", rclaims)
	}
}

func TestJWTWrongTypRejected(t *testing.T) {
	s := newJWTService()
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser}
	access, refresh, _ := s.IssuePair(u, nil)
	if _, err := s.ParseAccess(refresh); err == nil {
		t.Fatal("refresh as access should fail")
	}
	if _, err := s.ParseRefresh(access); err == nil {
		t.Fatal("access as refresh should fail")
	}
}

func TestJWTExpired(t *testing.T) {
	s := NewJWTService([]byte("test-secret-32-bytes-long-enough!!"), 1*time.Millisecond, 1*time.Millisecond)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser}
	access, _, _ := s.IssuePair(u, nil)
	time.Sleep(10 * time.Millisecond)
	if _, err := s.ParseAccess(access); err == nil {
		t.Fatal("expired token should fail")
	}
}

func TestJWTTamperedSignature(t *testing.T) {
	s := newJWTService()
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser}
	access, _, _ := s.IssuePair(u, nil)
	// Tamper: flip the first character of the signature segment. The *last*
	// base64url character of a 32-byte HS256 signature only carries 4
	// significant bits, so flipping it occasionally decodes to the identical
	// signature and the token still validates (flaky test).
	sigStart := strings.LastIndexByte(access, '.') + 1
	tampered := access[:sigStart] + string(flip(access[sigStart])) + access[sigStart+1:]
	if _, err := s.ParseAccess(tampered); err == nil {
		t.Fatal("tampered token should fail")
	}
}

func TestJWTAlgNoneAttack(t *testing.T) {
	// A token signed with "none" must be rejected (WithValidMethods enforces HS256).
	claims := &Claims{
		UserID: "u1",
		Type:   typAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	unsigned, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)

	s := newJWTService()
	if _, err := s.ParseAccess(unsigned); err == nil {
		t.Fatal("alg=none token should be rejected")
	}
}

func TestJWTOAuthState(t *testing.T) {
	s := newJWTService()
	state, err := s.IssueOAuthState("/pipelines", "nonce-1")
	if err != nil {
		t.Fatalf("IssueOAuthState: %v", err)
	}
	claims, err := s.ParseOAuthState(state)
	if err != nil {
		t.Fatalf("ParseOAuthState: %v", err)
	}
	if claims.Type != typOAuthState {
		t.Fatalf("type = %q", claims.Type)
	}
	if claims.Username != "/pipelines" {
		t.Fatalf("username (redirect) = %q", claims.Username)
	}
	if claims.Nonce != "nonce-1" {
		t.Fatalf("nonce = %q, want nonce-1", claims.Nonce)
	}
	// State token is not a valid access token.
	if _, err := s.ParseAccess(state); err == nil {
		t.Fatal("state token should not parse as access")
	}
}

func TestJWTSecretMismatch(t *testing.T) {
	s1 := NewJWTService([]byte("secret-one-32-bytes-long-enough!!!"), 15*time.Minute, 168*time.Hour)
	s2 := NewJWTService([]byte("secret-two-32-bytes-long-enough!!!"), 15*time.Minute, 168*time.Hour)
	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser}
	access, _, _ := s1.IssuePair(u, nil)
	if _, err := s2.ParseAccess(access); err == nil {
		t.Fatal("different secret should reject")
	}
}

// flip returns the byte b with its low bit flipped (for tamper tests).
func flip(b byte) byte {
	if b == 'a' {
		return 'b'
	}
	return 'a'
}
