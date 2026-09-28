package main

import (
	"strings"
	"testing"

	"github.com/vorash/vora/internal/tlsutil"
)

func TestPublicRegistryRequiresMutualTLS(t *testing.T) {
	for _, tc := range []struct {
		addr      string
		config    tlsutil.Config
		wantError bool
	}{
		{"0.0.0.0:9301", tlsutil.Config{}, true},
		{"0.0.0.0:9301", tlsutil.Config{CertFile: "cert", KeyFile: "key"}, true},
		{"127.0.0.1:9301", tlsutil.Config{}, false},
		{"0.0.0.0:9301", tlsutil.Config{CertFile: "cert", KeyFile: "key", CAFile: "ca"}, false},
	} {
		if err := requireRegistryTLS(tc.addr, tc.config); (err != nil) != tc.wantError {
			t.Errorf("addr=%s error=%v wantError=%v", tc.addr, err, tc.wantError)
		}
	}
}

func TestTunnelRequiresTLSExceptExplicitLoopback(t *testing.T) {
	for _, tc := range []struct {
		addr                string
		config              tlsutil.Config
		insecure, wantError bool
	}{
		{"0.0.0.0:9302", tlsutil.Config{}, false, true},
		{"0.0.0.0:9302", tlsutil.Config{}, true, true},
		{"127.0.0.1:9302", tlsutil.Config{}, true, false},
		{"0.0.0.0:9302", tlsutil.Config{CertFile: "cert", KeyFile: "key", CAFile: "ca"}, false, false},
	} {
		if err := requireTunnelTLS(tc.addr, tc.config, tc.insecure); (err != nil) != tc.wantError {
			t.Errorf("addr=%s error=%v wantError=%v", tc.addr, err, tc.wantError)
		}
	}
}

func TestPublicRegistryAndTunnelRequireJoinToken(t *testing.T) {
	for _, tc := range []struct {
		addr, token string
		wantError   bool
	}{
		{"127.0.0.1:9301", "", false},
		{"0.0.0.0:9301", "", true},
		{"[::]:9301", "", true},
		{"0.0.0.0:9301", "secret", false},
	} {
		if err := requireJoinToken(tc.addr, tc.token); (err != nil) != tc.wantError {
			t.Errorf("addr=%s token=%q error=%v wantError=%v", tc.addr, tc.token, err, tc.wantError)
		}
	}
}

// The HTTP API carries bearer keys, file contents and exec I/O. On a
// non-loopback bind without TLS all of it crosses the network in clear, unless
// the operator says a proxy terminates TLS in front.
func TestPublicHTTPRequiresTLSOrExplicitOptOut(t *testing.T) {
	tlsCfg := tlsutil.Config{CertFile: "cert", KeyFile: "key"}
	for _, tc := range []struct {
		addr      string
		config    tlsutil.Config
		insecure  bool
		wantError bool
	}{
		{"0.0.0.0:9300", tlsutil.Config{}, false, true},
		{":9300", tlsutil.Config{}, false, true},
		{"[::]:9300", tlsutil.Config{}, false, true},
		{"192.0.2.10:9300", tlsutil.Config{}, false, true},
		{"0.0.0.0:9300", tlsutil.Config{}, true, false},
		{"0.0.0.0:9300", tlsCfg, false, false},
		{"127.0.0.1:9300", tlsutil.Config{}, false, false},
		{"[::1]:9300", tlsutil.Config{}, false, false},
		{"localhost:9300", tlsutil.Config{}, false, false},
	} {
		if err := requireHTTPTLS(tc.addr, tc.config, tc.insecure); (err != nil) != tc.wantError {
			t.Errorf("addr=%s tls=%v insecure=%v error=%v wantError=%v", tc.addr, tc.config.Enabled(), tc.insecure, err, tc.wantError)
		}
	}
}

// The bootstrap key is a global admin credential that can be guessed online.
func TestBootstrapKeyHasMinimumLength(t *testing.T) {
	for _, tc := range []struct {
		key       string
		wantError bool
	}{
		{"", false}, // no bootstrap key configured
		{"admin", true},
		{"vora_bootstrap_mtls", true},
		{strings.Repeat("k", 31), true},
		{strings.Repeat("k", 32), false},
	} {
		if err := requireBootstrapKey(tc.key); (err != nil) != tc.wantError {
			t.Errorf("key length %d error=%v wantError=%v", len(tc.key), err, tc.wantError)
		}
	}
}

// A loopback registry may run without a join token, but then any local process
// can register as a runner and receive new sandboxes; that must not be silent.
func TestRegistryWithoutJoinTokenWarns(t *testing.T) {
	mtls := tlsutil.Config{CertFile: "cert", KeyFile: "key", CAFile: "ca"}
	for _, tc := range []struct {
		token    string
		registry tlsutil.Config
		warn     bool
	}{
		{"", tlsutil.Config{}, true},
		{"", tlsutil.Config{CertFile: "cert", KeyFile: "key"}, true},
		{"secret", tlsutil.Config{}, false},
		{"", mtls, false},
	} {
		if got := joinTokenWarning(tc.token, tc.registry) != ""; got != tc.warn {
			t.Errorf("token=%q mtls=%v warn=%v want %v", tc.token, tc.registry.CAFile != "", got, tc.warn)
		}
	}
}
