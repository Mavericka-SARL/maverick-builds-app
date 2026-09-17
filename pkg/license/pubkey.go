package license

// defaultPublicKeyB64 is the Ed25519 public key every maverickbuilds.app binary
// verifies license keys against. The matching PRIVATE key is held by the
// vendor outside any repository (see cmd/license keygen) — a key pair
// generated on 2026-09-15. Rotating it means shipping a new binary; a
// deployment can also override it with MAVERICKS_LICENSE_PUBLIC_KEY.
const defaultPublicKeyB64 = "0UHc1v5toRNhavIi1abWhUTt9G/lYT76Kv4AfjV2d64="
