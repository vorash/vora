package network

import (
	"context"
	"sync"
)

// fakeHost is one shared network namespace, as seen by every manager handed
// the same instance: a set of link names with their owner labels and a set of
// nftables table names. Pids start past the kernel's pid_max so holderAlive
// never finds a real process.
type fakeHost struct {
	mu     sync.Mutex
	links  map[string]string
	tables map[string]bool
	pids   int

	claimErr error
	runErr   error
}

type fakeHolder struct{ id int }

func (h fakeHolder) pid() int { return h.id }
func (fakeHolder) kill()      {}

func newFakeHost() *fakeHost {
	return &fakeHost{links: map[string]string{}, tables: map[string]bool{}}
}

func (f *fakeHost) holdNetns() (netnsHolder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pids++
	return fakeHolder{id: 1<<24 + f.pids}, nil
}

func (f *fakeHost) claimLink(_ context.Context, iface, _ string, _ int, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return f.claimErr
	}
	if _, taken := f.links[iface]; taken {
		return errExists
	}
	f.links[iface] = owner
	return nil
}

func (f *fakeHost) releaseLink(_ context.Context, iface, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.links[iface]
	if !ok {
		return nil
	}
	if current != owner {
		return errForeign
	}
	delete(f.links, iface)
	return nil
}

func (f *fakeHost) claimTable(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tables[name] {
		return errExists
	}
	f.tables[name] = true
	return nil
}

func (f *fakeHost) releaseTable(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tables, name)
	return nil
}

func (f *fakeHost) run(context.Context, string, ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runErr
}

func (f *fakeHost) nft(context.Context, int, string) error { return nil }

func (f *fakeHost) enableForwarding() error { return nil }

// vanish drops a link the way the kernel does when the namespace holding its
// peer dies, leaving the name free for anyone.
func (f *fakeHost) vanish(iface string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.links, iface)
}

func (f *fakeHost) owner(iface string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.links[iface]
	return o, ok
}

func (f *fakeHost) hasTable(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tables[name]
}

func (f *fakeHost) linkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.links)
}
