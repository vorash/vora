package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/audit"
	"github.com/vorash/vora/internal/runtime"
	"github.com/vorash/vora/internal/sandbox"
)

// auditServer returns a runner whose "fake" backend completes every exec.
func auditServer(t *testing.T) *Server {
	t.Helper()
	stream := &fakeExecStream{
		chunks:  []runtime.ExecChunk{{Exit: &runtime.ExecResult{ExitCode: 0, Reason: runtime.ReasonExited}}},
		release: make(chan struct{}),
	}
	stream.Close() // no test here observes mid-stream state; let Recv reach EOF
	reg := runtime.NewRegistry()
	if err := reg.Register(&fakeExecBackend{stream: stream}); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, reg)
	if err := os.MkdirAll(filepath.Join(s.cfg.StateDir, "images", "img", "latest", "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

func startSandbox(t *testing.T, s *Server) string {
	t.Helper()
	ctx := context.Background()
	created, err := s.CreateSandbox(ctx, &v1.CreateSandboxRequest{Spec: &v1.SandboxSpec{
		Image: "img", Backend: "fake",
		Resources: &v1.ResourceLimits{CpuMillis: 100, MemoryBytes: 1 << 20, Pids: 4},
		Network:   &v1.NetworkPolicy{Mode: v1.NetworkMode_NETWORK_NONE},
	}})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	id := created.GetSandbox().GetId()
	if _, err := s.StartSandbox(ctx, &v1.StartSandboxRequest{SandboxId: id}); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	return id
}

// auditedSandbox runs one exec in a fresh sandbox and records one egress
// decision, so every audit stream (events, execs, egress) has content.
func auditedSandbox(t *testing.T) (*Server, string) {
	t.Helper()
	s := auditServer(t)
	id := startSandbox(t, s)
	ctx := context.Background()
	ss := &execServerStream{got: make(chan struct{}, 8)}
	if err := s.ExecSandbox(&v1.ExecSandboxRequest{SandboxId: id, Exec: &v1.ExecRequest{Command: "true"}}, ss); err != nil {
		t.Fatalf("ExecSandbox: %v", err)
	}
	if err := s.audit.AppendEgress(ctx, id, sandbox.EgressRecord{SandboxID: id, Host: "example.com", Allowed: true, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return s, id
}

// backdate rewrites a record's updated_at, which Store.Save always resets.
func backdate(t *testing.T, s *Server, id string, age time.Duration) {
	t.Helper()
	path := filepath.Join(s.store.Dir(id), "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec["updated_at"] = time.Now().Add(-age).UTC().Format(time.RFC3339Nano)
	data, err = json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// requireAuditIntact fails unless the sandbox's whole audit trail is still
// readable: the point of the trail is to outlive the sandbox it describes.
func requireAuditIntact(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx := context.Background()
	execs, err := s.GetSandboxExecs(ctx, &v1.GetSandboxExecsRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("GetSandboxExecs: %v", err)
	}
	if len(execs.GetExecs()) != 1 {
		t.Fatalf("exec records = %d, want 1: the trail was erased", len(execs.GetExecs()))
	}
	events, err := s.GetSandboxEvents(ctx, &v1.GetSandboxEventsRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	var destroyed bool
	for _, e := range events.GetEvents() {
		if e.GetTo() == string(sandbox.StateDestroyed) {
			destroyed = true
		}
	}
	if !destroyed {
		t.Fatalf("events = %v, want the DESTROYED transition retained", events.GetEvents())
	}
	egress, err := s.GetSandboxEgress(ctx, &v1.GetSandboxEgressRequest{SandboxId: id})
	if err != nil {
		t.Fatalf("GetSandboxEgress: %v", err)
	}
	if len(egress.GetRecords()) != 1 {
		t.Fatalf("egress records = %d, want 1: the trail was erased", len(egress.GetRecords()))
	}
}

// A sandbox-scoped key can destroy its own sandbox, and TTL expiry does the
// same to every sandbox. The record of what ran must not disappear with it.
func TestAuditSurvivesDestroy(t *testing.T) {
	s, id := auditedSandbox(t)
	if _, err := s.DestroySandbox(context.Background(), &v1.DestroySandboxRequest{SandboxId: id}); err != nil {
		t.Fatal(err)
	}
	requireAuditIntact(t, s, id)
}

func TestAuditSurvivesTTLExpiry(t *testing.T) {
	s, id := auditedSandbox(t)
	rec, err := s.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	rec.Spec.TTL = time.Second
	rec.CreatedAt = time.Now().Add(-time.Hour)
	if err := s.store.Save(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.ReapExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireAuditIntact(t, s, id)
}

func TestAuditSurvivesDiskQuotaEnforcement(t *testing.T) {
	s, id := auditedSandbox(t)
	rec, err := s.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	rec.Spec.Resources.DiskBytes = 1024
	if err := s.store.Save(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.Spec.Workspace.WorkspaceDir, "data"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.EnforceDiskQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireAuditIntact(t, s, id)
}

func TestAuditSurvivesReconcile(t *testing.T) {
	s, id := auditedSandbox(t)
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Reconcile fails the sandbox rather than destroying it, so the terminal
	// event is FAILED; the trail is what matters here.
	execs, err := s.GetSandboxExecs(context.Background(), &v1.GetSandboxExecsRequest{SandboxId: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(execs.GetExecs()) != 1 {
		t.Fatalf("exec records = %d, want 1 after reconcile", len(execs.GetExecs()))
	}
}

type failingExecAudit struct{ audit.Store }

func (failingExecAudit) AppendExec(context.Context, string, sandbox.ExecRecord) error {
	return errors.New("disk full")
}

// A command that ran but left no record is an audit gap. It must show up in
// vora_audit_write_failed_total, the same signal a lost egress record raises,
// so an operator can alert on it instead of finding it in a Warn log line.
func TestExecRecordWriteFailureIsMetered(t *testing.T) {
	s := testServer(t, runtime.NewRegistry())
	s.SetAudit(failingExecAudit{Store: s.audit})

	s.recordExec("sbx_x", runtime.ExecRequest{ExecutionID: "exe_x", Command: "true"}, time.Now(), 0, 0, 0, runtime.ReasonExited, runtime.Usage{}, "", execOptions{})

	var m dto.Metric
	if err := s.metrics.AuditWriteFailedTotal.Write(&m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetCounter().GetValue(); got != 1 {
		t.Fatalf("vora_audit_write_failed_total = %v, want 1", got)
	}
}

// Retention is the only thing allowed to delete an audit trail, and only for
// sandboxes that are finished and older than the operator's limit.
func TestPruneAuditHonoursRetention(t *testing.T) {
	ctx := context.Background()
	s := auditServer(t)
	old, fresh := startSandbox(t, s), startSandbox(t, s)
	for _, id := range []string{old, fresh} {
		if _, err := s.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: id}); err != nil {
			t.Fatal(err)
		}
	}
	backdate(t, s, old, 48*time.Hour)

	if err := s.PruneAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Load(old); err != nil {
		t.Fatalf("PruneAudit with no retention configured removed a trail: %v", err)
	}

	s.cfg.AuditRetention = 24 * time.Hour
	if err := s.PruneAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Load(old); err == nil {
		t.Fatal("trail older than the retention limit was kept")
	}
	if _, err := s.store.Load(fresh); err != nil {
		t.Fatalf("trail younger than the retention limit was removed: %v", err)
	}
}

// A STOPPED sandbox is not active but is not finished either: pruning it would
// delete a sandbox that can still be restarted, along with its workspace.
func TestPruneAuditOnlyRemovesFinishedSandboxes(t *testing.T) {
	s := auditServer(t)
	s.cfg.AuditRetention = time.Nanosecond
	running := startSandbox(t, s)
	stopped := startSandbox(t, s)
	rec, err := s.store.Load(stopped)
	if err != nil {
		t.Fatal(err)
	}
	rec.State = sandbox.StateStopped
	if err := s.store.Save(rec); err != nil {
		t.Fatal(err)
	}
	backdate(t, s, running, 48*time.Hour)
	backdate(t, s, stopped, 48*time.Hour)

	if err := s.PruneAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"running": running, "stopped": stopped} {
		if _, err := s.store.Load(id); err != nil {
			t.Fatalf("%s sandbox was pruned: %v", name, err)
		}
	}
}

// With an external audit store the tombstone is the only way the API reaches
// that store's rows, so retention (a local-disk policy) must leave it alone.
func TestPruneAuditSkipsExternalAuditStore(t *testing.T) {
	ctx := context.Background()
	s := auditServer(t)
	id := startSandbox(t, s)
	if _, err := s.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: id}); err != nil {
		t.Fatal(err)
	}
	backdate(t, s, id, 48*time.Hour)
	s.cfg.AuditRetention = time.Hour
	s.SetAudit(failingExecAudit{Store: s.audit})

	if err := s.PruneAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Load(id); err != nil {
		t.Fatalf("tombstone pruned although audit lives in an external store: %v", err)
	}
}

type failingEventAudit struct{ audit.Store }

func (failingEventAudit) AppendEvent(context.Context, string, sandbox.Event) error {
	return errors.New("disk full")
}

// A lost lifecycle event, including the DESTROYED transition that marks a
// tombstone, is as much an audit gap as a lost exec record and must raise the
// same alertable metric.
func TestLifecycleEventWriteFailureIsMetered(t *testing.T) {
	s := auditServer(t)
	id := startSandbox(t, s)
	s.SetAudit(failingEventAudit{Store: s.audit})
	if _, err := s.DestroySandbox(context.Background(), &v1.DestroySandboxRequest{SandboxId: id}); err != nil {
		t.Fatal(err)
	}
	var m dto.Metric
	if err := s.metrics.AuditWriteFailedTotal.Write(&m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetCounter().GetValue(); got < 1 {
		t.Fatalf("vora_audit_write_failed_total = %v, want the lost DESTROYED event counted", got)
	}
}

// Pruning must not delete a tombstone out from under a destroy that is still
// running: the destroy would then recreate a partial record with no exec or
// egress history, which reads like tampering. Pruning re-checks the record
// under the sandbox lock, after any in-flight destroy has finished.
func TestPruneAuditRechecksUnderSandboxLock(t *testing.T) {
	ctx := context.Background()
	s := auditServer(t)
	s.cfg.AuditRetention = time.Hour
	id := startSandbox(t, s)
	if _, err := s.DestroySandbox(ctx, &v1.DestroySandboxRequest{SandboxId: id}); err != nil {
		t.Fatal(err)
	}
	backdate(t, s, id, 48*time.Hour)

	// Stand in for a destroy in progress: hold the lock, and before releasing
	// it rewrite the record the way a destroy's transition does.
	unlock := s.lockSandbox(id)
	done := make(chan error, 1)
	go func() { done <- s.PruneAudit(ctx) }()
	time.Sleep(50 * time.Millisecond)
	rec, err := s.store.Load(id)
	if err != nil {
		unlock()
		t.Fatalf("prune ran while the sandbox lock was held: %v", err)
	}
	if err := s.store.Save(rec); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Load(id); err != nil {
		t.Fatalf("prune deleted a tombstone that was rewritten while it waited: %v", err)
	}
}
