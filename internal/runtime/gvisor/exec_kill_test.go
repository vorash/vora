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
// -internal-pid-file and then runs for $FAKE_EXEC_SECONDS; `kill` is only logged.
const fakeExecRunsc = `#!/bin/sh
sub=""
pidfile=""
prev=""
for a in "$@"; do
  case "$a" in exec|kill) [ -z "$sub" ] && sub="$a" ;; esac
  [ "$prev" = "-internal-pid-file" ] && pidfile="$a"
  prev="$a"
done
if [ "$sub" = "kill" ]; then
  echo "$@" >> "$FAKE_KILL_LOG"
  [ -f "$FAKE_SLEEP_PID" ] && kill "$(cat "$FAKE_SLEEP_PID")" 2>/dev/null
  exit 0
fi
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

type fakeHandle struct{ id string }

func (h fakeHandle) ID() string { return h.id }
