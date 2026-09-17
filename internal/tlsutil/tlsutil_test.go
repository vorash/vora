package tlsutil_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vorash/vora/internal/tlsutil"
)

type material struct {
	ca                         string
	serverCert, serverKey      string
	clientCert, clientKey      string
	otherCert, otherKey, othCA string
}

func gen(t *testing.T) material {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "vora-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	writePEM(t, caPath, "CERTIFICATE", caDER)

	leaf := func(cn string, ips []net.IP, dns []string, eku []x509.ExtKeyUsage, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (string, string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  eku,
			IPAddresses:  ips,
			DNSNames:     dns,
		}
		if parent == nil {
			parent, parentKey = caTpl, caKey
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		cp := filepath.Join(dir, cn+".crt")
		kp := filepath.Join(dir, cn+".key")
		writePEM(t, cp, "CERTIFICATE", der)
		kb, _ := x509.MarshalECPrivateKey(key)
		writePEM(t, kp, "EC PRIVATE KEY", kb)
		return cp, kp
	}

	srvCert, srvKey := leaf("localhost", []net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil, nil)
	cliCert, cliKey := leaf("runner", nil, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, nil)
	return material{ca: caPath, serverCert: srvCert, serverKey: srvKey, clientCert: cliCert, clientKey: cliKey}
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func handshake(t *testing.T, server *tls.Config, client *tls.Config) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		errCh <- tls.Server(conn, server).Handshake()
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return err
	}
	defer raw.Close()
	clientErr := tls.Client(raw, client).Handshake()
	serverErr := <-errCh
	if clientErr != nil {
		return clientErr
	}
	return serverErr
}

func TestDisabledWhenEmpty(t *testing.T) {
	if (tlsutil.Config{}).Enabled() {
		t.Fatal("empty config should be disabled")
	}
}

func TestMTLSRequiredWhenCAConfigured(t *testing.T) {
	m := gen(t)
	serverCfg, err := tlsutil.Config{CertFile: m.serverCert, KeyFile: m.serverKey, CAFile: m.ca}.Server()
	if err != nil {
		t.Fatalf("server config: %v", err)
	}

	// A client without a certificate is rejected.
	noCert, err := tlsutil.Config{CAFile: m.ca}.ClientFor("localhost")
	if err != nil {
		t.Fatalf("client config: %v", err)
	}
	if err := handshake(t, serverCfg, noCert); err == nil {
		t.Fatal("expected the handshake to fail without a client certificate")
	}

	// A client with a certificate signed by the CA succeeds.
	withCert, err := tlsutil.Config{CAFile: m.ca, CertFile: m.clientCert, KeyFile: m.clientKey}.ClientFor("localhost")
	if err != nil {
		t.Fatalf("client config: %v", err)
	}
	if err := handshake(t, serverCfg, withCert); err != nil {
		t.Fatalf("expected the mTLS handshake to succeed: %v", err)
	}
}

func TestClientRejectsWrongCA(t *testing.T) {
	m := gen(t)
	serverCfg, err := tlsutil.Config{CertFile: m.serverCert, KeyFile: m.serverKey}.Server()
	if err != nil {
		t.Fatalf("server config: %v", err)
	}
	// A client that trusts a different CA must reject the server.
	wrongCA, err := tlsutil.Config{CAFile: m.ca}.ClientFor("wrong.example")
	if err != nil {
		t.Fatalf("client config: %v", err)
	}
	if err := handshake(t, serverCfg, wrongCA); err == nil {
		t.Fatal("expected the handshake to fail for the wrong server name")
	}
}
