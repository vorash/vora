package bwrap

import (
	"fmt"
	"sort"
)

// SocketInside is where the agent control socket lives inside the sandbox.
const SocketInside = "/vora/control.sock"

// SocketDirInside is the mount point for the per-sandbox ipc directory. It
// must exist in the image rootfs.
const SocketDirInside = "/vora"

// InitInside is the agent binary path inside the sandbox. The binary is copied
// into the ipc directory, which is bound read-write at SocketDirInside, so no
// extra mount point (and no writable root) is required.
const InitInside = "/vora/vora-init"

// BuildOptions are the inputs for the Bubblewrap command line.
type BuildOptions struct {
	RootfsDir    string
	WorkspaceDir string
	HomeDir      string
	IPCDir       string
	Hostname     string
	Env          map[string]string
	// ShareNet keeps the current network namespace instead of unsharing it.
	// The caller must have placed the process in a prepared netns (see the
	// NetworkRestricted path and docs/adr/012-egress-gateway.md).
	ShareNet bool
	// ResolvConf is a host file bound read-only over /etc/resolv.conf so the
	// sandbox resolves names through the gateway resolver. The image must
	// provide the mount point.
	ResolvConf string
}

// Args builds the bubblewrap argv for a sandbox. It is pure so it can be
// unit-tested without a running sandbox.
//
// Isolation flags:
//
//	--unshare-all  user, pid, net, ipc, uts and cgroup namespaces
//	--die-with-parent
//	--new-session  prevent TIOCSTI injection from the sandbox
//
// Filesystem:
//
//	rootfs is bound read-only at / and can be shared between sandboxes;
//	only /workspace, /root and /tmp are writable.
func Args(opts BuildOptions) ([]string, error) {
	if opts.RootfsDir == "" {
		return nil, fmt.Errorf("bwrap: rootfs is required")
	}
	if opts.WorkspaceDir == "" {
		return nil, fmt.Errorf("bwrap: workspace is required")
	}
	if opts.IPCDir == "" {
		return nil, fmt.Errorf("bwrap: ipc dir is required")
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname = "vora"
	}

	argv := []string{
		"bwrap",
		"--unshare-all",
		"--unshare-user",
		"--disable-userns",
		"--die-with-parent",
		"--new-session",
		"--hostname", hostname,
	}
	if opts.ShareNet {
		// Retain the prepared network namespace; egress is controlled by the
		// sandbox's nftables DNAT to the egress proxy.
		argv = append(argv, "--share-net")
	}
	argv = append(argv,
		// Read-only shared root fs.
		"--ro-bind", opts.RootfsDir, "/",
	)
	if opts.ResolvConf != "" {
		// Point the sandbox at the gateway resolver (ADR 012). The image must
		// carry /etc/resolv.conf; the caller checks that before starting.
		argv = append(argv, "--ro-bind", opts.ResolvConf, "/etc/resolv.conf")
	}
	argv = append(argv,
		// Per-sandbox writable mounts.
		"--bind", opts.WorkspaceDir, "/workspace",
		"--bind", opts.HomeDir, "/root",
		"--bind", opts.IPCDir, SocketDirInside,
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
		"--chdir", "/workspace",
		"--setenv", "HOME", "/root",
		"--setenv", "PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	)

	// Deterministic extra environment.
	keys := make([]string, 0, len(opts.Env))
	for k := range opts.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--setenv", k, opts.Env[k])
	}

	// The agent binary lives in the ipc directory (bound at /vora) and runs
	// as PID 1. No separate file bind is needed, which keeps the root fs
	// genuinely read-only.
	argv = append(argv,
		"--", InitInside, "--socket", SocketInside,
	)
	return argv, nil
}
