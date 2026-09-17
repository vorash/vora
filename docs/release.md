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

The runner pulls an image when it is not present in containerd, so a GHCR ref is
all a host needs:

```bash
vora sandbox create --image ghcr.io/vorash/vora-python:v0.4.0 \
  --network restricted --allow-domain example.com
```

Two things to know:

- **`/etc/resolv.conf` must exist in the image layers.** Base images do not ship
  one — Docker injects it into every container at start, which never happens for
  a containerd snapshot. The restricted path binds the sandbox's resolver config
  over that path and refuses to start against an image without it, so every
  `images/*/Dockerfile` creates the file and `push-images.sh` verifies it before
  publishing.
- **Package visibility.** GHCR packages are private by default. Either make them
  public (nothing in them is secret) or configure registry credentials on every
  runner; `scripts/push-images.sh` prints the digests to pin in
  `vora-ansible`'s `group_vars/all/all.yml`.

## Pitfalls

- Images are `amd64` only. gVisor/KVM today is `amd64` too; multi-arch is future
  work.
- The workflow never deletes a release or moves a tag. Re-running it uploads
  assets with `--clobber`, which is the intended way to replace a bad build of
  the same version.
- Release assets are not signed yet (`SHA256SUMS` only). Signing the checksums
  with the same key that signs tags is a follow-up.
