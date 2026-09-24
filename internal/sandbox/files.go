package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/vorash/vora/internal/runtime"
)

// MaxTransferBytes caps a single files-API read or write. It is enforced by
// the runner and by the control plane so neither buffers an unbounded file.
const MaxTransferBytes = 16 << 20 // 16 MiB

// ErrPathOutsideWritableRoots rejects a sandbox path that is not under a
// durable, writable directory.
var ErrPathOutsideWritableRoots = errors.New("sandbox: path is outside the writable roots /workspace and /root")

// ErrFileTooLarge rejects a transfer that exceeds MaxTransferBytes.
var ErrFileTooLarge = errors.New("sandbox: file exceeds the transfer limit")

// ResolveFilePath maps an absolute sandbox path to the host directory that
// backs it and the path relative to that directory. Only the durable writable
// roots are served: /workspace (WorkspaceDir) and /root (HomeDir). The
// read-only image rootfs and the ephemeral /tmp tmpfs are deliberately not
// reachable, so a files call can never mutate the image or reach host paths.
func ResolveFilePath(spec runtime.WorkspaceSpec, sandboxPath string) (rootDir, rel string, err error) {
	if sandboxPath == "" {
		return "", "", errors.New("sandbox: path is required")
	}
	if !strings.HasPrefix(sandboxPath, "/") {
		return "", "", errors.New("sandbox: path must be absolute")
	}
	clean := path.Clean(sandboxPath)
	switch {
	case clean == "/workspace":
		return spec.WorkspaceDir, ".", nil
	case strings.HasPrefix(clean, "/workspace/"):
		return spec.WorkspaceDir, strings.TrimPrefix(clean, "/workspace/"), nil
	case clean == "/root":
		return spec.HomeDir, ".", nil
	case strings.HasPrefix(clean, "/root/"):
		return spec.HomeDir, strings.TrimPrefix(clean, "/root/"), nil
	default:
		return "", "", fmt.Errorf("%w: %s", ErrPathOutsideWritableRoots, sandboxPath)
	}
}

// ReadFile reads one sandbox file through a traversal-safe root. Symbolic
// links that escape the writable root are refused by os.Root.
func ReadFile(spec runtime.WorkspaceSpec, sandboxPath string) ([]byte, error) {
	dir, rel, err := ResolveFilePath(spec, sandboxPath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("sandbox: not a regular file: %s", sandboxPath)
	}

	data, err := io.ReadAll(io.LimitReader(f, MaxTransferBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxTransferBytes {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrFileTooLarge, MaxTransferBytes)
	}
	return data, nil
}

// WriteFile writes one sandbox file through a traversal-safe root. It refuses
// content larger than MaxTransferBytes before touching the filesystem.
func WriteFile(spec runtime.WorkspaceSpec, sandboxPath string, content []byte, createParents bool) (int64, error) {
	if len(content) > MaxTransferBytes {
		return 0, fmt.Errorf("%w: limit is %d bytes", ErrFileTooLarge, MaxTransferBytes)
	}
	dir, rel, err := ResolveFilePath(spec, sandboxPath)
	if err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()

	if createParents {
		if parent := path.Dir(rel); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return 0, err
			}
		}
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return 0, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return 0, fmt.Errorf("sandbox: not a regular file: %s", sandboxPath)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return 0, err
	}
	n, err := f.Write(content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return int64(n), err
}
