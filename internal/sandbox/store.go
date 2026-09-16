package sandbox

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
)

// ErrNotFound is returned when a sandbox record does not exist.
var ErrNotFound = errors.New("sandbox: not found")

const configFile = "config.json"

// Record is the persisted state of a sandbox.
type Record struct {
	ID        string              `json:"id"`
	Spec      runtime.SandboxSpec `json:"spec"`
	State     State               `json:"state"`
	RunnerID  string              `json:"runner_id,omitempty"`
	Reason    runtime.ExitReason  `json:"reason,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// Store persists sandbox records as one directory per sandbox under root.
type Store struct {
	root string
	mu   sync.Mutex
}

// NewStore returns a store rooted at dir.
func NewStore(dir string) *Store { return &Store{root: dir} }

// Root returns the store root directory.
func (s *Store) Root() string { return s.root }

// Dir returns the per-sandbox directory.
func (s *Store) Dir(id string) string { return filepath.Join(s.root, id) }

// Create writes a new record, creating the sandbox directory. It fails if the
// sandbox already exists.
func (s *Store) Create(rec *Record) error {
	if err := validateID(rec.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.Dir(rec.ID)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("sandbox: %s already exists", rec.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("sandbox: create dir: %w", err)
	}
	now := time.Now().UTC()
	rec.CreatedAt = now
	rec.UpdatedAt = now
	return s.write(rec)
}

// Save persists a record, updating its timestamp.
func (s *Store) Save(rec *Record) error {
	if err := validateID(rec.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.UpdatedAt = time.Now().UTC()
	return s.write(rec)
}

// write persists the record atomically (temp file + rename). Callers hold mu.
func (s *Store) write(rec *Record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("sandbox: marshal: %w", err)
	}
	dir := s.Dir(rec.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("sandbox: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+configFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("sandbox: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("sandbox: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sandbox: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sandbox: close: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, configFile)); err != nil {
		return fmt.Errorf("sandbox: rename: %w", err)
	}
	return nil
}

// Load reads a record by id.
func (s *Store) Load(id string) (*Record, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.Dir(id), configFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("sandbox: read: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("sandbox: unmarshal %s: %w", id, err)
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
		return nil, fmt.Errorf("sandbox: list: %w", err)
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

// Delete removes a sandbox directory.
func (s *Store) Delete(id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.Dir(id)); err != nil {
		return fmt.Errorf("sandbox: delete: %w", err)
	}
	return nil
}

// validateID rejects ids that could escape the store root.
func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("sandbox: empty id")
	}
	if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return fmt.Errorf("sandbox: invalid id %q", id)
	}
	return nil
}
