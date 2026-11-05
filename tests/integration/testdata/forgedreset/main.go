// Command forgedreset is what a workload can try over the control socket. It runs only
// inside a sandbox, as part of TestForgedResetGainsNothing. Every mode prints what the
// agent did, and each attack has to reach the socket for the test to pass, so an attack
// that broke cannot pass as one that was refused.
//
// Usage:
//
//	forgedreset rpc <name> <manifest file> <marker file>  call one RPC through the runner's client
//	forgedreset raw <socket>                              send a raw Shutdown call, report the reply
//	forgedreset relay <socket>                            serve <socket>, forwarding to the control socket
//	forgedreset orphan <result file>                      connect, send Shutdown, and be gone before the accept
//	forgedreset hold <connector PID> <result file>        the part of orphan that outlives the connector
//	forgedreset nested                                    send a raw Shutdown from a nested PID namespace
//	forgedreset flood <workers> <duration>                connect and close as fast as possible
//	forgedreset replace <result file>                     put a listener of its own at the control socket's path
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/agentproto"
)

const (
	controlSocket = "/vora/control.sock"
	agentPID      = 2
	callTimeout   = 5 * time.Second
	// heldFD is where `orphan` hands its connection to the process that outlives it.
	heldFD = 3
)

func main() {
	var err error
	switch os.Args[1] {
	case "rpc":
		err = callRPC(os.Args[2], os.Args[3], os.Args[4])
	case "raw":
		err = rawShutdown(os.Args[2])
	case "relay":
		err = relay(os.Args[2])
	case "orphan":
		err = orphan(os.Args[2])
	case "hold":
		err = hold(os.Args[2], os.Args[3])
	case "nested":
		err = nested()
	case "flood":
		err = flood(os.Args[2], os.Args[3])
	case "replace":
		err = replace(os.Args[2])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Println("attack failed:", err)
		os.Exit(2)
	}
}

// callRPC calls one RPC the way the runner does, so nothing but the agent can refuse it.
func callRPC(name, manifestFile, marker string) error {
	manifest, err := os.ReadFile(manifestFile)
	if err != nil {
		return err
	}
	client, err := agentproto.Dial(controlSocket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	var callErr error
	switch name {
	case "Ping":
		_, callErr = client.Ping(ctx)
	case "Exec":
		callErr = execMarker(ctx, client, marker)
	case "Signal":
		callErr = client.Signal(ctx, int32(syscall.SIGKILL))
	case "Capture":
		_, callErr = client.Capture(ctx)
	case "Reset":
		_, callErr = client.Reset(ctx, manifest)
	case "Shutdown":
		callErr = client.Shutdown(ctx)
	default:
		return fmt.Errorf("unknown RPC %q", name)
	}
	if callErr == nil {
		fmt.Println(name, "SUCCEEDED")
		return nil
	}
	fmt.Println(name, "refused:", callErr)
	return nil
}

func execMarker(ctx context.Context, client *agentproto.Client, marker string) error {
	stream, err := client.Exec(ctx, &v1.ExecRequest{Command: "/bin/sh", Args: []string{"-c", ": > " + marker}, Cwd: "/"})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for {
		chunk, err := stream.Recv()
		if err != nil {
			return err
		}
		if chunk.Exit != nil {
			return nil
		}
	}
}

// shutdownCall is a whole gRPC Shutdown call as bytes: the client preface, its SETTINGS,
// and one request. It has no reply to wait for, so it can sit in the socket's buffer and
// run after the sender is gone.
func shutdownCall() []byte {
	literal := func(name, value string) []byte {
		field := []byte{0, byte(len(name))}
		field = append(field, name...)
		field = append(field, byte(len(value)))
		return append(field, value...)
	}
	frame := func(kind, flags byte, stream uint32, payload []byte) []byte {
		header := make([]byte, 9)
		header[0], header[1], header[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
		header[3], header[4] = kind, flags
		binary.BigEndian.PutUint32(header[5:], stream)
		return append(header, payload...)
	}
	const (
		frameData, frameHeaders, frameSettings = 0, 1, 4
		flagEndStream, flagEndHeaders          = 1, 4
	)
	var headers []byte
	for _, field := range [][2]string{
		{":method", "POST"},
		{":scheme", "http"},
		{":path", v1.AgentService_Shutdown_FullMethodName},
		{":authority", "localhost"},
		{"content-type", "application/grpc"},
		{"te", "trailers"},
	} {
		headers = append(headers, literal(field[0], field[1])...)
	}
	call := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	call = append(call, frame(frameSettings, 0, 0, nil)...)
	call = append(call, frame(frameHeaders, flagEndHeaders, 1, headers)...)
	return append(call, frame(frameData, flagEndStream, 1, make([]byte, 5))...)
}

// report says what the other end of conn did. A gRPC server speaks first, so an agent
// that took the connection sends bytes without waiting for anything.
func report(conn net.Conn) string {
	_ = conn.SetReadDeadline(time.Now().Add(callTimeout))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	switch {
	case n > 0:
		return fmt.Sprintf("REPLIED %d bytes", n)
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "OPEN and silent"
	default:
		return "closed: " + err.Error()
	}
}

func rawShutdown(socket string) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	// The agent may already have closed its end, and the read below says so.
	_, _ = conn.Write(shutdownCall())
	fmt.Println(report(conn))
	return nil
}

func relay(socket string) error {
	lis, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	fmt.Println("ready")
	for {
		client, err := lis.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer func() { _ = client.Close() }()
			agent, err := net.Dial("unix", controlSocket)
			if err != nil {
				return
			}
			defer func() { _ = agent.Close() }()
			go func() { _, _ = io.Copy(agent, client) }()
			_, _ = io.Copy(client, agent)
		}()
	}
}

// orphan makes the agent read the credentials of a task that no longer exists: it stops
// the agent so nothing accepts, connects, leaves a Shutdown in the socket, and exits. The
// `hold` process it starts keeps the connection open, waits for this process to be
// reaped, and only then lets the agent run.
func orphan(resultFile string) (err error) {
	if err := syscall.Kill(agentPID, syscall.SIGSTOP); err != nil {
		return fmt.Errorf("stop the agent: %w", err)
	}
	// From here on the holder resumes the agent. Until it is running, a failure has to,
	// or the test hangs on a stopped agent instead of reading why this failed.
	defer func() {
		if err != nil {
			_ = syscall.Kill(agentPID, syscall.SIGCONT)
		}
	}()
	conn, err := net.Dial("unix", controlSocket)
	if err != nil {
		return err
	}
	if _, err := conn.Write(shutdownCall()); err != nil {
		return err
	}
	file, err := conn.(*net.UnixConn).File()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	holder := exec.Command(self, "hold", strconv.Itoa(os.Getpid()), resultFile)
	holder.ExtraFiles = []*os.File{file}
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return holder.Start()
}

func hold(connector, resultFile string) error {
	resumed := false
	defer func() {
		if !resumed {
			_ = syscall.Kill(agentPID, syscall.SIGCONT)
		}
	}()
	result, err := os.Create(resultFile)
	if err != nil {
		return err
	}
	defer func() { _ = result.Close() }()
	pid, err := strconv.Atoi(connector)
	if err != nil {
		return err
	}
	gone := false
	for deadline := time.Now().Add(callTimeout); !gone && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		gone = errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
	_, _ = fmt.Fprintln(result, "connector gone:", gone)
	if err := syscall.Kill(agentPID, syscall.SIGCONT); err != nil {
		return err
	}
	resumed = true
	conn, err := net.FileConn(os.NewFile(heldFD, "held"))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(result, report(conn))
	return err
}

// nested tries to be a peer the agent's namespace sees differently. The kernel should
// deny the namespace, and if it allows one the agent still sees the process.
func nested() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "raw", controlSocket)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID}
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		fmt.Println("namespace denied:", err)
	}
	return nil
}

func flood(workers, duration string) error {
	n, err := strconv.Atoi(workers)
	if err != nil {
		return err
	}
	length, err := time.ParseDuration(duration)
	if err != nil {
		return err
	}
	var connected, failed atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(length)
	for range n {
		wg.Go(func() {
			for time.Now().Before(deadline) {
				conn, err := net.Dial("unix", controlSocket)
				if err != nil {
					failed.Add(1)
					continue
				}
				_ = conn.Close()
				connected.Add(1)
			}
		})
	}
	wg.Wait()
	fmt.Println("flood done, connected", connected.Load(), "failed", failed.Load())
	return nil
}

// replace puts its own listener where the runner dials and notes what each connection
// sends. A runner that talked to it would send the HTTP/2 preface straight away.
func replace(resultFile string) error {
	result, err := os.Create(resultFile)
	if err != nil {
		return err
	}
	if err := os.Remove(controlSocket); err != nil {
		return err
	}
	lis, err := net.Listen("unix", controlSocket)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(result, "ready")
	for {
		conn, err := lis.Accept()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(callTimeout))
		n, _ := conn.Read(make([]byte, 64))
		_ = conn.Close()
		_, _ = fmt.Fprintln(result, "connection sent", n, "bytes")
	}
}
