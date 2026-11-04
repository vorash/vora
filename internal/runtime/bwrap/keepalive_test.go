package bwrap

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/agentproto"
	"github.com/vorash/vora/internal/config"
)

const testKeepalive = 10 * time.Millisecond

// countingAgent stands in for vora-init. agentproto answers a Ping with PID,
// so pings counts the runner's keepalive pings.
type countingAgent struct{ pings atomic.Int64 }

func (a *countingAgent) PID() int {
	a.pings.Add(1)
	return 1
}
func (a *countingAgent) Exec(context.Context, *v1.ExecRequest, func(*v1.ExecChunk) error) error {
	return nil
}
func (a *countingAgent) Signal(context.Context, int32) error { return nil }
func (a *countingAgent) Shutdown(context.Context) error      { return nil }

// readySandbox registers a handle whose agent is ready, as Start leaves it,
// and returns the agent and a function that stops the agent's server.
func readySandbox(t *testing.T, b *Backend, id string) (*handle, *countingAgent, func()) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "control.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	agent := &countingAgent{}
	server := grpc.NewServer()
	v1.RegisterAgentServiceServer(server, agentproto.NewServer(agent))
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	client, err := agentproto.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	hd := &handle{id: id, client: client}
	b.mu.Lock()
	b.handles[id] = hd
	b.mu.Unlock()
	b.startKeepalive(hd)
	return hd, agent, server.Stop
}

type runnerLog struct {
	mu  sync.Mutex
	out strings.Builder
}

func (l *runnerLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.out.Write(p)
}

func (l *runnerLog) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.out.String(), substr)
}

func newTestBackend(t *testing.T) (*Backend, *runnerLog) {
	t.Helper()
	t.Setenv("VORA_INIT_BINARY", "/bin/true")
	log := &runnerLog{}
	b, err := New(config.Config{}, nil, slog.New(slog.NewTextHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	b.keepalive = testKeepalive
	t.Cleanup(func() { _ = b.Close() })
	return b, log
}

func waitPings(t *testing.T, agent *countingAgent, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for agent.pings.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("agent got %d keepalive pings, want at least %d", agent.pings.Load(), want)
		}
		time.Sleep(testKeepalive)
	}
}

// assertNoMorePings fails if the agent is pinged again over many intervals. A
// ping cancelled by the stop can still reach the server, so the count settles
// first.
func assertNoMorePings(t *testing.T, agent *countingAgent) {
	t.Helper()
	time.Sleep(5 * testKeepalive)
	before := agent.pings.Load()
	time.Sleep(20 * testKeepalive)
	if after := agent.pings.Load(); after != before {
		t.Fatalf("agent got %d pings after keepalive stopped", after-before)
	}
}

// vora-init exits once nothing contacts it for its grace, so an idle sandbox
// must keep receiving pings from the runner (vorash/vora#113).
func TestKeepalivePingsAnIdleSandbox(t *testing.T) {
	b, _ := newTestBackend(t)
	_, agent, _ := readySandbox(t, b, "sbx_idle")
	waitPings(t, agent, 5)
}

// Destroy ends the sandbox's keepalive before it shuts the agent down and
// closes the client. A keepalive left running would ping a closed client and
// report a healthy teardown as a failure in the runner log.
func TestKeepaliveStopsOnDestroy(t *testing.T) {
	b, log := newTestBackend(t)
	hd, agent, _ := readySandbox(t, b, "sbx_destroy")
	other, otherAgent, _ := readySandbox(t, b, "sbx_other")
	waitPings(t, agent, 2)

	if err := b.Destroy(context.Background(), hd); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	assertNoMorePings(t, agent)
	if n := log.count("keepalive ping failed"); n != 0 {
		t.Fatalf("runner logged %d keepalive failures after a clean destroy", n)
	}

	before := otherAgent.pings.Load()
	waitPings(t, otherAgent, before+2)
	if _, err := b.handle(other); err != nil {
		t.Fatalf("other sandbox lost after a sibling's destroy: %v", err)
	}
}

// Runner shutdown must stop every keepalive, so the watchdog still ends
// sandboxes whose runner is gone.
func TestKeepaliveStopsOnClose(t *testing.T) {
	b, _ := newTestBackend(t)
	_, first, _ := readySandbox(t, b, "sbx_first")
	_, second, _ := readySandbox(t, b, "sbx_second")
	waitPings(t, first, 2)
	waitPings(t, second, 2)

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertNoMorePings(t, first)
	assertNoMorePings(t, second)
}

// A failed ping is not a verdict on the sandbox: the handle stays registered
// with its client and keepalive, a sibling keeps being pinged, and the outage
// is logged once rather than on every interval.
func TestKeepaliveFailureLeavesTheSandbox(t *testing.T) {
	b, log := newTestBackend(t)
	hd, _, stopAgent := readySandbox(t, b, "sbx_unreachable")
	_, sibling, _ := readySandbox(t, b, "sbx_sibling")
	stopAgent()

	before := sibling.pings.Load()
	waitPings(t, sibling, before+10)
	if n := log.count("keepalive ping failed"); n != 1 {
		t.Fatalf("runner logged %d keepalive failures for one outage, want 1", n)
	}
	if _, err := b.handle(hd); err != nil {
		t.Fatalf("unreachable sandbox dropped after failed pings: %v", err)
	}
	if hd.client == nil || hd.stopKeepalive == nil {
		t.Fatal("failed pings tore down the handle's agent client or keepalive")
	}
}
