// Package workspace manages the per-sandbox mutable directories and produces
// the mount specification used by runtime backends.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"

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
		if err := os.MkdirAll(d, 0o755); err != nil {
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

// Clean removes a sandbox's mutable directories and logs but keeps the root
// and config.json so a destroyed sandbox remains as a tombstone record.
func (m *Manager) Clean(id string) error {
	spec := m.Paths(id)
	for _, d := range []string{spec.WorkspaceDir, spec.HomeDir, spec.IPCDir, filepath.Join(spec.RootDir, logsDir)} {
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("workspace: clean %s: %w", d, err)
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
