package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/vorash/vora/internal/agentproto"
)

// refusalLogInterval limits the log to one line per interval, however fast a workload
// connects. A count stands in for the rest, so a workload that spins on connect or
// forks through PIDs grows neither the log nor a per-PID table.
const refusalLogInterval = time.Minute

// refuseSandboxPeers reports whether the agent must refuse connections from inside its
// own PID namespace. PID 2 is proof of a PID namespace (see isBwrapAgent), and a real
// sandbox must never run with this off, so it does not also ask PID 1's name, which
// would leave the check off under a differently named bwrap. Outside a namespace every
// client reads nonzero, so an agent run by hand keeps the check off.
func refuseSandboxPeers(pid int) bool {
	return pid == agentPID
}

// hostOnlyListener passes on only connections made from outside the agent's PID
// namespace. SO_PEERCRED translates the connecting task's PID into the namespace of the
// reader, and a task the namespace cannot see reads 0. The runner is in an ancestor
// namespace, and anything in the sandbox, nested namespaces included, reads nonzero.
//
// It works in Accept rather than a gRPC handshake because gRPC starts a goroutine per
// accepted connection before it would run one. Here a refused connection costs one
// accept, one getsockopt and one close, in the Serve loop's own goroutine, and gRPC
// never sees it. So it reads no frame and cannot touch the watchdog's clock.
type hostOnlyListener struct {
	net.Listener
	peerPID  func(net.Conn) (int32, error)
	refusals *refusalLog
}

func newHostOnlyListener(lis net.Listener, logger *slog.Logger) *hostOnlyListener {
	return &hostOnlyListener{
		Listener: lis,
		peerPID:  peerPID,
		refusals: &refusalLog{logger: logger, now: time.Now},
	}
}

// Accept returns the next connection from outside the namespace. A refusal must not
// surface as an error: gRPC's Serve would return, and main ends the sandbox on that.
func (l *hostOnlyListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		pid, err := l.peerPID(conn)
		if err == nil && pid == 0 {
			return conn, nil
		}
		_ = conn.Close()
		l.refusals.note(pid, err)
	}
}

func peerPID(conn net.Conn) (int32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix connection")
	}
	cred, err := agentproto.PeerCred(unixConn)
	if err != nil {
		return 0, fmt.Errorf("read peer credentials: %w", err)
	}
	return cred.Pid, nil
}

// refusalLog is only used from the Serve loop's goroutine, so it takes no lock.
type refusalLog struct {
	logger  *slog.Logger
	now     func() time.Time
	last    time.Time
	refused int
}

func (r *refusalLog) note(pid int32, err error) {
	r.refused++
	now := r.now()
	if !r.last.IsZero() && now.Sub(r.last) < refusalLogInterval {
		return
	}
	r.logger.Warn("refused an agent connection from inside the sandbox", "pid", pid, "error", err, "refused_since_last_line", r.refused)
	r.last, r.refused = now, 0
}
