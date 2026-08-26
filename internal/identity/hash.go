package identity

import "crypto/sha256"

// sha256Sum is split out so the hashing choice for session lookup keys is in
// one obvious place. SHA-256 is right here and bcrypt is not: a refresh token
// is 32 bytes of entropy from a CSPRNG, so there is nothing to brute-force and
// no reason to pay a slow KDF on every refresh.
func sha256Sum(s string) [32]byte { return sha256.Sum256([]byte(s)) }
