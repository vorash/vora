package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DefaultDNSPort is the port the sandbox resolver listens on. resolv.conf
// cannot carry a port, so this is fixed.
const DefaultDNSPort = "53"

// DNS decision reasons recorded on the audit trail.
const (
	ReasonDNSResolved      = "dns_resolved"
	ReasonDNSNotAllowed    = "dns_not_allowed"
	ReasonDNSBlockedAnswer = "dns_blocked_address"
	ReasonDNSResolveFailed = "dns_resolve_failed"
	ReasonDNSRefusedType   = "dns_refused_type"
)

const (
	dnsTTL        = 30 * time.Second
	dnsCacheMax   = 512
	dnsQueryLimit = 5 * time.Second
)

// DNS is the per-sandbox resolver on the gateway address. It answers only names
// on the sandbox's allowlist, refuses answers that map to a private, loopback,
// link-local or CGNAT address (reusing the same check that guards upstreams, so
// a rebinding name cannot reach the metadata service), and audits every query.
// It is reachable only from inside the sandbox network namespace.
type DNS struct {
	sandboxID string
	policy    Policy
	logger    *slog.Logger
	onEvent   func(Event) error
	lookup    func(ctx context.Context, name string) ([]net.IP, error)

	flight chan struct{}
	ttl    time.Duration

	mu    sync.Mutex
	cache map[string]dnsCacheEntry

	udp *net.UDPConn
	tcp net.Listener
}

type dnsCacheEntry struct {
	ips     []net.IP
	expires time.Time
}

// NewDNS returns a resolver for a sandbox. onEvent may be nil.
func NewDNS(sandboxID string, policy Policy, logger *slog.Logger, onEvent func(Event) error, limits ...Limits) *DNS {
	if logger == nil {
		logger = slog.Default()
	}
	l := Limits{MaxConnections: defaultMaxConnections}
	if len(limits) > 0 && limits[0].MaxConnections > 0 {
		l.MaxConnections = limits[0].MaxConnections
	}
	return &DNS{
		sandboxID: sandboxID,
		policy:    policy,
		logger:    logger,
		onEvent:   onEvent,
		lookup:    systemLookup,
		flight:    make(chan struct{}, l.MaxConnections),
		ttl:       dnsTTL,
		cache:     map[string]dnsCacheEntry{},
	}
}

// systemLookup resolves a name with the runner host's resolver.
func systemLookup(ctx context.Context, name string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, name)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("egress: %s has no addresses", name)
	}
	return ips, nil
}

// Listen binds the resolver to addr (the sandbox gateway on port 53) and starts
// serving UDP and TCP. It returns once both listeners are bound.
func (d *DNS) Listen(ctx context.Context, addr string) error {
	udp, err := net.ListenUDP("udp", mustUDPAddr(addr))
	if err != nil {
		return fmt.Errorf("egress: dns listen udp %s: %w", addr, err)
	}
	d.udp = udp
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		_ = udp.Close()
		d.udp = nil
		return fmt.Errorf("egress: dns listen tcp %s: %w", addr, err)
	}
	d.tcp = tcp

	go func() {
		<-ctx.Done()
		_ = d.Close()
	}()
	go d.serveUDP(ctx)
	go d.serveTCP(ctx)
	return nil
}

// Close stops the resolver.
func (d *DNS) Close() error {
	var err error
	if d.udp != nil {
		err = d.udp.Close()
		d.udp = nil
	}
	if d.tcp != nil {
		if cerr := d.tcp.Close(); err == nil {
			err = cerr
		}
		d.tcp = nil
	}
	return err
}

func mustUDPAddr(addr string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		// Only fails for a syntactically invalid address, which the caller
		// built from the gateway IP and a fixed port.
		panic("egress: invalid dns address " + addr)
	}
	return a
}

func (d *DNS) serveUDP(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		udp := d.udp
		if udp == nil {
			return
		}
		n, from, err := udp.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			d.logger.Warn("dns udp read failed", "sandbox_id", d.sandboxID, "error", err)
			continue
		}
		query := append([]byte(nil), buf[:n]...)
		if !d.acquire() {
			d.deny(ctx, "", ReasonConnectionLimit)
			continue
		}
		go func() {
			defer d.release()
			reply := d.handle(ctx, query)
			if len(reply) == 0 {
				return
			}
			if _, err := udp.WriteToUDP(reply, from); err != nil {
				d.logger.Debug("dns udp write failed", "sandbox_id", d.sandboxID, "error", err)
			}
		}()
	}
}

func (d *DNS) serveTCP(ctx context.Context) {
	for {
		tcp := d.tcp
		if tcp == nil {
			return
		}
		conn, err := tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			d.logger.Warn("dns tcp accept failed", "sandbox_id", d.sandboxID, "error", err)
			continue
		}
		if !d.acquire() {
			d.deny(ctx, "", ReasonConnectionLimit)
			_ = conn.Close()
			continue
		}
		go func() {
			defer d.release()
			defer conn.Close()
			d.handleTCPConn(ctx, conn)
		}()
	}
}

// handleTCPConn serves one length-prefixed DNS message per connection.
func (d *DNS) handleTCPConn(ctx context.Context, conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(dnsQueryLimit))
	size := make([]byte, 2)
	if _, err := io.ReadFull(conn, size); err != nil {
		return
	}
	query := make([]byte, binary.BigEndian.Uint16(size))
	if _, err := io.ReadFull(conn, query); err != nil {
		return
	}
	reply := d.handle(ctx, query)
	if len(reply) == 0 {
		return
	}
	out := make([]byte, 2+len(reply))
	binary.BigEndian.PutUint16(out, uint16(len(reply)))
	copy(out[2:], reply)
	_, _ = conn.Write(out)
}

func (d *DNS) acquire() bool {
	select {
	case d.flight <- struct{}{}:
		return true
	default:
		return false
	}
}

func (d *DNS) release() { <-d.flight }

// handle answers one query. It returns nil when the query is malformed.
func (d *DNS) handle(ctx context.Context, query []byte) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil || header.Response || header.OpCode != 0 {
		return nil
	}
	question, err := parser.Question()
	if err != nil {
		return nil
	}
	name := normalizeHost(question.Name.String())
	ctx, cancel := context.WithTimeout(ctx, dnsQueryLimit)
	defer cancel()

	// name is untrusted input and appears in the audit trail only; never log
	// a resolved value the caller did not ask for.
	if !d.policy.Allows(name) {
		d.deny(ctx, name, ReasonDNSNotAllowed)
		return d.errorResponse(header, question, dnsmessage.RCodeNameError)
	}
	switch question.Type {
	case dnsmessage.TypeA, dnsmessage.TypeAAAA:
	default:
		d.deny(ctx, name, ReasonDNSRefusedType)
		return d.errorResponse(header, question, dnsmessage.RCodeRefused)
	}

	ips, err := d.resolve(ctx, name)
	if err != nil {
		d.deny(ctx, name, ReasonDNSResolveFailed)
		d.logger.Warn("dns resolve failed", "sandbox_id", d.sandboxID, "host", name, "error", err)
		return d.errorResponse(header, question, dnsmessage.RCodeServerFailure)
	}
	for _, ip := range ips {
		if blockedIP(ip) {
			d.deny(ctx, name, ReasonDNSBlockedAnswer)
			return d.errorResponse(header, question, dnsmessage.RCodeNameError)
		}
	}
	if err := d.allow(name); err != nil {
		d.logger.Error("persist dns allow failed", "sandbox_id", d.sandboxID, "host", name, "error", err)
		return d.errorResponse(header, question, dnsmessage.RCodeServerFailure)
	}
	return d.answer(name, header, question, ips)
}

// resolve returns the addresses for an allowlisted name, with a short cache so
// a sandbox cannot turn every request into a host resolver lookup.
func (d *DNS) resolve(ctx context.Context, name string) ([]net.IP, error) {
	d.mu.Lock()
	entry, ok := d.cache[name]
	d.mu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.ips, nil
	}
	ips, err := d.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if len(d.cache) >= dnsCacheMax {
		d.cache = map[string]dnsCacheEntry{}
	}
	d.cache[name] = dnsCacheEntry{ips: ips, expires: time.Now().Add(d.ttl)}
	d.mu.Unlock()
	return ips, nil
}

func (d *DNS) answer(name string, header dnsmessage.Header, question dnsmessage.Question, ips []net.IP) []byte {
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 header.ID,
		Response:           true,
		RecursionDesired:   header.RecursionDesired,
		RecursionAvailable: true,
	})
	if err := builder.StartQuestions(); err != nil {
		return nil
	}
	if err := builder.Question(question); err != nil {
		return nil
	}
	if err := builder.StartAnswers(); err != nil {
		return nil
	}
	ttl := uint32(d.ttl / time.Second)
	for _, ip := range ips {
		switch question.Type {
		case dnsmessage.TypeA:
			v4 := ip.To4()
			if v4 == nil {
				continue
			}
			var addr [4]byte
			copy(addr[:], v4)
			if err := builder.AResource(dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: ttl},
				dnsmessage.AResource{A: addr}); err != nil {
				return nil
			}
		case dnsmessage.TypeAAAA:
			v6 := ip.To16()
			if v6 == nil || ip.To4() != nil {
				continue
			}
			var addr [16]byte
			copy(addr[:], v6)
			if err := builder.AAAAResource(dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: ttl},
				dnsmessage.AAAAResource{AAAA: addr}); err != nil {
				return nil
			}
		}
	}
	msg, err := builder.Finish()
	if err != nil {
		return nil
	}
	return msg
}

func (d *DNS) errorResponse(header dnsmessage.Header, question dnsmessage.Question, rcode dnsmessage.RCode) []byte {
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 header.ID,
		Response:           true,
		RecursionDesired:   header.RecursionDesired,
		RecursionAvailable: true,
		RCode:              rcode,
	})
	if err := builder.StartQuestions(); err != nil {
		return nil
	}
	if err := builder.Question(question); err != nil {
		return nil
	}
	msg, err := builder.Finish()
	if err != nil {
		return nil
	}
	return msg
}

func (d *DNS) allow(name string) error {
	return d.event(Event{SandboxID: d.sandboxID, Host: name, Allowed: true, Reason: ReasonDNSResolved, Time: time.Now()})
}

func (d *DNS) deny(_ context.Context, name, reason string) error {
	err := d.event(Event{SandboxID: d.sandboxID, Host: name, Allowed: false, Reason: reason, Time: time.Now()})
	if err != nil {
		d.logger.Error("persist dns deny failed", "sandbox_id", d.sandboxID, "reason", reason, "error", err)
	}
	return err
}

func (d *DNS) event(e Event) error {
	if d.onEvent == nil {
		return nil
	}
	return d.onEvent(e)
}
