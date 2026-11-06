package api

import (
	"sync"
)

// runnerConns keeps one client per runner for the pool routes, so a run doesn't
// pay for a new connection and streams share the runner's. The mutex guards the
// map only: callers use a client after it is released, so no RPC or stream ever
// holds it. The clients stay open until the runner moves or the server closes,
// so callers must not close what they get.
type runnerConns struct {
	mu    sync.Mutex
	conns map[string]runnerConn
}

type runnerConn struct {
	address  string
	tunneled bool
	client   RunnerClient
}

// get returns the runner's client. A runner that registered with a new address
// gets a new one, and streams on the old connection end, as that runner has gone.
func (c *runnerConns) get(r Runner, dial func(Runner) (RunnerClient, error)) (RunnerClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.conns[r.ID]
	if ok && cur.address == r.Address && cur.tunneled == r.Tunneled {
		return cur.client, nil
	}
	client, err := dial(r)
	if err != nil {
		return nil, err
	}
	if ok {
		_ = cur.client.Close()
	}
	if c.conns == nil {
		c.conns = make(map[string]runnerConn)
	}
	c.conns[r.ID] = runnerConn{address: r.Address, tunneled: r.Tunneled, client: client}
	return client, nil
}

func (c *runnerConns) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, conn := range c.conns {
		_ = conn.client.Close()
		delete(c.conns, id)
	}
}
