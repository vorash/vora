// Package workspace manages the per-sandbox mutable directories and produces
// the mount specification used by runtime backends.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vorash/vora/internal/runtime"
)

// Directory names inside a sandbox root.
const (
	workspaceDir = "workspace"
	homeDir      = "home"
	ipcDir       = "ipc"
	logsDir      = "logs"
	execLogDir   = "exec"
	controlSock  = "control.sock"
	// recordFile is the sandbox store's record, kept as the tombstone.
	recordFile = "config.json"
)

// Manager creates and removes sandbox directories under a root.
type Manager struct {
	root string
}

// NewManager returns a manager rooted at the sandboxes directory.
func NewManager(sandboxesDir string) *Manager {
	return &Manager{root: sandboxesDir}
}

// Root returns the sandboxes directory.
func (m *Manager) Root() string { return m.root }

// Paths returns the workspace layout for a sandbox id. RootfsDir is left empty
// and filled in by the caller from the image cache.
func (m *Manager) Paths(id string) runtime.WorkspaceSpec {
	root := filepath.Join(m.root, id)
	return runtime.WorkspaceSpec{
		RootDir:       root,
		WorkspaceDir:  filepath.Join(root, workspaceDir),
		HomeDir:       filepath.Join(root, homeDir),
		IPCDir:        filepath.Join(root, ipcDir),
		ControlSocket: filepath.Join(root, ipcDir, controlSock),
	}
}

// Create makes the sandbox directories idempotently and returns the layout.
// Paths must have been produced by Paths (RootfsDir is preserved).
func (m *Manager) Create(spec runtime.WorkspaceSpec) (runtime.WorkspaceSpec, error) {
	if spec.RootDir == "" {
		return spec, fmt.Errorf("workspace: empty root dir")
	}
	dirs := []string{
		spec.RootDir,
		spec.WorkspaceDir,
		spec.HomeDir,
		spec.IPCDir,
		filepath.Join(spec.RootDir, logsDir),
		filepath.Join(spec.RootDir, logsDir, execLogDir),
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return spec, fmt.Errorf("workspace: mkdir %s: %w", d, err)
		}
	}
	return spec, nil
}

// Destroy removes a sandbox's directories. It is idempotent.
func (m *Manager) Destroy(id string) error {
	root := filepath.Join(m.root, id)
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("workspace: destroy %s: %w", id, err)
	}
	return nil
}

// Clean reduces a sandbox directory to its tombstone: config.json (and any
// in-flight temp copy of it) plus logs/. Everything else goes, including
// backend leftovers such as a gVisor bundle/ or a bound resolv.conf. The
// workload can destroy its own sandbox, so destroy must never erase the record
// of what it ran.
func (m *Manager) Clean(id string) error {
	root := m.Paths(id).RootDir
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("workspace: clean %s: %w", root, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == recordFile || name == logsDir || strings.HasPrefix(name, "."+recordFile+".tmp-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return fmt.Errorf("workspace: clean %s: %w", name, err)
		}
	}
	return nil
}

// LogsDir returns the logs directory for a sandbox.
func (m *Manager) LogsDir(id string) string {
	return filepath.Join(m.root, id, logsDir)
}

// ExecLogDir returns the per-execution log directory for a sandbox.
func (m *Manager) ExecLogDir(id string) string {
	return filepath.Join(m.root, id, logsDir, execLogDir)
}
