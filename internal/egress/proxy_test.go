package egress

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func clientHello(host string) []byte {
	name := []byte(host)
	var sni bytes.Buffer
	sni.WriteByte(0) // name_type = host_name
	binary.Write(&sni, binary.BigEndian, uint16(len(name)))
	sni.Write(name)

	var list bytes.Buffer
	binary.Write(&list, binary.BigEndian, uint16(sni.Len()))
	list.Write(sni.Bytes())

	var ext bytes.Buffer
	binary.Write(&ext, binary.BigEndian, uint16(extensionServerName))
	binary.Write(&ext, binary.BigEndian, uint16(list.Len()))
	ext.Write(list.Bytes())

	var body bytes.Buffer
	body.Write([]byte{0x03, 0x03}) // legacy_version
	body.Write(make([]byte, 32))   // random
	body.WriteByte(0)              // session id length
	binary.Write(&body, binary.BigEndian, uint16(2))
	body.Write([]byte{0x13, 0x01}) // cipher suite
	body.WriteByte(1)
	body.WriteByte(0) // compression method
	binary.Write(&body, binary.BigEndian, uint16(ext.Len()))
	body.Write(ext.Bytes())

	var hs bytes.Buffer
	hs.WriteByte(handshakeClientHello)
	hs.Write([]byte{byte(body.Len() >> 16), byte(body.Len() >> 8), byte(body.Len())})
	hs.Write(body.Bytes())

	var rec bytes.Buffer
	rec.WriteByte(recordTypeHandshake)
	rec.Write([]byte{0x03, 0x01})
	binary.Write(&rec, binary.BigEndian, uint16(hs.Len()))
	rec.Write(hs.Bytes())
	return rec.Bytes()
}

func TestParseSNI(t *testing.T) {
	if got, err := parseSNI(clientHello("api.example.com")[5:]); err != nil || got != "api.example.com" {
		t.Fatalf("parseSNI = %q, %v", got, err)
	}
	if _, err := parseSNI([]byte("GET / HTTP/1.1")); err == nil {
		t.Fatal("expected an error for non-TLS input")
	}
	if _, err := parseSNI([]byte{handshakeClientHello, 0, 0, 1, 0}); err == nil {
		t.Fatal("expected an error for a truncated hello")
	}
}

func TestPolicyAllows(t *testing.T) {
	p := Policy{AllowedDomains: []string{"Example.com", "pypi.org"}}
	for host, want := range map[string]bool{
		"example.com":     true,
		"API.example.com": true,
		"evil.com":        false,
		"notexample.com":  false,
		"pypi.org":        true,
		"":                false,
	} {
		if got := p.Allows(host); got != want {
			t.Errorf("Allows(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestReadClientHelloRejectsPlaintext(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() { _, _ = client.Write([]byte("GET / HTTP/1.1\r\n")) }()
	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := readClientHello(server); err == nil {
		t.Fatal("expected non-TLS error")
	}
}

// startEcho starts a raw TCP upstream that echoes everything it reads.
func startEcho(t *testing.T) (addr, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return ln.Addr().String(), port
}

func dialProxy(t *testing.T, p *Proxy) net.Conn {
	t.Helper()
	p.mu.Lock()
	started := p.listener != nil
	p.mu.Unlock()
	if !started {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		if err := p.Listen(ctx, "127.0.0.1:0"); err != nil {
			t.Fatalf("listen: %v", err)
		}
	}
	p.mu.Lock()
	addr := p.listener.Addr().String()
	p.mu.Unlock()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestProxyForwardsAllowlistedSNI(t *testing.T) {
	_, port := startEcho(t)
	events := make(chan Event, 1)
	p := New("sbx_test", Policy{AllowedDomains: []string{"example.com"}}, nil, func(e Event) error {
		events <- e
		return nil
	})
	p.port = port
	p.skipIPCheck = true
	p.dialHost = "127.0.0.1"
	conn := dialProxy(t, p)

	hello := clientHello("example.com")
	if _, err := conn.Write(hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if _, err := conn.Write([]byte("payload")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	want := append(append([]byte{}, hello...), []byte("payload")...)
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(buf, want) {
		t.Fatalf("echo = %q, want %q", buf, want)
	}
	select {
	case event := <-events:
		if !event.Allowed || event.Host != "example.com" {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing allow event")
	}
}

func TestProxyDeniesUnlistedSNI(t *testing.T) {
	_, port := startEcho(t)
	events := make(chan Event, 1)
	p := New("sbx_test", Policy{AllowedDomains: []string{"example.com"}}, nil, func(e Event) error {
		events <- e
		return nil
	})
	p.port = port
	p.skipIPCheck = true
	p.dialHost = "127.0.0.1"
	conn := dialProxy(t, p)

	if _, err := conn.Write(clientHello("evil.com")); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected the denied connection to be closed")
	}
	select {
	case event := <-events:
		if event.Allowed || event.Reason != ReasonNotAllowed {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing deny event")
	}
}

func TestProxyConnectionLimitAndSlotRelease(t *testing.T) {
	_, port := startEcho(t)
	events := make(chan Event, 8)
	p := New("sbx_test", Policy{AllowedDomains: []string{"example.com"}}, nil, func(e Event) error {
		events <- e
		return nil
	}, Limits{MaxConnections: 1, IdleTimeout: time.Second})
	p.port = port
	p.skipIPCheck = true
	p.dialHost = "127.0.0.1"
	first := dialProxy(t, p)
	hello := clientHello("example.com")
	if _, err := first.Write(hello); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(first, make([]byte, len(hello))); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	addr := p.listener.Addr().String()
	p.mu.Unlock()
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = second.Write(hello)
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection above the limit remained open")
	}
	_ = second.Close()
	select {
	case event := <-events:
		if event.Allowed {
			// The first event is the allowed first connection; consume the limit event.
			event = <-events
		}
		if event.Reason != ReasonConnectionLimit {
			t.Fatalf("limit event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing connection-limit event")
	}

	_ = first.Close()
	deadline := time.Now().Add(time.Second)
	for {
		third, err := net.Dial("tcp", addr)
		if err == nil {
			_, _ = third.Write(hello)
			_ = third.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			buf := make([]byte, len(hello))
			if _, err = io.ReadFull(third, buf); err == nil {
				_ = third.Close()
				break
			}
			_ = third.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("connection slot was not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProxyIdleTimeoutAndActiveTraffic(t *testing.T) {
	_, port := startEcho(t)
	p := New("sbx_test", Policy{AllowedDomains: []string{"example.com"}}, nil, nil,
		Limits{MaxConnections: 2, IdleTimeout: 80 * time.Millisecond})
	p.port = port
	p.skipIPCheck = true
	p.dialHost = "127.0.0.1"
	hello := clientHello("example.com")

	idle := dialProxy(t, p)
	_, _ = idle.Write(hello)
	if _, err := io.ReadFull(idle, make([]byte, len(hello))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	_ = idle.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection remained open")
	}
	_ = idle.Close()

	active := dialProxy(t, p)
	_, _ = active.Write(hello)
	if _, err := io.ReadFull(active, make([]byte, len(hello))); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond)
		if _, err := active.Write([]byte{'x'}); err != nil {
			t.Fatalf("active write %d: %v", i, err)
		}
		buf := make([]byte, 1)
		if _, err := io.ReadFull(active, buf); err != nil || buf[0] != 'x' {
			t.Fatalf("active read %d: %q, %v", i, buf, err)
		}
	}
}

func TestProxyFailsClosedWhenAllowAuditFails(t *testing.T) {
	_, port := startEcho(t)
	events := make(chan Event, 8)
	p := New("sbx_test", Policy{AllowedDomains: []string{"example.com"}}, nil, func(e Event) error {
		events <- e
		if e.Allowed {
			return errors.New("audit unavailable")
		}
		return nil
	})
	p.port = port
	p.skipIPCheck = true
	p.dialHost = "127.0.0.1"
	conn := dialProxy(t, p)
	hello := clientHello("example.com")
	if _, err := conn.Write(hello); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("allowed connection forwarded after audit failure")
	}
	select {
	case event := <-events:
		if !event.Allowed {
			t.Fatalf("first event = %+v, want allowed", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing allow event")
	}
	select {
	case event := <-events:
		if event.Allowed || event.Reason != ReasonAuditFailed {
			t.Fatalf("second event = %+v, want audit_failed", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing audit_failed denial")
	}
}

func TestProxyCloseTerminatesActiveConnections(t *testing.T) {
	p := New("sbx_test", Policy{}, nil, nil, Limits{MaxConnections: 1, IdleTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Listen(ctx, "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	addr := p.listener.Addr().String()
	p.mu.Unlock()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection remained open after proxy cancellation")
	}
}

func TestReasonFor(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{errNotTLS, ReasonNotTLS},
		{errNoSNI, ReasonNoSNI},
		{os.ErrDeadlineExceeded, ReasonHandshakeTimeout},
		{io.ErrUnexpectedEOF, ReasonHandshakeFailed},
	} {
		if got := reasonFor(tc.err); got != tc.reason {
			t.Errorf("reasonFor(%v) = %q, want %q", tc.err, got, tc.reason)
		}
	}
}

func TestProxyControlRejectsPrivateIP(t *testing.T) {
	events := make(chan Event, 1)
	p := New("sbx_test", Policy{AllowedDomains: []string{"example.com"}}, nil, func(e Event) error {
		events <- e
		return nil
	})
	p.port = "443"
	p.dialHost = "127.0.0.1"
	conn := dialProxy(t, p)
	if _, err := conn.Write(clientHello("example.com")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("private-IP upstream should be denied before connect")
	}
	select {
	case event := <-events:
		if event.Allowed || event.Reason != ReasonBlockedIP {
			t.Fatalf("event = %+v, want blocked_address", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing blocked-address event")
	}
}

func TestBlockedIP(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "240.0.0.1", "0.1.2.3", "::1"} {
		if !blockedIP(net.ParseIP(ip)) {
			t.Errorf("expected %s to be blocked", ip)
		}
	}
	if blockedIP(net.ParseIP("93.184.216.34")) {
		t.Error("expected a public IP to be allowed")
	}
}
