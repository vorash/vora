package bwrap

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A launcher that fails during setup says why only on stderr. The runner keeps
// the end of it for the log, bounded so a chatty sandbox cannot grow runner
// memory.
func TestCaptureStderrKeepsABoundedTail(t *testing.T) {
	cmd := exec.Command("sh", "-c", `head -c 10000 /dev/zero | tr '\0' x >&2; echo "bwrap: setting up uid map: Permission denied" >&2; exit 1`)
	tail, err := captureStderr(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		tail.abort()
		t.Fatal(err)
	}
	tail.started()
	if err := cmd.Wait(); err == nil {
		t.Fatal("command succeeded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasSuffix(tail.String(), "Permission denied\n") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := tail.String()
	if !strings.HasSuffix(got, "bwrap: setting up uid map: Permission denied\n") {
		t.Fatalf("tail lost the setup error: %q", got[max(0, len(got)-80):])
	}
	if len(got) > stderrTailBytes {
		t.Fatalf("tail holds %d bytes, want at most %d", len(got), stderrTailBytes)
	}
}
