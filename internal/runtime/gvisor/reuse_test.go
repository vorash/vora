package gvisor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

// fakeReuseRunsc stands in for runsc. Every call appends its subcommand to $FAKE/calls.
//
//	ps    prints the next line of $FAKE/ps, repeating the last one. HANG sleeps past any
//	      deadline with the pipes open, FAIL exits 128.
//	kill  logs the pid and exits with $FAKE/kill.rc, or 0.
//	exec  takes the mode from its last argument, saves stdin to $FAKE/<mode>.stdin, prints
//	      $FAKE/<mode>.out and exits with $FAKE/<mode>.rc.
//
// A file named after the mode changes what exec does before it would report a PID, which
// it logs as "started" and writes to -internal-pid-file if it was given one:
//
//	<mode>.hang          never answers
//	<mode>.answer-after  answers after that many seconds
//	<mode>.refuse        exits 128, as runsc does when it cannot run a command
//	<mode>.crash         exits 2 with no answer
//	<mode>.dies          is SIGKILLed with no answer
//
// and after it:
//
//	<mode>.runs          stays until the command is killed
//	<mode>.holds         exits, and leaves a child holding stdout and stderr
const fakeReuseRunsc = `#!/bin/sh
hang() {
  sleep 30 &
  echo $! >> "$FAKE/hung"
  wait
  exit 137
}
sub=""
prev=""
pidfile=""
for a in "$@"; do
  case "$a" in ps|kill|exec) [ -z "$sub" ] && sub="$a" ;; esac
  [ "$prev" = "-pid" ] && pid="$a"
  [ "$prev" = "-internal-pid-file" ] && pidfile="$a"
  prev="$a"
  last="$a"
done
case "$sub" in
ps)
  echo ps >> "$FAKE/calls"
  if [ -f "$FAKE/ps.after" ] && [ -f "$FAKE/killed" ]; then
    killed=$(cat "$FAKE/killed")
    now=$(date +%s%N)
    if [ $(( (now - killed) / 1000000 )) -ge "$(cat "$FAKE/ps.after")" ]; then
      echo "[1]"
      exit 0
    fi
  fi
  n=$(cat "$FAKE/ps.n" 2>/dev/null || echo 0)
  n=$((n+1))
  echo "$n" > "$FAKE/ps.n"
  total=$(wc -l < "$FAKE/ps")
  [ "$n" -gt "$total" ] && n="$total"
  line=$(sed -n "${n}p" "$FAKE/ps")
  case "$line" in
    HANG) hang ;;
    FAIL) echo "connection refused" >&2; exit 128 ;;
  esac
  echo "$line"
  ;;
kill)
  echo "kill $pid" >> "$FAKE/calls"
  [ -f "$FAKE/killed" ] || date +%s%N > "$FAKE/killed"
  [ -f "$FAKE/kill.rc" ] && exit "$(cat "$FAKE/kill.rc")"
  exit 0
  ;;
exec)
  echo "exec $last" >> "$FAKE/calls"
  cat > "$FAKE/$last.stdin"
  [ -f "$FAKE/$last.hang" ] && hang
  [ -f "$FAKE/$last.answer-after" ] && sleep "$(cat "$FAKE/$last.answer-after")"
  if [ -f "$FAKE/$last.refuse" ]; then echo "cannot execute" >&2; exit 128; fi
  [ -f "$FAKE/$last.crash" ] && exit 2
  [ -f "$FAKE/$last.dies" ] && kill -9 $$
  if [ -n "$pidfile" ]; then
    echo "started $last" >> "$FAKE/calls"
    echo 4242 > "$pidfile"
  fi
  if [ -f "$FAKE/$last.runs" ]; then
    while [ ! -f "$FAKE/killed" ]; do sleep 0.01; done
    exit 137
  fi
  if [ -f "$FAKE/$last.holds" ]; then
    sleep 30 &
    echo $! >> "$FAKE/hung"
    exit 0
  fi
  [ -f "$FAKE/$last.out" ] && cat "$FAKE/$last.out"
  [ -f "$FAKE/$last.rc" ] && exit "$(cat "$FAKE/$last.rc")"
  exit 0
  ;;
esac
`

const fakeManifest = `{"mounts":[{"path":"/workspace","entries":[{"rel":".","type":"dir"},{"rel":"keep","type":"file"}]},{"path":"/root","entries":[{"rel":".","type":"dir"}]}]}`

type fakeReuse struct {
	t   *testing.T
	dir string
	b   *Backend
	hd  *handle
}

func newFakeReuse(t *testing.T, psLines ...string) *fakeReuse {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "runsc")
	if err := os.WriteFile(script, []byte(fakeReuseRunsc), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE", dir)
	f := &fakeReuse{t: t, dir: dir}
	t.Cleanup(f.killHung)
	f.write("ps", strings.Join(psLines, "\n")+"\n")
	f.write("capture.out", fakeManifest)
	f.write("reset.out", `{"removed":3}`+"\n")

	ws := runtime.WorkspaceSpec{WorkspaceDir: filepath.Join(dir, "ws"), HomeDir: filepath.Join(dir, "home")}
	if err := os.MkdirAll(ws.WorkspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws.HomeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.writeFile(filepath.Join(ws.WorkspaceDir, "keep"))
	f.hd = &handle{id: "sbx_fake", bundleDir: filepath.Join(dir, "bundle"), spec: runtime.SandboxSpec{Workspace: ws}}
	if err := os.MkdirAll(f.hd.bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.b = &Backend{logger: slog.New(slog.DiscardHandler), runsc: script, root: dir, platform: "systrap", handles: map[string]*handle{f.hd.id: f.hd}}
	return f
}

// killHung ends the children the fake left holding pipes, which would otherwise sit there
// long after the test that hung them.
func (f *fakeReuse) killHung() {
	for _, pid := range strings.Fields(f.read("hung")) {
		if n, err := strconv.Atoi(pid); err == nil {
			_ = syscall.Kill(n, syscall.SIGKILL)
		}
	}
}

func (f *fakeReuse) writeFile(path string) {
	f.t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeReuse) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeReuse) read(name string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *fakeReuse) calls() []string {
	return strings.Fields(strings.ReplaceAll(f.read("calls"), " ", "="))
}

// restart forgets the calls so far and scripts new ps answers.
func (f *fakeReuse) restart(psLines ...string) {
	f.t.Helper()
	f.write("calls", "")
	f.write("ps.n", "0")
	f.write("ps", strings.Join(psLines, "\n")+"\n")
}

// prepared returns a member that passed admission, ready for a scripted reset.
func (f *fakeReuse) prepared() *resettable {
	f.t.Helper()
	r, err := f.b.Prepare(testCtx(f.t), f.hd)
	if err != nil {
		f.t.Fatalf("Prepare: %v", err)
	}
	return r.(*resettable)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("runsc calls:\n got %v\nwant %v", got, want)
	}
}

func TestResetKillsThenRunsTheHelperThenProves(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1,7,9]", "[1]", "[1]")

	report, err := r.Reset(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f.calls(), "ps", "kill=7", "kill=9", "ps", "exec=reset", "ps")
	if report.ProcessesKilled != 2 || report.EntriesRemoved != 3 {
		t.Fatalf("report %+v, want 2 killed and 3 removed", report)
	}
	if got := f.read("reset.stdin"); got != fakeManifest {
		t.Fatalf("helper got manifest %q, want the captured one", got)
	}
}

func TestResetOfACleanSandboxKillsNothing(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1]")
	report, err := r.Reset(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f.calls(), "ps", "exec=reset", "ps")
	if report.ProcessesKilled != 0 {
		t.Fatalf("report %+v", report)
	}
}

// A process seen by two passes is one process, and a kill that lost a race with the
// target's own death is no reason to give up on the rest.
func TestKillPassesCountProcessesOnceAndTolerateARefusal(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1,7]", "[1,7,8]", "[1]", "[1]")
	report, err := r.Reset(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if report.ProcessesKilled != 2 {
		t.Fatalf("report %+v, want 2 distinct processes", report)
	}

	f.restart("[1,7]", "[1]", "[1]")
	f.write("kill.rc", "128")
	report, err = r.Reset(testCtx(t))
	if err != nil {
		t.Fatalf("a refused kill followed by a clean listing is clean: %v", err)
	}
	if report.ProcessesKilled != 0 {
		t.Fatalf("report %+v counted a refused kill", report)
	}
}

// A survivor must stop the reset before any helper runs next to it. The helper is root
// with the manifest on its stdin, so its only protection is that nothing else exists.
func TestSurvivorFailsClosedBeforeTheHelper(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1,7]")
	_, err := r.Reset(testCtx(t))
	if !errors.Is(err, runtime.ErrUnproven) {
		t.Fatalf("err = %v, want ErrUnproven", err)
	}
	kills := 0
	for _, c := range f.calls() {
		if strings.HasPrefix(c, "exec=") {
			t.Fatalf("helper ran next to a survivor: %v", f.calls())
		}
		if strings.HasPrefix(c, "kill=") {
			kills++
		}
	}
	if kills != killPasses {
		t.Fatalf("%d kill passes, want %d", kills, killPasses)
	}
}

// A killed process stays listed until the Sentry has torn it down, longer if the member is
// out of CPU. The passes must outlast that rather than call it a survivor and cost a healthy
// member. Without the backoff this fake runs through all ten passes in a few tens of ms.
func TestKillPassesWaitForASlowDeath(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1,7]")
	f.write("ps.after", "400")
	report, err := r.Reset(testCtx(t))
	if err != nil {
		t.Fatalf("a process that takes 400 ms to die: %v", err)
	}
	if report.ProcessesKilled != 1 {
		t.Fatalf("report %+v", report)
	}
}

// What never dies must still fail closed, bounded by passes and well inside the pool's 10 s,
// and the backoff owes the deadline as much as any runsc call does.
func TestKillPassesStayBounded(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1,7]")
	start := time.Now()
	if _, err := r.Reset(testCtx(t)); !errors.Is(err, runtime.ErrUnproven) {
		t.Fatalf("Reset = %v, want ErrUnproven", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("giving up on a survivor took %v", took)
	}

}

// The longest sleep is as long as the pool's whole grace, so it has to end with the context.
// The seventh kill is followed by that sleep.
func TestKillBackoffEndsWithTheContext(t *testing.T) {
	const allowed = 100 * time.Millisecond
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	best := time.Hour
	for range 4 {
		f.restart("[1,7]")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := r.Reset(ctx)
			done <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for strings.Count(f.read("calls"), "kill 7") < 7 {
			if time.Now().After(deadline) {
				t.Fatalf("seven kills never came: %v", f.calls())
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
		start := time.Now()
		err := <-done
		best = min(best, time.Since(start))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Reset = %v, want the cancel", err)
		}
		if best <= allowed {
			return
		}
	}
	t.Fatalf("the backoff held a cancelled reset for %v at best", best)
}

func TestResetFailuresAreUnproven(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fakeReuse)
	}{
		{"ps fails", func(f *fakeReuse) { f.restart("FAIL") }},
		{"ps prints garbage", func(f *fakeReuse) { f.restart("not json") }},
		{"ps prints nothing", func(f *fakeReuse) { f.restart("") }},
		{"ps omits PID 1", func(f *fakeReuse) { f.restart("[7]") }},
		{"ps lists nobody", func(f *fakeReuse) { f.restart("[]") }},
		{"ps prints null", func(f *fakeReuse) { f.restart("null") }},
		{"proof fails", func(f *fakeReuse) { f.restart("[1]", "FAIL") }},
		{"proof finds a process", func(f *fakeReuse) { f.restart("[1]", "[1,12]") }},
		{"proof loses PID 1", func(f *fakeReuse) { f.restart("[1]", "[12]") }},
		{"helper exits nonzero", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.rc", "1") }},
		{"helper exits 128", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.rc", "128") }},
		{"helper exit 0, no report", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.out", "") }},
		{"helper exit 0, garbage", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.out", "all clean\n") }},
		{"helper exit 0, truncated", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.out", `{"removed":`) }},
		{"helper exit 0, no count", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.out", "{}\n") }},
		{"helper exit 0, negative count", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.out", `{"removed":-1}`) }},
		{"helper exit 0, unknown field", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.out", `{"removed":1,"partial":true}`) }},
		{"helper exit 0, trailing output", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.out", `{"removed":1} {"removed":2}`) }},
		{"host holds an extra file", func(f *fakeReuse) {
			f.restart("[1]")
			f.writeFile(filepath.Join(f.hd.spec.Workspace.WorkspaceDir, "planted"))
		}},
		{"host lost a file", func(f *fakeReuse) {
			f.restart("[1]")
			if err := os.Remove(filepath.Join(f.hd.spec.Workspace.WorkspaceDir, "keep")); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"host directory gone", func(f *fakeReuse) {
			f.restart("[1]")
			if err := os.RemoveAll(f.hd.spec.Workspace.HomeDir); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeReuse(t, "[1]")
			r := f.prepared()
			tc.setup(f)
			report, err := r.Reset(testCtx(t))
			if !errors.Is(err, runtime.ErrUnproven) {
				t.Fatalf("err = %v, want ErrUnproven", err)
			}
			if report != (runtime.ResetReport{}) {
				t.Fatalf("a failed reset reported %+v", report)
			}
		})
	}
}

// bestOverrun runs call until it comes back within grace of its deadline, a few times
// at most, and returns the overrun of the best try. A process killed and its pipes closed
// on time can still be scheduled late on a loaded runner. Code that really overruns does so
// on every try, so a fresh try costs the test nothing that it means to catch.
func bestOverrun(deadline, grace time.Duration, call func(ctx context.Context) error, check func(error)) time.Duration {
	best := time.Duration(1<<63 - 1)
	for range 4 {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		start := time.Now()
		err := call(ctx)
		overrun := time.Since(start) - deadline
		cancel()
		check(err)
		best = min(best, overrun)
		if best <= grace {
			break
		}
	}
	return best
}

// The pool waits CallGrace past its deadline and then destroys the sandbox, so a call that
// overruns by more than that is a call the pool has abandoned. A stopped Sentry hangs every
// runsc subcommand, measured on real gVisor. The fake's child keeps the pipes open, which
// a real runsc's would too, so the kill alone would not end these calls.
func TestDeadlineHoldsWhenRunscHangs(t *testing.T) {
	const deadline, grace = 150 * time.Millisecond, 200 * time.Millisecond
	for _, tc := range []struct {
		name  string
		setup func(f *fakeReuse)
	}{
		{"first ps", func(f *fakeReuse) { f.restart("HANG") }},
		{"proof ps", func(f *fakeReuse) { f.restart("[1]", "HANG") }},
		{"helper", func(f *fakeReuse) { f.restart("[1]"); f.write("reset.hang", "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeReuse(t, "[1]")
			r := f.prepared()
			overrun := bestOverrun(deadline, grace, func(ctx context.Context) error {
				tc.setup(f)
				_, err := r.Reset(ctx)
				return err
			}, func(err error) {
				if !errors.Is(err, runtime.ErrUnproven) {
					t.Fatalf("err = %v, want ErrUnproven", err)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v, want it to say the deadline passed", err)
				}
			})
			if overrun > grace {
				t.Fatalf("Reset overran a %v deadline by %v at best, more than the pool's %v grace", deadline, overrun, grace)
			}
		})
	}
	t.Run("alive", func(t *testing.T) {
		f := newFakeReuse(t, "[1]")
		r := f.prepared()
		overrun := bestOverrun(deadline, grace, func(ctx context.Context) error {
			f.restart("HANG")
			return r.Alive(ctx)
		}, func(err error) {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want the deadline", err)
			}
		})
		if overrun > grace {
			t.Fatalf("Alive overran a %v deadline by %v at best", deadline, overrun)
		}
	})
}

func TestResetOnAnExpiredContextStartsNothing(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1]")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Reset(ctx); !errors.Is(err, runtime.ErrUnproven) {
		t.Fatalf("err = %v, want ErrUnproven", err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("runsc ran after the deadline: %v", calls)
	}
}

func TestAlive(t *testing.T) {
	for _, tc := range []struct {
		name string
		ps   string
		ok   bool
	}{
		{"init alone", "[1]", true},
		{"init and a run", "[1,7]", true},
		{"sandbox dead", "FAIL", false},
		{"no init", "[7]", false},
		{"nobody", "[]", false},
		{"garbage", "oops", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeReuse(t, "[1]")
			r := f.prepared()
			f.restart(tc.ps)
			err := r.Alive(testCtx(t))
			if tc.ok != (err == nil) {
				t.Fatalf("Alive with ps %q = %v", tc.ps, err)
			}
			if got := f.calls(); len(got) != 1 {
				t.Fatalf("Alive made %d runsc calls: %v", len(got), got)
			}
		})
	}
}

func TestPrepareCapturesThenProvesAnAdmissionReset(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	wantCalls(t, f.calls(), "ps", "exec=capture", "ps", "exec=reset", "ps")
	if string(r.manifest) != fakeManifest {
		t.Fatalf("kept manifest %q", r.manifest)
	}
	if got := f.read("reset.stdin"); got != fakeManifest {
		t.Fatalf("admission reset got %q, want the capture", got)
	}
}

func TestPrepareFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ps    []string
		setup func(f *fakeReuse)
	}{
		{name: "sandbox dead", ps: []string{"FAIL"}},
		{name: "a process already runs", ps: []string{"[1,7]"}},
		{name: "capture refused", ps: []string{"[1]"}, setup: func(f *fakeReuse) { f.write("capture.rc", "1") }},
		{name: "capture prints garbage", ps: []string{"[1]"}, setup: func(f *fakeReuse) { f.write("capture.out", "nope") }},
		{name: "capture misses a bind mount", ps: []string{"[1]"}, setup: func(f *fakeReuse) { f.write("capture.out", `{"mounts":[{"path":"/root","entries":[{"rel":"."}]}]}`) }},
		{name: "admission helper refuses", ps: []string{"[1]"}, setup: func(f *fakeReuse) { f.write("reset.rc", "1") }},
		{name: "admission report garbage", ps: []string{"[1]"}, setup: func(f *fakeReuse) { f.write("reset.out", "x") }},
		{name: "admission proof fails", ps: []string{"[1]", "[1]", "[1,5]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeReuse(t, tc.ps...)
			if tc.setup != nil {
				tc.setup(f)
			}
			r, err := f.b.Prepare(testCtx(t), f.hd)
			if err == nil {
				t.Fatalf("Prepare succeeded with %v", f.calls())
			}
			if r != nil {
				t.Fatal("a failed Prepare returned a Resettable")
			}
		})
	}
}

func TestPrepareUnknownHandle(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	if _, err := f.b.Prepare(testCtx(t), &handle{id: "sbx_other"}); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The pool may destroy a sandbox while a Reset it gave up on is still running. Run under
// -race, this finds a Resettable that reads what Destroy changes.
func TestResetRacesDestroy(t *testing.T) {
	for i := range 10 {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f := newFakeReuse(t, "[1]")
			r := f.prepared()
			f.restart("[1,7]", "[1]", "[1]")
			f.hd.bundleDir = filepath.Join(f.dir, "bundle")
			done := make(chan error, 1)
			go func() {
				_, err := r.Reset(testCtx(t))
				done <- err
			}()
			if err := f.b.Destroy(testCtx(t), f.hd); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Reset did not return after Destroy")
			}
		})
	}
}

func (f *fakeReuse) exec(command string) runtime.ExecStream {
	f.t.Helper()
	stream, err := f.b.Exec(testCtx(f.t), f.hd, runtime.ExecRequest{Command: command})
	if err != nil {
		f.t.Fatalf("Exec: %v", err)
	}
	f.t.Cleanup(func() { _ = stream.Close() })
	return stream
}

func callsContain(f *fakeReuse, want string) bool { return slices.Contains(f.calls(), want) }

func waitForCall(t *testing.T, f *fakeReuse, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !callsContain(f, want) {
		if time.Now().After(deadline) {
			t.Fatalf("no %s among %v", want, f.calls())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A run's client can disconnect while `runsc exec` is mid-start, and the pool resets at once.
// If the client were killed there, its request could still reach the Sentry after the reset
// had looked, with nothing left to say it was coming. So the client lives to answer, the
// reset's first look comes after that, and the command gets killed.
func TestAResetWaitsForAnExecStillStarting(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1]")
	f.write("work.answer-after", "0.3")
	ctx, disconnect := context.WithCancel(testCtx(t))
	defer disconnect()
	time.AfterFunc(20*time.Millisecond, disconnect)
	stream, err := f.b.Exec(ctx, f.hd, runtime.ExecRequest{Command: "work"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	_ = stream.Close()

	if _, err := r.Reset(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	started, firstPS := slices.Index(calls, "started=work"), slices.Index(calls, "ps")
	if started < 0 {
		t.Fatalf("the client did not live to answer: %v", calls)
	}
	if firstPS < started {
		t.Fatalf("the reset looked before the exec had answered: %v", calls)
	}
	waitForCall(t, f, "kill=4242")
}

// A client that died without an answer may have left a request the Sentry has not read, so
// no later listing means anything. That must cost the member, on this reset and on any other.
func TestAnAbandonedExecFailsEveryLaterReset(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		cancel     bool
	}{
		{name: "client never answers, killed after the grace", file: "work.hang", cancel: true},
		{name: "client crashes", file: "work.crash"},
		{name: "client killed from outside", file: "work.dies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeReuse(t, "[1]")
			r := f.prepared()
			f.restart("[1]")
			f.write(tc.file, "")
			stream := f.exec("work")
			if tc.cancel {
				_ = stream.Close()
			}
			for range 2 {
				_, err := r.Reset(testCtx(t))
				if !errors.Is(err, runtime.ErrUnproven) || !strings.Contains(err.Error(), "abandoned") {
					t.Fatalf("Reset = %v, want ErrUnproven naming the abandoned exec", err)
				}
			}
			for _, c := range f.calls() {
				if c == "ps" || c == "exec=reset" {
					t.Fatalf("the reset went on past an abandoned exec: %v", f.calls())
				}
			}
		})
	}
}

// Nothing answers from a stopped Sentry, and the reset still owes its caller the deadline.
func TestAResetGivesUpOnAnExecThatNeverAnswers(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1]")
	f.write("work.hang", "")
	f.exec("work")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := r.Reset(ctx)
	if !errors.Is(err, runtime.ErrUnproven) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reset = %v, want ErrUnproven and the deadline", err)
	}
	if callsContain(f, "ps") {
		t.Fatalf("the reset looked past an exec that had not answered: %v", f.calls())
	}
}

// runsc refused the command, so its answer is that nothing started.
func TestARefusedExecLeavesTheMemberResettable(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("[1]")
	f.write("work.refuse", "")
	if got := lastChunk(t, f.exec("work")).Exit; got.ExitCode != 128 {
		t.Fatalf("exit %+v, want 128", got)
	}
	if _, err := r.Reset(testCtx(t)); err != nil {
		t.Fatalf("Reset after a refused exec: %v", err)
	}
}

// Only the reset's kill ends these, so a reset that waited for them would never finish: a
// process holding an exec's output after its client exited, and a command still running
// under a client that has reported its PID.
func TestAResetDoesNotWaitForWhatItKills(t *testing.T) {
	for _, file := range []string{"work.holds", "work.runs"} {
		t.Run(file, func(t *testing.T) {
			f := newFakeReuse(t, "[1]")
			r := f.prepared()
			f.restart("[1,4242]", "[1]", "[1]")
			f.write(file, "")
			f.exec("work")
			waitForCall(t, f, "started=work")

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			report, err := r.Reset(ctx)
			if err != nil {
				t.Fatalf("Reset with the exec's stream open: %v", err)
			}
			if report.ProcessesKilled != 1 {
				t.Fatalf("report %+v, want the process killed", report)
			}
		})
	}
}

// What an overlapping exec starts could land after any look the reset has taken. The pool
// never does this, so it is an error that says so rather than a proof that means nothing.
func TestAnExecDuringAResetIsRefused(t *testing.T) {
	f := newFakeReuse(t, "[1]")
	r := f.prepared()
	f.restart("HANG")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.Reset(ctx)
		done <- err
	}()
	waitForCall(t, f, "ps")

	stream, err := f.b.Exec(testCtx(t), f.hd, runtime.ExecRequest{Command: "work"})
	if err == nil {
		_ = stream.Close()
		t.Fatal("Exec during a reset was accepted")
	}
	if !strings.Contains(err.Error(), "being reset") {
		t.Fatalf("Exec = %v, want it to say why", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, runtime.ErrUnproven) {
		t.Fatalf("Reset = %v", err)
	}

	f.restart("[1]")
	if got := lastChunk(t, f.exec("work")).Exit; got.ExitCode != 0 {
		t.Fatalf("Exec after the reset ended: %+v", got)
	}
}
