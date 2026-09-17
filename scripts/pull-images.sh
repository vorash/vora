#!/usr/bin/env bash
#
# pull-images.sh imports the Vora rootfs images from Docker into the rootless
# containerd image store, where the runner resolves them (ADR 009).
#
# The images are built with Docker by scripts/build-images.sh (`make images`).
# This script only moves them into containerd; it does not build.
#
# Optional args limit which images to import (e.g. ./scripts/pull-images.sh base).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAMESPACE="${VORA_CONTAINERD_NAMESPACE:-vora}"
SOCK="${VORA_CONTAINERD_ADDRESS:-${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/containerd/containerd.sock}"

command -v docker >/dev/null 2>&1 || { echo "error: docker not found" >&2; exit 1; }
command -v ctr >/dev/null 2>&1 || { echo "error: ctr not found" >&2; exit 1; }
if [ ! -S "$SOCK" ]; then
  echo "error: containerd socket not found at $SOCK (run ./scripts/setup-containerd.sh)" >&2
  exit 1
fi

CTR=(ctr --address "$SOCK" --namespace "$NAMESPACE")

if [ $# -gt 0 ]; then
  names=("$@")
else
  names=(base python node)
fi

for name in "${names[@]}"; do
  src="vora/${name}:latest"
  if ! docker image inspect "$src" >/dev/null 2>&1; then
    echo "skip: $src is not in Docker (run 'make images')" >&2
    continue
  fi
  if "${CTR[@]}" images list --quiet 2>/dev/null | grep -q "vora/${name}:latest"; then
    echo "ok: $src present in containerd ($NAMESPACE)"
    continue
  fi

  echo "==> importing $src into containerd ($NAMESPACE)"
  tmp="$(mktemp -t vora-image-XXXXXX.tar)"
  docker save "$src" -o "$tmp"
  "${CTR[@]}" images import "$tmp" >/dev/null
  rm -f "$tmp"
  echo "    imported $src"
done

echo "containerd images in $NAMESPACE:"
"${CTR[@]}" images list | grep -E "vora/|REF" || true
