#!/usr/bin/env bash
#
# setup-containerd.sh ensures a rootless containerd is available as the Vora
# OCI image store (ADR 009). It is idempotent: if the socket already exists it
# only verifies the native snapshotter, otherwise it runs the upstream
# containerd-rootless-setuptool.sh.
set -euo pipefail

NAMESPACE="${VORA_CONTAINERD_NAMESPACE:-vora}"
SOCK="${VORA_CONTAINERD_ADDRESS:-${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/containerd/containerd.sock}"

if [ "$(id -u)" = "0" ]; then
  echo "error: run as the unprivileged user (rootless containerd)" >&2
  exit 1
fi

if [ ! -S "$SOCK" ]; then
  echo "containerd socket not found at $SOCK"
  if command -v containerd-rootless-setuptool.sh >/dev/null 2>&1; then
    echo "==> installing rootless containerd user service"
    containerd-rootless-setuptool.sh install
  else
    echo "error: install containerd and nerdctl, then re-run this script" >&2
    exit 1
  fi
fi

echo "==> containerd socket: $SOCK (namespace: $NAMESPACE)"
if command -v nerdctl >/dev/null 2>&1; then
  nerdctl -n "$NAMESPACE" info >/dev/null
  echo "==> containerd reachable; snapshotters:"
  nerdctl -n "$NAMESPACE" info 2>/dev/null | sed -n '/Storage:/p' || true
else
  echo "warning: nerdctl not found; skipping the connectivity check" >&2
fi

echo "containerd ready. Next: ./scripts/pull-images.sh"
