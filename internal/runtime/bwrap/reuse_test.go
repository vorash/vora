package bwrap

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/agentproto"
	"github.com/vorash/vora/internal/cgroup"
	"github.com/vorash/vora/internal/runtime"
)

// fakeAgent answers Capture with a fixed manifest and Reset through a hook.
type fakeAgent struct {
	countingAgent
	manifest []byte
	reset    func(manifest []byte) (int, error)
}

func (a *fakeAgent) Capture(context.Context) ([]byte, error) { return a.manifest, nil }
func (a *fakeAgent) Reset(_ context.Context, manifest []byte) (int, error) {
	return a.reset(manifest)
}

// fakeSandbox is a handle whose agent is in-process and whose cgroup is a
// directory of plain files, so the host side of a reset runs without a sandbox.
type fakeSandbox struct {
	b     *Backend
	hd    *handle
	agent *fakeAgent
	dir   string
	stop  func()
}

func newFakeSandbox(t *testing.T) *fakeSandbox {
	t.Helper()
	b, _ := newTestBackend(t)
	socket := filepath.Join(t.TempDir(), "control.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{manifest: []byte(`{"fake":true}`), reset: func([]byte) (int, error) { return 0, nil }}
	server := grpc.NewServer()
	v1.RegisterAgentServiceServer(server, agentproto.NewServer(agent))
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	client, err := agentproto.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.WaitReady(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}

	group, err := cgroup.OpenSubtree(t.TempDir(), "").Create(context.Background(), "sbx_fake", runtime.ResourceLimits{CPUMillis: 1000, MemoryBytes: 1 << 20, Pids: 16})
	if err != nil {
		t.Fatal(err)
	}
	hd := &handle{id: "sbx_fake", client: client, group: group}
	b.mu.Lock()
	b.handles[hd.id] = hd
	b.mu.Unlock()
	s := &fakeSandbox{b: b, hd: hd, agent: agent, dir: group.Path(), stop: server.Stop}
	s.setProcs(os.Getpid())
	s.write("cgroup.freeze", "0\n")
	s.write("cgroup.events", "populated 1\nfrozen 1\n")
	return s
}

func (s *fakeSandbox) write(name, content string) {
	if err := os.WriteFile(filepath.Join(s.dir, name), []byte(content), 0o644); err != nil {
		panic(err)
	}
}

func (s *fakeSandbox) read(name string) string {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(data))
}

func (s *fakeSandbox) setProcs(pids ...int) {
	var lines []string
	for _, pid := range pids {
		lines = append(lines, strconv.Itoa(pid))
	}
	s.write("cgroup.procs", strings.Join(lines, "\n")+"\n")
}

func (s *fakeSandbox) prepare(t *testing.T) runtime.Resettable {
	t.Helper()
	r, err := s.b.Prepare(context.Background(), s.hd)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return r
}

func TestPrepareCapturesAndAdmits(t *testing.T) {
	s := newFakeSandbox(t)
	var got []byte
	s.agent.reset = func(manifest []byte) (int, error) { got = manifest; return 0, nil }
	r := s.prepare(t)
	if string(got) != string(s.agent.manifest) {
		t.Fatalf("admission reset got manifest %q, want the captured %q", got, s.agent.manifest)
	}
	report, err := r.Reset(context.Background())
	if err != nil || report != (runtime.ResetReport{}) {
		t.Fatalf("Reset = %+v, %v, want nothing to do", report, err)
	}
	if err := r.Alive(context.Background()); err != nil {
		t.Fatalf("Alive: %v", err)
	}
}

// A fresh sandbox has nothing to reset. One whose admission reset finds
// something has a wrong baseline or manifest, and is not admitted.
func TestPrepareRefusesASandboxItCannotProve(t *testing.T) {
	t.Run("admission reset removed entries", func(t *testing.T) {
		s := newFakeSandbox(t)
		s.agent.reset = func([]byte) (int, error) { return 1, nil }
		if _, err := s.b.Prepare(context.Background(), s.hd); !errors.Is(err, runtime.ErrUnproven) {
			t.Fatalf("Prepare = %v, want ErrUnproven", err)
		}
	})
	t.Run("admission reset failed", func(t *testing.T) {
		s := newFakeSandbox(t)
		s.agent.reset = func([]byte) (int, error) { return 0, errors.New("listed entry changed") }
		if _, err := s.b.Prepare(context.Background(), s.hd); !errors.Is(err, runtime.ErrUnproven) {
			t.Fatalf("Prepare = %v, want ErrUnproven", err)
		}
	})
	t.Run("agent not in the cgroup", func(t *testing.T) {
		s := newFakeSandbox(t)
		s.setProcs(os.Getpid() + 1)
		if _, err := s.b.Prepare(context.Background(), s.hd); err == nil || !strings.Contains(err.Error(), "not in cgroup") {
			t.Fatalf("Prepare = %v, want the agent missing from the cgroup", err)
		}
	})
}

// The host proves the process set after the agent's reset too: an agent that
// reports clean while the cgroup holds more than the baseline is not believed.
func TestResetProvesTheProcessSetAfterTheAgent(t *testing.T) {
	s := newFakeSandbox(t)
	r := s.prepare(t)
	s.agent.reset = func([]byte) (int, error) {
		s.setProcs(os.Getpid(), os.Getppid())
		return 0, nil
	}
	_, err := r.Reset(context.Background())
	if !errors.Is(err, runtime.ErrUnproven) || !strings.Contains(err.Error(), "cgroup holds") {
		t.Fatalf("Reset = %v, want ErrUnproven naming the cgroup's processes", err)
	}
}

func TestResetWrapsEveryFailureInErrUnproven(t *testing.T) {
	t.Run("agent refused", func(t *testing.T) {
		s := newFakeSandbox(t)
		r := s.prepare(t)
		s.agent.reset = func([]byte) (int, error) { return 0, errors.New("listed entry changed: control.sock") }
		_, err := r.Reset(context.Background())
		if !errors.Is(err, runtime.ErrUnproven) || !strings.Contains(err.Error(), "control.sock") {
			t.Fatalf("Reset = %v, want ErrUnproven carrying the agent's reason", err)
		}
	})
	t.Run("cgroup gone", func(t *testing.T) {
		s := newFakeSandbox(t)
		r := s.prepare(t)
		if err := os.RemoveAll(s.dir); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		_, err := r.Reset(ctx)
		if !errors.Is(err, runtime.ErrUnproven) || !errors.Is(err, cgroup.ErrNotFound) {
			t.Fatalf("Reset = %v, want ErrUnproven and ErrNotFound", err)
		}
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Fatalf("Reset of a vanished cgroup took %v", took)
		}
	})
	t.Run("agent gone", func(t *testing.T) {
		s := newFakeSandbox(t)
		r := s.prepare(t)
		s.stop()
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := r.Reset(ctx); !errors.Is(err, runtime.ErrUnproven) {
			t.Fatalf("Reset = %v, want ErrUnproven", err)
		}
		if err := r.Alive(ctx); err == nil {
			t.Fatal("Alive succeeded with the agent gone")
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("Reset and Alive with the agent gone took %v", took)
		}
	})
}

// A cgroup that never freezes holds a task in an uninterruptible wait. The
// reset ends at the caller's deadline, unproven, and leaves the cgroup thawed
// so that destroying it does not hang.
func TestResetHonoursTheDeadlineAndThaws(t *testing.T) {
	s := newFakeSandbox(t)
	r := s.prepare(t)
	s.write("cgroup.events", "populated 1\nfrozen 0\n")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Reset(ctx)
	took := time.Since(start)
	if !errors.Is(err, runtime.ErrUnproven) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reset = %v, want ErrUnproven and the deadline", err)
	}
	if took > 500*time.Millisecond {
		t.Fatalf("Reset took %v past a 50ms deadline", took)
	}
	if got := s.read("cgroup.freeze"); got != "0" {
		t.Fatalf("cgroup.freeze = %q after a failed reset, want 0", got)
	}
}
