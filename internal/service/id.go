package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// randomHex returns 2n hex chars from n cryptographically random bytes.
// Panics only if the system PRNG is unavailable.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("rand read: %v", err))
	}
	return hex.EncodeToString(b)
}

// newID returns a fresh 32-char hex id (16 random bytes) for users/groups/
// projects/tokens.
func newID() string {
	return randomHex(16)
}
