package secret

import (
	"strings"
	"testing"
)

const masterKey = "test-master-key-that-is-long-enough"

func newSealer(t *testing.T) *Sealer {
	t.Helper()
	s, err := NewSealer(masterKey)
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	return s
}

// TestSealerRejectsShortKey asserts a weak or missing master key is refused at
// construction rather than silently protecting credentials with a truncated
// key.
func TestSealerRejectsShortKey(t *testing.T) {
	for _, key := range []string{"", "short", strings.Repeat("x", MinMasterKeyLength-1)} {
		if _, err := NewSealer(key); err == nil {
			t.Fatalf("NewSealer(%d bytes) succeeded, want error", len(key))
		}
	}
}

// TestSealerRoundTrip asserts a value survives seal/open and that the sealed
// form never contains the plaintext.
func TestSealerRoundTrip(t *testing.T) {
	s := newSealer(t)
	value := "ghp_supersecret"
	nonce, ct, err := s.Seal(value)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(string(ct), value) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := s.Open(nonce, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != value {
		t.Fatalf("Open = %q, want %q", got, value)
	}
}

// TestSealerRejectsWrongKey asserts a value sealed under one master key cannot
// be read with another: the operational guarantee behind key rotation.
func TestSealerRejectsWrongKey(t *testing.T) {
	other, err := NewSealer("a-different-master-key-long-enough")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	nonce, ct, err := newSealer(t).Seal("value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := other.Open(nonce, ct); err == nil {
		t.Fatal("decrypting with the wrong master key succeeded")
	}
}

// TestSealerRejectsTamperedCiphertext asserts GCM authentication catches
// modification of a stored credential.
func TestSealerRejectsTamperedCiphertext(t *testing.T) {
	s := newSealer(t)
	nonce, ct, err := s.Seal("value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tampered := append([]byte(nil), ct...)
	tampered[0] ^= 0xff
	if _, err := s.Open(nonce, tampered); err == nil {
		t.Fatal("decrypting tampered ciphertext succeeded")
	}

	badNonce := append([]byte(nil), nonce...)
	badNonce[0] ^= 0xff
	if _, err := s.Open(badNonce, ct); err == nil {
		t.Fatal("decrypting with a tampered nonce succeeded")
	}
}
