// Package tunnel implements the runner-initiated reverse tunnel that lets
// vora-api drive runners it cannot dial (for example behind NAT). The runner
// connects out and authenticates; the API multiplexes RunnerService
// connections over that one connection with yamux, opening a stream per
// API-to-runner gRPC connection. The API is therefore the yamux client and the
// runner is the yamux server, regardless of which side opened the TCP socket.
package tunnel

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

const handshakeTimeout = 10 * time.Second

// Hello is the first line a runner sends on the tunnel.
type Hello struct {
	RunnerID  string `json:"runner_id"`
	JoinToken string `json:"join_token"`
}

// Server accepts runner tunnels and opens streams to them. It lives in vora-api.
type Server struct {
	joinToken  string
	logger     *slog.Logger
	requireTLS bool

	mu       sync.Mutex
	sessions map[string]*yamux.Session
}

// NewServer returns a tunnel server. An empty joinToken accepts any runner
// (development only).
func NewServer(joinToken string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{joinToken: joinToken, logger: logger, sessions: map[string]*yamux.Session{}}
}

// RequireTLS binds each runner id to the client certificate's DNS SAN.
func (s *Server) RequireTLS() { s.requireTLS = true }

// Serve accepts tunnel connections until the listener closes or ctx is done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	if s.requireTLS {
		if _, ok := conn.(*tls.Conn); !ok {
			_ = conn.Close()
			return
		}
	}
	hello, err := readHello(conn)
	if err != nil {
		s.logger.Warn("tunnel handshake failed", "error", err)
		_ = conn.Close()
		return
	}
	if hello.RunnerID == "" || !s.authorized(hello.JoinToken) || (s.requireTLS && !peerIsRunner(conn.(*tls.Conn), hello.RunnerID)) {
		s.logger.Warn("tunnel rejected", "runner_id", hello.RunnerID)
		_ = conn.Close()
		return
	}
	session, err := yamux.Client(conn, nil)
	if err != nil {
		_ = conn.Close()
		return
	}
	s.mu.Lock()
	if old, ok := s.sessions[hello.RunnerID]; ok {
		_ = old.Close() // a new session supersedes the old one
	}
	s.sessions[hello.RunnerID] = session
	s.mu.Unlock()
	s.logger.Info("tunnel established", "runner_id", hello.RunnerID)

	<-session.CloseChan()
	s.mu.Lock()
	if s.sessions[hello.RunnerID] == session {
		delete(s.sessions, hello.RunnerID)
	}
	s.mu.Unlock()
	s.logger.Info("tunnel closed", "runner_id", hello.RunnerID)
}

func peerIsRunner(conn *tls.Conn, id string) bool {
	certs := conn.ConnectionState().PeerCertificates
	return len(certs) > 0 && certificateIsRunner(certs[0], id)
}

func certificateIsRunner(cert *x509.Certificate, id string) bool {
	for _, name := range cert.DNSNames {
		if name == id {
			return true
		}
	}
	return false
}

// Dial opens a stream to a tunnelled runner.
func (s *Server) Dial(_ context.Context, runnerID string) (net.Conn, error) {
	s.mu.Lock()
	session, ok := s.sessions[runnerID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("tunnel: runner %s has no live tunnel", runnerID)
	}
	return session.OpenStream()
}

// Has reports whether a runner currently has a live tunnel.
func (s *Server) Has(runnerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[runnerID]
	return ok
}

func (s *Server) authorized(token string) bool {
	if s.joinToken == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.joinToken)) == 1
}

// Client is the runner side of the tunnel.
type Client struct {
	Addr      string
	RunnerID  string
	JoinToken string
	Logger    *slog.Logger
	TLS       *tls.Config
}

// Listen dials the API, authenticates, and returns a listener whose Accept
// yields the streams the API opens. Serve a gRPC RunnerService over it.
func (c *Client) Listen(ctx context.Context) (net.Listener, error) {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	d := net.Dialer{Timeout: handshakeTimeout}
	var conn net.Conn
	var err error
	if c.TLS != nil {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: c.TLS}).DialContext(ctx, "tcp", c.Addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", c.Addr)
	}
	if err != nil {
		return nil, fmt.Errorf("tunnel: dial %s: %w", c.Addr, err)
	}
	if err := writeHello(conn, Hello{RunnerID: c.RunnerID, JoinToken: c.JoinToken}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	session, err := yamux.Server(conn, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("tunnel: yamux: %w", err)
	}
	logger.Info("tunnel connected", "addr", c.Addr, "runner_id", c.RunnerID)
	go func() {
		<-ctx.Done()
		_ = session.Close()
	}()
	return &streamListener{session: session, addr: conn.RemoteAddr()}, nil
}

// streamListener turns accepted yamux streams into a net.Listener.
type streamListener struct {
	session *yamux.Session
	addr    net.Addr
}

func (l *streamListener) Accept() (net.Conn, error) { return l.session.AcceptStream() }
func (l *streamListener) Close() error              { return l.session.Close() }
func (l *streamListener) Addr() net.Addr            { return l.addr }

func readHello(conn net.Conn) (Hello, error) {
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetReadDeadline(time.Time{})
	// Read one byte at a time: the hello is one short line, and a buffered
	// reader could swallow the first yamux frames that follow it.
	var line []byte
	buf := make([]byte, 1)
	for len(line) < 4096 {
		n, err := conn.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				var h Hello
				if err := json.Unmarshal(line, &h); err != nil {
					return Hello{}, fmt.Errorf("tunnel: bad hello: %w", err)
				}
				return h, nil
			}
			line = append(line, buf[0])
		}
		if err != nil {
			return Hello{}, err
		}
	}
	return Hello{}, fmt.Errorf("tunnel: hello too long")
}

func writeHello(conn net.Conn, h Hello) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	defer conn.SetWriteDeadline(time.Time{})
	_, err = conn.Write(data)
	return err
}
