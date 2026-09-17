// Package api implements the Vora control plane: an HTTP/JSON API for external
// clients and a gRPC runner registry that runners dial out to.
//
// API keys are stored hashed (SHA-256); the plaintext is shown once at
// creation. Runners authenticate registration with a join token and heartbeat
// on an interval.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// KeyPrefix marks Vora API keys.
const KeyPrefix = "vora_"

// NewAPIKey returns a new plaintext key and its SHA-256 hash.
func NewAPIKey() (plaintext, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plaintext = KeyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return plaintext, HashKey(plaintext), nil
}

// HashKey returns the hex SHA-256 of a plaintext key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
