package license

// trustedPublicKeys are the Ed25519 public keys every maverickbuilds.app
// binary accepts license keys from. The matching PRIVATE keys are held by
// the vendor outside any repository (see cmd/license keygen).
//
// There is deliberately no setting that adds a key: a deployment that could
// trust its own signer could issue itself any edition. Tests inject theirs
// through Options.PublicKey, which the gateway never sets.
//
// Rotating the signing key: generate a new pair, append its public key here
// and release; sign new licences with the new key once the release is out;
// remove the old public key only after every licence signed with it has
// expired (its transition period included).
var trustedPublicKeys = []string{
	"0UHc1v5toRNhavIi1abWhUTt9G/lYT76Kv4AfjV2d64=", // generated 2026-09-15
}
