package integration

import "crypto/sha256"

// deriveKey turns the operator-provided secret into a fixed 32-byte AES key.
func deriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}
