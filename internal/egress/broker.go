package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// BrokerPathPrefix is the broker route inside the sandbox:
// http://<gateway>:<port>/creds/<alias>/<upstream path>.
const BrokerPathPrefix = "/creds/"

// Credential schemes.
const (
	// SchemeBearer sends "Authorization: Bearer <value>".
	SchemeBearer = "bearer"
	// SchemeHeader sends "<header>: <value>".
	SchemeHeader = "header"
)

// Broker denial reasons recorded on the audit trail.
const (
	ReasonBrokerUnknownAlias   = "broker_unknown_alias"
	ReasonBrokerHostNotAllowed = "broker_host_not_allowed"
	ReasonBrokerUpstreamFailed = "broker_upstream_failed"
)

// Credential is a resolved brokered credential. The value is transient: it is
// attached by the broker and must never be persisted with a sandbox spec or
// written to a log.
type Credential struct {
	Alias  string
	Host   string
	Scheme string // SchemeBearer or SchemeHeader
	Header string
	Value  string
}

// ValidateCredential checks a resolved credential before a sandbox starts.
func ValidateCredential(c Credential) error {
	if c.Alias == "" {
		return errors.New("egress: credential alias is required")
	}
	if c.Host == "" {
		return fmt.Errorf("egress: credential %q has no bound host", c.Alias)
	}
	if strings.TrimSpace(c.Value) == "" {
		return fmt.Errorf("egress: credential %q has an empty value", c.Alias)
	}
	switch c.Scheme {
	case SchemeBearer:
		if c.Header != "" {
			return fmt.Errorf("egress: credential %q uses scheme %q with a header name", c.Alias, c.Scheme)
		}
	case SchemeHeader:
		if c.Header == "" {
			return fmt.Errorf("egress: credential %q uses scheme %q without a header name", c.Alias, c.Scheme)
		}
	default:
		return fmt.Errorf("egress: credential %q has unknown scheme %q", c.Alias, c.Scheme)
	}
	return nil
}

// Broker serves the credential broker route for one sandbox (ADR 013). The
// sandbox's half of the connection stays inside the per-sandbox network
// namespace; the broker re-originates to the alias's bound host over TLS with
// the credential attached. TLS is never terminated, no redirect is ever
// followed, and the sandbox's own Authorization/Cookie headers are dropped.
type Broker struct {
	sandboxID string
	policy    Policy
	creds     map[string]Credential
	logger    *slog.Logger
	onEvent   func(Event) error

	transport http.RoundTripper
	server    *http.Server

	// dialHost, dialPort and skipIPCheck are test seams.
	dialHost    string
	dialPort    string
	skipIPCheck bool

	idleTimeout time.Duration
	slots       chan struct{}
}

// NewBroker returns a broker for a sandbox. onEvent may be nil.
func NewBroker(sandboxID string, policy Policy, creds []Credential, logger *slog.Logger, onEvent func(Event) error, limits ...Limits) *Broker {
	if logger == nil {
		logger = slog.Default()
	}
	l := Limits{MaxConnections: defaultMaxConnections, IdleTimeout: defaultIdleTimeout}
	if len(limits) > 0 {
		if limits[0].MaxConnections > 0 {
			l.MaxConnections = limits[0].MaxConnections
		}
		if limits[0].IdleTimeout > 0 {
			l.IdleTimeout = limits[0].IdleTimeout
		}
	}
	byAlias := make(map[string]Credential, len(creds))
	for _, c := range creds {
		byAlias[c.Alias] = c
	}
	b := &Broker{
		sandboxID:   sandboxID,
		policy:      policy,
		creds:       byAlias,
		logger:      logger,
		onEvent:     onEvent,
		dialPort:    DefaultPort,
		idleTimeout: l.IdleTimeout,
		slots:       make(chan struct{}, l.MaxConnections),
	}
	b.transport = &http.Transport{
		DialContext:           b.dialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableKeepAlives:     true,
	}
	return b
}

// dialContext dials the upstream, refusing loopback/private/link-local
// destinations even when the requested host resolves to one.
func (b *Broker) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 15 * time.Second}
	if !b.skipIPCheck {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if blockedIP(net.ParseIP(host)) {
				return errBlockedAddr
			}
			return nil
		}
	}
	return d.DialContext(ctx, network, address)
}

// Listen binds the broker to addr and starts serving. It returns once the
// listener is bound so the sandbox can start knowing the broker is reachable.
func (b *Broker) Listen(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("egress: broker listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           b,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       b.idleTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	b.server = srv
	go func() {
		<-ctx.Done()
		_ = b.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.logger.Error("broker serve failed", "sandbox_id", b.sandboxID, "error", err)
		}
	}()
	return nil
}

// Close stops the broker.
func (b *Broker) Close() error {
	if b.server == nil {
		return nil
	}
	srv := b.server
	b.server = nil
	return srv.Close()
}

// ServeHTTP implements http.Handler for /creds/<alias>/<path>.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	default:
		b.deny(r.Context(), "", "", ReasonConnectionLimit)
		http.Error(w, "broker is at capacity", http.StatusServiceUnavailable)
		return
	}

	alias, rest, ok := brokerTarget(r.URL.Path)
	if !ok {
		http.Error(w, "expected "+BrokerPathPrefix+"<alias>/<path>", http.StatusNotFound)
		return
	}
	cred, ok := b.creds[alias]
	if !ok {
		b.deny(r.Context(), "", alias, ReasonBrokerUnknownAlias)
		http.Error(w, "unknown credential alias", http.StatusNotFound)
		return
	}
	if !b.policy.Allows(cred.Host) {
		b.deny(r.Context(), cred.Host, alias, ReasonBrokerHostNotAllowed)
		http.Error(w, "credential host is not on the sandbox allowlist", http.StatusForbidden)
		return
	}

	// Persist the allow before the credential leaves the host; if the audit
	// write fails the credential is not attached at all (fail closed).
	if err := b.allow(cred.Host, alias); err != nil {
		b.logger.Error("persist brokered allow failed", "sandbox_id", b.sandboxID, "alias", alias, "error", err)
		_ = b.deny(r.Context(), cred.Host, alias, ReasonAuditFailed)
		http.Error(w, "audit unavailable", http.StatusBadGateway)
		return
	}

	upstream, err := b.roundTrip(r, cred, rest)
	if err != nil {
		if errors.Is(err, errBlockedAddr) {
			b.deny(r.Context(), cred.Host, alias, ReasonBlockedIP)
		} else {
			b.deny(r.Context(), cred.Host, alias, ReasonBrokerUpstreamFailed)
		}
		b.logger.Warn("brokered upstream failed", "sandbox_id", b.sandboxID, "alias", alias, "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()

	relayResponse(w, upstream)
}

// roundTrip builds and sends the upstream request with the credential attached.
func (b *Broker) roundTrip(r *http.Request, cred Credential, rest string) (*http.Response, error) {
	host := cred.Host
	if b.dialHost != "" {
		host = b.dialHost
	}
	u := url.URL{
		Scheme:   "https",
		Host:     net.JoinHostPort(host, b.dialPort),
		Path:     "/" + rest,
		RawQuery: r.URL.RawQuery,
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), r.Body)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req.Header, r.Header)
	// The sandbox never supplies the credential, and must not be able to
	// override the header it is attached to. The upstream Host is always the
	// bound host, including when the broker dials a different address.
	req.Header.Del("Authorization")
	req.Header.Del("Cookie")
	req.Host = cred.Host
	switch cred.Scheme {
	case SchemeBearer:
		req.Header.Set("Authorization", "Bearer "+cred.Value)
	case SchemeHeader:
		req.Header.Set(cred.Header, cred.Value)
	}

	// A per-call client makes the redirect policy authoritative even when the
	// transport is replaced in tests: a 3xx is relayed to the sandbox, never
	// followed, so the credential cannot be replayed to another host.
	client := &http.Client{
		Transport: b.transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client.Do(req)
}

// hopByHopHeaders are never forwarded in either direction.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func relayResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func isHopByHop(header string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(h, header) {
			return true
		}
	}
	return false
}

// brokerTarget splits "/creds/<alias>/<rest>" into its alias and upstream path.
// rest never starts with a slash, so it cannot be read as an authority.
func brokerTarget(path string) (alias, rest string, ok bool) {
	if !strings.HasPrefix(path, BrokerPathPrefix) {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(path, BrokerPathPrefix)
	if trimmed == "" {
		return "", "", false
	}
	alias, rest, _ = strings.Cut(trimmed, "/")
	if alias == "" {
		return "", "", false
	}
	return alias, strings.TrimPrefix(rest, "/"), true
}

func (b *Broker) allow(host, alias string) error {
	return b.event(Event{SandboxID: b.sandboxID, Host: host, Alias: alias, Allowed: true, Time: time.Now()})
}

func (b *Broker) deny(_ context.Context, host, alias, reason string) error {
	err := b.event(Event{SandboxID: b.sandboxID, Host: host, Alias: alias, Allowed: false, Reason: reason, Time: time.Now()})
	if err != nil {
		b.logger.Error("persist brokered deny failed", "sandbox_id", b.sandboxID, "alias", alias, "reason", reason, "error", err)
	}
	return err
}

func (b *Broker) event(e Event) error {
	if b.onEvent == nil {
		return nil
	}
	return b.onEvent(e)
}
