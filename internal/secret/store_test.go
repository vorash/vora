package secret

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func rec(tenant, alias, host string) Record {
	return Record{TenantID: tenant, Alias: alias, Host: host, Scheme: SchemeBearer}
}

// TestMemStoreRoundTrip asserts the full lifecycle and that list output carries
// metadata only.
func TestMemStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore(newSealer(t))

	if err := s.Put(ctx, rec("t1", "github", "api.github.com"), "ghp_secret"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "t1", "github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Alias != "github" || got.Host != "api.github.com" || got.ID == "" {
		t.Fatalf("Get = %+v", got)
	}
	_, value, err := s.Reveal(ctx, "t1", "github")
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if value != "ghp_secret" {
		t.Fatalf("Reveal = %q", value)
	}

	list, err := s.List(ctx, "t1")
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d, %v", len(list), err)
	}
	if list[0].Alias != "github" {
		t.Fatalf("List = %+v", list)
	}

	// Replacing keeps the identity and creation time.
	if err := s.Put(ctx, rec("t1", "github", "api.github.com"), "new_secret"); err != nil {
		t.Fatalf("Put (replace): %v", err)
	}
	replaced, _ := s.Get(ctx, "t1", "github")
	if replaced.ID != got.ID || !replaced.CreatedAt.Equal(got.CreatedAt) {
		t.Fatalf("replace changed identity: %+v vs %+v", replaced, got)
	}
	if _, value, _ := s.Reveal(ctx, "t1", "github"); value != "new_secret" {
		t.Fatalf("Reveal after replace = %q", value)
	}

	if err := s.Delete(ctx, "t1", "github"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "t1", "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "t1", "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete twice = %v, want ErrNotFound", err)
	}
}

// TestMemStoreTenantIsolation asserts aliases are namespaced per tenant: a
// foreign tenant can neither see nor overwrite another tenant's credential.
func TestMemStoreTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore(newSealer(t))

	if err := s.Put(ctx, rec("t1", "github", "api.github.com"), "t1_secret"); err != nil {
		t.Fatalf("Put t1: %v", err)
	}
	if err := s.Put(ctx, rec("t2", "github", "api.other.com"), "t2_secret"); err != nil {
		t.Fatalf("Put t2: %v", err)
	}

	if _, err := s.Get(ctx, "t2", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Get = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Reveal(ctx, "t2", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Reveal = %v, want ErrNotFound", err)
	}
	got, value, err := s.Reveal(ctx, "t2", "github")
	if err != nil || value != "t2_secret" {
		t.Fatalf("t2 Reveal = %q, %v", value, err)
	}
	if got.Host != "api.other.com" {
		t.Fatalf("t2 host = %q", got.Host)
	}

	list, err := s.List(ctx, "t1")
	if err != nil || len(list) != 1 || list[0].TenantID != "t1" {
		t.Fatalf("List t1 = %+v, %v", list, err)
	}
	if err := s.Delete(ctx, "t2", "github"); err != nil {
		t.Fatalf("Delete t2: %v", err)
	}
	if _, value, err := s.Reveal(ctx, "t1", "github"); err != nil || value != "t1_secret" {
		t.Fatalf("t1 credential disturbed by t2 delete: %q, %v", value, err)
	}
}

// TestMemStoreSealsAtRest asserts the stored blob is not the plaintext.
func TestMemStoreSealsAtRest(t *testing.T) {
	s := NewMemStore(newSealer(t))
	if err := s.Put(context.Background(), rec("t1", "github", "api.github.com"), "ghp_plaintext"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	e := s.m[keyOf("t1", "github")]
	if strings.Contains(string(e.ct), "ghp_plaintext") {
		t.Fatal("stored ciphertext contains the plaintext")
	}
	if len(e.nonce) == 0 {
		t.Fatal("stored entry has no nonce")
	}
}

// TestMemStoreCorruptNonce asserts Reveal fails rather than panicking when a
// stored nonce is truncated, which is what a partial write leaves behind.
func TestMemStoreCorruptNonce(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore(newSealer(t))
	if err := s.Put(ctx, rec("t1", "github", "api.github.com"), "value"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	key := keyOf("t1", "github")
	corrupt := s.m[key]
	corrupt.nonce = corrupt.nonce[:4]
	s.m[key] = corrupt

	if _, _, err := s.Reveal(ctx, "t1", "github"); err == nil {
		t.Fatal("Reveal with a truncated nonce succeeded")
	}
}

// TestRecordValidate covers the record contract: aliases are opaque labels,
// hosts are DNS names (never IPs), and the scheme decides the header.
func TestRecordValidate(t *testing.T) {
	tests := []struct {
		name    string
		rec     Record
		wantErr string
	}{
		{"bearer ok", Record{Alias: "github", Host: "api.github.com", Scheme: SchemeBearer}, ""},
		{"header ok", Record{Alias: "gh", Host: "api.github.com", Scheme: SchemeHeader, Header: "X-Api-Key"}, ""},
		{"empty alias", Record{Alias: "", Host: "api.github.com", Scheme: SchemeBearer}, "alias"},
		{"uppercase alias", Record{Alias: "GitHub", Host: "api.github.com", Scheme: SchemeBearer}, "alias"},
		{"alias with slash", Record{Alias: "a/b", Host: "api.github.com", Scheme: SchemeBearer}, "alias"},
		{"empty host", Record{Alias: "github", Host: "", Scheme: SchemeBearer}, "host"},
		{"ip host", Record{Alias: "github", Host: "10.0.0.1", Scheme: SchemeBearer}, "DNS name"},
		{"uppercase host", Record{Alias: "github", Host: "API.github.com", Scheme: SchemeBearer}, "lowercase"},
		{"host with port", Record{Alias: "github", Host: "api.github.com:443", Scheme: SchemeBearer}, "host"},
		{"empty label", Record{Alias: "github", Host: "api..com", Scheme: SchemeBearer}, "label"},
		{"leading hyphen", Record{Alias: "github", Host: "-api.com", Scheme: SchemeBearer}, "label"},
		{"unknown scheme", Record{Alias: "github", Host: "api.github.com", Scheme: "magic"}, "unknown scheme"},
		{"bearer with header", Record{Alias: "github", Host: "api.github.com", Scheme: SchemeBearer, Header: "X-K"}, "header"},
		{"header without name", Record{Alias: "github", Host: "api.github.com", Scheme: SchemeHeader}, "header name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rec.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestMemStoreRejectsBadValue asserts empty and oversized values never reach
// the store.
func TestMemStoreRejectsBadValue(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore(newSealer(t))
	if err := s.Put(ctx, rec("t1", "github", "api.github.com"), "   "); err == nil {
		t.Fatal("empty value accepted")
	}
	if err := s.Put(ctx, rec("t1", "github", "api.github.com"), strings.Repeat("x", MaxValueLength+1)); err == nil {
		t.Fatal("oversized value accepted")
	}
}
