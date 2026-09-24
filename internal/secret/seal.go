// Package secret stores brokered third-party credentials encrypted at rest.
//
// Values are sealed with AES-256-GCM under a key derived (HKDF-SHA256) from the
// operator's master key (VORA_MASTER_KEY). The master key is only ever held by
// vora-api; runners never see it. A Record carries metadata only — the alias,
// the upstream host it is bound to, and how to attach it — so audit trails and
// list output can never leak a value.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// MinMasterKeyLength is the shortest accepted VORA_MASTER_KEY.
const MinMasterKeyLength = 32

const (
	// sealSalt and sealInfo domain-separate the secrets key from any future
	// key derived from the same master secret.
	sealSalt = "vora-secrets"
	sealInfo = "vora/secrets/v1"
)

// ErrInvalidMasterKey means VORA_MASTER_KEY was unset or too short to derive a
// key from.
var ErrInvalidMasterKey = errors.New("secret: master key must be at least 32 bytes")

// Sealer encrypts and decrypts secret values.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer derives an AES-256-GCM key from masterKey. A short or empty key is
// rejected so a misconfigured deployment fails at startup rather than storing
// weakly-protected credentials.
func NewSealer(masterKey string) (*Sealer, error) {
	if len(masterKey) < MinMasterKeyLength {
		return nil, ErrInvalidMasterKey
	}
	key, err := hkdf.Key(sha256.New, []byte(masterKey), []byte(sealSalt), sealInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("secret: derive key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

const boundKeyVersion = 2

func recordAAD(rec Record) []byte {
	data := []byte("vora/secrets/v2")
	for _, field := range []string{rec.TenantID, rec.Alias, rec.Host, rec.Scheme, rec.Header} {
		data = binary.BigEndian.AppendUint32(data, uint32(len(field)))
		data = append(data, field...)
	}
	return data
}

func (s *Sealer) SealBound(value string, rec Record) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secret: nonce: %w", err)
	}
	return nonce, s.aead.Seal(nil, nonce, []byte(value), recordAAD(rec)), nil
}

func (s *Sealer) OpenBound(nonce, ciphertext []byte, rec Record) (string, error) {
	if len(nonce) != s.aead.NonceSize() {
		return "", fmt.Errorf("secret: decrypt: invalid nonce length %d", len(nonce))
	}
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, recordAAD(rec))
	if err != nil {
		return "", fmt.Errorf("secret: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// Seal encrypts a value, returning the per-record nonce and its ciphertext.
func (s *Sealer) Seal(value string) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secret: nonce: %w", err)
	}
	return nonce, s.aead.Seal(nil, nonce, []byte(value), nil), nil
}

// Open decrypts a value. It fails if the ciphertext was tampered with or was
// sealed under a different master key.
func (s *Sealer) Open(nonce, ciphertext []byte) (string, error) {
	// GCM panics on a wrong-length nonce, and a persisted nonce can be
	// truncated by a partial write or an older record, so reject it here.
	if len(nonce) != s.aead.NonceSize() {
		return "", fmt.Errorf("secret: decrypt: invalid nonce length %d", len(nonce))
	}
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("secret: decrypt: %w", err)
	}
	return string(plaintext), nil
}
