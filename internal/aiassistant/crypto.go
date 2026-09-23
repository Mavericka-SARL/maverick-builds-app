package aiassistant

import "github.com/mavericks-engine/mavericks/internal/secretbox"

// API keys are stored through internal/secretbox — AES-256-GCM under
// SECRETS_ENCRYPTION_KEY (or the older AI_KEY_ENCRYPTION_SECRET), plaintext
// when neither is set. These two names stay for the callers that predate
// the shared box.

// EncryptAPIKey seals a key; see secretbox.Encrypt.
func EncryptAPIKey(plain string) (string, error) { return secretbox.Encrypt(plain) }

// DecryptAPIKey opens a stored key; see secretbox.Decrypt.
func DecryptAPIKey(stored string) (string, error) { return secretbox.Decrypt(stored) }
