// Package integration implements the REST API connector: typed configuration,
// SSRF-hardened outbound HTTP, credential encryption, templating, pagination,
// mapping, and the queue-backed runner the integration worker executes.
package integration

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Credential encryption is MANDATORY versioned authenticated encryption —
// deliberately not a reuse of internal/aiassistant's crypto, which falls back
// to storing plaintext when its secret is unset. Here a missing key is a hard
// error at both encrypt and decrypt time: a connector's Basic/bearer/OAuth
// secrets never touch the database unencrypted, in any environment.
//
// Format: "iv1:" + base64(nonce || AES-256-GCM ciphertext), sealed with
// associated data binding the ciphertext to (applicationID, connectionID) —
// a row copied onto another application or connection fails authentication
// instead of decrypting somewhere it was never granted to.
const credEncPrefix = "iv1:"

// ErrNoEncryptionKey is returned when INTEGRATION_CRED_KEY is unset or invalid.
var ErrNoEncryptionKey = errors.New("INTEGRATION_CRED_KEY is not configured (32+ byte secret required); refusing to handle connector credentials without encryption")

func credKey() ([]byte, error) {
	secret := os.Getenv("INTEGRATION_CRED_KEY")
	if len(secret) < 32 {
		return nil, ErrNoEncryptionKey
	}
	// Use the first 32 bytes of the configured secret directly if it is
	// exactly 32 raw bytes; otherwise derive via SHA-256? — no: keep it
	// boring and deterministic. The secret is operator-provided entropy; a
	// fixed-length key is derived below.
	return deriveKey(secret), nil
}

func credAAD(applicationID, connectionID string) []byte {
	return []byte("mavericks-integration-credential\x00" + applicationID + "\x00" + connectionID)
}

// EncryptCredential seals payload (a JSON credential document) for exactly
// one (application, connection) pair.
func EncryptCredential(payload []byte, applicationID, connectionID string) (string, error) {
	key, err := credKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("credential cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("credential cipher: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("credential nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, payload, credAAD(applicationID, connectionID))
	return credEncPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptCredential opens a sealed credential. There is no plaintext
// passthrough: a value without the version prefix is corrupt, not legacy.
func DecryptCredential(stored, applicationID, connectionID string) ([]byte, error) {
	if !strings.HasPrefix(stored, credEncPrefix) {
		return nil, fmt.Errorf("credential payload is not in a recognized encrypted format")
	}
	key, err := credKey()
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, credEncPrefix))
	if err != nil {
		return nil, fmt.Errorf("credential payload corrupt: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credential cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential cipher: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("credential payload corrupt: short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], credAAD(applicationID, connectionID))
	if err != nil {
		return nil, fmt.Errorf("credential authentication failed (wrong key, or payload bound to a different application/connection)")
	}
	return plain, nil
}
