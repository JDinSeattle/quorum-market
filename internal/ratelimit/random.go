package ratelimit

import (
	"crypto/rand"
	"encoding/hex"
)

// randomSuffix keeps two requests in the same nanosecond from colliding on the
// same sorted-set member, which would let one of them go uncounted.
func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "x"
	}
	return hex.EncodeToString(b[:])
}
