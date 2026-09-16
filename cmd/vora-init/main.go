// Command vora-init is the in-sandbox agent. It runs as PID 1 inside a
// Bubblewrap sandbox and serves the AgentService over a Unix socket bound in
// from the runner, providing exec, signal forwarding and shutdown.
//
// Bubblewrap cannot exec into an existing sandbox, so this agent is what makes
// a sandbox a long-lived unit with repeatable `Exec` calls.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/agentproto"
)

const chunkSize = 32 * 1024

func main() {
	socket := flag.String("socket", "/run/vora/control.sock", "control socket path inside the sandbox")
	grace := flag.Duration("grace", 20*time.Second, "exit if the runner has not contacted us for this long")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	a := newAgent(*socket, *grace, logger)

	if err := os.MkdirAll(filepath.Dir(*socket), 0o755); err != nil {
		logger.Error("create socket dir", "error", err)
		os.Exit(1)
	}
	_ = os.Remove(*socket)
	lis, err := net.Listen("unix", *socket)
	if err != nil {
		logger.Error("listen", "socket", *socket, "error", err)
		os.Exit(1)
	}
	// The socket must be reachable by the runner even across user namespaces.
	if err := os.Chmod(*socket, 0o666); err != nil {
		logger.Warn("chmod socket", "error", err)
	}

	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(a.unaryInterceptor),
		grpc.StreamInterceptor(a.streamInterceptor),
	)
	v1.RegisterAgentServiceServer(grpcServer, agentproto.NewServer(a))

	go a.reapOrphans()
	go a.watchdog()
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			logger.Error("agent serve", "error", err)
			a.triggerShutdown()
		}
	}()

	logger.Info("vora-init ready", "pid", os.Getpid(), "socket", *socket, "grace", grace.String())
	<-a.done

	logger.Info("vora-init shutting down")
	stopped := make(chan struct{})
	go func() { grpcServer.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		grpcServer.Stop()
	}
	os.Exit(0)
}

// agent implements agentproto.Handler.
type agent struct {
	socket   string
	grace    time.Duration
	logger   *slog.Logger
	lastSeen atomic.Int64

	// execMu serialises command execution against the orphan reaper, which
	// must never reap a child the Go runtime is waiting on.
	execMu sync.Mutex

	mu   sync.Mutex
	send sync.Mutex // serialises stream.Send
	pids map[int]struct{}

	done     chan struct{}
	doneOnce sync.Once
}

func newAgent(socket string, grace time.Duration, logger *slog.Logger) *agent {
	a := &agent{
		socket: socket,
		grace:  grace,
		logger: logger,
		pids:   make(map[int]struct{}),
		done:   make(chan struct{}),
	}
	a.lastSeen.Store(time.Now().UnixNano())
	return a
}

func (a *agent) touch()           { a.lastSeen.Store(time.Now().UnixNano()) }
func (a *agent) PID() int         { return os.Getpid() }
func (a *agent) triggerShutdown() { a.doneOnce.Do(func() { close(a.done) }) }
func (a *agent) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	a.touch()
	return handler(ctx, req)
}
func (a *agent) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	a.touch()
	return handler(srv, ss)
}

// watchdog exits the sandbox if the runner stops contacting the agent, which
// preserves die-with-parent semantics when the runner dies unexpectedly.
func (a *agent) watchdog() {
	interval := a.grace / 4
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			last := time.Unix(0, a.lastSeen.Load())
			if time.Since(last) > a.grace {
				a.logger.Warn("runner detached, exiting", "idle_for", time.Since(last).String())
				a.triggerShutdown()
				return
			}
		}
	}
}

// reapOrphans reaps reparented children when no exec is in flight, so it does
// not steal children that os/exec is waiting on.
func (a *agent) reapOrphans() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	for {
		select {
		case <-a.done:
			return
		case <-ch:
			a.execMu.Lock()
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
			}
			a.execMu.Unlock()
		}
	}
}

// Exec implements agentproto.Handler.
func (a *agent) Exec(ctx context.Context, req *v1.ExecRequest, send func(*v1.ExecChunk) error) error {
	a.touch()
	command := req.GetCommand()
	if command == "" {
		return fmt.Errorf("empty command")
	}

	cwd := req.GetCwd()
	if cwd == "" {
		cwd = "/workspace"
	}
	cmd := exec.Command(command, req.GetArgs()...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), req.GetEnv()...)
	if len(req.GetStdin()) > 0 {
		cmd.Stdin = bytes.NewReader(req.GetStdin())
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Hold execMu from before Start until after Wait so the orphan reaper
	// cannot reap this child.
	a.execMu.Lock()
	defer a.execMu.Unlock()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// Serialise sends across the two pipe readers.
	sendChunk := func(c *v1.ExecChunk) error {
		a.send.Lock()
		defer a.send.Unlock()
		return send(c)
	}

	if err := cmd.Start(); err != nil {
		_ = sendChunk(&v1.ExecChunk{Payload: &v1.ExecChunk_Stderr{Stderr: []byte("vora-init: " + err.Error() + "\n")}})
		_ = sendChunk(&v1.ExecChunk{Payload: &v1.ExecChunk_Exit{Exit: &v1.ExecExit{
			ExitCode: 127,
			Reason:   v1.ExitReason_RUNTIME_FAILURE,
		}}})
		return nil
	}

	pgid := cmd.Process.Pid
	a.trackPID(pgid, true)
	defer a.trackPID(pgid, false)

	var wg sync.WaitGroup
	wg.Add(2)
	go streamPipe(&wg, stdout, sendChunk, func(b []byte) *v1.ExecChunk {
		return &v1.ExecChunk{Payload: &v1.ExecChunk_Stdout{Stdout: b}}
	})
	go streamPipe(&wg, stderr, sendChunk, func(b []byte) *v1.ExecChunk {
		return &v1.ExecChunk{Payload: &v1.ExecChunk_Stderr{Stderr: b}}
	})

	// Kill the process group if the client goes away.
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-waitCtx.Done()
		if ctx.Err() != nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	exitCode := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
			if exitCode < 0 {
				exitCode = 128
			}
		} else {
			return waitErr
		}
	}
	return sendChunk(&v1.ExecChunk{Payload: &v1.ExecChunk_Exit{Exit: &v1.ExecExit{
		ExitCode: int32(exitCode),
		Reason:   v1.ExitReason_EXITED,
	}}})
}

type chunkFunc func([]byte) *v1.ExecChunk

func streamPipe(wg *sync.WaitGroup, r io.Reader, send func(*v1.ExecChunk) error, mk chunkFunc) {
	defer wg.Done()
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if sendErr := send(mk(chunk)); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Signal implements agentproto.Handler: it signals all running process groups.
func (a *agent) Signal(_ context.Context, sig int32) error {
	a.touch()
	a.mu.Lock()
	pids := make([]int, 0, len(a.pids))
	for pid := range a.pids {
		pids = append(pids, pid)
	}
	a.mu.Unlock()
	for _, pid := range pids {
		_ = syscall.Kill(-pid, syscall.Signal(sig))
	}
	// Also signal PID 1's group so a bare sandbox responds.
	_ = syscall.Kill(0, syscall.Signal(sig))
	return nil
}

// Shutdown implements agentproto.Handler.
func (a *agent) Shutdown(context.Context) error {
	a.triggerShutdown()
	return nil
}

func (a *agent) trackPID(pid int, add bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if add {
		a.pids[pid] = struct{}{}
	} else {
		delete(a.pids, pid)
	}
}
