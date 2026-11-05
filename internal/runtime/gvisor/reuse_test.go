package gvisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
//	      $FAKE/<mode>.out and exits with $FAKE/<mode>.rc. $FAKE/<mode>.hang sleeps instead.
const fakeReuseRunsc = `#!/bin/sh
sub=""
prev=""
for a in "$@"; do
  case "$a" in ps|kill|exec) [ -z "$sub" ] && sub="$a" ;; esac
  [ "$prev" = "-pid" ] && pid="$a"
  prev="$a"
  last="$a"
done
case "$sub" in
ps)
  echo ps >> "$FAKE/calls"
  n=$(cat "$FAKE/ps.n" 2>/dev/null || echo 0)
  n=$((n+1))
  echo "$n" > "$FAKE/ps.n"
  total=$(wc -l < "$FAKE/ps")
  [ "$n" -gt "$total" ] && n="$total"
  line=$(sed -n "${n}p" "$FAKE/ps")
  case "$line" in
    HANG) sleep 30 ;;
    FAIL) echo "connection refused" >&2; exit 128 ;;
  esac
  echo "$line"
  ;;
kill)
  echo "kill $pid" >> "$FAKE/calls"
  [ -f "$FAKE/kill.rc" ] && exit "$(cat "$FAKE/kill.rc")"
  exit 0
  ;;
exec)
  echo "exec $last" >> "$FAKE/calls"
  cat > "$FAKE/$last.stdin"
  [ -f "$FAKE/$last.hang" ] && sleep 30
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
	f.hd = &handle{id: "sbx_fake", spec: runtime.SandboxSpec{Workspace: ws}}
	f.b = &Backend{runsc: script, root: dir, platform: "systrap", handles: map[string]*handle{f.hd.id: f.hd}}
	return f
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

func (f *fakeReuse) calls() []string { return strings.Fields(strings.ReplaceAll(f.read("calls"), " ", "=")) }

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
			tc.setup(f)
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			start := time.Now()
			_, err := r.Reset(ctx)
			took := time.Since(start)
			if !errors.Is(err, runtime.ErrUnproven) {
				t.Fatalf("err = %v, want ErrUnproven", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want it to say the deadline passed", err)
			}
			if took > deadline+grace {
				t.Fatalf("Reset took %v past a %v deadline, more than the pool's %v grace", took-deadline, deadline, grace)
			}
		})
	}
	t.Run("alive", func(t *testing.T) {
		f := newFakeReuse(t, "[1]")
		r := f.prepared()
		f.restart("HANG")
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		start := time.Now()
		err := r.Alive(ctx)
		if took := time.Since(start); took > deadline+grace {
			t.Fatalf("Alive took %v past a %v deadline", took-deadline, deadline)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the deadline", err)
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
