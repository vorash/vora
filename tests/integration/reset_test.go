//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/vorash/vora/internal/agentproto"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/runtime/gvisor"
	"github.com/vorash/vora/internal/sandboxreset"
	"github.com/vorash/vora/internal/workspace"
)

// planted leaves in each writable place every kind of thing a run can leave behind.
// The symlinks point at listed entries, so a reset that followed one would destroy
// them, and at /etc, which is read-only and has to stay out of the walk.
const planted = `
set -e
for d in "$@"; do
	mkdir -p "$d/.hidden/nested/deeper"
	echo run > "$d/.hidden/nested/deeper/file"
	echo run > "$d/.dotfile"
	mkdir "$d/locked"
	echo run > "$d/locked/secret"
	chmod 000 "$d/locked"
	ln -s /workspace/keep "$d/link-to-file"
	ln -s /workspace/keepdir "$d/link-to-dir"
	ln -s /etc "$d/link-out"
done
`

// plantedAttrs puts extended attributes and times on what a run planted. tmpfs needs a
// kernel recent enough for user xattrs, so only /workspace has to accept them.
const plantedAttrs = `
import os, sys
for d in sys.argv[1:]:
    path = d + "/.dotfile"
    os.utime(path, (1000, 1000))
    os.utime(d + "/.hidden", (1000, 1000))
    try:
        os.setxattr(path, "user.vora", b"run")
    except OSError:
        if d == "/workspace":
            raise
`

// fsSnapshot prints what a next run would read about each path, except for access times,
// which a read moves.
const fsSnapshot = `
import os, sys
def show(path):
    st = os.lstat(path)
    print(path, oct(st.st_mode), st.st_uid, st.st_gid, st.st_ino, st.st_mtime_ns)
for top in sys.argv[1:]:
    show(top)
    for root, dirs, files in os.walk(top):
        dirs.sort()
        for name in sorted(dirs + files):
            show(os.path.join(root, name))
`

const printPIDs = `
import json, os
print(json.dumps({"self": os.getpid(), "pids": sorted(int(p) for p in os.listdir("/proc") if p.isdigit())}))
`

var bwrapWritable = []string{"/tmp", "/workspace", "/root", "/dev/shm", "/vora"}

// execSandbox runs one command in a sandbox and returns its output and exit code.
func execSandbox(t *testing.T, backend runtime.Backend, h runtime.Handle, req runtime.ExecRequest) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req.ExecutionID = "exe_reset"
	req.Cwd = "/workspace"
	stream, err := backend.Exec(ctx, h, req)
	if err != nil {
		t.Fatalf("Exec %s: %v", req.Command, err)
	}
	defer func() { _ = stream.Close() }()
	var out, errOut strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out.String(), errOut.String(), code
		}
		if err != nil {
			t.Fatalf("Recv %s: %v", req.Command, err)
		}
		out.Write(chunk.Stdout)
		errOut.Write(chunk.Stderr)
		if chunk.Exit != nil {
			code = chunk.Exit.ExitCode
		}
	}
}

// mustExec fails the test unless the command exits 0, so a plant that did not happen
// cannot pass as a reset that cleaned it.
func mustExec(t *testing.T, backend runtime.Backend, h runtime.Handle, command string, args ...string) string {
	t.Helper()
	stdout, stderr, code := execSandbox(t, backend, h, runtime.ExecRequest{Command: command, Args: args})
	if code != 0 {
		t.Fatalf("%s %v: exit %d\nstdout: %s\nstderr: %s", command, args, code, stdout, stderr)
	}
	return stdout
}

func plant(t *testing.T, backend runtime.Backend, h runtime.Handle, dirs []string) {
	t.Helper()
	mustExec(t, backend, h, "/bin/sh", append([]string{"-c", planted, "plant"}, dirs...)...)
	mustExec(t, backend, h, "/usr/bin/python3", append([]string{"-c", plantedAttrs}, dirs...)...)
}

func snapshot(t *testing.T, backend runtime.Backend, h runtime.Handle, dirs []string) string {
	t.Helper()
	return mustExec(t, backend, h, "/usr/bin/python3", append([]string{"-c", fsSnapshot}, dirs...)...)
}

// processes returns the PIDs a command in the sandbox sees, without the command itself.
func processes(t *testing.T, backend runtime.Backend, h runtime.Handle) []int {
	t.Helper()
	var got struct {
		Self int   `json:"self"`
		PIDs []int `json:"pids"`
	}
	out := mustExec(t, backend, h, "/usr/bin/python3", "-c", printPIDs)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	return slices.DeleteFunc(got.PIDs, func(pid int) bool { return pid == got.Self })
}

// resetSandbox is a real bwrap sandbox, driven through the agent RPCs the runner uses.
type resetSandbox struct {
	backend runtime.Backend
	h       runtime.Handle
	ws      runtime.WorkspaceSpec
	client  *agentproto.Client
}

// newResetSandbox seeds the workspace from the host, because a real template is in place
// before the first capture and no exec may run before it. It denies IPC like every
// sandbox that gets reset, so these tests also prove Capture and Reset work without it.
func newResetSandbox(t *testing.T) *resetSandbox {
	t.Helper()
	return newResetSandboxWith(t, true)
}

func newResetSandboxWith(t *testing.T, denyIPC bool) *resetSandbox {
	t.Helper()
	backend, h, ws := startBwrapSandbox(t, runtime.SandboxSpec{
		Resources: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
		DenyIPC:   denyIPC,
	})
	if err := os.WriteFile(filepath.Join(ws.WorkspaceDir, "keep"), []byte("template\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws.WorkspaceDir, "keepdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.WorkspaceDir, "keepdir", "inner"), []byte("template\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	client, err := agentproto.Dial(ws.ControlSocket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.WaitReady(context.Background(), 15*time.Second); err != nil {
		t.Fatalf("agent not ready: %v", err)
	}
	return &resetSandbox{backend: backend, h: h, ws: ws, client: client}
}

func (s *resetSandbox) capture(t *testing.T) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manifest, err := s.client.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return manifest
}

func (s *resetSandbox) reset(manifest []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return s.client.Reset(ctx, manifest)
}

func decodeManifest(t *testing.T, raw []byte) sandboxreset.Manifest {
	t.Helper()
	var m sandboxreset.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

func listing(t *testing.T, m sandboxreset.Manifest, path string) sandboxreset.MountListing {
	t.Helper()
	for _, l := range m.Mounts {
		if l.Path == path {
			return l
		}
	}
	t.Fatalf("no listing for %s in %+v", path, m.Mounts)
	return sandboxreset.MountListing{}
}

func listsEntry(l sandboxreset.MountListing, rel string) bool {
	return slices.ContainsFunc(l.Entries, func(e sandboxreset.Entry) bool { return e.Rel == rel })
}

// The whole point of a warm pool: whatever a run leaves in any writable place is gone
// for the next one, and what was there before it is untouched. A run that could leave a
// file could leave a shell rc or a Python .pth, and own every later run.
func TestResetRemovesWhatARunPlanted(t *testing.T) {
	s := newResetSandbox(t)
	manifest := s.capture(t)

	m := decodeManifest(t, manifest)
	if !listsEntry(listing(t, m, "/vora"), "control.sock") {
		t.Fatal("capture does not list control.sock: it ran before the agent listened")
	}
	for _, path := range []string{"/tmp", "/workspace", "/root", "/dev"} {
		listing(t, m, path)
	}
	if !listsEntry(listing(t, m, "/dev"), "shm") {
		t.Fatal("capture does not list /dev/shm, the directory a run plants in")
	}

	before := snapshot(t, s.backend, s.h, bwrapWritable)
	threads := mustExec(t, s.backend, s.h, "/bin/sh", "-c", `grep Threads /proc/2/status`)
	plant(t, s.backend, s.h, bwrapWritable)
	if dirty := snapshot(t, s.backend, s.h, bwrapWritable); dirty == before {
		t.Fatal("planting changed nothing, so this test proves nothing")
	}

	removed, err := s.reset(manifest)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if removed == 0 {
		t.Fatal("Reset removed nothing")
	}
	t.Logf("removed %d entries", removed)

	if after := snapshot(t, s.backend, s.h, bwrapWritable); after != before {
		t.Fatalf("not back to the start state\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := mustExec(t, s.backend, s.h, "/bin/cat", "/workspace/keep", "/workspace/keepdir/inner"); got != "template\ntemplate\n" {
		t.Fatalf("listed files = %q: the reset followed a symlink", got)
	}

	// A pool member's workload budget is pids minus the agent's threads, so a reset
	// must not take any more of it.
	if got := mustExec(t, s.backend, s.h, "/bin/sh", "-c", `grep Threads /proc/2/status`); got != threads {
		t.Errorf("agent threads went from %q to %q across a reset", threads, got)
	}

	// The execs above moved access times and nothing else, so one more reset has
	// nothing to do. This is what every run after the first relies on.
	if removed, err := s.reset(manifest); err != nil || removed != 0 {
		t.Fatalf("second Reset = %d, %v, want 0 and no error", removed, err)
	}
}

// Removing a planted file moves its directory's times, so Restore writes them back,
// and a run setting them outright is treated the same.
func TestResetPutsListedDirectoryTimesBack(t *testing.T) {
	s := newResetSandbox(t)
	manifest := s.capture(t)
	before := snapshot(t, s.backend, s.h, bwrapWritable)

	mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", `
import os
for d in ("/tmp", "/workspace", "/workspace/keepdir", "/root", "/dev/shm", "/vora"):
    os.utime(d, (1000, 1000))
`)
	if _, err := s.reset(manifest); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if after := snapshot(t, s.backend, s.h, bwrapWritable); after != before {
		t.Fatalf("directory times not put back\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Restore never repairs. A listed entry a run changed cannot be made trustworthy again,
// so the reset must fail and the runner must destroy the sandbox.
func TestResetFailsOnChangedListedEntries(t *testing.T) {
	cases := map[string]string{
		"a time on a listed file":      `os.utime("/workspace/keep", (1000, 1000))`,
		"an xattr on a listed file":    `os.setxattr("/workspace/keep", "user.vora", b"run")`,
		"an xattr on a listed dir":     `os.setxattr("/workspace/keepdir", "user.vora", b"run")`,
		"new content in a listed file": `open("/workspace/keep", "w").write("template\n")`,
		"a mode on a listed file":      `os.chmod("/workspace/keep", 0o600)`,
		"a listed file deleted":        `os.remove("/workspace/keep")`,
		"a listed dir deleted":         `import shutil; shutil.rmtree("/workspace/keepdir")`,
		"a hard link to a listed file": `os.link("/workspace/keep", "/workspace/link")`,
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := newResetSandbox(t)
			manifest := s.capture(t)
			mustExec(t, s.backend, s.h, "/usr/bin/python3", "-c", "import os\n"+change)
			removed, err := s.reset(manifest)
			if err == nil {
				t.Fatalf("Reset succeeded with %d removed: %s went unnoticed", removed, name)
			}
			if !strings.Contains(err.Error(), "listed entry changed") {
				t.Fatalf("err = %v, want a changed listed entry", err)
			}
		})
	}
}

// The runner's connection outlives the socket's path, so a run that removed it would
// otherwise only show up at the next dial. The client here connected first, because a
// fresh dial would fail on its own and prove something else.
func TestResetFailsWithoutTheControlSocket(t *testing.T) {
	s := newResetSandbox(t)
	manifest := s.capture(t)
	mustExec(t, s.backend, s.h, "/bin/rm", "/vora/control.sock")
	_, err := s.reset(manifest)
	if err == nil {
		t.Fatal("Reset succeeded with the control socket gone")
	}
	if !strings.Contains(err.Error(), "listed entry changed") || !strings.Contains(err.Error(), "control.sock") {
		t.Fatalf("err = %v, want it to name control.sock", err)
	}
}

// This is the first thing to run the real guarded kill(-1), in the only place it may.
// A process left by a run survives the exec's process group kill by leaving the group,
// and it is exactly what could write into the next run.
func TestResetKillsWhatARunLeftBehind(t *testing.T) {
	s := newResetSandbox(t)
	manifest := s.capture(t)

	mustExec(t, s.backend, s.h, "/bin/sh", "-c", `
setsid sleep 300 </dev/null >/dev/null 2>&1 &
nohup sleep 300 </dev/null >/dev/null 2>&1 &
(setsid sh -c 'sleep 300 & sleep 300' </dev/null >/dev/null 2>&1 &)
`)
	if got := processes(t, s.backend, s.h); len(got) <= 2 {
		t.Fatalf("processes = %v: nothing was left running, so this test proves nothing", got)
	}

	if _, err := s.reset(manifest); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := processes(t, s.backend, s.h); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("processes after reset = %v, want only bwrap and the agent", got)
	}
}

func buildForgedReset(t *testing.T) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "forgedreset")
	cmd := exec.Command("go", "build", "-o", out, "./tests/integration/testdata/forgedreset")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build forgedreset: %v\n%s", err, b)
	}
	binary, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// A workload has the agent's uid and can reach control.sock, so it can call Reset with a
// manifest of its own. Whatever that does, the host's manifest still decides: the host's
// reset either proves the start state or fails.
func TestForgedResetGainsNothing(t *testing.T) {
	forger := buildForgedReset(t)

	setup := func(t *testing.T, s *resetSandbox, forged []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(s.ws.WorkspaceDir, "forgedreset"), forger, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.ws.WorkspaceDir, "forged.json"), forged, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The exec that started the call holds execMu, so this waits and gives up, and the
	// run has done itself no harm: the kill never happened.
	t.Run("from a foreground process it only times out", func(t *testing.T) {
		s := newResetSandbox(t)
		manifest := s.capture(t)
		setup(t, s, manifest)
		out := mustExec(t, s.backend, s.h, "/workspace/forgedreset", "/workspace/forged.json", "500ms")
		if !strings.Contains(out, "DeadlineExceeded") && !strings.Contains(out, "deadline") {
			t.Fatalf("forged call = %q, want it to time out behind its own exec", out)
		}
		if removed, err := s.reset(manifest); err != nil || removed != 2 {
			t.Fatalf("host Reset = %d, %v, want the two planted files removed", removed, err)
		}
	})

	t.Run("a garbled manifest changes nothing", func(t *testing.T) {
		s := newResetSandbox(t)
		manifest := s.capture(t)
		setup(t, s, []byte("{not json"))
		mustExec(t, s.backend, s.h, "/bin/sh", "-c", "setsid /workspace/forgedreset /workspace/forged.json 5s >/tmp/forged.out 2>&1 </dev/null &")
		time.Sleep(time.Second)
		out := mustExec(t, s.backend, s.h, "/bin/cat", "/tmp/forged.out")
		if !strings.Contains(out, "parse manifest") {
			t.Fatalf("forged call = %q, want a parse error", out)
		}
		if _, err := s.reset(manifest); err != nil {
			t.Fatalf("host Reset: %v", err)
		}
	})

	// A forged manifest that leaves out a listed file makes Restore delete that file,
	// which the workload could have done with rm. Either the forger died first and the
	// file is still there, or the host's reset sees that it is gone. What must never
	// happen is a successful host reset with the file missing.
	t.Run("a manifest that drops a listed file cannot pass the host's reset", func(t *testing.T) {
		s := newResetSandbox(t)
		manifest := s.capture(t)
		forged := decodeManifest(t, manifest)
		for i, l := range forged.Mounts {
			if l.Path == "/workspace" {
				forged.Mounts[i].Entries = slices.DeleteFunc(l.Entries, func(e sandboxreset.Entry) bool { return e.Rel == "keep" })
			}
		}
		raw, err := json.Marshal(forged)
		if err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, s.backend, s.h, bwrapWritable)
		setup(t, s, raw)
		mustExec(t, s.backend, s.h, "/bin/sh", "-c", "setsid /workspace/forgedreset /workspace/forged.json 20s >/dev/null 2>&1 </dev/null &")
		time.Sleep(time.Second)

		_, err = s.reset(manifest)
		if err != nil {
			if !strings.Contains(err.Error(), "listed entry changed") && !strings.Contains(err.Error(), "queued across another reset") {
				t.Fatalf("host Reset: %v, want a changed listed entry or the forged call in its way", err)
			}
			t.Logf("host reset refused the sandbox: %v", err)
			return
		}
		if after := snapshot(t, s.backend, s.h, bwrapWritable); after != before {
			t.Fatalf("host reset passed on a changed sandbox\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
}

// The helper modes are for gVisor, but CI can only reach them under bwrap, where they
// are also what a workload could run for itself. The agent is there, so the process
// check must refuse and nothing may be removed.
func TestHelperModesUnderBwrap(t *testing.T) {
	s := newResetSandbox(t)

	manifest, errOut, code := execSandbox(t, s.backend, s.h, runtime.ExecRequest{Command: "/vora/vora-init", Args: []string{"capture"}})
	if code != 0 {
		t.Fatalf("vora-init capture: exit %d: %s", code, errOut)
	}
	if !listsEntry(listing(t, decodeManifest(t, []byte(manifest)), "/vora"), "control.sock") {
		t.Fatal("capture does not list control.sock")
	}

	mustExec(t, s.backend, s.h, "/bin/sh", "-c", "echo run > /tmp/planted")
	stdout, stderr, code := execSandbox(t, s.backend, s.h, runtime.ExecRequest{
		Command: "/vora/vora-init",
		Args:    []string{"reset"},
		Stdin:   []byte(manifest),
	})
	if code != 1 {
		t.Fatalf("vora-init reset beside the agent: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("a failed reset printed a report: %q", stdout)
	}
	if !strings.Contains(stderr, "processes still present") {
		t.Fatalf("stderr = %q, want it to name the process check", stderr)
	}
	mustExec(t, s.backend, s.h, "/bin/cat", "/tmp/planted")

	// The helper's manifest is the agent's: the host keeps one kind of manifest.
	removed, err := s.reset([]byte(manifest))
	if err != nil {
		t.Fatalf("Reset with the helper's manifest: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want the one planted file", removed)
	}
}

// Under gVisor, nothing in the sandbox kills or reaps for the helper: the host does the
// killing and PID 1 is `vora-init pause`. This is the helper's whole path on the
// filesystems it will really meet, and the first time sandboxreset walks them. Run it
// with `make test-gvisor`, on a host with /dev/kvm.
func TestGvisorHelperCaptureAndReset(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("gVisor Method 2 requires a rootlesskit userns or root (run under rootlesskit)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	rootfs := imageRootfs(t, "base")

	cfg := config.Config{StateDir: t.TempDir(), GvisorPlatform: "kvm", GvisorRunRoot: filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "vora-gvisor-itest")}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	subtree, err := cgroup.NewSubtree()
	if err != nil {
		t.Skipf("cgroup subtree unavailable: %v", err)
	}
	backend, err := gvisor.New(cfg, logger, subtree)
	if err != nil {
		t.Skipf("gvisor backend unavailable: %v", err)
	}

	id := "sbx_" + ulid.Make().String()
	ws := workspace.NewManager(cfg.SandboxesDir())
	wsp := ws.Paths(id)
	wsp.RootfsDir = rootfs
	if _, err := ws.Create(wsp); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Destroy(id) })
	if err := os.WriteFile(filepath.Join(wsp.WorkspaceDir, "keep"), []byte("template\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(wsp.WorkspaceDir, "keepdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	h, err := backend.Create(ctx, runtime.SandboxSpec{
		ID:        id,
		Image:     "base",
		Resources: runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 256 << 20, Pids: 64},
		Network:   runtime.NetworkPolicy{Mode: runtime.NetworkNone},
		TTL:       time.Minute,
		Workspace: wsp,
		DenyIPC:   true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backend.Start(ctx, h); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), h) })

	manifest, errOut, code := execSandbox(t, backend, h, runtime.ExecRequest{Command: "/vora/vora-init", Args: []string{"capture"}})
	if code != 0 {
		t.Fatalf("vora-init capture: exit %d: %s", code, errOut)
	}
	m := decodeManifest(t, []byte(manifest))
	for _, path := range []string{"/tmp", "/workspace", "/root", "/dev"} {
		listing(t, m, path)
	}

	writable := []string{"/tmp", "/workspace", "/root", "/dev/shm"}
	before := snapshot(t, backend, h, writable)
	plant(t, backend, h, writable)

	stdout, stderr, code := execSandbox(t, backend, h, runtime.ExecRequest{Command: "/vora/vora-init", Args: []string{"reset"}, Stdin: []byte(manifest)})
	if code != 0 {
		t.Fatalf("vora-init reset: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	var report struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("report %q does not parse: %v", stdout, err)
	}
	if report.Removed == 0 {
		t.Fatalf("report = %q: nothing removed", stdout)
	}
	if after := snapshot(t, backend, h, writable); after != before {
		t.Fatalf("not back to the start state\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The host kills before the helper runs. If that ever fails to happen, the helper
	// must refuse and leave the files alone, because it has no kill of its own.
	mustExec(t, backend, h, "/bin/sh", "-c", "echo run > /tmp/planted; setsid sleep 300 </dev/null >/dev/null 2>&1 &")
	stdout, stderr, code = execSandbox(t, backend, h, runtime.ExecRequest{Command: "/vora/vora-init", Args: []string{"reset"}, Stdin: []byte(manifest)})
	if code != 1 || stdout != "" {
		t.Fatalf("vora-init reset with a survivor: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "processes still present") {
		t.Fatalf("stderr = %q, want it to name the process check", stderr)
	}
	mustExec(t, backend, h, "/bin/cat", "/tmp/planted")
}
