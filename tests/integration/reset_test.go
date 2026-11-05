//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sys/unix"

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

// forgedOut is where a background attack's output goes.
const forgedOut = "/tmp/forged.out"

// startForged starts an attack the way a workload that outlives its exec would, so no
// call of the host's is holding execMu behind it.
func (s *resetSandbox) startForged(t *testing.T, args ...string) {
	t.Helper()
	mustExec(t, s.backend, s.h, "/bin/sh", append([]string{"-c", `setsid /workspace/forgedreset "$@" >` + forgedOut + ` 2>&1 </dev/null &`, "forge"}, args...)...)
}

// waitForOutput returns what read returns once it contains want. An attack that never
// gets there fails the test, so one that broke cannot pass as a refusal.
func waitForOutput(t *testing.T, read func() string, want string) string {
	t.Helper()
	var got string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		got = read()
		if strings.Contains(got, want) {
			return got
		}
	}
	t.Fatalf("waited for %q, got %q", want, got)
	return ""
}

// waitForged waits on an attack's result file. It reads forgedOut as well, so a timeout
// shows why an attack that writes elsewhere never got going.
func (s *resetSandbox) waitForged(t *testing.T, path, want string) string {
	t.Helper()
	return waitForOutput(t, func() string {
		return mustExec(t, s.backend, s.h, "/bin/sh", "-c", `cat "$1" "$2" 2>/dev/null || true`, "read", path, forgedOut)
	}, want)
}

// forgedRaw runs a raw attack in the foreground and fails unless the agent closed the
// connection without a byte of HTTP/2.
func (s *resetSandbox) forgedRaw(t *testing.T, args ...string) {
	t.Helper()
	out := mustExec(t, s.backend, s.h, "/workspace/forgedreset", args...)
	if !strings.Contains(out, "closed: ") {
		t.Fatalf("%v = %q, want the connection closed unanswered", args, out)
	}
}

// assertHealthy is what a refused attack must leave behind: the host still drives the
// sandbox through every call, the connection it had from the start included.
func (s *resetSandbox) assertHealthy(t *testing.T, manifest []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if pid, err := s.client.Ping(ctx); err != nil || pid != 2 {
		t.Fatalf("host Ping = %d, %v, want the agent at PID 2", pid, err)
	}
	if got := mustExec(t, s.backend, s.h, "/bin/echo", "alive"); got != "alive\n" {
		t.Fatalf("host Exec = %q", got)
	}
	s.capture(t)
	if _, err := s.reset(manifest); err != nil {
		t.Fatalf("host Reset: %v", err)
	}
	if got := processes(t, s.backend, s.h); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("processes after the host's reset = %v, want only bwrap and the agent", got)
	}
}

// A workload has the agent's uid and can reach control.sock, so only the agent can keep
// it from calling Exec, Reset and the rest. Each of these attacks must be refused and
// leave the sandbox healthy, from the host's point of view and in fact.
func TestForgedResetGainsNothing(t *testing.T) {
	forger := buildForgedReset(t)

	// The binary is in place before the capture, as a template's files are.
	setup := func(t *testing.T) (*resetSandbox, []byte) {
		t.Helper()
		s := newResetSandbox(t)
		if err := os.WriteFile(filepath.Join(s.ws.WorkspaceDir, "forgedreset"), forger, 0o755); err != nil {
			t.Fatal(err)
		}
		return s, s.capture(t)
	}

	// An RPC that got through shows in its output and also in what it did: Exec leaves
	// the marker, Reset removes the planted file and kills this process, Shutdown and
	// Signal with SIGKILL end the sandbox.
	t.Run("every RPC is refused", func(t *testing.T) {
		s, manifest := setup(t)
		if err := os.WriteFile(filepath.Join(s.ws.WorkspaceDir, "forged.json"), manifest, 0o644); err != nil {
			t.Fatal(err)
		}
		mustExec(t, s.backend, s.h, "/bin/sh", "-c", "echo run > /tmp/planted")
		for _, rpc := range []string{"Ping", "Exec", "Signal", "Capture", "Reset", "Shutdown"} {
			mustExec(t, s.backend, s.h, "/bin/sh", "-c",
				"setsid /workspace/forgedreset rpc "+rpc+" /workspace/forged.json /tmp/forged-exec >/tmp/forged."+rpc+" 2>&1 </dev/null &")
			out := s.waitForged(t, "/tmp/forged."+rpc, rpc+" ")
			if !strings.Contains(out, rpc+" refused: ") {
				t.Fatalf("forged %s = %q", rpc, out)
			}
		}
		if out := mustExec(t, s.backend, s.h, "/bin/sh", "-c", "ls /tmp/forged-exec 2>&1; cat /tmp/planted"); !strings.Contains(out, "No such file") || !strings.Contains(out, "run") {
			t.Fatalf("forged calls left %q, want no marker and the planted file intact", out)
		}
		s.assertHealthy(t, manifest)
	})

	t.Run("a raw call on the socket gets no reply", func(t *testing.T) {
		s, manifest := setup(t)
		s.forgedRaw(t, "raw", "/vora/control.sock")
		s.assertHealthy(t, manifest)
	})

	// What the agent reads is the task that connected, however the bytes arrive. Both
	// kinds of socket a relay could offer are tried, since abstract ones have no path to
	// protect.
	for _, relay := range []string{"/tmp/relay.sock", "@vora-relay"} {
		t.Run("a relay is still a peer inside the sandbox: "+relay, func(t *testing.T) {
			s, manifest := setup(t)
			s.startForged(t, "relay", relay)
			s.waitForged(t, forgedOut, "ready")
			s.forgedRaw(t, "raw", relay)
			s.assertHealthy(t, manifest)
		})
	}

	// SO_PEERCRED reads 0 for a task the namespace cannot see, so a kernel that read a
	// dead task the same way would let anyone in. The call sits in the socket and the
	// connection stays open, so an agent that took it would run the Shutdown.
	t.Run("a peer that is gone before the accept is refused", func(t *testing.T) {
		s, manifest := setup(t)
		s.startForged(t, "orphan", "/tmp/orphan.out")
		out := s.waitForged(t, "/tmp/orphan.out", "closed: ")
		if !strings.Contains(out, "connector gone: true") {
			t.Fatalf("orphan = %q: the connector was still there, so this proved nothing", out)
		}
		s.assertHealthy(t, manifest)
	})

	// The sandbox has no capability to make a PID namespace, and if it could, the agent's
	// namespace is an ancestor of the new one and sees the process.
	t.Run("a nested PID namespace does not read as the host", func(t *testing.T) {
		s, manifest := setup(t)
		out := mustExec(t, s.backend, s.h, "/workspace/forgedreset", "nested")
		t.Logf("nested: %s", out)
		if !strings.Contains(out, "namespace denied: ") && !strings.Contains(out, "closed: ") {
			t.Fatalf("nested = %q", out)
		}
		s.assertHealthy(t, manifest)
	})

	t.Run("a flood costs the agent nothing and the host keeps working", func(t *testing.T) {
		s, manifest := setup(t)
		// The socket count covers every Unix socket in the sandbox's network namespace,
		// so a refused connection the agent kept would show in it.
		const held = `grep Threads /proc/2/status; grep -c . /proc/net/unix`
		before := mustExec(t, s.backend, s.h, "/bin/sh", "-c", held)
		t.Logf("before the flood: %q, %s, idle Ping: %s", before, agentRSS(t, s), describeLatency(s.pingLatency(t, 200)))

		s.startForged(t, "flood", "8", "5s")
		var pings, execs, dials []time.Duration
		// While it lasts, every call the runner makes must work: on the connection it
		// has, as a new exec, and as a new connection. The bounds are the runner's own:
		// 2s for a keepalive Ping and 15s for an agent to become ready.
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			pings = append(pings, s.pingLatency(t, 20)...)
			start := time.Now()
			if got := mustExec(t, s.backend, s.h, "/bin/echo", "alive"); got != "alive\n" {
				t.Fatalf("host Exec during the flood = %q", got)
			}
			execs = append(execs, time.Since(start))
			start = time.Now()
			fresh, err := agentproto.Dial(s.ws.ControlSocket)
			if err != nil {
				t.Fatalf("new host connection during the flood: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err = fresh.WaitReady(ctx, 15*time.Second)
			cancel()
			_ = fresh.Close()
			if err != nil {
				t.Fatalf("new host connection during the flood: %v", err)
			}
			dials = append(dials, time.Since(start))
		}
		t.Logf("during the flood: Ping %s; Exec %s; new connection %s", describeLatency(pings), describeLatency(execs), describeLatency(dials))
		if slowest := slices.Max(pings); slowest > 2*time.Second {
			t.Fatalf("slowest host Ping during the flood took %v", slowest)
		}

		out := s.waitForged(t, forgedOut, "flood done")
		t.Logf("%s", out)
		if strings.Contains(out, "connected 0 ") {
			t.Fatalf("flood = %q: nothing connected, so this proved nothing", out)
		}
		// Polled because closed sockets leave the list a moment after the close.
		waitForOutput(t, func() string { return mustExec(t, s.backend, s.h, "/bin/sh", "-c", held) }, before)
		t.Logf("after the flood: %s", agentRSS(t, s))
		s.assertHealthy(t, manifest)
	})

	// The runner's connection outlives the path, so a replacement does nothing until it
	// has to dial again. Here it has to: the workload's listener gets the connection, and
	// the dialer must drop it before gRPC sends a byte, or the workload could forge exec
	// results and a clean reset.
	t.Run("the runner does not talk to a replaced socket", func(t *testing.T) {
		s, _ := setup(t)
		s.startForged(t, "replace", "/workspace/replaced.out")
		s.waitForged(t, "/workspace/replaced.out", "ready")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.client.Ping(ctx); err != nil {
			t.Fatalf("host Ping on its existing connection: %v", err)
		}
		if got := mustExec(t, s.backend, s.h, "/bin/echo", "alive"); got != "alive\n" {
			t.Fatalf("host Exec on its existing connection = %q", got)
		}

		dropAgentConnections(t)
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			pingCtx, cancelPing := context.WithTimeout(context.Background(), time.Second)
			_, err := s.client.Ping(pingCtx)
			cancelPing()
			if err == nil {
				t.Fatal("host Ping succeeded through a replaced socket")
			}
		}
		execCtx, cancelExec := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelExec()
		if stream, err := s.backend.Exec(execCtx, s.h, runtime.ExecRequest{ExecutionID: "exe_reset", Command: "/bin/echo", Args: []string{"forged"}}); err == nil {
			_, err = stream.Recv()
			_ = stream.Close()
			if err == nil {
				t.Fatal("the backend's Exec got a reply through a replaced socket")
			}
		}

		// The agent is out of reach by now, so this reads from the host's side.
		got := waitForOutput(t, func() string {
			got, _ := os.ReadFile(filepath.Join(s.ws.WorkspaceDir, "replaced.out"))
			return string(got)
		}, "connection sent")
		if strings.Contains(strings.ReplaceAll(got, "connection sent 0 bytes", ""), "connection sent") {
			t.Fatalf("the runner sent bytes to a replaced socket:\n%s", got)
		}
	})

	t.Run("a reset fails on a replaced socket", func(t *testing.T) {
		s, manifest := setup(t)
		s.startForged(t, "replace", "/tmp/replaced.out")
		s.waitForged(t, "/tmp/replaced.out", "ready")
		_, err := s.reset(manifest)
		if err == nil {
			t.Fatal("Reset succeeded with the control socket replaced")
		}
		if !strings.Contains(err.Error(), "listed entry changed") || !strings.Contains(err.Error(), "control.sock") {
			t.Fatalf("err = %v, want it to name control.sock", err)
		}
	})
}

// pingLatency times n Pings on the host's existing connection.
func (s *resetSandbox) pingLatency(t *testing.T, n int) []time.Duration {
	t.Helper()
	took := make([]time.Duration, 0, n)
	for range n {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		start := time.Now()
		_, err := s.client.Ping(ctx)
		cancel()
		if err != nil {
			t.Fatalf("host Ping: %v", err)
		}
		took = append(took, time.Since(start))
	}
	return took
}

func describeLatency(took []time.Duration) string {
	sorted := slices.Sorted(slices.Values(took))
	return fmt.Sprintf("n=%d median=%v p99=%v max=%v", len(sorted), sorted[len(sorted)/2], sorted[len(sorted)*99/100], sorted[len(sorted)-1])
}

func agentRSS(t *testing.T, s *resetSandbox) string {
	t.Helper()
	return strings.TrimSpace(mustExec(t, s.backend, s.h, "/bin/grep", "VmRSS", "/proc/2/status"))
}

// dropAgentConnections cuts every connection this process has to an agent, which makes
// gRPC dial again as it would after any transport failure. The peer's name is the path
// the agent bound inside its sandbox.
func dropAgentConnections(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	dropped := 0
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		peer, err := unix.Getpeername(fd)
		if err != nil {
			continue
		}
		if name, ok := peer.(*unix.SockaddrUnix); ok && name.Name == "/vora/control.sock" {
			if err := unix.Shutdown(fd, unix.SHUT_RDWR); err != nil {
				t.Fatal(err)
			}
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatal("no connection to an agent to drop")
	}
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
