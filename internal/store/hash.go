package store

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// RequestHash binds an idempotency key to a show and a set of seats.
// Seat order does not change the hash: ["A2","A1"] is the same request as ["A1","A2"].
func RequestHash(showID string, seats []string) string {
	cp := append([]string(nil), seats...)
	sort.Strings(cp)
	sum := sha256.Sum256([]byte(showID + "\n" + strings.Join(cp, "\n")))
	return hex.EncodeToString(sum[:])
}
