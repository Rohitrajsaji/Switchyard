package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// New returns a cryptographically random opaque identifier. Tokens use 256 bits.
func New(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secure random unavailable")
	}
	return prefix + hex.EncodeToString(b)
}
func Hash(token string) []byte { h := sha256.Sum256([]byte(token)); return h[:] }
