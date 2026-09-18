# Releases and host artifacts

Status: v0.4 · applies to `vora` releases cut by
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
  sudo ctr --namespace vora images pull \
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

- **The state directory must stay `root:root`.** The units use
  `StateDirectory=vora`, which systemd refuses to use for a service whose
  directory is owned by someone else; if `/var/lib/vora` is chowned to a `vora`
  user, the service fails with status `238/STATE_DIRECTORY` (`EPERM`) and
  crash-loops. `scripts/install-system.sh` creates it `root:root` and warns if it
  finds it otherwise.
- **`vora-api` waits for its store, and the network can take a while after a
  reboot.** The unit retries indefinitely with a 5-second backoff, because a
  Supabase DSN can need ~a minute of IPv6 route convergence after boot and the
  default systemd start limit (5 starts in 10s) would give up and leave the API
  dead rather than late.
- Images are `amd64` only. gVisor/KVM today is `amd64` too; multi-arch is future
  work.
- The workflow never deletes a release or moves a tag. Re-running it uploads
  assets with `--clobber`, which is the intended way to replace a bad build of
  the same version.
- Release assets are not signed yet (`SHA256SUMS` only). Signing the checksums
  with the same key that signs tags is a follow-up.
