package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/vorash/vora/internal/audit"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
	"github.com/vorash/vora/internal/ulid"
)

// A reader of the trail must be able to tell "this run used nothing" from
// "nobody measured this run": the second is a gap, and printing it as zero makes
// the gap look like a result.
func TestExecRecordSaysWhatWasMeasured(t *testing.T) {
	absent := []string{"cpuMillis", "memoryPeakBytes", "oomKills", "pidsLimitHits"}
	cases := []struct {
		name    string
		backend *fakeExecBackend
		present []string
		missing []string
	}{
		{
			name:    "a backend that cannot meter",
			backend: &fakeExecBackend{meterErr: errors.New("no cgroup")},
			missing: absent,
		},
		{
			name:    "a window that cannot be read",
			backend: &fakeExecBackend{meter: fakeMeter{err: errors.New("sandbox died")}},
			missing: absent,
		},
		{
			name:    "a measured zero",
			backend: &fakeExecBackend{meter: fakeMeter{usage: runtime.Usage{PeakScope: runtime.PeakWindow}}},
			present: absent,
		},
		{
			name:    "counters read with no memory.peak to read",
			backend: &fakeExecBackend{meter: fakeMeter{usage: runtime.Usage{CPUMillis: 7}}},
			present: []string{"cpuMillis", "oomKills", "pidsLimitHits"},
			missing: []string{"memoryPeakBytes"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.backend.stream = exitStream(0, runtime.ReasonExited)
			_, rec := execOnce(t, c.backend)
			data, err := protojson.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			wire := string(data)
			for _, field := range c.present {
				if !strings.Contains(wire, `"`+field+`"`) {
					t.Errorf("%s is missing from %s, so a measurement reads as not measured", field, wire)
				}
			}
			for _, field := range c.missing {
				if strings.Contains(wire, `"`+field+`"`) {
					t.Errorf("%s is in %s, but nothing measured it", field, wire)
				}
			}
		})
	}
}

// failingCounts is an audit store that can't answer EgressCounts.
type failingCounts struct{ audit.Store }

func (failingCounts) EgressCounts(context.Context, string, time.Time, time.Time) (int64, int64, error) {
	return 0, 0, errors.New("database gone")
}

func TestExecRecordEgressCounts(t *testing.T) {
	counted := func(t *testing.T, s *Server, id string) *sandbox.ExecRecord {
		t.Helper()
		s.recordExec(id, runtime.ExecRequest{ExecutionID: ulid.NewExec(), Command: "true"}, time.Now().Add(-time.Minute), 0, 0, 0, runtime.ReasonExited, runtime.Usage{}, "", execOptions{})
		recs, err := s.audit.Execs(context.Background(), id)
		if err != nil || len(recs) != 1 {
			t.Fatalf("Execs = %v, %v", recs, err)
		}
		return &recs[0]
	}
	t.Run("counted", func(t *testing.T) {
		s := auditServer(t)
		rec := counted(t, s, startSandbox(t, s))
		if rec.EgressAllowed == nil || *rec.EgressAllowed != 0 || rec.EgressDenied == nil || *rec.EgressDenied != 0 {
			t.Fatalf("egress = %v/%v, want a counted 0/0", rec.EgressAllowed, rec.EgressDenied)
		}
	})
	t.Run("the store could not count", func(t *testing.T) {
		s := auditServer(t)
		id := startSandbox(t, s)
		s.SetAudit(failingCounts{s.audit})
		rec := counted(t, s, id)
		if rec.EgressAllowed != nil || rec.EgressDenied != nil {
			t.Fatalf("egress = %v/%v, want not measured: a failed count must not claim no blocked traffic", rec.EgressAllowed, rec.EgressDenied)
		}
	})
}

// A member's reset outcome reaches the trail in both forms: the columns for a
// query, and reason for the wire, whose SandboxEvent carries nothing else.
func TestPoolEventKeepsKindDetailAndReasonTogether(t *testing.T) {
	h := newWarmHarness(t)
	p := h.create(poolSpec(0, 1))
	h.s.events.flush()
	events, err := h.s.audit.Events(context.Background(), p.GetId())
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %v, %v", events, err)
	}
	e := events[0]
	if e.Kind != "pool_create" || e.Detail == "" || e.Reason != e.Kind+": "+e.Detail {
		t.Fatalf("event = %+v, want reason to be kind and detail", e)
	}
}
