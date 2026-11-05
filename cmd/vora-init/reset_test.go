package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/sandboxreset"
)

// fakeSandbox replaces everything a reset does to the machine. No test in this file
// can kill or remove anything, because a resetter's fields are all it can reach.
type fakeSandbox struct {
	calls []string

	killErr      error
	pids         [][]int // one list per poll, the last one repeating
	listErr      error
	mountinfoErr error
	captureErr   error
	restoreErr   error
	removed      int

	restored     []sandboxreset.Manifest
	mountinfos   []string
	opened       int
	restoreBlock func(context.Context) error
}

const fakeSelf = 57

func (f *fakeSandbox) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeSandbox) resetter() *resetter {
	return &resetter{
		timeout: 5 * time.Second,
		settle:  200 * time.Millisecond,
		self:    fakeSelf,
		killOthers: func() error {
			f.record("kill")
			return f.killErr
		},
		reap: func() { f.record("reap") },
		listPIDs: func() ([]int, error) {
			f.record("list")
			if f.listErr != nil {
				return nil, f.listErr
			}
			if len(f.pids) == 0 {
				return []int{initPID, fakeSelf}, nil
			}
			next := f.pids[0]
			if len(f.pids) > 1 {
				f.pids = f.pids[1:]
			}
			return slices.Clone(next), nil
		},
		openMountinfo: func() (io.ReadCloser, error) {
			f.record("mountinfo")
			if f.mountinfoErr != nil {
				return nil, f.mountinfoErr
			}
			f.opened++
			return io.NopCloser(strings.NewReader(fmt.Sprintf("table %d", f.opened))), nil
		},
		capture: func(_ context.Context, mountinfo io.Reader) (sandboxreset.Manifest, error) {
			f.record("capture")
			table, _ := io.ReadAll(mountinfo)
			f.mountinfos = append(f.mountinfos, string(table))
			if f.captureErr != nil {
				return sandboxreset.Manifest{}, f.captureErr
			}
			return sandboxreset.Manifest{Mounts: []sandboxreset.MountListing{{Path: "/tmp"}}}, nil
		},
		restore: func(ctx context.Context, m sandboxreset.Manifest, mountinfo io.Reader) (int, error) {
			f.record("restore")
			table, _ := io.ReadAll(mountinfo)
			f.mountinfos = append(f.mountinfos, string(table))
			f.restored = append(f.restored, m)
			if f.restoreBlock != nil {
				if err := f.restoreBlock(ctx); err != nil {
					return 0, err
				}
			}
			return f.removed, f.restoreErr
		},
	}
}

func (f *fakeSandbox) agent() *agent {
	a := testAgent()
	a.resetter = f.resetter()
	return a
}

const goodManifest = `{"table":[],"mounts":[{"path":"/workspace","entries":[]}]}`

// Restore removes files on the promise that nothing can write behind it, so it must
// come after the kill and after the proof, and the proof must see the host's mount
// table fresh.
func TestResetKillsThenProvesThenRestores(t *testing.T) {
	f := &fakeSandbox{removed: 3}
	removed, err := f.agent().Reset(context.Background(), []byte(goodManifest))
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}
	want := []string{"kill", "reap", "list", "mountinfo", "restore"}
	if got := f.calls; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if len(f.restored) != 1 || f.restored[0].Mounts[0].Path != "/workspace" {
		t.Fatalf("restored %+v: not the manifest the host sent", f.restored)
	}
	if f.mountinfos[0] != "table 1" {
		t.Fatalf("restore read %q, not the table the mountinfo source returned", f.mountinfos[0])
	}
}

// Any failure must reach the caller as an error, since the runner reuses a sandbox
// on success. A failure before Restore must also leave every file alone.
func TestResetFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name      string
		sandbox   fakeSandbox
		manifest  string
		wantErr   string
		wantCalls []string
	}{
		{name: "a garbled manifest kills nothing", manifest: "{not json", wantErr: "parse manifest"},
		{name: "an empty manifest kills nothing", manifest: "", wantErr: "parse manifest"},
		{
			name:      "the kill fails",
			sandbox:   fakeSandbox{killErr: boom},
			manifest:  goodManifest,
			wantErr:   "boom",
			wantCalls: []string{"kill"},
		},
		{
			name:      "/proc cannot be read",
			sandbox:   fakeSandbox{listErr: boom},
			manifest:  goodManifest,
			wantErr:   "boom",
			wantCalls: []string{"kill", "reap", "list"},
		},
		{
			name:      "/proc does not list PID 1",
			sandbox:   fakeSandbox{pids: [][]int{{fakeSelf}}},
			manifest:  goodManifest,
			wantErr:   "does not list both",
			wantCalls: []string{"kill", "reap", "list"},
		},
		{
			name:      "/proc does not list this process",
			sandbox:   fakeSandbox{pids: [][]int{{initPID}}},
			manifest:  goodManifest,
			wantErr:   "does not list both",
			wantCalls: []string{"kill", "reap", "list"},
		},
		{
			name:      "/proc lists nothing at all",
			sandbox:   fakeSandbox{pids: [][]int{{}}},
			manifest:  goodManifest,
			wantErr:   "does not list both",
			wantCalls: []string{"kill", "reap", "list"},
		},
		{
			name:      "the mount table cannot be read",
			sandbox:   fakeSandbox{mountinfoErr: boom},
			manifest:  goodManifest,
			wantErr:   "boom",
			wantCalls: []string{"kill", "reap", "list", "mountinfo"},
		},
		{
			name:      "restore fails",
			sandbox:   fakeSandbox{restoreErr: boom, removed: 7},
			manifest:  goodManifest,
			wantErr:   "boom",
			wantCalls: []string{"kill", "reap", "list", "mountinfo", "restore"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &tc.sandbox
			removed, err := f.agent().Reset(context.Background(), []byte(tc.manifest))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to hold %q", err, tc.wantErr)
			}
			if removed != 0 {
				t.Fatalf("removed = %d alongside an error", removed)
			}
			if !slices.Equal(f.calls, tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", f.calls, tc.wantCalls)
			}
		})
	}
}

// A process that stays must fail the reset, or Restore would race a writer and the
// next run would inherit it. The error has to name it, since this is what an operator
// reads when a member is destroyed.
func TestResetFailsWhenAProcessRemains(t *testing.T) {
	f := &fakeSandbox{pids: [][]int{{initPID, fakeSelf, 4242}}}
	_, err := f.agent().Reset(context.Background(), []byte(goodManifest))
	if err == nil || !strings.Contains(err.Error(), "processes still present") || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("err = %v, want it to name process 4242", err)
	}
	if len(f.restored) != 0 {
		t.Fatal("restore ran with a process still present")
	}
}

// A fork bomb's survivors all land in this error, which travels in a gRPC trailer and
// a log line.
func TestProcessErrorStaysShort(t *testing.T) {
	survivors := []int{initPID, fakeSelf}
	for pid := 100000; pid < 105000; pid++ {
		survivors = append(survivors, pid)
	}
	f := &fakeSandbox{pids: [][]int{survivors}}
	_, err := f.agent().Reset(context.Background(), []byte(goodManifest))
	if err == nil || !strings.Contains(err.Error(), "and 4995 more") || len(err.Error()) > 500 {
		t.Fatalf("err = %v (%d bytes)", err, len(fmt.Sprint(err)))
	}
}

// bwrap's PID 1 collects a killed orphan when it next runs, so a process listed right
// after the kill is normal. Reaping has to happen before each look, or a child of this
// process would stay listed for as long as nobody collects it.
func TestProofWaitsForKilledProcessesToGo(t *testing.T) {
	f := &fakeSandbox{pids: [][]int{
		{initPID, fakeSelf, 4242, 4243},
		{initPID, fakeSelf, 4243},
		{initPID, fakeSelf},
	}}
	if _, err := f.agent().Reset(context.Background(), []byte(goodManifest)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	want := []string{"kill", "reap", "list", "reap", "list", "reap", "list", "mountinfo", "restore"}
	if got := f.calls; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// The wait for a survivor must not use up the whole reset's deadline: the runner should
// learn quickly that a member is lost.
func TestProofGivesUpAtTheSettleTime(t *testing.T) {
	f := &fakeSandbox{pids: [][]int{{initPID, fakeSelf, 4242}}}
	a := f.agent()
	a.resetter.timeout = time.Minute
	a.resetter.settle = 30 * time.Millisecond
	start := time.Now()
	if _, err := a.Reset(context.Background(), []byte(goodManifest)); err == nil {
		t.Fatal("Reset succeeded with a process still present")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("gave up after %v, want about the settle time", took)
	}
}

// Without a deadline on the lock, a reset would wait for as long as an exec's output
// stays open, and the runner could not tell that member from a slow one.
func TestResetDeadlineCoversTheWaitForExecMu(t *testing.T) {
	f := &fakeSandbox{}
	a := f.agent()
	a.resetter.timeout = 30 * time.Millisecond
	a.execMu.Lock()
	defer a.execMu.Unlock()

	result := make(chan error, 1)
	go func() {
		_, err := a.Reset(context.Background(), []byte(goodManifest))
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reset is still waiting for execMu past its deadline")
	}
	if got := f.calls; len(got) != 0 {
		t.Fatalf("calls = %v: nothing may run without the lock", got)
	}
}

// untilDeadline stands in for work that runs until told to stop, and gives up by itself
// so that a call with no deadline fails its test and does not hang the run.
func untilDeadline(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("no deadline reached this call")
	}
}

// A caller cannot lift the ceiling by sending no deadline of its own, and Restore must
// be the one told, since it is what can run for a long time.
func TestRestoreRunsUnderTheDeadline(t *testing.T) {
	f := &fakeSandbox{restoreBlock: untilDeadline}
	a := f.agent()
	a.resetter.timeout = 30 * time.Millisecond
	_, err := a.Reset(context.Background(), []byte(goodManifest))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}

// A caller that has gone, like a workload the host just killed, must not start a kill.
func TestResetFromACancelledCallerDoesNothing(t *testing.T) {
	f := &fakeSandbox{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.agent().Reset(ctx, []byte(goodManifest)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancelled", err)
	}
	if got := f.calls; len(got) != 0 {
		t.Fatalf("calls = %v", got)
	}
}

// Exec holds execMu from fork to wait. A reset that ran beside it could remove files
// from under a command, and could reap the child os/exec is waiting on.
func TestResetAndCaptureHoldExecMu(t *testing.T) {
	var a *agent
	f := &fakeSandbox{restoreBlock: func(context.Context) error {
		if a.execMu.TryLock() {
			a.execMu.Unlock()
			t.Error("execMu was free during restore")
		}
		return nil
	}}
	a = f.agent()
	if _, err := a.Reset(context.Background(), []byte(goodManifest)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !a.execMu.TryLock() {
		t.Fatal("Reset left execMu held")
	}
	a.execMu.Unlock()

	a.resetter.capture = func(context.Context, io.Reader) (sandboxreset.Manifest, error) {
		if a.execMu.TryLock() {
			a.execMu.Unlock()
			t.Error("execMu was free during capture")
		}
		return sandboxreset.Manifest{}, nil
	}
	if _, err := a.Capture(context.Background()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !a.execMu.TryLock() {
		t.Fatal("Capture left execMu held")
	}
	a.execMu.Unlock()
}

// A caller that went away while queued has nobody to report to, and its command would
// only give the host's kill more to do.
func TestExecFromACancelledCallerDoesNotStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := &execResult{}
	err := testAgent().Exec(ctx, &v1.ExecRequest{Command: "/bin/sh", Args: []string{"-c", ": > " + marker}, Cwd: t.TempDir()}, res.send)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancelled", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the command ran: stat %v", err)
	}
}

// The mount table can only change between capture and reset if something is wrong, and
// a cached copy would hide that from Restore's comparison. It also has to be a new read
// at every call, because the agent keeps nothing between them.
func TestEveryCallReadsTheMountTableAgain(t *testing.T) {
	f := &fakeSandbox{}
	a := f.agent()
	if _, err := a.Capture(context.Background()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := a.Capture(context.Background()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := a.Reset(context.Background(), []byte(goodManifest)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if want := []string{"table 1", "table 2", "table 3"}; !slices.Equal(f.mountinfos, want) {
		t.Fatalf("read %v, want %v", f.mountinfos, want)
	}
}

// The host keeps these bytes and hands them back, so they must decode to what capture
// listed, and a capture must not kill or wait on anything.
func TestCaptureReturnsTheManifestItListed(t *testing.T) {
	f := &fakeSandbox{}
	raw, err := f.agent().Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	var m sandboxreset.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if len(m.Mounts) != 1 || m.Mounts[0].Path != "/tmp" {
		t.Fatalf("manifest = %+v", m)
	}
	if want := []string{"mountinfo", "capture"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

func TestCaptureFailures(t *testing.T) {
	boom := errors.New("boom")
	for name, f := range map[string]*fakeSandbox{
		"mount table": {mountinfoErr: boom},
		"listing":     {captureErr: boom},
	} {
		t.Run(name, func(t *testing.T) {
			if raw, err := f.agent().Capture(context.Background()); !errors.Is(err, boom) || raw != nil {
				t.Fatalf("Capture = %q, %v, want the error alone", raw, err)
			}
		})
	}
}

func TestCaptureDeadlineCoversTheWaitForExecMu(t *testing.T) {
	f := &fakeSandbox{}
	a := f.agent()
	a.resetter.timeout = 30 * time.Millisecond
	a.execMu.Lock()
	defer a.execMu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, err := a.Capture(context.Background())
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Capture is still waiting for execMu past its deadline")
	}
	if got := f.calls; len(got) != 0 {
		t.Fatalf("calls = %v: nothing may run without the lock", got)
	}
}

// guardedKill is the only thing between a mistake and every process this user owns.
func TestGuardRefusesEverywhereButPID2UnderBwrap(t *testing.T) {
	refused := []struct {
		name string
		pid  int
		comm string
	}{
		{"this test process", os.Getpid(), bwrapComm},
		{"gVisor's PID 1", 1, "vora-init"},
		{"PID 1 of a bwrap", 1, bwrapComm},
		{"a workload under bwrap", 3, bwrapComm},
		{"a helper under gVisor", 57, "vora-init"},
		{"a process on a host", 12345, "systemd"},
		{"no PID", 0, bwrapComm},
		{"a negative PID", -1, bwrapComm},
		{"PID 2 of a container", 2, "systemd"},
		{"PID 2 of a shell container", 2, "sh"},
		{"PID 2 with an unreadable name", 2, ""},
		{"PID 2 with a name that only starts right", 2, "bwrap2"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			err := guardedKill(tc.pid, tc.comm, func(int, syscall.Signal) error {
				t.Error("the kill was reached")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("err = %v, want a refusal", err)
			}
		})
	}
}

func TestGuardKillsEveryProcessAtPID2UnderBwrap(t *testing.T) {
	var pids []int
	var sigs []syscall.Signal
	err := guardedKill(agentPID, bwrapComm, func(pid int, sig syscall.Signal) error {
		pids, sigs = append(pids, pid), append(sigs, sig)
		return nil
	})
	if err != nil {
		t.Fatalf("guardedKill: %v", err)
	}
	if !slices.Equal(pids, []int{-1}) || !slices.Equal(sigs, []syscall.Signal{syscall.SIGKILL}) {
		t.Fatalf("killed %v with %v, want -1 with SIGKILL", pids, sigs)
	}
}

// kill(-1) returns ESRCH when nothing else exists, which is the normal case because the
// host has already killed the run. Anything else means a process may have survived.
func TestGuardReadsTheKillResult(t *testing.T) {
	kill := func(err error) func(int, syscall.Signal) error {
		return func(int, syscall.Signal) error { return err }
	}
	if err := guardedKill(agentPID, bwrapComm, kill(syscall.ESRCH)); err != nil {
		t.Fatalf("ESRCH: %v", err)
	}
	if err := guardedKill(agentPID, bwrapComm, kill(syscall.EPERM)); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("EPERM: %v", err)
	}
}

// The real kill is safe in a test binary only because no test calls it, and a future
// test that did would pass until the day the guard broke. startAgentProcess runs a real
// agent, so a Reset through its client is banned here as well.
func TestNoTestNamesTheRealKillOrTheRealResetter(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v, %v", files, err)
	}
	banned := []string{"kill" + "OtherProcesses", "agent" + "Resetter(", "sandboxreset." + "Restore", "client.Reset" + "("}
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range banned {
			if bytes.Contains(source, []byte(name)) {
				t.Errorf("%s names %s", file, name)
			}
		}
	}
}

// Under gVisor there is no PID 2 for the guard to accept, and the helper has no business
// killing: the host already has, without trusting guest code.
func TestHelperHasNoKill(t *testing.T) {
	if helperResetter().killOthers != nil {
		t.Fatal("the helper has a kill")
	}
}

func helperFake(f *fakeSandbox) *resetter {
	r := f.resetter()
	r.killOthers = nil
	return r
}

// gVisor's PID 1 is `vora-init pause`, and the helper is whatever PID runsc gave it, so
// the proof allows those two and the helper runs nothing else before Restore.
func TestResetModeRestoresAndReports(t *testing.T) {
	f := &fakeSandbox{removed: 4}
	var stdout, stderr bytes.Buffer
	code := runResetMode(context.Background(), helperFake(f), strings.NewReader(goodManifest), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var report resetReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("report %q does not parse: %v", stdout.String(), err)
	}
	if report.Removed != 4 {
		t.Fatalf("report = %+v, want 4 removed", report)
	}
	if !strings.HasSuffix(stdout.String(), "}\n") {
		t.Fatalf("report %q is not one line", stdout.String())
	}
	if want := []string{"reap", "list", "mountinfo", "restore"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if len(f.restored) != 1 || f.restored[0].Mounts[0].Path != "/workspace" {
		t.Fatalf("restored %+v: not the manifest from stdin", f.restored)
	}
}

// The runner takes a nonzero exit as a lost member. A failure must also leave stdout
// without a report, so nothing can read one by mistake.
func TestResetModeFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name    string
		sandbox fakeSandbox
		stdin   io.Reader
		wantErr string
		restore bool
	}{
		{name: "a process remains", sandbox: fakeSandbox{pids: [][]int{{initPID, fakeSelf, 4242}}}, stdin: strings.NewReader(goodManifest), wantErr: "4242"},
		{name: "the agent is still there", sandbox: fakeSandbox{pids: [][]int{{initPID, agentPID, fakeSelf}}}, stdin: strings.NewReader(goodManifest), wantErr: "processes still present"},
		{name: "/proc cannot be read", sandbox: fakeSandbox{listErr: boom}, stdin: strings.NewReader(goodManifest), wantErr: "boom"},
		{name: "empty stdin", stdin: strings.NewReader(""), wantErr: "parse manifest"},
		{name: "garbled stdin", stdin: strings.NewReader("{not json"), wantErr: "parse manifest"},
		{name: "stdin fails", stdin: failingReader{}, wantErr: "read manifest"},
		{name: "restore fails", sandbox: fakeSandbox{restoreErr: boom}, stdin: strings.NewReader(goodManifest), wantErr: "boom", restore: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &tc.sandbox
			var stdout, stderr bytes.Buffer
			code := runResetMode(context.Background(), helperFake(f), tc.stdin, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q on failure", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("stderr = %q, want it to hold %q", stderr.String(), tc.wantErr)
			}
			if !tc.restore && len(f.restored) != 0 {
				t.Fatal("restore ran after a failure")
			}
		})
	}
}

// A report the host never got is no report: a clean sandbox it cannot confirm gets
// destroyed like any other.
func TestResetModeFailsWhenTheReportCannotBeWritten(t *testing.T) {
	f := &fakeSandbox{}
	var stderr bytes.Buffer
	if code := runResetMode(context.Background(), helperFake(f), strings.NewReader(goodManifest), failingWriter{}, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestResetModeRunsUnderTheDeadline(t *testing.T) {
	f := &fakeSandbox{restoreBlock: untilDeadline}
	r := helperFake(f)
	r.timeout = 30 * time.Millisecond
	var stdout, stderr bytes.Buffer
	if code := runResetMode(context.Background(), r, strings.NewReader(goodManifest), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), context.DeadlineExceeded.Error()) {
		t.Fatalf("stderr = %q, want the deadline", stderr.String())
	}
}

func TestCaptureModeRunsUnderTheDeadline(t *testing.T) {
	r := helperFake(&fakeSandbox{})
	r.timeout = 30 * time.Millisecond
	r.capture = func(ctx context.Context, _ io.Reader) (sandboxreset.Manifest, error) {
		return sandboxreset.Manifest{}, untilDeadline(ctx)
	}
	var stdout, stderr bytes.Buffer
	if code := runCaptureMode(context.Background(), r, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), context.DeadlineExceeded.Error()) {
		t.Fatalf("stderr = %q, want the deadline", stderr.String())
	}
}

func TestCaptureModeWritesTheManifest(t *testing.T) {
	f := &fakeSandbox{}
	var stdout, stderr bytes.Buffer
	if code := runCaptureMode(context.Background(), helperFake(f), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var m sandboxreset.Manifest
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil {
		t.Fatalf("stdout %q does not parse: %v", stdout.String(), err)
	}
	if len(m.Mounts) != 1 || m.Mounts[0].Path != "/tmp" {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestCaptureModeFailures(t *testing.T) {
	f := &fakeSandbox{captureErr: errors.New("boom")}
	var stdout, stderr bytes.Buffer
	if code := runCaptureMode(context.Background(), helperFake(f), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	if code := runCaptureMode(context.Background(), helperFake(&fakeSandbox{}), failingWriter{}, &stderr); code != 1 {
		t.Fatalf("exit %d when stdout fails, want 1", code)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
