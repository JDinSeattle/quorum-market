package events

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// newEventID gives every event a stable identity, which is what lets a
// subscriber recognise a redelivery and skip work it has already done.
func newEventID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	return "evt-" + hex.EncodeToString(b[:])
}
