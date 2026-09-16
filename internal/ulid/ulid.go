// Package ulid generates prefixed, lexicographically sortable identifiers.
package ulid

import (
	"crypto/rand"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Identifier prefixes. Format: <prefix>_<ULID>, e.g. sbx_01J....
const (
	PrefixSandbox = "sbx"
	PrefixRunner  = "run"
	PrefixExec    = "exe"
	PrefixAgent   = "agt"
)

// entropy is monotonic so that identifiers created within the same millisecond
// still sort in creation order. ulid.Monotonic is not concurrency-safe, so all
// reads are serialised by entropyMu.
var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// New returns a new prefixed ULID for the given prefix.
func New(prefix string) string {
	entropyMu.Lock()
	id, err := ulid.New(ulid.Timestamp(time.Now()), entropy)
	entropyMu.Unlock()
	if err != nil {
		// crypto/rand failure is unrecoverable and should never happen.
		panic("ulid: cannot read entropy: " + err.Error())
	}
	return prefix + "_" + id.String()
}

// NewSandbox returns a new sandbox identifier.
func NewSandbox() string { return New(PrefixSandbox) }

// NewRunner returns a new runner identifier.
func NewRunner() string { return New(PrefixRunner) }

// NewExec returns a new execution identifier.
func NewExec() string { return New(PrefixExec) }

// NewAgent returns a new agent identifier.
func NewAgent() string { return New(PrefixAgent) }

// Valid reports whether id is a well-formed prefixed ULID with one of the
// known prefixes.
func Valid(id string) bool {
	prefix, rest, ok := strings.Cut(id, "_")
	if !ok || rest == "" {
		return false
	}
	switch prefix {
	case PrefixSandbox, PrefixRunner, PrefixExec, PrefixAgent:
	default:
		return false
	}
	_, err := ulid.ParseStrict(rest)
	return err == nil
}
