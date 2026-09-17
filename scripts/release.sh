#!/usr/bin/env bash
#
# release.sh builds the static release artifacts for one version.
#
# Output (dist/ by default):
#   vora                     the CLI
#   vora-runner              the runner daemon
#   vora-init                the in-sandbox agent (must ship next to vora-runner)
#   vora-api                 the control plane
#   SHA256SUMS               checksums of the four binaries
#   vora_<version>_linux_amd64.tar.gz
#                            binaries + systemd units + LICENSE
#
# The binaries are static (CGO_ENABLED=0) because sandbox rootfs images are
# minimal and may be musl-based (Alpine), and they carry the version so
# `vora version`, the runner log and /metrics agree with the release tag.
#
# Usage:
#   scripts/release.sh [--version v0.4.0] [--out dist]
#
# Called by `make release-artifacts` locally and by .github/workflows/release.yml.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

VERSION=""
OUT="dist"
PKG="github.com/vorash/vora/internal/version"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --out) OUT="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,28p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$VERSION" ]]; then
  VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
fi
if [[ "$VERSION" == *-dirty ]]; then
  echo "error: refusing to build release artifacts from a dirty tree ($VERSION)" >&2
  exit 1
fi

GO="${GO:-go}"
command -v "$GO" >/dev/null 2>&1 || { echo "error: $GO not found" >&2; exit 1; }

echo "==> building vora $VERSION (linux/amd64, static)"
rm -rf "$OUT"
mkdir -p "$OUT"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO" build \
  -trimpath \
  -ldflags "-s -w -X ${PKG}.Version=${VERSION}" \
  -o "$OUT/" ./cmd/...

# The agent must be findable next to the runner: resolveInit() looks beside the
# executable before it falls back to PATH.
for bin in vora vora-runner vora-init vora-api; do
  if [[ ! -x "$OUT/$bin" ]]; then
    echo "error: expected binary $OUT/$bin was not built" >&2
    exit 1
  fi
done

echo "==> verifying the built binary reports the version"
reported="$("$OUT/vora" version 2>/dev/null || true)"
if [[ "$reported" != *"$VERSION"* ]]; then
  echo "error: '$OUT/vora version' reported '$reported', want '$VERSION'" >&2
  exit 1
fi

echo "==> packing vora_${VERSION}_linux_amd64.tar.gz"
TARBALL="vora_${VERSION}_linux_amd64.tar.gz"
tar --owner=0 --group=0 --numeric-owner -czf "$OUT/$TARBALL" \
  -C "$OUT" vora vora-runner vora-init vora-api \
  -C "$REPO_ROOT" LICENSE \
  -C "$REPO_ROOT" deploy/systemd/vora-runner.service \
  -C "$REPO_ROOT" deploy/systemd/vora-runner.system.service \
  -C "$REPO_ROOT" deploy/systemd/vora-api.service

echo "==> checksums"
(
  cd "$OUT"
  sha256sum vora vora-runner vora-init vora-api "$TARBALL" > SHA256SUMS
  cat SHA256SUMS
)
echo "release artifacts in $OUT/ for $VERSION"
