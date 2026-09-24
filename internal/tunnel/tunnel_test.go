package tunnel

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestTunnelCertificateBindsRunnerID(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{"run_owner"}}
	if !certificateIsRunner(cert, "run_owner") || certificateIsRunner(cert, "run_victim") {
		t.Fatal("a tunnel can be replaced under a different runner id")
	}
}

func TestTunnelRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := NewServer("secret", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, ln) }()

	client := &Client{Addr: ln.Addr().String(), RunnerID: "run_1", JoinToken: "secret"}
	streams, err := client.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = streams.Close() })

	// The runner side echoes whatever the API sends on a stream.
	go func() {
		for {
			c, err := streams.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()

	waitFor(t, func() bool { return srv.Has("run_1") })
	conn, err := srv.Dial(ctx, "run_1")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("echo = %q", buf)
	}
}

func TestTunnelRejectsBadToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := NewServer("secret", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, ln) }()

	client := &Client{Addr: ln.Addr().String(), RunnerID: "run_bad", JoinToken: "wrong"}
	if _, err := client.Listen(ctx); err != nil {
		// A rejected hello closes the connection; Listen may still return a
		// listener whose streams then fail. Either way the runner must not be
		// registered.
		t.Logf("Listen returned: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if srv.Has("run_bad") {
		t.Fatal("a runner with a bad join token must not establish a tunnel")
	}
}

func TestDialUnknownRunner(t *testing.T) {
	srv := NewServer("", nil)
	if _, err := srv.Dial(context.Background(), "nobody"); err == nil {
		t.Fatal("dialing a runner without a tunnel must fail")
	}
}
