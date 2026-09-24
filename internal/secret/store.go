package secret

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vorash/vora/internal/ulid"
)

// ErrNotFound is returned when a tenant has no secret with that alias.
var ErrNotFound = errors.New("secret: not found")

// Schemes describe how the broker attaches a value to an upstream request.
const (
	// SchemeBearer sends "Authorization: Bearer <value>".
	SchemeBearer = "bearer"
	// SchemeHeader sends "<header>: <value>".
	SchemeHeader = "header"
)

// MaxValueLength bounds a stored credential.
const MaxValueLength = 16 << 10

var (
	aliasRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	headerRE = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// Record is the metadata of a stored brokered credential. It never carries the
// value.
type Record struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Alias     string    `json:"alias"`
	Host      string    `json:"host"`
	Scheme    string    `json:"scheme"`
	Header    string    `json:"header,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks alias, host and attachment scheme. It is independent of any
// store so the API can reject a bad record before touching the backend.
func (r Record) Validate() error {
	if !aliasRE.MatchString(r.Alias) {
		return fmt.Errorf("alias must match %s", aliasRE.String())
	}
	if err := validateHost(r.Host); err != nil {
		return err
	}
	switch r.Scheme {
	case SchemeBearer:
		if r.Header != "" {
			return errors.New("scheme bearer must not set header")
		}
	case SchemeHeader:
		if !headerRE.MatchString(r.Header) {
			return errors.New("scheme header requires a valid header name")
		}
	default:
		return fmt.Errorf("unknown scheme %q (want %q or %q)", r.Scheme, SchemeBearer, SchemeHeader)
	}
	return nil
}

// validateHost accepts a lowercase DNS name without scheme, port or IP literal.
func validateHost(host string) error {
	if host == "" || len(host) > 253 {
		return fmt.Errorf("host %q is not a valid DNS name", host)
	}
	if host != strings.ToLower(host) {
		return fmt.Errorf("host %q must be lowercase", host)
	}
	if net.ParseIP(host) != nil {
		return fmt.Errorf("host %q must be a DNS name, not an IP address", host)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("host %q has an invalid label", host)
		}
		for i, c := range label {
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			case c == '-' && i != 0 && i != len(label)-1:
			default:
				return fmt.Errorf("host %q has an invalid label", host)
			}
		}
	}
	return nil
}

// Store persists encrypted credentials. Every read and write is scoped to a
// tenant, so one tenant can never observe another's alias.
type Store interface {
	// Put creates or replaces the credential for rec.TenantID/rec.Alias.
	Put(ctx context.Context, rec Record, value string) error
	// Get returns a tenant's record metadata.
	Get(ctx context.Context, tenant, alias string) (Record, error)
	// Reveal returns a tenant's record metadata and its plaintext value.
	Reveal(ctx context.Context, tenant, alias string) (Record, string, error)
	// List returns a tenant's records, oldest first. Values are never included.
	List(ctx context.Context, tenant string) ([]Record, error)
	// Delete removes a tenant's credential.
	Delete(ctx context.Context, tenant, alias string) error
	Close() error
}

func keyOf(tenant, alias string) string { return tenant + "\x00" + alias }

type entry struct {
	rec     Record
	nonce   []byte
	ct      []byte
	version int
}

// MemStore is an in-memory Store used for development and tests. Values are
// sealed with the same Sealer as the PostgreSQL store, so the encryption path
// is exercised either way.
type MemStore struct {
	mu     sync.RWMutex
	sealer *Sealer
	m      map[string]entry
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory store.
func NewMemStore(sealer *Sealer) *MemStore {
	return &MemStore{sealer: sealer, m: map[string]entry{}}
}

// Put implements Store.
func (m *MemStore) Put(ctx context.Context, rec Record, value string) error {
	_ = ctx
	stored, err := prepare(m.sealer, rec, value)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.m[keyOf(rec.TenantID, rec.Alias)]; ok {
		stored.rec.ID = prev.rec.ID
		stored.rec.CreatedAt = prev.rec.CreatedAt
	}
	m.m[keyOf(rec.TenantID, rec.Alias)] = stored
	return nil
}

// Get implements Store.
func (m *MemStore) Get(_ context.Context, tenant, alias string) (Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.m[keyOf(tenant, alias)]
	if !ok {
		return Record{}, ErrNotFound
	}
	return e.rec, nil
}

// Reveal implements Store.
func (m *MemStore) Reveal(_ context.Context, tenant, alias string) (Record, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.m[keyOf(tenant, alias)]
	if !ok {
		return Record{}, "", ErrNotFound
	}
	var value string
	var err error
	if e.version == boundKeyVersion {
		value, err = m.sealer.OpenBound(e.nonce, e.ct, e.rec)
	} else {
		value, err = m.sealer.Open(e.nonce, e.ct)
		if err == nil {
			e.nonce, e.ct, err = m.sealer.SealBound(value, e.rec)
			if err == nil {
				e.version = boundKeyVersion
				m.m[keyOf(tenant, alias)] = e
			}
		}
	}
	if err != nil {
		return Record{}, "", err
	}
	return e.rec, value, nil
}

// List implements Store.
func (m *MemStore) List(_ context.Context, tenant string) ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Record, 0, len(m.m))
	for _, e := range m.m {
		if e.rec.TenantID == tenant {
			out = append(out, e.rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Delete implements Store.
func (m *MemStore) Delete(_ context.Context, tenant, alias string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.m[keyOf(tenant, alias)]; !ok {
		return ErrNotFound
	}
	delete(m.m, keyOf(tenant, alias))
	return nil
}

// Close implements Store.
func (m *MemStore) Close() error { return nil }

// ValidateValue rejects an empty or oversized credential before it is sealed.
func ValidateValue(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("secret value must not be empty")
	}
	if len(value) > MaxValueLength {
		return fmt.Errorf("secret value exceeds %d bytes", MaxValueLength)
	}
	return nil
}

// prepare validates a record and seals its value.
func prepare(sealer *Sealer, rec Record, value string) (entry, error) {
	if err := rec.Validate(); err != nil {
		return entry{}, err
	}
	if err := ValidateValue(value); err != nil {
		return entry{}, err
	}
	if rec.ID == "" {
		rec.ID = ulid.NewSecretID()
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	rec.UpdatedAt = time.Now().UTC()
	nonce, ct, err := sealer.SealBound(value, rec)
	if err != nil {
		return entry{}, err
	}
	return entry{rec: rec, nonce: nonce, ct: ct, version: boundKeyVersion}, nil
}
