package aiassistant

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

// API keys are stored AES-256-GCM encrypted when AI_KEY_ENCRYPTION_SECRET is
// set. Encrypted values carry the "enc:v1:" prefix; values without it are
// treated as legacy plaintext (pre-encryption rows, or dev setups without the
// secret) so existing keys keep working after the secret is introduced.
const encPrefix = "enc:v1:"

func encryptionKey() []byte {
	secret := os.Getenv("AI_KEY_ENCRYPTION_SECRET")
	if secret == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// EncryptAPIKey encrypts plain with AES-256-GCM. Without a configured secret
// it returns the input unchanged (plaintext mode).
func EncryptAPIKey(plain string) (string, error) {
	key := encryptionKey()
	if key == nil || plain == "" {
		return plain, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("encrypt api key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("encrypt api key: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("encrypt api key: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptAPIKey reverses EncryptAPIKey. Non-prefixed values pass through
// unchanged (legacy plaintext).
func DecryptAPIKey(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	key := encryptionKey()
	if key == nil {
		return "", fmt.Errorf("api key is encrypted but AI_KEY_ENCRYPTION_SECRET is not set")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("decrypt api key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("decrypt api key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decrypt api key: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("decrypt api key: ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt api key: %w", err)
	}
	return string(plain), nil
}
