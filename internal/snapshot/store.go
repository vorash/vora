// Package snapshot captures a sandbox's writable state (workspace and home)
// so that it can be forked into new sandboxes. Snapshot contents are written by
// untrusted workload code and are treated as such on restore.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/ulid"
)

// ErrNotFound is returned when a snapshot record does not exist.
var ErrNotFound = errors.New("snapshot: not found")
var ErrInvalidID = errors.New("snapshot: invalid id")

const (
	recordFile = "config.json"
	dataFile   = "data.tar"
)

// Record is the persisted state of a snapshot.
//
// Spec is the source sandbox's specification. Its Workspace paths still point
// at the source sandbox and must be replaced with the fork's own layout before
// use; only the image, resources, network, env and TTL are meaningful here.
type Record struct {
	ID        string              `json:"id"`
	SandboxID string              `json:"sandbox_id"`
	TenantID  string              `json:"tenant_id,omitempty"`
	RunnerID  string              `json:"runner_id,omitempty"`
	Spec      runtime.SandboxSpec `json:"spec"`
	Bytes     int64               `json:"bytes"`
	CreatedAt time.Time           `json:"created_at"`
}

// Store persists snapshot records as one directory per snapshot under root.
type Store struct {
	root string
	mu   sync.Mutex
}

// NewStore returns a store rooted at dir.
func NewStore(dir string) *Store { return &Store{root: dir} }

// Root returns the store root directory.
func (s *Store) Root() string { return s.root }

// Dir returns the per-snapshot directory.
func (s *Store) Dir(id string) string { return filepath.Join(s.root, id) }

// DataPath returns the archive path for a snapshot.
func (s *Store) DataPath(id string) string { return filepath.Join(s.Dir(id), dataFile) }

// Create writes a new record and its directory. It fails if the snapshot
// already exists.
func (s *Store) Create(rec *Record) error {
	if err := validateID(rec.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.Dir(rec.ID)
	if _, err := os.Stat(filepath.Join(dir, recordFile)); err == nil {
		return fmt.Errorf("snapshot: %s already exists", rec.ID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("snapshot: create dir: %w", err)
	}
	rec.CreatedAt = time.Now().UTC()
	return s.write(rec)
}

// Save persists a record.
func (s *Store) Save(rec *Record) error {
	if err := validateID(rec.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(rec)
}

// write persists the record atomically (temp file + rename). Callers hold mu.
func (s *Store) write(rec *Record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("snapshot: marshal: %w", err)
	}
	dir := s.Dir(rec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("snapshot: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+recordFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("snapshot: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("snapshot: close: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, recordFile)); err != nil {
		return fmt.Errorf("snapshot: rename: %w", err)
	}
	return nil
}

// Load reads a record by id.
func (s *Store) Load(id string) (*Record, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.Dir(id), recordFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("snapshot: read: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("snapshot: unmarshal %s: %w", id, err)
	}
	return &rec, nil
}

// List returns all records sorted by id (ULIDs sort chronologically).
func (s *Store) List() ([]*Record, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapshot: list: %w", err)
	}
	var recs []*Record
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rec, err := s.Load(e.Name())
		if err != nil {
			// Skip unreadable/partial records rather than failing the listing.
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	return recs, nil
}

// Delete removes a snapshot directory including its archive. It is idempotent.
func (s *Store) Delete(id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.Dir(id)); err != nil {
		return fmt.Errorf("snapshot: delete: %w", err)
	}
	return nil
}

// validateID rejects ids that could escape the store root.
func validateID(id string) error {
	if !ulid.ValidPrefix(id, ulid.PrefixSnapshot) {
		return fmt.Errorf("%w %q", ErrInvalidID, id)
	}
	return nil
}
