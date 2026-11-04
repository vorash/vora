# ADR 027 — Kernel-arbitrated network allocation

- Status: accepted
- Date: 2026-11-04 (`vorash/vora#110`)

## Context

ADR 012 gave every networked sandbox a veth pair whose host end is
`vora-h<n>` with `10.7.x.1/30`, and ADR 016 gave each private-mode tenant
`10.9.<tenantIndex>.0/24`. Both `n` and `tenantIndex` were counters in the
runner's memory, starting at zero, and every runner put its interfaces in the
host network namespace.

The two-runner layout of `docs/release.md` (a non-root bwrap runner next to a
root gVisor runner, both on one host) breaks this. The second runner's first
networked sandbox asks for `vora-h0` and `10.7.0.1/30` while the first runner
holds them. Worse, when that `ip link add` failed, `Create` ran `ip link del
vora-h0` on the way out and cut off the other runner's live sandbox. A
restarted runner had the same power: its reap deleted every interface its
stale state named, even one whose pair had died with the runner and whose name
another runner had since claimed.

The counters also only grew: a runner refused its 16385th networked sandbox
and its 257th private-mode tenant, however many had been destroyed.

## Decision

**1. Names are claimed from the kernel, not counted.** Runners sharing a
network namespace do not coordinate, so the kernel is the arbiter. A veth
index is claimed with one `ip link add vora-h<n> ... type veth peer name
vora-s<n> netns <holder>`; EEXIST means a runner or a leftover holds that
name, and the claim moves to the next index. The restricted/full `/30`
derives from the claimed index, so a unique name is a unique subnet. A
private-mode tenant `/24` is claimed with `nft create table inet
vora_tenant_<t>`, an empty table that exists only to be owned; host slots
inside an owned `/24` are the runner's to allocate in memory.

**2. Every claim carries its owner's label, and a release checks it.** A host
veth carries its sandbox id as its alias; a tenant table holds a hookless
chain named with the runner's owner token, a ULID minted once into
`<state>/netns/owner`. A release deletes a link or table only when the label
matches. This holds for a failed create, `Destroy` and the startup reap, so a
runner can never remove what it did not claim, including a name its stale
state records that another runner claimed after a reboot wiped the kernel
objects but not the state files. The alias is set with `ip link set` right
after the claim: the kernel applies `IFLA_IFALIAS` only on `RTM_SETLINK`, so
an alias passed to `ip link add` is dropped without an error. The chain is
created in the same `nft -f` transaction as the table.

**3. The peer is born inside the sandbox namespace.** The holder is observed
to have left the runner's namespace before the claim, and `netns <pid>` on the
peer puts it there in the same operation, so no `vora-s<n>` is stranded in the
shared namespace.

**4. Indexes and tenant claims are recycled.** The index hint wraps at 16384;
a tenant's table is released with its last sandbox on that runner and its
name is recorded in the sandbox state so a reap can release it too. A create
fails only when every index, or every tenant `/24`, is live on the host.

**5. The kernel-facing calls sit behind a `host` interface** (`ip`, `nft`,
`nsenter`, the holder process, `ip_forward`) so the allocation and release
logic is tested against one in-memory host shared by several managers,
without root.

## Alternatives

- **A per-runner interface prefix or index range from configuration.** Explicit
  and free of runtime logic, but the single-runner default must stay
  `vora-h`/`10.7.0.0/16`, so the operator of a second runner has to remember to
  set it, and a forgotten setting reproduces the bug silently. Kernel claims
  are safe by default.
- **A shared lock directory on the filesystem.** Needs a path both a root and a
  non-root runner can write, and stale entries after a crash; the kernel
  already holds the authoritative state.

## Consequences

- One runner on a host behaves as before: a fresh runner's first sandbox gets
  `vora-h0` and `10.7.0.1/30`, its first private tenant `10.9.0.0/24`.
- Each `Create` costs one `ip link add` per index another runner holds between
  this runner's hint and the next free index; each release costs one `ip -j
  link show` for the alias check.
- Interfaces created by a runner older than this decision carry no alias, so an
  upgraded runner's reap leaves them in place (and logs it); they still die
  with their holder, which the reap kills.
- A crash between `ip link add` and the alias write leaves an unlabelled link
  that no release will delete; it still dies with its holder. A crash between
  the claim and the state write leaks that index on this boot. Each window is
  one exec.
- Conntrack entries from a masqueraded sandbox can outlive it and meet a later
  sandbox at the same `/30`; this predates the change and is unchanged by it.
- `scripts/test-system-unit.sh` starts a second runner unit and checks that
  sandboxes on both work at once, that the same tenant lands in different
  `/24`s, and that destroying or restarting one runner leaves the other's
  sandboxes intact.

## Refs

- Issue vorash/vora#110; ADR 012 (egress gateway), ADR 016 (network modes),
  ADR 017 (gVisor restricted egress).
- `internal/network` (`allocate`, `release`, `host`), `scripts/test-system-unit.sh`.
