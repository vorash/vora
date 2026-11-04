package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// errExists reports that a kernel object name is already taken, by this
// runner, another runner in the same network namespace, or a leftover.
var errExists = errors.New("network: name already in use")

// errForeign reports that a link or table exists but carries another owner's
// label, so the caller must leave it alone.
var errForeign = errors.New("network: object owned by someone else")

// host is the kernel surface the manager allocates through. Runners sharing a
// network namespace do not coordinate with each other, so the kernel is the
// arbiter: a claim is an atomic create that fails with errExists when the name
// is taken, and a release deletes a link only when it carries the caller's
// owner label. Tests share one in-memory host between several managers.
type host interface {
	// holdNetns starts a process whose only job is to keep a fresh network
	// namespace alive.
	holdNetns() (netnsHolder, error)
	// claimLink creates the veth pair iface/peer with peer inside the network
	// namespace of pid, then labels iface with owner.
	claimLink(ctx context.Context, iface, peer string, pid int, owner string) error
	// releaseLink deletes iface when its label is owner. A missing link is not
	// an error; a link with another label is errForeign.
	releaseLink(ctx context.Context, iface, owner string) error
	// claimTable creates an empty inet nftables table labelled with owner, in
	// one transaction.
	claimTable(ctx context.Context, name, owner string) error
	// releaseTable deletes the table when its label is owner, with the same
	// contract as releaseLink.
	releaseTable(ctx context.Context, name, owner string) error
	// run executes a command in the runner's network namespace.
	run(ctx context.Context, name string, args ...string) error
	// nft loads a ruleset in the network namespace of pid, or the runner's
	// own when pid is 0.
	nft(ctx context.Context, pid int, rules string) error
	// enableForwarding turns on IPv4 forwarding in the runner's network
	// namespace, as NetworkFull (NAT to the uplink) and NetworkPrivate
	// (routing between a tenant's sandboxes) need.
	enableForwarding() error
}

type netnsHolder interface {
	pid() int
	kill()
}

// shellHost drives the kernel through ip, nft and nsenter.
type shellHost struct{}

type processHolder struct{ cmd *exec.Cmd }

func (h *processHolder) pid() int { return h.cmd.Process.Pid }

func (h *processHolder) kill() {
	_ = h.cmd.Process.Kill()
	_, _ = h.cmd.Process.Wait()
}

func (shellHost) holdNetns() (netnsHolder, error) {
	cmd := exec.Command("unshare", "--net", holderCmd, holderArg)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := &processHolder{cmd: cmd}
	if err := waitNetns(cmd.Process.Pid); err != nil {
		h.kill()
		return nil, err
	}
	return h, nil
}

// waitNetns returns once pid has left the runner's network namespace. Right
// after Start the holder may not have called unshare yet, and a veth peer
// handed to its pid at that moment would land in the runner's namespace.
func waitNetns(pid int) error {
	own, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid))
		if err == nil && target != own {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("network: netns holder %d did not enter a new network namespace", pid)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (shellHost) claimLink(ctx context.Context, iface, peer string, pid int, owner string) error {
	if err := asExists(run(ctx, "ip", "link", "add", iface, "type", "veth",
		"peer", "name", peer, "netns", strconv.Itoa(pid))); err != nil {
		return err
	}
	// The kernel applies IFLA_IFALIAS only on RTM_SETLINK, so an alias given
	// to `ip link add` is dropped silently; the label needs its own call.
	if err := run(ctx, "ip", "link", "set", "dev", iface, "alias", owner); err != nil {
		_ = run(ctx, "ip", "link", "del", iface)
		return err
	}
	return nil
}

func (shellHost) releaseLink(ctx context.Context, iface, owner string) error {
	out, err := exec.CommandContext(ctx, "ip", "-j", "link", "show", "dev", iface).Output()
	if err != nil {
		return nil
	}
	var links []struct {
		Alias string `json:"ifalias"`
	}
	if err := json.Unmarshal(out, &links); err != nil || len(links) != 1 {
		return fmt.Errorf("network: ip -j link show %s: unexpected output %q", iface, out)
	}
	if links[0].Alias != owner {
		return errForeign
	}
	return run(ctx, "ip", "link", "del", iface)
}

// The owner label is a regular chain, which has no hook and so no effect on
// traffic. A chain works on every nftables version; a table comment does not.
func (h shellHost) claimTable(ctx context.Context, name, owner string) error {
	return asExists(h.nft(ctx, 0, fmt.Sprintf("create table inet %s\nadd chain inet %s %s\n", name, name, owner)))
}

func (shellHost) releaseTable(ctx context.Context, name, owner string) error {
	out, err := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", name).Output()
	if err != nil {
		return nil
	}
	var listing struct {
		Objects []struct {
			Chain *struct {
				Name string `json:"name"`
			} `json:"chain"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return fmt.Errorf("network: nft -j list table %s: unexpected output %q", name, out)
	}
	for _, o := range listing.Objects {
		if o.Chain != nil && o.Chain.Name == owner {
			return run(ctx, "nft", "delete", "table", "inet", name)
		}
	}
	return errForeign
}

func (shellHost) run(ctx context.Context, name string, args ...string) error {
	return run(ctx, name, args...)
}

func (shellHost) nft(ctx context.Context, pid int, rules string) error {
	var cmd *exec.Cmd
	if pid == 0 {
		cmd = exec.CommandContext(ctx, "nft", "-f", "-")
	} else {
		cmd = exec.CommandContext(ctx, "nsenter", "-t", strconv.Itoa(pid), "-n", "nft", "-f", "-")
	}
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("network: nft: %w: %s", err, out)
	}
	return nil
}

func (shellHost) enableForwarding() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("network: enable ip_forward: %w", err)
	}
	return nil
}

// asExists maps the EEXIST both ip and nft report as "File exists" to
// errExists, so callers can tell a taken name from a real failure.
func asExists(err error) error {
	if err != nil && strings.Contains(err.Error(), "File exists") {
		return fmt.Errorf("%w: %v", errExists, err)
	}
	return err
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("network: %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
