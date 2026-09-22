package runner

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/vorash/vora/internal/snapshot"
)

// writeArchive archives dirs into the file at path, writing via a temp file
// and rename so a crash never leaves a half-written archive in place.
func writeArchive(path string, dirs snapshot.Folders) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".data.tar.tmp-*")
	if err != nil {
		return 0, fmt.Errorf("snapshot: temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	n, err := snapshot.Archive(tmp, dirs)
	if err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, fmt.Errorf("snapshot: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("snapshot: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, fmt.Errorf("snapshot: rename: %w", err)
	}
	return n, nil
}

// restoreArchive extracts the snapshot archive at path into dirs.
func restoreArchive(path string, dirs snapshot.Folders) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("snapshot: open archive: %w", err)
	}
	defer f.Close()
	return snapshot.Restore(f, dirs)
}
