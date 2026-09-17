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
	"strings"
	"sync"
	"time"
)

// DefaultPort is the TLS port the proxy forwards to.
const DefaultPort = "443"

// Denial reasons recorded on the audit trail.
const (
	ReasonNotAllowed = "domain_not_allowed"
	ReasonNotTLS     = "not_tls"
	ReasonBlockedIP  = "blocked_address"
	ReasonDialFailed = "dial_failed"
)

// Policy is the per-sandbox egress allowlist.
type Policy struct {
	AllowedDomains []string
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
	onEvent   func(Event)
	dialer    *net.Dialer

	// port, dialHost and skipIPCheck are test seams.
	port        string
	dialHost    string
	skipIPCheck bool

	mu       sync.Mutex
	listener net.Listener
}

// New returns a proxy for a sandbox. onEvent may be nil.
func New(sandboxID string, policy Policy, logger *slog.Logger, onEvent func(Event)) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	return &Proxy{
		sandboxID: sandboxID,
		policy:    policy,
		logger:    logger,
		onEvent:   onEvent,
		dialer:    &net.Dialer{Timeout: 15 * time.Second},
		port:      DefaultPort,
	}
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
			return
		}
		go p.handle(ctx, conn)
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
		p.deny(ctx, host, "", ReasonDialFailed)
		return
	}
	defer upstream.Close()

	if remote := upstream.RemoteAddr().(*net.TCPAddr); !p.skipIPCheck && blockedIP(remote.IP) {
		p.deny(ctx, host, remote.String(), ReasonBlockedIP)
		return
	}

	p.allow(host, upstream.RemoteAddr().String())
	if _, err := upstream.Write(raw); err != nil {
		return
	}
	pipe(conn, upstream)
}

func (p *Proxy) allow(host, remote string) {
	p.event(Event{SandboxID: p.sandboxID, Host: host, Remote: remote, Allowed: true, Time: time.Now()})
}

func (p *Proxy) deny(ctx context.Context, host, remote, reason string) {
	p.event(Event{SandboxID: p.sandboxID, Host: host, Remote: remote, Allowed: false, Reason: reason, Time: time.Now()})
}

func (p *Proxy) event(e Event) {
	if p.onEvent != nil {
		p.onEvent(e)
	}
	if !e.Allowed {
		p.logger.Debug("egress denied", "sandbox_id", e.SandboxID, "host", e.Host, "reason", e.Reason)
	}
}

// pipe copies both directions until either side closes.
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
	if errors.Is(err, errNotTLS) {
		return ReasonNotTLS
	}
	return ReasonNotTLS
}

func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	return h
}
