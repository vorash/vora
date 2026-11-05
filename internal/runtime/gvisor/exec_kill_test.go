package gvisor

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

// fakeExecRunsc stands in for runsc: `exec` reports a sandbox PID through
// -internal-pid-file and then runs for $FAKE_EXEC_SECONDS; `kill` is only logged;
// `state` prints $FAKE_STATE. $FAKE_REFUSE makes exec fail as runsc does, before any PID,
// and $FAKE_EXIT makes it report a PID and exit with that code.
const fakeExecRunsc = `#!/bin/sh
sub=""
pidfile=""
prev=""
for a in "$@"; do
  case "$a" in exec|kill|state) [ -z "$sub" ] && sub="$a" ;; esac
  [ "$prev" = "-internal-pid-file" ] && pidfile="$a"
  prev="$a"
done
if [ "$sub" = "kill" ]; then
  echo "$@" >> "$FAKE_KILL_LOG"
  [ -f "$FAKE_SLEEP_PID" ] && kill "$(cat "$FAKE_SLEEP_PID")" 2>/dev/null
  exit 0
fi
if [ "$sub" = "state" ]; then
  printf '{"status":"%s"}\n' "$FAKE_STATE"
  exit 0
fi
if [ -n "$FAKE_REFUSE" ]; then
  echo "cannot execute" >&2
  exit 128
fi
if [ -n "$FAKE_EXIT" ]; then
  echo 4242 > "$pidfile"
  exit "$FAKE_EXIT"
fi
# A process that left the command's group: "kill -pid" never reaches it.
[ -n "$FAKE_HOLDER_SECONDS" ] && { sleep "$FAKE_HOLDER_SECONDS" <&0 & }
# Like the real runsc, the command outlives this client and keeps its pipes.
sleep "$FAKE_EXEC_SECONDS" &
echo $! > "$FAKE_SLEEP_PID"
[ -n "$pidfile" ] && echo 4242 > "$pidfile"
wait
`

func fakeExecBackend(t *testing.T, execSeconds string) (*Backend, *handle, string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "runsc")
	if err := os.WriteFile(script, []byte(fakeExecRunsc), 0o755); err != nil {
		t.Fatal(err)
	}
	killLog := filepath.Join(dir, "kill.log")
	t.Setenv("FAKE_KILL_LOG", killLog)
	t.Setenv("FAKE_EXEC_SECONDS", execSeconds)
	t.Setenv("FAKE_SLEEP_PID", filepath.Join(dir, "sleep.pid"))
	hd := &handle{id: "sbx_fake", bundleDir: filepath.Join(dir, "bundle")}
	if err := os.MkdirAll(hd.bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Backend{runsc: script, root: dir, platform: "systrap", handles: map[string]*handle{hd.id: hd}}
	return b, hd, killLog
}

func lastChunk(t *testing.T, stream runtime.ExecStream) runtime.ExecChunk {
	t.Helper()
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			t.Fatal("stream ended without an exit chunk")
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Exit != nil {
			return chunk
		}
	}
}

func readKillLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// Killing the `runsc exec` client does not stop the process it started inside
// the sandbox. Reporting TIMEOUT_EXCEEDED while that process keeps running (and
// keeps using the sandbox's CPU and network) would be a false record, so the
// exec must be killed in the sandbox by the PID runsc reported.
func TestExecTimeoutKillsTheProcessInsideTheSandbox(t *testing.T) {
	b, hd, killLog := fakeExecBackend(t, "30")
	stream, err := b.Exec(t.Context(), runtime.Handle(fakeHandle{hd.id}), runtime.ExecRequest{Command: "sleep", Args: []string{"60"}, Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	start := time.Now()
	if got := lastChunk(t, stream).Exit.Reason; got != runtime.ReasonTimeout {
		t.Fatalf("reason = %s, want %s", got, runtime.ReasonTimeout)
	}
	// The command holds the pipes for 30s; only killing it inside the sandbox
	// lets the stream finish in time.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("exit chunk took %v: the command was not killed at the timeout", elapsed)
	}
	got := readKillLog(t, killLog)
	if !strings.Contains(got, "kill") || !strings.Contains(got, "-pid 4242") || !strings.Contains(got, "KILL") || !strings.Contains(got, hd.id) {
		t.Fatalf("runsc kill log = %q, want a KILL of pid 4242 in %s", got, hd.id)
	}
}

func TestExecCancelKillsTheProcessInsideTheSandbox(t *testing.T) {
	b, hd, killLog := fakeExecBackend(t, "30")
	stream, err := b.Exec(t.Context(), runtime.Handle(fakeHandle{hd.id}), runtime.ExecRequest{Command: "sleep", Args: []string{"60"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let runsc report the PID
	stream.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(readKillLog(t, killLog), "-pid 4242") {
		if time.Now().After(deadline) {
			t.Fatal("cancelling the exec never killed its process in the sandbox")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A workload must not outlive its timeout by moving a process out of the group
// the kill reaches: that process keeps the pipes, which used to hold the stream
// open until it exited. The stdin case also exercises cmd.Wait's stdin copy, which
// a holder of stdin can block when the input exceeds the pipe buffer.
func TestExecTimeoutIgnoresEscapedPipeHolder(t *testing.T) {
	for name, stdin := range map[string][]byte{"no stdin": nil, "large stdin": make([]byte, 1<<20)} {
		t.Run(name, func(t *testing.T) {
			b, hd, _ := fakeExecBackend(t, "30")
			t.Setenv("FAKE_HOLDER_SECONDS", "10")
			stream, err := b.Exec(t.Context(), runtime.Handle(fakeHandle{hd.id}), runtime.ExecRequest{Command: "sleep", Args: []string{"60"}, Stdin: stdin, Timeout: 300 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			start := time.Now()
			if got := lastChunk(t, stream).Exit.Reason; got != runtime.ReasonTimeout {
				t.Fatalf("reason = %s, want %s", got, runtime.ReasonTimeout)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("exit chunk took %v: the stream waited for the escaped process", elapsed)
			}
		})
	}
}

func TestExecThatFinishesIsNotKilled(t *testing.T) {
	b, hd, killLog := fakeExecBackend(t, "0")
	stream, err := b.Exec(t.Context(), runtime.Handle(fakeHandle{hd.id}), runtime.ExecRequest{Command: "true", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if got := lastChunk(t, stream).Exit.Reason; got != runtime.ReasonExited {
		t.Fatalf("reason = %s, want %s", got, runtime.ReasonExited)
	}
	if got := readKillLog(t, killLog); got != "" {
		t.Fatalf("a finished exec was killed: %q", got)
	}
}

// An exec into a stopped sandbox, or one whose sandbox died under it, used to read as the
// program exiting 128, which a caller counts as the program's own result (#120). The rows
// next to it must keep their reason: a program may exit 128 in a live sandbox, and a command
// that is missing is the caller's mistake.
func TestExecReasonWhenRunscExits128(t *testing.T) {
	for _, tc := range []struct {
		name          string
		refuse, state string
		exit          string
		want          runtime.ExitReason
	}{
		{name: "sandbox stopped", refuse: "1", state: "stopped", want: runtime.ReasonRuntimeFailure},
		{name: "missing command in a running sandbox", refuse: "1", state: "running", want: runtime.ReasonExited},
		{name: "sandbox dies after the command started", exit: "128", state: "stopped", want: runtime.ReasonRuntimeFailure},
		{name: "program exits 128", exit: "128", state: "running", want: runtime.ReasonExited},
		{name: "program exits 3", exit: "3", state: "stopped", want: runtime.ReasonExited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, hd, _ := fakeExecBackend(t, "0")
			t.Setenv("FAKE_REFUSE", tc.refuse)
			t.Setenv("FAKE_EXIT", tc.exit)
			t.Setenv("FAKE_STATE", tc.state)
			stream, err := b.Exec(t.Context(), runtime.Handle(fakeHandle{hd.id}), runtime.ExecRequest{Command: "true"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			if got := lastChunk(t, stream).Exit.Reason; got != tc.want {
				t.Fatalf("reason = %s, want %s", got, tc.want)
			}
		})
	}
}

type fakeHandle struct{ id string }

func (h fakeHandle) ID() string { return h.id }
