package bwrap

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// stderrTailBytes bounds how much of a sandbox launcher's stderr the runner
// keeps: enough for bwrap's or nsenter's setup error, never the workload's
// output.
const stderrTailBytes = 4096

// stderrTail keeps the last stderrTailBytes written to it.
type stderrTail struct {
	mu  sync.Mutex
	buf []byte
	w   *os.File
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - stderrTailBytes; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// captureStderr points cmd's stderr at a pipe drained into a stderrTail. A
// pipe, not an io.Writer, so cmd.Wait does not wait for sandbox processes that
// inherited stderr to close it. Call started after cmd.Start, or abort if it
// failed.
func captureStderr(cmd *exec.Cmd) (*stderrTail, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("bwrap: stderr pipe: %w", err)
	}
	t := &stderrTail{w: w}
	cmd.Stderr = w
	go func() {
		_, _ = io.Copy(t, r)
		_ = r.Close()
	}()
	return t, nil
}

// started closes the runner's copy of the write end, so the reader sees EOF
// once the sandbox side closes.
func (t *stderrTail) started() { _ = t.w.Close() }

// abort releases the pipe when the command never started.
func (t *stderrTail) abort() { _ = t.w.Close() }
