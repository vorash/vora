package gvisor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/vorash/vora/internal/network"
	"github.com/vorash/vora/internal/runtime"
)

// The sandbox init is Vora's own static binary, exposed by the ipc directory
// (bound at /vora), never a program from the image: a shell-less (distroless)
// image has no /bin/sh to keep the sandbox alive. See issue #40.
const (
	initInside = "/vora/vora-init"
	initMode   = "pause"
)

// writeBundle generates an OCI bundle for a sandbox.
//
// The baseline spec comes from `runsc spec` (so it matches the installed
// gVisor), then we point root at the read-only image rootfs and add the Vora
// workspace mounts. All mutable state lives in the bound workspace paths.
func writeBundle(runsc, bundleDir string, spec runtime.SandboxSpec, resolvConf string, restricted bool) error {
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
	// The init only keeps the sandbox alive; commands are run with
	// `runsc exec` (NativeExec). It comes from Vora, not from the image.
	s.Process.Args = []string{initInside, initMode}
	s.Process.Cwd = "/workspace"
	s.Process.Env = envSlice(spec.Env)

	s.Mounts = append(s.Mounts,
		bindMount(spec.Workspace.WorkspaceDir, "/workspace"),
		bindMount(spec.Workspace.HomeDir, "/root"),
		bindMount(spec.Workspace.IPCDir, "/vora"),
		tmpfsMount("/tmp"),
	)
	if resolvConf != "" {
		// Point the sandbox at the gateway resolver (ADR 012). The image must
		// carry /etc/resolv.conf; the caller checks that before starting.
		s.Mounts = append(s.Mounts, roBindMount(resolvConf, "/etc/resolv.conf"))
	}
	if restricted {
		// gVisor runs with --network host, which uses the network namespace of
		// the runsc process — the sandbox netns we launched it in. The baseline
		// OCI spec carries a network namespace entry, which would make gVisor
		// create a fresh, empty namespace instead: no veth, no route, and the
		// runner-side policy never sees a packet. Drop it so the sandbox shares
		// the prepared netns (ADR 017).
		if s.Linux != nil {
			kept := s.Linux.Namespaces[:0]
			for _, ns := range s.Linux.Namespaces {
				if ns.Type == specs.NetworkNamespace {
					continue
				}
				kept = append(kept, ns)
			}
			s.Linux.Namespaces = kept
		}
	}

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
	cmd := exec.Command(runsc, "spec", "--", initInside, initMode)
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

func roBindMount(src, dst string) specs.Mount {
	return specs.Mount{
		Destination: dst,
		Type:        "bind",
		Source:      src,
		Options:     []string{"rbind", "ro", "nosuid", "nodev"},
	}
}

// writeResolvConf points the sandbox at the gateway resolver and returns the
// host path bound over /etc/resolv.conf. A missing image mount point fails the
// start loudly rather than leaving the sandbox unable to resolve names.
func writeResolvConf(spec runtime.SandboxSpec, netns *network.Net) (string, error) {
	imageFile := filepath.Join(spec.Workspace.RootfsDir, "etc", "resolv.conf")
	if _, err := os.Stat(imageFile); err != nil {
		return "", fmt.Errorf("gvisor: image %q must provide /etc/resolv.conf for NetworkRestricted: %w", spec.Image, err)
	}
	path := filepath.Join(spec.Workspace.RootDir, "resolv.conf")
	content := fmt.Sprintf("# Managed by Vora: the sandbox gateway is the only resolver (ADR 012).\nnameserver %s\noptions timeout:2 attempts:2\n", netns.GatewayIP)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("gvisor: write resolv.conf: %w", err)
	}
	return path, nil
}

func tmpfsMount(dst string) specs.Mount {
	return specs.Mount{
		Destination: dst,
		Type:        "tmpfs",
		Source:      "tmpfs",
		Options:     []string{"nosuid", "nodev", "mode=1777"},
	}
}
