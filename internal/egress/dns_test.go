package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsEvents collects resolver decisions with synchronisation, like eventLog.
type dnsEvents struct {
	mu     sync.Mutex
	events []Event
	fail   error
}

func (l *dnsEvents) sink(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return l.fail
}

func (l *dnsEvents) all() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.events...)
}

func newDNSFor(t *testing.T, policy Policy, lookup func(context.Context, string) ([]net.IP, error), log *dnsEvents) *DNS {
	t.Helper()
	d := NewDNS("sbx_dns_test", policy, slog.New(slog.NewTextHandler(io.Discard, nil)), log.sink)
	d.lookup = lookup
	return d
}

func query(t *testing.T, name string, qtype dnsmessage.Type) []byte {
	t.Helper()
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 42, RecursionDesired: true})
	if err := builder.StartQuestions(); err != nil {
		t.Fatalf("start questions: %v", err)
	}
	question := dnsmessage.Question{
		Name:  dnsmessage.MustNewName(name),
		Type:  qtype,
		Class: dnsmessage.ClassINET,
	}
	if err := builder.Question(question); err != nil {
		t.Fatalf("question: %v", err)
	}
	msg, err := builder.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return msg
}

func parseReply(t *testing.T, reply []byte) (dnsmessage.Header, []dnsmessage.AResource) {
	t.Helper()
	var parser dnsmessage.Parser
	header, err := parser.Start(reply)
	if err != nil {
		t.Fatalf("parse reply: %v", err)
	}
	if err := parser.SkipAllQuestions(); err != nil {
		t.Fatalf("skip questions: %v", err)
	}
	var addrs []dnsmessage.AResource
	for {
		if _, err := parser.AnswerHeader(); errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		} else if err != nil {
			t.Fatalf("answer header: %v", err)
		}
		resource, err := parser.AResource()
		if err != nil {
			t.Fatalf("a resource: %v", err)
		}
		addrs = append(addrs, resource)
	}
	return header, addrs
}

// TestDNSAnswersAllowlistedName asserts an allowlisted name resolves and the
// decision is audited with the resolved reason.
func TestDNSAnswersAllowlistedName(t *testing.T) {
	log := &dnsEvents{}
	d := newDNSFor(t, Policy{AllowedDomains: []string{"example.com"}},
		func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}, log)

	reply := d.handle(context.Background(), query(t, "api.example.com.", dnsmessage.TypeA))
	if reply == nil {
		t.Fatal("no reply for an allowlisted name")
	}
	header, addrs := parseReply(t, reply)
	if header.RCode != dnsmessage.RCodeSuccess || len(addrs) != 1 {
		t.Fatalf("header = %+v, addrs = %+v", header, addrs)
	}
	if got := net.IP(addrs[0].A[:]).String(); got != "93.184.216.34" {
		t.Fatalf("answer = %s", got)
	}
	events := log.all()
	if len(events) != 1 || !events[0].Allowed || events[0].Host != "api.example.com" || events[0].Reason != ReasonDNSResolved {
		t.Fatalf("events = %+v", events)
	}
}

// TestDNSDeniesNotAllowlisted asserts an unlisted name is refused without ever
// asking the host resolver: the sandbox must not learn whether it exists.
func TestDNSDeniesNotAllowlisted(t *testing.T) {
	looked := false
	log := &dnsEvents{}
	d := newDNSFor(t, Policy{AllowedDomains: []string{"example.com"}},
		func(context.Context, string) ([]net.IP, error) {
			looked = true
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}, log)

	reply := d.handle(context.Background(), query(t, "evil.example.net.", dnsmessage.TypeA))
	header, _ := parseReply(t, reply)
	if header.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("rcode = %v, want NXDOMAIN", header.RCode)
	}
	if looked {
		t.Fatal("the host resolver was queried for a non-allowlisted name")
	}
	events := log.all()
	if len(events) != 1 || events[0].Allowed || events[0].Reason != ReasonDNSNotAllowed {
		t.Fatalf("events = %+v", events)
	}
}

// TestDNSRefusesBlockedAnswers asserts a name that resolves to a loopback,
// private or metadata address is refused: this is what stops DNS rebinding from
// reaching the metadata service.
func TestDNSRefusesBlockedAnswers(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.5", "169.254.169.254", "100.64.1.1", "::1", "fd00::1"} {
		t.Run(ip, func(t *testing.T) {
			log := &dnsEvents{}
			parsed := net.ParseIP(ip)
			d := newDNSFor(t, Policy{AllowedDomains: []string{"rebind.example.com"}},
				func(context.Context, string) ([]net.IP, error) { return []net.IP{parsed}, nil }, log)

			reply := d.handle(context.Background(), query(t, "rebind.example.com.", dnsmessage.TypeA))
			header, addrs := parseReply(t, reply)
			if header.RCode != dnsmessage.RCodeNameError || len(addrs) != 0 {
				t.Fatalf("%s: rcode = %v, addrs = %+v; want NXDOMAIN with no answers", ip, header.RCode, addrs)
			}
			events := log.all()
			if len(events) != 1 || events[0].Allowed || events[0].Reason != ReasonDNSBlockedAnswer {
				t.Fatalf("%s: events = %+v", ip, events)
			}
		})
	}
}

// TestDNSRefusesUnsupportedType asserts non-address queries are refused rather
// than resolved into records the sandbox could use to exfiltrate data.
func TestDNSRefusesUnsupportedType(t *testing.T) {
	log := &dnsEvents{}
	d := newDNSFor(t, Policy{AllowedDomains: []string{"example.com"}},
		func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }, log)

	reply := d.handle(context.Background(), query(t, "example.com.", dnsmessage.TypeTXT))
	header, _ := parseReply(t, reply)
	if header.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("rcode = %v, want REFUSED", header.RCode)
	}
	if events := log.all(); len(events) != 1 || events[0].Reason != ReasonDNSRefusedType {
		t.Fatalf("events = %+v", events)
	}
}

// TestDNSAuditFailureFailsClosed asserts an allow that cannot be audited is not
// answered.
func TestDNSAuditFailureFailsClosed(t *testing.T) {
	log := &dnsEvents{fail: errors.New("audit down")}
	d := newDNSFor(t, Policy{AllowedDomains: []string{"example.com"}},
		func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }, log)

	reply := d.handle(context.Background(), query(t, "example.com.", dnsmessage.TypeA))
	header, _ := parseReply(t, reply)
	if header.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("rcode = %v, want SERVFAIL", header.RCode)
	}
}

// TestDNSCachesLookups asserts a repeated query does not re-query the host
// resolver, so a sandbox cannot turn every request into resolver load.
func TestDNSCachesLookups(t *testing.T) {
	var calls int
	log := &dnsEvents{}
	d := newDNSFor(t, Policy{AllowedDomains: []string{"example.com"}},
		func(context.Context, string) ([]net.IP, error) {
			calls++
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}, log)

	for i := 0; i < 3; i++ {
		if reply := d.handle(context.Background(), query(t, "example.com.", dnsmessage.TypeA)); reply == nil {
			t.Fatal("no reply")
		}
	}
	if calls != 1 {
		t.Fatalf("host resolver calls = %d, want 1 (cached)", calls)
	}
}

// TestDNSMalformedQuery asserts junk is dropped instead of answered.
func TestDNSMalformedQuery(t *testing.T) {
	log := &dnsEvents{}
	d := newDNSFor(t, Policy{AllowedDomains: []string{"example.com"}},
		func(context.Context, string) ([]net.IP, error) { return nil, nil }, log)

	for _, junk := range [][]byte{nil, {}, []byte("not a dns message")} {
		if reply := d.handle(context.Background(), junk); reply != nil {
			t.Fatalf("malformed query %q was answered: %x", junk, reply)
		}
	}
	if events := log.all(); len(events) != 0 {
		t.Fatalf("malformed queries produced audit events: %+v", events)
	}
}

// TestDNSListenAndServe walks the real socket path: a query over UDP and one
// over TCP are both answered.
func TestDNSListenAndServe(t *testing.T) {
	log := &dnsEvents{}
	d := newDNSFor(t, Policy{AllowedDomains: []string{"example.com"}},
		func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Listen takes a fixed port; bind ephemeral ports directly to keep the test
	// independent of any resolver already running on this host.
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	d.udp = udp
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	d.tcp = tcp
	go d.serveUDP(ctx)
	go d.serveTCP(ctx)
	t.Cleanup(func() { _ = d.Close() })

	msg := query(t, "example.com.", dnsmessage.TypeA)

	// UDP.
	conn, err := net.Dial("udp", udp.LocalAddr().String())
	if err != nil {
		t.Fatalf("udp dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("udp write: %v", err)
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("udp read: %v", err)
	}
	header, addrs := parseReply(t, buf[:n])
	if header.RCode != dnsmessage.RCodeSuccess || len(addrs) != 1 {
		t.Fatalf("udp reply = %+v %+v", header, addrs)
	}

	// TCP (length-prefixed).
	tconn, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatalf("tcp dial: %v", err)
	}
	defer tconn.Close()
	framed := append([]byte{byte(len(msg) >> 8), byte(len(msg))}, msg...)
	if _, err := tconn.Write(framed); err != nil {
		t.Fatalf("tcp write: %v", err)
	}
	size := make([]byte, 2)
	if _, err := io.ReadFull(tconn, size); err != nil {
		t.Fatalf("tcp size: %v", err)
	}
	reply := make([]byte, int(size[0])<<8|int(size[1]))
	if _, err := io.ReadFull(tconn, reply); err != nil {
		t.Fatalf("tcp read: %v", err)
	}
	if header, addrs := parseReply(t, reply); header.RCode != dnsmessage.RCodeSuccess || len(addrs) != 1 {
		t.Fatalf("tcp reply = %+v %+v", header, addrs)
	}
}
