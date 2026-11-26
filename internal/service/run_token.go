package service

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// newRunToken returns a fresh 32-byte hex run token and its SHA-256 hash.
func newRunToken() (token, hash string) {
	token = randomHex(32)
	return token, hashRunToken(token)
}

// hashRunToken returns the hex-encoded SHA-256 hash of token.
func hashRunToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyRunToken reports whether token matches hash in constant time. An empty
// hash or token never matches (fail closed).
func VerifyRunToken(hash, token string) bool {
	if hash == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hash), []byte(hashRunToken(token))) == 1
}
