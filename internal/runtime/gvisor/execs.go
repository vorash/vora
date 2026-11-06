package gvisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pidPoll is how often a waiter looks for a client's PID file. A healthy exec answers in
// about 9 ms.
const pidPoll = 2 * time.Millisecond

// execAnswerWait bounds how long Exec waits to learn whether a command started.
const execAnswerWait = 2 * time.Second

// execClients is what a reset must know about one sandbox's `runsc exec` clients.
//
// A client that has not answered is the one thing a host `runsc ps` cannot see. The Sentry
// serves each control connection on its own goroutine and finishes a request it has read
// whether or not the client is still there, so a task can start after any later `ps`. How
// much later is the Sentry's scheduling and nothing else. Measured: 83 ms after the client's
// SIGKILL on a member throttled at cpu=1000m, and 503 ms when the Sentry was paused for 500 ms
// at the wrong moment, 78 times out of 300. So no wait after a kill proves anything. What
// does: a client that lives to answer has created the task before it reports the PID or
// exits, so `ps` lists whatever it started. Exec therefore leaves a client alive until it
// answers, and a reset waits for those answers.
//
// The zero value is ready to use.
type execClients struct {
	mu        sync.Mutex
	live      map[*execClient]struct{}
	resetting int
	// abandoned is sticky: a client died with its request unanswered, so a command may
	// yet start and no later listing can rule that out.
	abandoned bool
}

// execClient is one running `runsc exec`.
type execClient struct {
	pidFile string
	exited  chan struct{}
}

func newExecClient(pidFile string) *execClient {
	return &execClient{pidFile: pidFile, exited: make(chan struct{})}
}

// pid returns the sandbox PID the client reported, or 0 if it has not. An empty or partial
// file reads as not yet.
func (c *execClient) pid() int {
	data, err := os.ReadFile(c.pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// answer waits until the client has reported a PID or exited. It returns the PID, or 0 for
// a client that exited without one.
func (c *execClient) answer(ctx context.Context) (int, error) {
	for {
		if pid := c.pid(); pid > 0 {
			return pid, nil
		}
		select {
		case <-c.exited:
			return c.pid(), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(pidPoll):
		}
	}
}

// begin registers a client about to start. It refuses during a reset, whose proof would
// not cover what the client starts.
func (s *execClients) begin(id string, c *execClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resetting > 0 {
		return fmt.Errorf("gvisor: sandbox %s is being reset", id)
	}
	if s.live == nil {
		s.live = map[*execClient]struct{}{}
	}
	s.live[c] = struct{}{}
	return nil
}

// end records that a client is gone. abandoned means it died without answering.
func (s *execClients) end(c *execClient, abandoned bool) {
	s.mu.Lock()
	delete(s.live, c)
	s.abandoned = s.abandoned || abandoned
	s.mu.Unlock()
	close(c.exited)
}

// settle holds new execs off until the returned function runs, and returns once every
// client that started earlier has answered. From then on `ps` lists every command they
// started, or the command has already exited. A client that reported its PID may still be
// waiting for its command, and an exec's stream may stay open for as long as a process
// holds its pipes. Neither is waited for: the reset's kill is what ends them.
func (s *execClients) settle(ctx context.Context) (resume func(), err error) {
	s.mu.Lock()
	s.resetting++
	live := slices.Collect(maps.Keys(s.live))
	s.mu.Unlock()
	resume = func() {
		s.mu.Lock()
		s.resetting--
		s.mu.Unlock()
	}
	for _, c := range live {
		if _, err := c.answer(ctx); err != nil {
			return resume, fmt.Errorf("waiting for an exec to start: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.abandoned {
		return resume, errors.New("an exec was abandoned before runsc reported its PID, so its command may yet start")
	}
	return resume, nil
}
