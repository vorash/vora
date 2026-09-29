package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/api"
	"github.com/vorash/vora/internal/ulid"
)

// writeClientCert writes a self-signed client certificate with the given DNS
// SANs and returns its path and parsed form.
func writeClientCert(t *testing.T, dnsNames ...string) (string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "vora runner registry client"},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "client.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, cert
}

// A registry with a client CA only accepts the runner ID named in the client
// certificate. A runner that drew a fresh ID each start could never register
// with a certificate issued before it ran, so the ID must come from the
// certificate, and be the same on every start.
func TestRunnerIDFromCertificateIsAcceptedByTheRegistry(t *testing.T) {
	want := ulid.NewRunner()
	path, cert := writeClientCert(t, "vora-runner", want)

	registry := api.New(api.Options{RequireRunnerIdentity: true})
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
	}}})
	for start := range 2 {
		id, err := runnerIDFromCerts(path)
		if err != nil {
			t.Fatalf("start %d: %v", start, err)
		}
		if id != want {
			t.Fatalf("start %d: runner id = %q, want the certificate's %q", start, id, want)
		}
		if _, err := registry.Register(ctx, &v1.RegisterRunnerRequest{RunnerId: id, Address: "127.0.0.1:9200"}); err != nil {
			t.Fatalf("start %d: registry refused the certificate's runner id: %v", start, err)
		}
	}
}

func TestRunnerIDFromCerts(t *testing.T) {
	a, b := ulid.NewRunner(), ulid.NewRunner()
	withA, _ := writeClientCert(t, a)
	alsoA, _ := writeClientCert(t, "vora-runner", a)
	withB, _ := writeClientCert(t, b)
	withBoth, _ := writeClientCert(t, a, b)
	withNone, _ := writeClientCert(t, "vora-runner", "run_not-a-ulid")

	for _, tc := range []struct {
		name    string
		paths   []string
		want    string
		wantErr bool
	}{
		// No client certificate, or one without a runner SAN (e.g. an API
		// that does not require runner identity): keep a generated ID rather
		// than refusing to start.
		{"no certificate", nil, "", false},
		{"certificate without a runner SAN", []string{withNone}, "", false},
		{"registry and tunnel agree", []string{withA, alsoA}, a, false},
		{"only one of two names a runner", []string{withNone, withA}, a, false},
		// Ambiguity must not be resolved by picking one: the API would bind
		// the runner to whichever the runner happened to choose.
		{"two runner SANs", []string{withBoth}, "", true},
		{"registry and tunnel disagree", []string{withA, withB}, "", true},
		{"unreadable certificate", []string{filepath.Join(t.TempDir(), "missing.crt")}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runnerIDFromCerts(tc.paths...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("runner id = %q, want %q", got, tc.want)
			}
		})
	}
}
