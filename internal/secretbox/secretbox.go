// Package secretbox is how the platform stores a credential a tenant hands
// it — an AI provider key, a Google service account's private key, an OAuth
// refresh token — so that a copy of the database is not a copy of the
// credential. AES-256-GCM under a key derived from SECRETS_ENCRYPTION_KEY
// (the older AI_KEY_ENCRYPTION_SECRET is still honoured, since deployments
// set it before the box held more than AI keys).
//
// Without a key the box runs in plaintext mode, so a developer's stack
// works with no setup; a store that must never write plaintext checks
// Configured first and refuses. Encrypted values carry the "enc:v1:"
// prefix; values without it are read back as they are, so rows written
// before a key existed keep working after one is introduced.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

const prefix = "enc:v1:"

// EnvVar names the key's environment variable; LegacyEnvVar the one it
// replaced.
const (
	EnvVar       = "SECRETS_ENCRYPTION_KEY"
	LegacyEnvVar = "AI_KEY_ENCRYPTION_SECRET"
)

func key() []byte {
	secret := os.Getenv(EnvVar)
	if secret == "" {
		secret = os.Getenv(LegacyEnvVar)
	}
	if secret == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// Configured reports whether a key is set — whether Encrypt encrypts.
func Configured() bool { return key() != nil }

// Encrypt seals plain; without a key it returns plain unchanged.
func Encrypt(plain string) (string, error) {
	k := key()
	if k == nil || plain == "" {
		return plain, nil
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", fmt.Errorf("encrypt secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("encrypt secret: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("encrypt secret: %w", err)
	}
	return prefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// Decrypt reverses Encrypt; a value without the prefix is returned as it is.
func Decrypt(stored string) (string, error) {
	if !strings.HasPrefix(stored, prefix) {
		return stored, nil
	}
	k := key()
	if k == nil {
		return "", fmt.Errorf("the value is encrypted but %s is not set", EnvVar)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("decrypt secret: ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plain), nil
}
