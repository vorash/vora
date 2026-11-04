package runner

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
)

// execOnce runs one exec on backend and returns the exit chunk the client got
// and the exec record the audit log held at the moment that chunk was sent.
// Reading the record inside send pins persist-before-send without a race.
func execOnce(t *testing.T, backend *fakeExecBackend) (*v1.ExecExit, *v1.SandboxExec) {
	t.Helper()
	s, id := startFakeSandbox(t, backend)
	b, h, err := s.handle(id)
	if err != nil {
		t.Fatal(err)
	}
	var (
		exit *v1.ExecExit
		rec  *v1.SandboxExec
	)
	send := func(c *v1.ExecChunk) error {
		if c.GetExit() == nil {
			return nil
		}
		exit = c.GetExit()
		execs, err := s.GetSandboxExecs(context.Background(), &v1.GetSandboxExecsRequest{SandboxId: id})
		if err != nil {
			t.Fatal(err)
		}
		if len(execs.GetExecs()) != 1 {
			t.Fatalf("exec records when the exit chunk was sent = %v, want exactly one", execs.GetExecs())
		}
		rec = execs.GetExecs()[0]
		return nil
	}
	if err := s.runExec(context.Background(), id, b, h, runtime.ExecRequest{Command: "true"}, "", send); err != nil {
		t.Fatalf("runExec: %v", err)
	}
	if exit == nil {
		t.Fatal("no exit chunk was sent")
	}
	return exit, rec
}

func exitStream(code int, reason runtime.ExitReason) *fakeExecStream {
	return endedStream(runtime.ExecChunk{Exit: &runtime.ExecResult{ExitCode: code, Reason: reason}})
}

// The exit chunk is the only place a streaming client learns what its exec
// used and which audit record describes it, so both must match the record.
func TestExecExitCarriesUsageAndID(t *testing.T) {
	usage := runtime.Usage{CPUMillis: 120, MemoryPeak: 64 << 20, PeakScope: runtime.PeakWindow, PidsLimitHits: 2}
	backend := &fakeExecBackend{stream: exitStream(0, runtime.ReasonExited), meter: fakeMeter{usage: usage}}

	exit, rec := execOnce(t, backend)

	if exit.GetExecutionId() == "" || exit.GetExecutionId() != rec.GetExecutionId() {
		t.Fatalf("exit execution_id = %q, record %q: want the same, non-empty", exit.GetExecutionId(), rec.GetExecutionId())
	}
	got := exit.GetUsage()
	if got.GetCpuMillis() != 120 || got.GetMemoryPeakBytes() != 64<<20 ||
		got.GetMemoryPeakScope() != v1.PeakScope_PEAK_SCOPE_WINDOW || got.GetPidsLimitHits() != 2 || got.GetOomKills() != 0 {
		t.Fatalf("exit usage = %v, want the meter's %+v", got, usage)
	}
	if rec.GetCpuMillis() != got.GetCpuMillis() || rec.GetMemoryPeakBytes() != got.GetMemoryPeakBytes() ||
		rec.GetMemoryPeakScope() != got.GetMemoryPeakScope() || rec.GetPidsLimitHits() != got.GetPidsLimitHits() ||
		rec.GetOomKills() != got.GetOomKills() {
		t.Fatalf("exec record = %v, want the usage the exit chunk reported (%v)", rec, got)
	}
	// A pids hit on a successful exit does not make it a failure.
	if exit.GetReason() != v1.ExitReason_EXITED || rec.GetReason() != v1.ExitReason_EXITED {
		t.Fatalf("reason chunk=%v record=%v, want EXITED", exit.GetReason(), rec.GetReason())
	}
	if backend.meter.stops != 1 {
		t.Fatalf("meter stopped %d times, want once", backend.meter.stops)
	}
}

// From inside the sandbox an OOM kill looks like a process that died of
// SIGKILL, so the agent reports EXITED. The window counted the kill, and the
// client and the audit log must both be told OOM_KILLED.
func TestExecOOMDeltaBecomesOOMKilled(t *testing.T) {
	backend := &fakeExecBackend{
		stream: exitStream(137, runtime.ReasonExited),
		meter:  fakeMeter{usage: runtime.Usage{OOMKills: 1, PeakScope: runtime.PeakWindow, MemoryPeak: 1 << 20}},
	}

	exit, rec := execOnce(t, backend)

	if exit.GetReason() != v1.ExitReason_OOM_KILLED || rec.GetReason() != v1.ExitReason_OOM_KILLED {
		t.Fatalf("reason chunk=%v record=%v, want OOM_KILLED in both", exit.GetReason(), rec.GetReason())
	}
	if exit.GetExitCode() != 137 || rec.GetOomKills() != 1 {
		t.Fatalf("exit code %d, record oom_kills %d: want 137 and 1", exit.GetExitCode(), rec.GetOomKills())
	}
}

// Metering is observability, not admission: a backend that cannot open a
// window must still run the command, and must not invent a peak scope.
func TestExecRunsUnmeteredWhenMeterFails(t *testing.T) {
	backend := &fakeExecBackend{stream: exitStream(0, runtime.ReasonExited), meterErr: errors.New("cgroup gone")}

	exit, rec := execOnce(t, backend)

	if exit.GetReason() != v1.ExitReason_EXITED || exit.GetExecutionId() != rec.GetExecutionId() {
		t.Fatalf("exit = %v, want EXITED with the record's id", exit)
	}
	if exit.GetUsage().GetMemoryPeakScope() != v1.PeakScope_PEAK_SCOPE_UNSPECIFIED || rec.GetMemoryPeakScope() != v1.PeakScope_PEAK_SCOPE_UNSPECIFIED {
		t.Fatalf("scope chunk=%v record=%v, want UNSPECIFIED", exit.GetUsage().GetMemoryPeakScope(), rec.GetMemoryPeakScope())
	}
}

// A client that goes away mid-stream ends runExec on a failed send, a path no
// record covers. The window's memory.peak fd must still be closed.
func TestExecMeterStopsWhenSendFails(t *testing.T) {
	backend := &fakeExecBackend{stream: endedStream(runtime.ExecChunk{Stdout: []byte("x")})}
	s, id := startFakeSandbox(t, backend)
	b, h, err := s.handle(id)
	if err != nil {
		t.Fatal(err)
	}
	gone := errors.New("client gone")
	err = s.runExec(context.Background(), id, b, h, runtime.ExecRequest{Command: "true"}, "", func(*v1.ExecChunk) error { return gone })
	if !errors.Is(err, gone) {
		t.Fatalf("runExec = %v, want the send error", err)
	}
	if backend.meter.stops != 1 {
		t.Fatalf("meter stopped %d times, want once", backend.meter.stops)
	}
}
