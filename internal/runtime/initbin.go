package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ResolveInitBinary finds vora-init, the binary Vora runs as PID 1 inside a
// sandbox: VORA_INIT_BINARY, then next to the running executable, then $PATH.
//
// The init must come from Vora, not from the image. The bwrap backend runs it
// as the in-sandbox agent; the gVisor backend runs it in pause mode, because a
// shell-less (distroless) image has no /bin/sh to keep the sandbox alive.
func ResolveInitBinary() (string, error) {
	if p := os.Getenv("VORA_INIT_BINARY"); p != "" {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "vora-init")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("vora-init"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("vora-init binary not found (set VORA_INIT_BINARY or build ./cmd/vora-init)")
}

// InstallInitBinary copies the init binary into the sandbox's ipc directory,
// which is bound into the sandbox at /vora so no writable root is required. The
// write is atomic so a stale binary is never executed.
func InstallInitBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read init binary: %w", err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("install init binary: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("install init binary: %w", err)
	}
	return nil
}
