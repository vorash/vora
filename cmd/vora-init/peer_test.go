package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/agentproto"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// listenHostOnly serves a real gRPC agent on a real socket behind wrap, as main does.
// This test process is the peer, and it reads nonzero, as a workload in a sandbox does.
func listenHostOnly(t *testing.T, wrap func(net.Listener) net.Listener) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "control.sock")
	lis, err := listenAgent(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	v1.RegisterAgentServiceServer(server, agentproto.NewServer(testAgent()))
	served := make(chan error, 1)
	go func() { served <- server.Serve(wrap(lis)) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return socket
}

func hostOnly(lis net.Listener) net.Listener { return newHostOnlyListener(lis, discardLogger()) }

// firstRead dials and reports what the agent sends without being asked. A gRPC server
// sends its SETTINGS as soon as it has a connection.
func firstRead(t *testing.T, socket string) (int, error) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn.Read(make([]byte, 64))
}

// The whole point: a peer the namespace can see gets a closed socket and no gRPC. The
// unguarded case is what makes the first assertion mean something.
func TestSandboxPeerGetsNoGRPC(t *testing.T) {
	if n, err := firstRead(t, listenHostOnly(t, hostOnly)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("a refused peer read %d bytes, %v, want EOF", n, err)
	}
	unguarded := func(lis net.Listener) net.Listener { return lis }
	if n, err := firstRead(t, listenHostOnly(t, unguarded)); n == 0 || err != nil {
		t.Fatalf("an unguarded agent sent %d bytes, %v: this test cannot tell a refusal apart", n, err)
	}
}

// The runner reads as PID 0, and everything it does goes through this.
func TestHostPeerReachesTheAgent(t *testing.T) {
	socket := listenHostOnly(t, func(lis net.Listener) net.Listener {
		l := newHostOnlyListener(lis, discardLogger())
		l.peerPID = func(net.Conn) (int32, error) { return 0, nil }
		return l
	})
	client, err := agentproto.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.WaitReady(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("host peer was not served: %v", err)
	}
}

// Credentials that cannot be read are no proof of a host peer, and one bad connection
// must not end the listener, because that ends the sandbox.
func TestUnreadablePeerIsRefusedAndTheNextIsServed(t *testing.T) {
	var calls atomic.Int32
	socket := listenHostOnly(t, func(lis net.Listener) net.Listener {
		l := newHostOnlyListener(lis, discardLogger())
		l.peerPID = func(net.Conn) (int32, error) {
			if calls.Add(1) == 1 {
				return 0, errors.New("unreadable")
			}
			return 0, nil
		}
		return l
	})
	if n, err := firstRead(t, socket); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("a peer with unreadable credentials read %d bytes, %v, want EOF", n, err)
	}
	if n, err := firstRead(t, socket); n == 0 || err != nil {
		t.Fatalf("the next peer read %d bytes, %v: the refusal took the listener with it", n, err)
	}
}

func TestNonUnixPeerHasNoCredentials(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := peerPID(a); err == nil {
		t.Fatal("read credentials from a connection that has none")
	}
}

// main relies on Serve returning when the listener really fails.
func TestListenerFailureStillReachesServe(t *testing.T) {
	lis, err := listenAgent(filepath.Join(t.TempDir(), "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	l := newHostOnlyListener(lis, discardLogger())
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept on a closed listener = %v", err)
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// A workload can spin on connect for as long as it runs, and the agent lives through it
// on a task budget it cannot grow. Nothing may stay behind a refused connection.
func TestRefusedConnectionsHoldNothing(t *testing.T) {
	const connections = 5000
	var seen atomic.Int32
	socket := listenHostOnly(t, func(lis net.Listener) net.Listener {
		l := newHostOnlyListener(lis, discardLogger())
		l.peerPID = func(conn net.Conn) (int32, error) {
			seen.Add(1)
			return peerPID(conn)
		}
		return l
	})
	// One connection first, so that lazily started runtime and gRPC state is not
	// counted against the burst.
	_, _ = firstRead(t, socket)
	fds, goroutines := openFDs(t), runtime.NumGoroutine()

	var wg sync.WaitGroup
	var sent atomic.Int32
	for range 8 {
		wg.Go(func() {
			for sent.Add(1) <= connections {
				for {
					conn, err := net.Dial("unix", socket)
					// A full backlog is EAGAIN, and the agent drains it.
					if errors.Is(err, syscall.EAGAIN) {
						continue
					}
					if err != nil {
						t.Errorf("dial: %v", err)
						return
					}
					_ = conn.Close()
					break
				}
			}
		})
	}
	wg.Wait()
	for deadline := time.Now().Add(10 * time.Second); seen.Load() <= connections && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if got := seen.Load(); got <= connections {
		t.Fatalf("the listener saw %d of %d connections", got, connections)
	}
	if got := openFDs(t); got > fds {
		t.Errorf("open fds went from %d to %d", fds, got)
	}
	if got := runtime.NumGoroutine(); got > goroutines {
		t.Errorf("goroutines went from %d to %d", goroutines, got)
	}
}

// A workload that spins on connect must not be able to fill the host's journal.
func TestRefusalsAreLoggedAtARate(t *testing.T) {
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	log := &refusalLog{
		logger: slog.New(slog.NewTextHandler(&out, nil)),
		now:    func() time.Time { return now },
	}
	const burst = 10000
	for pid := range burst {
		log.note(int32(pid+3), nil)
	}
	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Fatalf("%d refusals inside one interval logged %d lines, want 1:\n%s", burst, got, out.String())
	}
	if !strings.Contains(out.String(), "pid=3") {
		t.Fatalf("the first refusal's PID is missing:\n%s", out.String())
	}

	out.Reset()
	now = now.Add(refusalLogInterval)
	log.note(99, errors.New("unreadable"))
	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Fatalf("logged %d lines after the interval, want 1:\n%s", got, out.String())
	}
	for _, want := range []string{fmt.Sprintf("refused_since_last_line=%d", burst), "unreadable"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("line lacks %q:\n%s", want, out.String())
		}
	}
}

// Getting this wrong one way opens a sandbox to its own workload, and the other way locks
// a host-run agent's client out. The PID decides first because comm cannot be trusted to
// be readable outside a sandbox, where it does not matter.
func TestRefuseSandboxPeersDecision(t *testing.T) {
	if !refuseSandboxPeers(agentPID) {
		t.Fatal("PID 2 must refuse sandbox peers, whatever PID 1 is called")
	}
	for _, pid := range []int{initPID, 12345, os.Getpid()} {
		if refuseSandboxPeers(pid) {
			t.Fatalf("PID %d refuses its clients, but every client outside a namespace reads nonzero", pid)
		}
	}
}
