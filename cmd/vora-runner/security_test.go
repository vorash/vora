package main

import (
	"crypto/x509"
	"testing"

	"github.com/vorash/vora/internal/tlsutil"
)

func TestRunnerTCPBindsClientIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cert    *x509.Certificate
		allowed bool
	}{
		{"API client", &x509.Certificate{DNSNames: []string{"vora-api"}}, true},
		{"another CA-signed client", &x509.Certificate{DNSNames: []string{"another-service"}}, false},
		{"common name without SAN", &x509.Certificate{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyAPIIdentity(tc.cert, "vora-api"); (got == nil) != tc.allowed {
				t.Fatalf("identity result=%v, allowed=%v", got, tc.allowed)
			}
		})
	}
}

func TestRunnerTCPRequiresMutualTLS(t *testing.T) {
	for _, tc := range []struct {
		name      string
		addr      string
		insecure  bool
		config    tlsutil.Config
		wantError bool
	}{
		{"public plaintext", "0.0.0.0:9200", false, tlsutil.Config{}, true},
		{"public server TLS only", "0.0.0.0:9200", false, tlsutil.Config{CertFile: "cert", KeyFile: "key"}, true},
		{"loopback plaintext without opt in", "127.0.0.1:9200", false, tlsutil.Config{}, true},
		{"public insecure opt in", "0.0.0.0:9200", true, tlsutil.Config{}, true},
		{"loopback insecure opt in", "127.0.0.1:9200", true, tlsutil.Config{}, false},
		{"public mutual TLS", "0.0.0.0:9200", false, tlsutil.Config{CertFile: "cert", KeyFile: "key", CAFile: "ca"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRunnerTCP(tc.addr, tc.config, tc.insecure)
			if (err != nil) != tc.wantError {
				t.Fatalf("validation error=%v, want error=%v", err, tc.wantError)
			}
		})
	}
}
