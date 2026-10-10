package service

import (
	"crypto/sha256"
	"encoding/hex"
)

// hashPayload is the short digest that makes an idempotency key stable.
//
// It is a digest rather than the payload itself because the key is a database
// column with a unique index: two proposals that differ in one character are
// different proposals, and the digest keeps that distinction without storing the
// action's arguments twice.
func hashPayload(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:8])
}
