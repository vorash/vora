// Package egress implements the Vora egress gateway: a transparent TCP proxy
// that inspects the TLS SNI of each connection and forwards it only to hosts
// on the sandbox's allowlist. TLS is passed through, never terminated (ADR 004).
//
// Non-TLS traffic and connections to private/loopback/link-local addresses are
// denied by default, even when the requested hostname is allowlisted.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultPort is the TLS port the proxy forwards to.
const DefaultPort = "443"

const (
	defaultMaxConnections = 128
	defaultIdleTimeout    = 5 * time.Minute
)

// Denial reasons recorded on the audit trail.
const (
	ReasonNotAllowed       = "domain_not_allowed"
	ReasonNotTLS           = "not_tls"
	ReasonBlockedIP        = "blocked_address"
	ReasonDialFailed       = "dial_failed"
	ReasonConnectionLimit  = "connection_limit"
	ReasonAuditFailed      = "audit_failed"
	ReasonNoSNI            = "no_sni"
	ReasonHandshakeTimeout = "handshake_timeout"
	ReasonHandshakeFailed  = "handshake_failed"
)

var errBlockedAddr = errors.New("egress: blocked address")

// Policy is the per-sandbox egress allowlist.
type Policy struct {
	AllowedDomains []string
}

// Limits bounds host resources consumed by one sandbox proxy.
type Limits struct {
	MaxConnections int
	IdleTimeout    time.Duration
}

// Allows reports whether host matches an allowlisted domain. Matching is
// case-insensitive and accepts subdomains (example.com allows api.example.com).
func (p Policy) Allows(host string) bool {
	host = normalizeHost(host)
	if host == "" {
		return false
	}
	for _, d := range p.AllowedDomains {
		d = normalizeHost(d)
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Event is one egress decision, emitted for the audit trail and metrics.
type Event struct {
	SandboxID string
	Host      string
	Remote    string
	Allowed   bool
	Reason    string
	Time      time.Time
}

// Proxy is a transparent SNI-inspecting TCP proxy for one sandbox.
type Proxy struct {
	sandboxID string
	policy    Policy
	logger    *slog.Logger
	onEvent   func(Event) error
	dialer    *net.Dialer

	// port, dialHost and skipIPCheck are test seams.
	port        string
	dialHost    string
	skipIPCheck bool

	maxConnections int
	idleTimeout    time.Duration
	slots          chan struct{}

	mu       sync.Mutex
	listener net.Listener
	active   map[net.Conn]struct{}
}

// New returns a proxy for a sandbox. onEvent may be nil.
func New(sandboxID string, policy Policy, logger *slog.Logger, onEvent func(Event) error, configured ...Limits) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	limits := Limits{MaxConnections: defaultMaxConnections, IdleTimeout: defaultIdleTimeout}
	if len(configured) > 0 {
		if configured[0].MaxConnections > 0 {
			limits.MaxConnections = configured[0].MaxConnections
		}
		if configured[0].IdleTimeout > 0 {
			limits.IdleTimeout = configured[0].IdleTimeout
		}
	}
	p := &Proxy{
		sandboxID:      sandboxID,
		policy:         policy,
		logger:         logger,
		onEvent:        onEvent,
		dialer:         &net.Dialer{Timeout: 15 * time.Second},
		port:           DefaultPort,
		maxConnections: limits.MaxConnections,
		idleTimeout:    limits.IdleTimeout,
		slots:          make(chan struct{}, limits.MaxConnections),
		active:         make(map[net.Conn]struct{}),
	}
	p.dialer.Control = p.controlDial
	return p
}

func (p *Proxy) controlDial(_ string, address string, _ syscall.RawConn) error {
	if p.skipIPCheck {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if blockedIP(net.ParseIP(host)) {
		return errBlockedAddr
	}
	return nil
}

// Listen binds the proxy to addr (the sandbox's gateway address) and starts
// serving. It returns once the listener is bound so callers can start the
// sandbox knowing the proxy is reachable.
func (p *Proxy) Listen(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("egress: listen %s: %w", addr, err)
	}
	p.mu.Lock()
	p.listener = ln
	p.mu.Unlock()
	go func() {
		<-ctx.Done()
		_ = p.Close()
	}()
	go p.serve(ctx)
	return nil
}

// Close stops accepting connections.
func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener == nil {
		return nil
	}
	err := p.listener.Close()
	p.listener = nil
	for conn := range p.active {
		_ = conn.Close()
	}
	return err
}

func (p *Proxy) serve(ctx context.Context) {
	for {
		p.mu.Lock()
		ln := p.listener
		p.mu.Unlock()
		if ln == nil {
			return
		}
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			p.logger.Error("egress accept failed", "sandbox_id", p.sandboxID, "error", err)
			if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOMEM) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		select {
		case p.slots <- struct{}{}:
			p.mu.Lock()
			p.active[conn] = struct{}{}
			p.mu.Unlock()
			go func() {
				defer func() {
					<-p.slots
					p.mu.Lock()
					delete(p.active, conn)
					p.mu.Unlock()
				}()
				p.handle(ctx, conn)
			}()
		default:
			p.deny(ctx, "", conn.RemoteAddr().String(), ReasonConnectionLimit)
			_ = conn.Close()
		}
	}
}

func (p *Proxy) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
	}

	// Bound how long a peer may hold the connection before sending a
	// ClientHello; without this an idle client could pin a goroutine.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	host, raw, err := readClientHello(conn)
	if err != nil {
		p.deny(ctx, "", "", reasonFor(err))
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if !p.policy.Allows(host) {
		p.deny(ctx, host, "", ReasonNotAllowed)
		return
	}

	dialTarget := host
	if p.dialHost != "" {
		dialTarget = p.dialHost
	}
	upstream, err := p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(dialTarget, p.port))
	if err != nil {
		reason := ReasonDialFailed
		if errors.Is(err, errBlockedAddr) {
			reason = ReasonBlockedIP
		}
		p.deny(ctx, host, "", reason)
		return
	}
	defer upstream.Close()

	if remote := upstream.RemoteAddr().(*net.TCPAddr); !p.skipIPCheck && blockedIP(remote.IP) {
		p.deny(ctx, host, remote.String(), ReasonBlockedIP)
		return
	}

	if err := p.allow(host, upstream.RemoteAddr().String()); err != nil {
		p.logger.Error("persist allowed egress decision failed", "sandbox_id", p.sandboxID, "host", host, "error", err)
		_ = p.deny(ctx, host, upstream.RemoteAddr().String(), ReasonAuditFailed)
		return
	}
	if _, err := upstream.Write(raw); err != nil {
		return
	}
	pipe(withIdleTimeout(conn, p.idleTimeout), withIdleTimeout(upstream, p.idleTimeout))
}

func (p *Proxy) allow(host, remote string) error {
	return p.event(Event{SandboxID: p.sandboxID, Host: host, Remote: remote, Allowed: true, Time: time.Now()})
}

func (p *Proxy) deny(_ context.Context, host, remote, reason string) error {
	err := p.event(Event{SandboxID: p.sandboxID, Host: host, Remote: remote, Allowed: false, Reason: reason, Time: time.Now()})
	if err != nil {
		p.logger.Error("persist denied egress decision failed", "sandbox_id", p.sandboxID, "host", host, "reason", reason, "error", err)
	}
	return err
}

func (p *Proxy) event(e Event) error {
	var err error
	if p.onEvent != nil {
		err = p.onEvent(e)
	}
	if !e.Allowed {
		p.logger.Debug("egress denied", "sandbox_id", e.SandboxID, "host", e.Host, "reason", e.Reason)
	}
	return err
}

type idleConn struct {
	net.Conn
	timeout time.Duration
}

func withIdleTimeout(conn net.Conn, timeout time.Duration) net.Conn {
	return &idleConn{Conn: conn, timeout: timeout}
}

func (c *idleConn) Read(buf []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(buf)
}

func (c *idleConn) Write(buf []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(buf)
}

func (c *idleConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return c.Conn.Close()
}

// pipe copies both directions until either side closes or becomes idle.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// blockedIP reports whether ip is loopback, private, link-local, multicast,
// unspecified or otherwise not a public unicast address.
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10 (CGNAT) and 169.254.0.0/16 are covered by IsPrivate
		// for the latter; block CGNAT explicitly.
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
	}
	return false
}

// readClientHello reads the TLS record carrying the ClientHello and returns
// the SNI plus the raw bytes (which must be forwarded to the upstream).
func readClientHello(r io.Reader) (string, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return "", nil, err
	}
	if header[0] != recordTypeHandshake {
		return "", nil, errNotTLS
	}
	n := int(header[3])<<8 | int(header[4])
	if n <= 0 || n > 1<<16 {
		return "", nil, fmt.Errorf("egress: implausible record length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", nil, err
	}
	raw := make([]byte, 0, len(header)+len(body))
	raw = append(raw, header...)
	raw = append(raw, body...)
	sni, err := parseSNI(body)
	return sni, raw, err
}

var errNotTLS = errors.New("egress: not a TLS handshake")

func reasonFor(err error) string {
	switch {
	case errors.Is(err, errNotTLS):
		return ReasonNotTLS
	case errors.Is(err, errNoSNI):
		return ReasonNoSNI
	case errors.Is(err, os.ErrDeadlineExceeded):
		return ReasonHandshakeTimeout
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return ReasonHandshakeTimeout
		}
		return ReasonHandshakeFailed
	}
}

func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	return h
}
