//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/api"
	"github.com/vorash/vora/internal/registry"
	"github.com/vorash/vora/internal/tlsutil"
)

// TestRegistryMTLS proves the runner↔API channel enforces mutual TLS: a runner
// must present a client certificate signed by the API's CA, and a plaintext
// client is rejected.
func TestRegistryMTLS(t *testing.T) {
	certs := generateCerts(t)

	apiTLS := tlsutil.Config{CertFile: certs.serverCert, KeyFile: certs.serverKey, CAFile: certs.ca}
	serverCfg, err := apiTLS.Server()
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}
	apiSrv := api.New(api.Options{Store: api.NewMemStore(), JoinToken: "join"})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverCfg)))
	v1.RegisterRunnerRegistryServer(grpcSrv, apiSrv)
	go func() { _ = grpcSrv.Serve(ln) }()
	t.Cleanup(grpcSrv.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := &v1.RegisterRunnerRequest{RunnerId: "run_mtls", Address: "127.0.0.1:9200"}

	// Plaintext client is rejected.
	plain, err := registry.Dial(ln.Addr().String(), "join")
	if err != nil {
		t.Fatalf("dial plaintext: %v", err)
	}
	if _, err := plain.Register(ctx, req); err == nil {
		t.Fatal("expected a plaintext client to be rejected")
	}
	plain.Close()

	// TLS client that trusts the CA but presents no certificate is rejected (mTLS).
	noCertCfg, err := tlsutil.Config{CAFile: certs.ca}.Client()
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}
	noCert, err := registry.Dial(ln.Addr().String(), "join", grpc.WithTransportCredentials(credentials.NewTLS(noCertCfg)))
	if err != nil {
		t.Fatalf("dial no-cert: %v", err)
	}
	if _, err := noCert.Register(ctx, req); err == nil {
		t.Fatal("expected a client without a certificate to be rejected")
	}
	noCert.Close()

	// Full mTLS client succeeds.
	mtlsCfg, err := tlsutil.Config{CAFile: certs.ca, CertFile: certs.clientCert, KeyFile: certs.clientKey}.Client()
	if err != nil {
		t.Fatalf("mTLS client tls: %v", err)
	}
	client, err := registry.Dial(ln.Addr().String(), "join", grpc.WithTransportCredentials(credentials.NewTLS(mtlsCfg)))
	if err != nil {
		t.Fatalf("dial mTLS: %v", err)
	}
	defer client.Close()
	if _, err := client.Register(ctx, req); err != nil {
		t.Fatalf("mTLS register: %v", err)
	}
	if _, err := client.Heartbeat(ctx, &v1.HeartbeatRequest{RunnerId: "run_mtls"}); err != nil {
		t.Fatalf("mTLS heartbeat: %v", err)
	}
}

type certFiles struct {
	ca                    string
	serverCert, serverKey string
	clientCert, clientKey string
}

func generateCerts(t *testing.T) certFiles {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "vora-integration-ca"},
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

	leaf := func(cn string, eku []x509.ExtKeyUsage, ips []net.IP) (string, string) {
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
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, caTpl, &key.PublicKey, caKey)
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

	srvCert, srvKey := leaf("127.0.0.1", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")})
	cliCert, cliKey := leaf("runner", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	return certFiles{ca: caPath, serverCert: srvCert, serverKey: srvKey, clientCert: cliCert, clientKey: cliKey}
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
