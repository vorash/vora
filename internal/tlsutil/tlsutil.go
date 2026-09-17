// Package tlsutil builds TLS configurations for the Vora control plane and the
// runner channel from optional certificate files.
//
// An empty Config means "TLS disabled" (plaintext, for local development). When
// a CA is configured on the server side, client certificates are required and
// verified (mTLS).
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// Config points at TLS material on disk. All fields are optional.
type Config struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// Enabled reports whether any TLS material is configured.
func (c Config) Enabled() bool {
	return c.CertFile != "" || c.KeyFile != "" || c.CAFile != ""
}

// Server builds a server TLS config. A cert/key pair is required. When CAFile
// is set, client certificates are required and verified against it (mTLS).
func (c Config) Server() (*tls.Config, error) {
	if c.CertFile == "" || c.KeyFile == "" {
		return nil, fmt.Errorf("tls: server requires a certificate and key")
	}
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: load server key pair: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
	}
	if c.CAFile != "" {
		pool, err := loadPool(c.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// Client builds a client TLS config. The server is verified against CAFile (or
// the system roots when CAFile is empty). A client cert is presented when
// cert/key are set.
func (c Config) Client() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pool, err := loadPool(c.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" || c.KeyFile != "" {
		if c.CertFile == "" || c.KeyFile == "" {
			return nil, fmt.Errorf("tls: client needs both certificate and key")
		}
		pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls: load client key pair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// ClientFor builds a client TLS config with an explicit server name override.
func (c Config) ClientFor(serverName string) (*tls.Config, error) {
	cfg, err := c.Client()
	if err != nil {
		return nil, err
	}
	cfg.ServerName = serverName
	return cfg, nil
}

func loadPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tls: read CA %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tls: no certificates found in %s", path)
	}
	return pool, nil
}
