package bwrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// SocketInside is where the agent control socket lives inside the sandbox.
const SocketInside = "/vora/control.sock"

// SocketDirInside is the mount point for the per-sandbox ipc directory. It
// must exist in the image rootfs.
const SocketDirInside = "/vora"

// InitInside is the agent binary path inside the sandbox. The binary is copied
// into the ipc directory, which is bound read-write at SocketDirInside so the
// agent can create its control socket there. The binary itself is then bound
// read-only over this path, so a workload cannot replace what later runs inside
// a reused sandbox.
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
	// DenyIPC has the agent add seccomp.IPCSyscalls to its filter.
	DenyIPC bool
}

const binfmtMisc = "/proc/sys/fs/binfmt_misc"

var pid1Masked = []string{"/proc/1/clear_refs", "/proc/1/task/1/clear_refs", "/proc/1/sched", "/proc/1/task/1/sched"}

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
		// Must follow the directory bind: its target only exists once /vora is
		// bound, and the other way round the sandbox does not start.
		"--ro-bind", filepath.Join(opts.IPCDir, "vora-init"), InitInside,
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		// A user namespace owns the IPC and PID namespace sysctls, so a workload could
		// write them. kernel.cad_pid is among them on kernel 7.0 yet sets the host's value.
		"--ro-bind", "/proc/sys", "/proc/sys",
	)
	// The bind above carries the host's binfmt_misc mount with it, which would show what
	// the host has registered. The target must already exist: /proc/sys is read-only now.
	// Writable, this tmpfs would keep what a run left in it, since a reset leaves /proc alone.
	if info, err := os.Stat(binfmtMisc); err == nil && info.IsDir() {
		argv = append(argv, "--tmpfs", binfmtMisc, "--remount-ro", binfmtMisc)
	}
	// PID 1 is the same uid as a workload and outlives every run, and a write to these
	// resets its page and scheduler statistics for a later run to read. No program needs
	// PID 1's, and a mask costs a reset nothing where comparing them would read its memory.
	for _, target := range pid1Masked {
		argv = append(argv, "--ro-bind", "/dev/null", target)
	}
	argv = append(argv,
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

	// The agent binary comes from the ipc directory (bound at /vora), not the
	// image, so the root fs stays genuinely read-only.
	argv = append(argv,
		"--", InitInside, "--socket", SocketInside,
	)
	if opts.DenyIPC {
		argv = append(argv, "--deny-ipc")
	}
	return argv, nil
}
