package main

import (
	"github.com/vorash/vora/internal/tlsutil"
	"testing"
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
