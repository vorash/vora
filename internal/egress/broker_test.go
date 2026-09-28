package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

var errTestAudit = errors.New("audit unavailable")

// eventLog collects brokered decisions. All access is synchronised: the broker
// calls the sink from request goroutines.
type eventLog struct {
	mu     sync.Mutex
	events []Event
	fail   error
}

func (l *eventLog) sink(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return l.fail
}

func (l *eventLog) all() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.events...)
}

// upstream records what the broker actually sent.
type upstream struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	status   int
	location string
	block    chan struct{}
	started  chan struct{}
	calls    int
}

func (u *upstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, r.Clone(context.Background()))
		u.bodies = append(u.bodies, string(body))
		u.calls++
		block, started := u.block, u.started
		status, location := u.status, u.location
		u.mu.Unlock()

		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		if block != nil {
			<-block
		}
		if status != 0 {
			if location != "" {
				w.Header().Set("Location", location)
			}
			w.WriteHeader(status)
			return
		}
		w.Header().Set("X-Upstream", "yes")
		_, _ = w.Write([]byte("upstream-body"))
	})
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *upstream) first() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return nil
	}
	return u.requests[0]
}

func (u *upstream) firstBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return ""
	}
	return u.bodies[0]
}

// newBrokerFor wires a broker to a test upstream: the credential host is the
// allowlisted name, while the dial target points at the TLS test server.
func newBrokerFor(t *testing.T, up *httptest.Server, policy Policy, creds []Credential, log *eventLog) *Broker {
	t.Helper()
	b := NewBroker("sbx_broker_test", policy, creds, slog.New(slog.NewTextHandler(io.Discard, nil)), log.sink)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatalf("upstream url: %v", err)
	}
	b.transport = up.Client().Transport
	b.dialHost = u.Hostname()
	b.dialPort = u.Port()
	return b
}

func brokerRequest(t *testing.T, b *Broker, method, path string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	return rec.Result()
}

// TestBrokerAttachesCredential asserts the credential replaces whatever the
// sandbox sent, the rest of the request is relayed, and the decision is audited
// by alias (never by value).
func TestBrokerAttachesCredential(t *testing.T) {
	up := &upstream{}
	ts := httptest.NewTLSServer(up.handler())
	defer ts.Close()

	log := &eventLog{}
	b := newBrokerFor(t, ts, Policy{AllowedDomains: []string{"api.example.com"}},
		[]Credential{{Alias: "github", Host: "api.example.com", Scheme: SchemeBearer, Value: "ghp_real_secret"}}, log)

	resp := brokerRequest(t, b, http.MethodPost, "/creds/github/user/repos?per_page=5",
		strings.NewReader("payload"),
		map[string]string{
			"Authorization": "Bearer sandbox-supplied",
			"Cookie":        "session=sandbox",
			"X-Custom":      "kept",
			"Host":          "gateway",
		})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream-body" || resp.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("relayed response = %q, headers %v", body, resp.Header)
	}

	got := up.first()
	if got == nil {
		t.Fatal("upstream was never called")
	}
	if got.Method != http.MethodPost || got.URL.Path != "/user/repos" || got.URL.RawQuery != "per_page=5" {
		t.Fatalf("upstream request = %s %s", got.Method, got.URL)
	}
	if got.Header.Get("Authorization") != "Bearer ghp_real_secret" {
		t.Fatalf("upstream Authorization = %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("Cookie") != "" {
		t.Fatalf("sandbox cookie was forwarded: %q", got.Header.Get("Cookie"))
	}
	if got.Header.Get("X-Custom") != "kept" {
		t.Fatalf("X-Custom = %q, want kept", got.Header.Get("X-Custom"))
	}
	if got.Host != "api.example.com" {
		t.Fatalf("upstream Host = %q, want the bound host", got.Host)
	}
	if up.firstBody() != "payload" {
		t.Fatalf("upstream body = %q", up.firstBody())
	}

	events := log.all()
	if len(events) != 1 || !events[0].Allowed || events[0].Alias != "github" || events[0].Host != "api.example.com" {
		t.Fatalf("events = %+v", events)
	}
}

// TestBrokerHeaderScheme asserts the header scheme attaches a custom header and
// sends no Authorization header.
func TestBrokerHeaderScheme(t *testing.T) {
	up := &upstream{}
	ts := httptest.NewTLSServer(up.handler())
	defer ts.Close()

	log := &eventLog{}
	b := newBrokerFor(t, ts, Policy{AllowedDomains: []string{"api.example.com"}},
		[]Credential{{Alias: "openai", Host: "api.example.com", Scheme: SchemeHeader, Header: "X-Api-Key", Value: "sk_secret"}}, log)

	resp := brokerRequest(t, b, http.MethodGet, "/creds/openai/v1/models", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := up.first()
	if got == nil {
		t.Fatal("upstream was never called")
	}
	if got.Header.Get("X-Api-Key") != "sk_secret" || got.Header.Get("Authorization") != "" {
		t.Fatalf("headers = %v", got.Header)
	}
}

// TestBrokerDeniesWithoutCallingUpstream asserts unknown aliases and
// non-allowlisted hosts fail closed before the credential is used.
func TestBrokerDeniesWithoutCallingUpstream(t *testing.T) {
	up := &upstream{}
	ts := httptest.NewTLSServer(up.handler())
	defer ts.Close()

	tests := []struct {
		name   string
		path   string
		policy Policy
		creds  []Credential
		status int
		reason string
	}{
		{
			name: "unknown alias", path: "/creds/github/user",
			policy: Policy{AllowedDomains: []string{"api.example.com"}},
			creds:  nil, status: http.StatusNotFound, reason: ReasonBrokerUnknownAlias,
		},
		{
			name: "host not allowlisted", path: "/creds/github/user",
			policy: Policy{AllowedDomains: []string{"other.example.com"}},
			creds:  []Credential{{Alias: "github", Host: "api.example.com", Scheme: SchemeBearer, Value: "v"}},
			status: http.StatusForbidden, reason: ReasonBrokerHostNotAllowed,
		},
		{
			name: "bad route", path: "/not-creds/github",
			policy: Policy{AllowedDomains: []string{"api.example.com"}},
			creds:  []Credential{{Alias: "github", Host: "api.example.com", Scheme: SchemeBearer, Value: "v"}},
			status: http.StatusNotFound, reason: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := &eventLog{}
			b := newBrokerFor(t, ts, tc.policy, tc.creds, log)
			resp := brokerRequest(t, b, http.MethodGet, tc.path, nil, nil)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if up.count() != 0 {
				t.Fatalf("upstream was called %d times", up.count())
			}
			if tc.reason != "" {
				events := log.all()
				if len(events) != 1 || events[0].Allowed || events[0].Reason != tc.reason {
					t.Fatalf("events = %+v", events)
				}
			}
		})
	}
}

// TestBrokerDoesNotFollowRedirect asserts a 3xx is relayed to the sandbox and
// the credential is never replayed to the redirect target.
func TestBrokerDoesNotFollowRedirect(t *testing.T) {
	up := &upstream{status: http.StatusFound, location: "https://attacker.example.com/steal"}
	ts := httptest.NewTLSServer(up.handler())
	defer ts.Close()

	attacker := &upstream{}
	attackerTS := httptest.NewTLSServer(attacker.handler())
	defer attackerTS.Close()

	log := &eventLog{}
	b := newBrokerFor(t, ts, Policy{AllowedDomains: []string{"api.example.com"}},
		[]Credential{{Alias: "github", Host: "api.example.com", Scheme: SchemeBearer, Value: "ghp_secret"}}, log)

	resp := brokerRequest(t, b, http.MethodGet, "/creds/github/user", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want the redirect relayed", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "https://attacker.example.com/steal" {
		t.Fatalf("Location = %q", got)
	}
	if up.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.count())
	}
	if attacker.count() != 0 {
		t.Fatal("the broker followed the redirect")
	}
}

// TestBrokerAuditFailureDenies asserts an allow that cannot be audited stops
// the credential from being used at all.
func TestBrokerAuditFailureDenies(t *testing.T) {
	up := &upstream{}
	ts := httptest.NewTLSServer(up.handler())
	defer ts.Close()

	log := &eventLog{fail: errTestAudit}
	b := newBrokerFor(t, ts, Policy{AllowedDomains: []string{"api.example.com"}},
		[]Credential{{Alias: "github", Host: "api.example.com", Scheme: SchemeBearer, Value: "v"}}, log)

	resp := brokerRequest(t, b, http.MethodGet, "/creds/github/user", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if up.count() != 0 {
		t.Fatalf("upstream was called %d times", up.count())
	}
}

// TestBrokerCapacity asserts concurrent requests are bounded and a saturated
// broker refuses with an audited reason instead of queueing without limit.
func TestBrokerCapacity(t *testing.T) {
	up := &upstream{block: make(chan struct{}), started: make(chan struct{}, 4)}
	ts := httptest.NewTLSServer(up.handler())
	defer ts.Close()

	log := &eventLog{}
	b := newBrokerFor(t, ts, Policy{AllowedDomains: []string{"api.example.com"}},
		[]Credential{{Alias: "github", Host: "api.example.com", Scheme: SchemeBearer, Value: "v"}}, log)
	b.slots = make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		resp := brokerRequest(t, b, http.MethodGet, "/creds/github/user", nil, nil)
		resp.Body.Close()
		close(done)
	}()
	select {
	case <-up.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the upstream")
	}

	resp := brokerRequest(t, b, http.MethodGet, "/creds/github/user", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("saturated status = %d, want 503", resp.StatusCode)
	}
	var sawLimit bool
	for _, e := range log.all() {
		if !e.Allowed && e.Reason == ReasonConnectionLimit {
			sawLimit = true
		}
	}
	if !sawLimit {
		t.Fatalf("no connection_limit event: %+v", log.all())
	}

	close(up.block)
	<-done
	if up.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.count())
	}
}

// TestBrokerTarget parses the route: everything after the alias is the upstream
// path, and a path cannot be read as an authority.
func TestBrokerTarget(t *testing.T) {
	tests := []struct {
		path  string
		alias string
		rest  string
		ok    bool
	}{
		{"/creds/github/user", "github", "user", true},
		{"/creds/github/user/repos", "github", "user/repos", true},
		{"/creds/github", "github", "", true},
		{"/creds/github/", "github", "", true},
		{"/creds//user", "", "", false},
		{"/creds/", "", "", false},
		{"/creds", "", "", false},
		{"/other/github", "", "", false},
		{"/", "", "", false},
	}
	for _, tc := range tests {
		alias, rest, ok := brokerTarget(tc.path)
		if ok != tc.ok || alias != tc.alias || rest != tc.rest {
			t.Fatalf("brokerTarget(%q) = %q, %q, %v; want %q, %q, %v", tc.path, alias, rest, ok, tc.alias, tc.rest, tc.ok)
		}
	}
}

// TestValidateCredential covers the credential contract enforced before a
// brokered sandbox starts.
func TestValidateCredential(t *testing.T) {
	base := Credential{Alias: "github", Host: "api.github.com", Scheme: SchemeBearer, Value: "v"}
	tests := []struct {
		name    string
		cred    Credential
		wantErr string
	}{
		{"bearer ok", base, ""},
		{"header ok", Credential{Alias: "gh", Host: "api.github.com", Scheme: SchemeHeader, Header: "X-K", Value: "v"}, ""},
		{"no alias", Credential{Host: "api.github.com", Scheme: SchemeBearer, Value: "v"}, "alias"},
		{"no host", Credential{Alias: "gh", Scheme: SchemeBearer, Value: "v"}, "host"},
		{"no value", Credential{Alias: "gh", Host: "api.github.com", Scheme: SchemeBearer}, "empty value"},
		{"unknown scheme", Credential{Alias: "gh", Host: "api.github.com", Scheme: "magic", Value: "v"}, "unknown scheme"},
		{"header missing", Credential{Alias: "gh", Host: "api.github.com", Scheme: SchemeHeader, Value: "v"}, "header name"},
		{"bearer with header", Credential{Alias: "gh", Host: "api.github.com", Scheme: SchemeBearer, Header: "X-K", Value: "v"}, "header name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCredential(tc.cred)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateCredential = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateCredential = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TRACE makes a server echo the request it received, and the broker has just
// attached the credential to that request: the sandbox would read the secret
// out of the response body. Unknown methods get the same refusal.
func TestBrokerRefusesMethodsThatCanReflectTheCredential(t *testing.T) {
	up := &upstream{}
	ts := httptest.NewTLSServer(up.handler())
	defer ts.Close()

	for _, method := range []string{"TRACE", "TRACK", "CONNECT", "PROPFIND", "trace"} {
		t.Run(method, func(t *testing.T) {
			log := &eventLog{}
			b := newBrokerFor(t, ts, Policy{AllowedDomains: []string{"api.example.com"}}, []Credential{
				{Alias: "github", Host: "api.example.com", Scheme: SchemeBearer, Value: "secret"},
			}, log)
			resp := brokerRequest(t, b, method, "/creds/github/user", nil, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", resp.StatusCode)
			}
			if up.count() != 0 {
				t.Fatalf("upstream was called %d times", up.count())
			}
			events := log.all()
			if len(events) != 1 || events[0].Allowed || events[0].Reason != ReasonBrokerMethodNotAllowed || events[0].Alias != "github" {
				t.Fatalf("events = %+v, want one audited denial for alias github", events)
			}
		})
	}
}

func TestBrokerForwardsOrdinaryMethods(t *testing.T) {
	up := &upstream{}
	ts := httptest.NewTLSServer(up.handler())
	defer ts.Close()

	methods := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	log := &eventLog{}
	b := newBrokerFor(t, ts, Policy{AllowedDomains: []string{"api.example.com"}}, []Credential{
		{Alias: "github", Host: "api.example.com", Scheme: SchemeBearer, Value: "secret"},
	}, log)
	for _, method := range methods {
		resp := brokerRequest(t, b, method, "/creds/github/user", nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", method, resp.StatusCode)
		}
	}
	if up.count() != len(methods) {
		t.Fatalf("upstream calls = %d, want %d", up.count(), len(methods))
	}
}
