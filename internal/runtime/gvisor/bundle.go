package gvisor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/vorash/vora/internal/runtime"
)

// writeBundle generates an OCI bundle for a sandbox.
//
// The baseline spec comes from `runsc spec` (so it matches the installed
// gVisor), then we point root at the read-only image rootfs and add the Vora
// workspace mounts. All mutable state lives in the bound workspace paths.
func writeBundle(runsc, bundleDir string, spec runtime.SandboxSpec) error {
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return fmt.Errorf("gvisor: create bundle dir: %w", err)
	}
	if err := runscSpec(runsc, bundleDir); err != nil {
		return err
	}

	path := filepath.Join(bundleDir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("gvisor: read generated spec: %w", err)
	}
	var s specs.Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("gvisor: parse generated spec: %w", err)
	}

	s.Hostname = "vora"
	s.Root = &specs.Root{Path: spec.Workspace.RootfsDir, Readonly: true}
	if s.Process == nil {
		s.Process = &specs.Process{}
	}
	s.Process.Terminal = false
	// The init process only keeps the sandbox alive; commands are run with
	// `runsc exec` (NativeExec).
	s.Process.Args = []string{"/bin/sh", "-c", "sleep 86400"}
	s.Process.Cwd = "/workspace"
	s.Process.Env = envSlice(spec.Env)

	s.Mounts = append(s.Mounts,
		bindMount(spec.Workspace.WorkspaceDir, "/workspace"),
		bindMount(spec.Workspace.HomeDir, "/root"),
		bindMount(spec.Workspace.IPCDir, "/vora"),
		tmpfsMount("/tmp"),
	)

	out, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("gvisor: marshal spec: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("gvisor: write spec: %w", err)
	}
	return nil
}

// runscSpec writes a baseline OCI spec into dir.
func runscSpec(runsc, dir string) error {
	cmd := exec.Command(runsc, "spec", "--", "/bin/sh", "-c", "sleep 86400")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gvisor: runsc spec: %w: %s", err, out)
	}
	return nil
}

func envSlice(env map[string]string) []string {
	out := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func bindMount(src, dst string) specs.Mount {
	return specs.Mount{
		Destination: dst,
		Type:        "bind",
		Source:      src,
		Options:     []string{"rbind", "rw", "nosuid", "nodev"},
	}
}

func tmpfsMount(dst string) specs.Mount {
	return specs.Mount{
		Destination: dst,
		Type:        "tmpfs",
		Source:      "tmpfs",
		Options:     []string{"nosuid", "nodev", "mode=1777"},
	}
}
