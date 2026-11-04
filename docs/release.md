# Releases and host artifacts

Status: v0.7 · applies to `vora` releases cut by
[`.github/workflows/release.yml`](../.github/workflows/release.yml).

## What a release contains

| Artifact | Purpose |
| -------- | ------- |
| `vora`, `vora-runner`, `vora-init`, `vora-api` | static `linux/amd64` binaries (`CGO_ENABLED=0`, so they also run against musl rootfs images) |
| `SHA256SUMS` | checksums for the four binaries and the tarball |
| `vora_<version>_linux_amd64.tar.gz` | the binaries plus `LICENSE` and the three systemd units |
| `ghcr.io/<owner>/vora-base\|vora-python\|vora-node:<version>` | sandbox rootfs images, pulled by reference by the runner (ADR 009) |

`vora` reports the version baked in at build time, so `vora version`, the
runner's startup log and `/metrics` agree with the release tag.

## Cutting a release

Either push a version tag (the workflow runs on `v*`), or dispatch it for an
existing tag:

```bash
gh workflow run release.yml -f version=v0.4.1          # binaries + images
gh workflow run release.yml -f version=v0.4.0 -f skip_images=true
```

Locally, the same scripts produce the same artifacts:

```bash
make release-artifacts                 # dist/ for the current git describe
make push-images TAG=v0.4.0 LOAD=1      # build images without pushing
```

`scripts/release.sh` refuses to build from a dirty tree, and
`scripts/push-images.sh` refuses to publish an image whose layers lack
`/etc/resolv.conf` (see below). The images job pushes only the version tag by
default; pass `--latest` (or `LATEST=1`) to also move `:latest`, so a rebuild of
an older tag cannot drag the convenience tag backwards.

## Installing on a host

Before `vora-ansible` manages the host, converge it by hand:

```bash
curl -fsSLO https://github.com/vorash/vora/releases/download/v0.4.0/vora_v0.4.0_linux_amd64.tar.gz
curl -fsSLO https://github.com/vorash/vora/releases/download/v0.4.0/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf vora_v0.4.0_linux_amd64.tar.gz
sudo ./scripts/install-system.sh --from . --api   # (from a repo checkout)
```

`install-system.sh` installs the binaries into `/usr/local/bin`, the system units
into `/etc/systemd/system`, creates `/etc/vora/*.env` templates (never
overwriting existing ones) and enables the services. The units **require**
those env files, so a host that is not configured fails to start rather than
running with defaults.

Before installation, provision the `vora` system user (home `/var/lib/vora`)
with subordinate uid/gid ranges and a lingering `systemd --user` manager, then
run rootless containerd as `vora` (`containerd-rootless-setuptool.sh install`
from nerdctl's `extras/rootless`), which listens at
`/run/user/<vora-uid>/containerd/containerd.sock`. The installer refuses a
missing rootless socket rather than granting `vora` access to the host-root
containerd daemon. `scripts/test-system-unit.sh setup` is the reference for
these steps; CI runs it on Ubuntu 26.04. For an existing root deployment, stop
the runner,
migrate `/var/lib/vora` to `vora:vora` and the images to the rootless daemon,
and update any preserved `/etc/vora/runner.env`; the installer does not copy
existing secrets or state automatically. Runner TLS keys must be readable by
`vora` (mode 0600); API TLS keys must be group-readable by `vora-api-tls`
(mode 0640). Keep `/etc/vora/*.env` root-only (systemd reads them before
switching users). The directories `/etc/vora` and `/etc/vora/tls` must be
traversable, but the key files must remain private.

When migrating a root runner that used gVisor, clear its `runsc` state before
the recursive chown (#104). `runsc` bind-mounts a network namespace at
`$root/null-netns`, where `$root` is the runner's `VORA_GVISOR_ROOT`, and a
runner whose gVisor root was under `/var/lib/vora` stacked one such mount per
start; `chown -R` fails on these nsfs inodes. With the runner stopped, unmount
them until none is visible
(`while mountpoint -q "$root/null-netns"; do umount "$root/null-netns" || break; done`),
stopping when `umount` fails: a mount made before the `/var/lib/vora` volume
was mounted stays hidden underneath it and cannot be reached (it is gone after
the next reboot). Then remove the gVisor state directory. The non-root unit
does not offer gVisor, so nothing there is needed.

To offer gVisor on such a host, run a second runner as root with
`VORA_BACKENDS=gvisor`, and set `VORA_BACKENDS=bwrap` on the non-root runner.
vora-api places each sandbox on a runner that advertises its backend, so bwrap
work stays on the non-root runner, and a listed backend that fails to start
stops its runner instead of leaving it registered without that backend. The
two runners must not share state: give the root runner its own
`VORA_STATE_DIR`, `VORA_SOCKET`, `VORA_METRICS_ADDR`, `VORA_GVISOR_ROOT`,
`VORA_RUNNER_ADDR` and client certificate. Both runners allocate sandbox
interfaces and subnets in the host network namespace and claim them from the
kernel ([ADR 027](adr/027-kernel-arbitrated-network-allocation.md)), so
networked sandboxes on both runners coexist; the `System unit` workflow
checks this with a second runner unit.

`vora-init` is installed next to `vora-runner` because `resolveInit()` looks
beside the executable before falling back to `PATH`.

## Images and the restricted path

The runner pulls an image when it is not present in containerd. These packages are
private and the runner cannot authenticate to GHCR, so the order that works is:
pre-pull the ref into the runner's containerd namespace, then reference it from a
sandbox:

```bash
vora sandbox create \
  --image ghcr.io/vorash/vora-python@sha256:<digest from the release> \
  --network restricted --allow-domain example.com
```

The example pins a digest rather than `:v0.4.2`: a pre-pulled private image must
be referenced as what was pulled, and the digest is what the release pins anyway.

Two things to know:

- **`/etc/resolv.conf` must exist in the image layers.** Base images do not ship
  one — Docker injects it into every container at start, which never happens for
  a containerd snapshot. The restricted path binds the sandbox's resolver config
  over that path and refuses to start against an image without it, so every
  `images/*/Dockerfile` creates the file and `push-images.sh` verifies it before
  publishing.
- **Package visibility: the GHCR packages are private, so pre-pull them.** The
  runner pulls through containerd's native client, which has no credential hook,
  so it cannot authenticate to GHCR. Pull each digest-pinned ref into the
  namespace the runner uses and it never has to:

  ```bash
  # Run as vora, against its rootless socket (not /run/containerd/containerd.sock):
  ctr --address /run/user/<vora-uid>/containerd/containerd.sock \
    --namespace vora images pull \
    --user <github-user>:<PAT with read:packages> \
    ghcr.io/vorash/vora-python@sha256:<digest from the release>
  ```

  The pull stores the manifest, config and layers in containerd's content store;
  the runner then unpacks them into its own snapshotter locally, with no registry
  contact at all (its `VORA_CONTAINERD_SNAPSHOTTER`, default `native`). Do **not**
  add `--snapshotter` to the pull: on ctr 2.x it fails with `unable to initialize
  unpacker: no unpack platforms defined` — reproduced on 2.3.5, with and without
  `--platform`, and via `CONTAINERD_SNAPSHOTTER` — and it buys nothing, since the
  runner unpacks for itself. The namespace label
  `containerd.io/defaults/snapshotter` is read by the containerd *Go client* when
  no snapshotter is given, not by `ctr images pull`, so it does not redirect this
  either.

  The consequence of a private registry is that a ref nobody pre-pulled fails at
  sandbox start. The message misleads: it reads `images: image not found in cache:
  ghcr.io/... (run 'make images')`, but for a registry ref it means the pull
  failed (no credentials, wrong digest), not that a local build is missing.

  `scripts/push-images.sh` prints the digests to pin in `vora-ansible`'s
  `group_vars/all/all.yml`; pin them per release, because Docker builds are not
  byte-reproducible even from an unchanged Dockerfile.

## Pitfalls

- **Chown the state directory before rootless containerd uses it.** With
  `User=vora`, `StateDirectory=vora` makes systemd chown `/var/lib/vora`
  recursively whenever its top directory is not already owned by `vora:vora`.
  Rootless containerd keeps its store under the same tree
  (`/var/lib/vora/.local/share/containerd`), where files of non-root image
  users belong to `vora`'s subordinate ids, and a recursive chown rewrites
  them. Chown the top directory (and migrate old root-owned state) before
  setting up rootless containerd or starting the unit.
- **The runner state directory must belong to `vora:vora`.** The system
  runner uses `User=vora`, delegated user cgroups and only CAP_SYS_ADMIN,
  CAP_NET_ADMIN (namespace/network setup) and CAP_NET_BIND_SERVICE (the
  restricted-mode resolver on port 53). These host capabilities are still
  powerful; this is not a fully rootless runner. The unit confines filesystem
  writes to its state and runtime directories. Never give `vora` access to the
  host-root containerd socket: it would restore host-root authority.
  `NetworkFull`/`NetworkPrivate` also require the host administrator to set
  `net.ipv4.ip_forward=1` before starting Vora; the hardened runner cannot
  change protected kernel tunables itself.
- **`vora-api` waits for its store, and the network can take a while after a
  reboot.** The unit retries indefinitely with a 5-second backoff, because a
  Supabase DSN can need ~a minute of IPv6 route convergence after boot and the
  default systemd start limit (5 starts in 10s) would give up and leave the API
  dead rather than late.
- Images are `amd64` only. gVisor/KVM today is `amd64` too; multi-arch is future
  work.
- **gVisor's default platform is `kvm`, which needs `/dev/kvm`.** A host without
  it — a mini-PC, or a VM without nested virtualisation — runs the `systrap`
  platform instead: set `VORA_GVISOR_PLATFORM=systrap`. runsc may fall back on its
  own, but the setting is what makes it deterministic; the rest of the runner is
  unchanged (`Delegate=yes` on the unit is enough for the cgroup subtree, verified
  with gVisor as a system service on a KVM-less mini-PC).
- The workflow never deletes a release or moves a tag. Re-running it uploads
  assets with `--clobber`, which is the intended way to replace a bad build of
  the same version.
- Release assets are not signed yet (`SHA256SUMS` only). Signing the checksums
  with the same key that signs tags is a follow-up.
