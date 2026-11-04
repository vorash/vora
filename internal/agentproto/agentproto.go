// Package agentproto implements the in-sandbox agent transport: the gRPC
// server served by vora-init and the client used by the runner's Bubblewrap
// backend.
//
// Bubblewrap cannot exec into an existing sandbox, so the runner reaches a
// long-lived agent over a per-sandbox Unix socket bound into the sandbox.
package agentproto

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/runtime"
)

// Handler is the process-management side of the agent, implemented by
// vora-init. Exec must send exactly one final chunk with Exit set.
type Handler interface {
	// PID returns the agent's PID (1 inside its namespace).
	PID() int
	// Exec runs a command, calling send for each output chunk.
	Exec(ctx context.Context, req *v1.ExecRequest, send func(*v1.ExecChunk) error) error
	// Signal sends a signal to the process group.
	Signal(ctx context.Context, sig int32) error
	// Shutdown asks the agent to terminate the sandbox.
	Shutdown(ctx context.Context) error
}

// Server adapts a Handler to the generated AgentService server.
type Server struct {
	v1.UnimplementedAgentServiceServer
	handler Handler
}

// NewServer returns an AgentService backed by h.
func NewServer(h Handler) *Server { return &Server{handler: h} }

// Ping reports the agent PID and readiness.
func (s *Server) Ping(context.Context, *v1.PingRequest) (*v1.PingResponse, error) {
	return &v1.PingResponse{Pid: int32(s.handler.PID())}, nil
}

// Exec streams the output of a command.
func (s *Server) Exec(req *v1.ExecRequest, stream grpc.ServerStreamingServer[v1.ExecChunk]) error {
	return s.handler.Exec(stream.Context(), req, stream.Send)
}

// Signal forwards a signal to the agent's process group.
func (s *Server) Signal(ctx context.Context, req *v1.SignalRequest) (*v1.SignalResponse, error) {
	if err := s.handler.Signal(ctx, req.GetSignal()); err != nil {
		return nil, err
	}
	return &v1.SignalResponse{}, nil
}

// Shutdown terminates the sandbox.
func (s *Server) Shutdown(ctx context.Context, _ *v1.ShutdownRequest) (*v1.ShutdownResponse, error) {
	if err := s.handler.Shutdown(ctx); err != nil {
		return nil, err
	}
	return &v1.ShutdownResponse{}, nil
}

// Client is a gRPC client for the in-sandbox agent.
type Client struct {
	conn *grpc.ClientConn
	stub v1.AgentServiceClient
}

// Dial connects to the agent socket. It does not wait for readiness.
func Dial(socket string) (*Client, error) {
	var mu sync.Mutex
	var original os.FileInfo
	var trusted *unix.Ucred
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		before, err := os.Lstat(socket)
		if err != nil {
			return nil, err
		}
		if before.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("agentproto: control path is not a socket")
		}
		connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		fail := func(err error) (net.Conn, error) { _ = connection.Close(); return nil, err }
		after, err := os.Lstat(socket)
		if err != nil {
			return fail(err)
		}
		if !os.SameFile(before, after) {
			return fail(fmt.Errorf("agentproto: control socket changed during dial"))
		}
		unixConn := connection.(*net.UnixConn)
		raw, err := unixConn.SyscallConn()
		if err != nil {
			return fail(err)
		}
		var peer *unix.Ucred
		var peerErr error
		if err := raw.Control(func(fd uintptr) { peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
			return fail(err)
		}
		if peerErr != nil {
			return fail(peerErr)
		}
		mu.Lock()
		defer mu.Unlock()
		if original == nil {
			original, trusted = before, peer
		} else if !os.SameFile(original, before) || trusted.Pid != peer.Pid || trusted.Uid != peer.Uid || trusted.Gid != peer.Gid {
			return fail(fmt.Errorf("agentproto: control socket identity changed"))
		}
		return connection, nil
	}
	// The runner dials before vora-init binds the socket, so the first attempt
	// always fails, and the default 1 s backoff delayed every start (#114).
	// MinConnectTimeout must be set: grpc uses zero as the attempt deadline.
	conn, err := grpc.NewClient("unix://"+socket,
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  10 * time.Millisecond,
				Multiplier: backoff.DefaultConfig.Multiplier,
				Jitter:     backoff.DefaultConfig.Jitter,
				MaxDelay:   100 * time.Millisecond,
			},
			MinConnectTimeout: 20 * time.Second,
		}))
	if err != nil {
		return nil, fmt.Errorf("agentproto: dial %s: %w", socket, err)
	}
	return &Client{conn: conn, stub: v1.NewAgentServiceClient(conn)}, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

// Ping reports the agent PID.
func (c *Client) Ping(ctx context.Context) (int, error) {
	resp, err := c.stub.Ping(ctx, &v1.PingRequest{})
	if err != nil {
		return 0, err
	}
	return int(resp.GetPid()), nil
}

// WaitReady polls Ping until the agent responds or the timeout elapses.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := c.Ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("agentproto: agent not ready: %w", lastErr)
}

// Exec starts a command and returns a stream of output chunks.
func (c *Client) Exec(ctx context.Context, req *v1.ExecRequest) (*ExecStream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := c.stub.Exec(streamCtx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	return &ExecStream{stream: stream, cancel: cancel}, nil
}

// Signal sends a signal to the agent's process group.
func (c *Client) Signal(ctx context.Context, sig int32) error {
	_, err := c.stub.Signal(ctx, &v1.SignalRequest{Signal: sig})
	return err
}

// Shutdown asks the agent to terminate the sandbox.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.stub.Shutdown(ctx, &v1.ShutdownRequest{})
	return err
}

// ExecStream adapts the gRPC stream to runtime.ExecStream.
type ExecStream struct {
	stream grpc.ServerStreamingClient[v1.ExecChunk]
	cancel context.CancelFunc
}

// Recv returns the next output chunk, or io.EOF after the final chunk.
func (e *ExecStream) Recv() (runtime.ExecChunk, error) {
	msg, err := e.stream.Recv()
	if err != nil {
		return runtime.ExecChunk{}, err
	}
	switch payload := msg.GetPayload().(type) {
	case *v1.ExecChunk_Stdout:
		return runtime.ExecChunk{Stdout: payload.Stdout}, nil
	case *v1.ExecChunk_Stderr:
		return runtime.ExecChunk{Stderr: payload.Stderr}, nil
	case *v1.ExecChunk_Exit:
		return runtime.ExecChunk{Exit: &runtime.ExecResult{
			ExitCode: int(payload.Exit.GetExitCode()),
			Reason:   reasonFromProto(payload.Exit.GetReason()),
		}}, nil
	default:
		return runtime.ExecChunk{}, nil
	}
}

// Close cancels the stream.
func (e *ExecStream) Close() error {
	e.cancel()
	return nil
}

func reasonFromProto(r v1.ExitReason) runtime.ExitReason {
	switch r {
	case v1.ExitReason_OOM_KILLED:
		return runtime.ReasonOOMKilled
	case v1.ExitReason_PIDS_LIMIT_EXCEEDED:
		return runtime.ReasonPidsLimit
	case v1.ExitReason_TIMEOUT_EXCEEDED:
		return runtime.ReasonTimeout
	case v1.ExitReason_POLICY_VIOLATION:
		return runtime.ReasonPolicyViolation
	case v1.ExitReason_RUNTIME_FAILURE:
		return runtime.ReasonRuntimeFailure
	default:
		return runtime.ReasonExited
	}
}
