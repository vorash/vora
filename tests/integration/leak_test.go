//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vorash/vora/internal/runtime"
)

// leakSandbox is a pool member under test on either backend: a run is one exec,
// and a reset comes between runs. writable lists the mounts a run can write to,
// and initPIDs is what a run sees in /proc besides itself when nothing leaked.
type leakSandbox struct {
	backend  runtime.Backend
	h        runtime.Handle
	reset    func(context.Context) (runtime.ResetReport, error)
	writable []string
	initPIDs []int
}

// leakCase is one row of the design's leak matrix: run A plants, the member is
// reset, and run B checks. check prints what leaked and nothing when clean.
// Every case also compares a snapshot of the writable mounts across the reset,
// so the files a plant leaves are checked whether or not check looks for them.
type leakCase struct {
	name  string
	plant string
	// timeout, when set, is the plant's exec timeout, and the plant is expected
	// to be killed by it.
	timeout time.Duration
	env     map[string]string
	check   string
}

// userSite is where Python loads usercustomize.py and .pth files from, under
// the home a reset must put back.
const userSite = `$(python3 -c 'import site; print(site.getusersitepackages())')`

var leakCases = []leakCase{
	{
		name: "L1 files in /tmp, /dev/shm and /dev",
		plant: `set -e
for d in /tmp /dev/shm; do
	mkdir -p "$d/.hidden/nested/deeper"; echo run > "$d/.hidden/nested/deeper/file"
	mkdir "$d/locked"; echo run > "$d/locked/secret"; chmod 000 "$d/locked"
	ln -s /etc "$d/link-out"
done
echo run > /dev/planted`,
		check: `find /tmp /dev/shm -mindepth 1 2>/dev/null; [ -e /dev/planted ] && echo /dev/planted; true`,
	},
	{
		name: "L2 files in /workspace and /root, usercustomize.py, a .pth and a shell rc",
		plant: `set -e
echo run > /workspace/.planted
mkdir -p /root/.config; echo run > /root/.config/planted
site=` + userSite + `
mkdir -p "$site"
echo 'print("LEAK usercustomize")' > "$site/usercustomize.py"
echo 'import os; os.environ["LEAK_PTH"] = "1"' > "$site/leak.pth"
echo 'export LEAK_RC=1' > /root/.profile
python3 -c 'import os; print(os.environ.get("LEAK_PTH", ""))' | grep -q 1`,
		check: `python3 -c 'import os; print(os.environ.get("LEAK_PTH", ""))'; sh -lc 'echo "$LEAK_RC"'`,
	},
	{
		name:  "L3 extra files in /vora",
		plant: `set -e; echo run > /vora/planted; mkdir /vora/dir; echo run > /vora/dir/inner`,
		check: `ls /vora/planted /vora/dir 2>/dev/null; true`,
	},
	{
		name:  "L4 environment variables",
		plant: `export LEAK_EXPORTED=1; [ "$LEAK_REQUEST" = 1 ]`,
		env:   map[string]string{"LEAK_REQUEST": "1"},
		check: `echo "$LEAK_EXPORTED$LEAK_REQUEST"`,
	},
	{
		name: "L5 background processes",
		plant: `
sleep 300 </dev/null >/dev/null 2>&1 &
setsid sleep 300 </dev/null >/dev/null 2>&1 &
(setsid sh -c 'sleep 300 & sleep 300' </dev/null >/dev/null 2>&1 &)
nohup sleep 300 </dev/null >/dev/null 2>&1 &
`,
	},
	{
		name:    "L6 a process holding the exec's stdout",
		plant:   `setsid sleep 300 </dev/null & sleep 300`,
		timeout: time.Second,
	},
}

// testLeakMatrix runs every case on one member, as a pool would, and fails on
// the first thing a later run could read. The snapshot before the first plant
// is the member's prepared state, and every reset must return to it.
func testLeakMatrix(t *testing.T, s leakSandbox) {
	t.Helper()
	ctx := context.Background()
	before := snapshot(t, s.backend, s.h, s.writable)
	for _, tc := range leakCases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := execSandbox(t, s.backend, s.h, runtime.ExecRequest{
				Command: "/bin/sh", Args: []string{"-c", tc.plant}, Env: tc.env, Timeout: tc.timeout,
			})
			if tc.timeout == 0 && code != 0 {
				t.Fatalf("plant exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			if tc.timeout != 0 && code == 0 {
				t.Fatalf("plant exited 0, want it killed by its %s timeout", tc.timeout)
			}
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			report, err := s.reset(rctx)
			if err != nil {
				t.Fatalf("reset: %v", err)
			}
			t.Logf("reset: killed %d, removed %d", report.ProcessesKilled, report.EntriesRemoved)

			if after := snapshot(t, s.backend, s.h, s.writable); after != before {
				t.Fatalf("writable mounts not back to the prepared state\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if tc.check != "" {
				if out := strings.TrimSpace(mustExec(t, s.backend, s.h, "/bin/sh", "-c", tc.check)); out != "" {
					t.Fatalf("run B found what run A left:\n%s", out)
				}
			}
			if got := processes(t, s.backend, s.h); !slices.Equal(got, s.initPIDs) {
				t.Fatalf("processes after the reset = %v, want %v", got, s.initPIDs)
			}
		})
	}
}

// testLeakControlSocketGone is L3's second half and L9: a run that removed the
// control socket leaves a member that is never proven clean again, however
// often a reset is tried.
func testLeakControlSocketGone(t *testing.T, s leakSandbox) {
	t.Helper()
	mustExec(t, s.backend, s.h, "/bin/rm", "/vora/control.sock")
	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := s.reset(ctx)
		cancel()
		if !errors.Is(err, runtime.ErrUnproven) || !strings.Contains(err.Error(), "control.sock") {
			t.Fatalf("reset %d = %v, want ErrUnproven naming control.sock", i, err)
		}
	}
}
