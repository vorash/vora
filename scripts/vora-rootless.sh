#!/usr/bin/env bash
#
# Run vora-runner inside a persistent rootlesskit user namespace.
#
# gVisor "Method 2" requires the runner (and therefore runsc) to execute as
# UID 0 inside a user namespace; `runsc --rootless` alone does not support
# `create`. This wrapper mirrors containerd-rootless.sh: rootlesskit owns the
# user namespace, and vora-runner runs inside it.
#
# Two things must be handled inside the namespace:
#   * the runner socket must be visible on the host, so we bind-mount the host
#     runtime directory to /run/vora;
#   * the mapped UID is 0, so we pin the config to the user's state dir and the
#     user systemd mode (otherwise config.Default() picks root paths).
set -euo pipefail

RUNNER_BIN="${VORA_RUNNER_BIN:-$HOME/.local/bin/vora-runner}"
RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
STATE_DIR="${VORA_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/vora}"
SOCKET_DIR="$RUNTIME_DIR/vora"

if [ "${_VORA_ROOTLESS_CHILD:-0}" != "1" ]; then
    if [ "$(id -u)" = "0" ]; then
        echo "error: do not run this as root (run it as the unprivileged user)" >&2
        exit 1
    fi
    export _VORA_ROOTLESS_CHILD=1
    exec rootlesskit \
        --state-dir="${VORA_ROOTLESSKIT_STATE_DIR:-$RUNTIME_DIR/vora-rootlesskit}" \
        --net=slirp4netns --mtu=65520 --disable-host-loopback \
        --copy-up=/etc --copy-up=/run --copy-up=/var/lib \
        --propagation=rslave \
        "$0" "$@"
fi

# Inside the user namespace.
mkdir -p "$SOCKET_DIR" /run/vora
if ! mountpoint -q /run/vora 2>/dev/null; then
    mount --bind "$SOCKET_DIR" /run/vora
fi

# The rootless containerd bind-mounts its data dir to /var/lib/containerd inside
# its own namespace, so it reports snapshot rootfs paths as
# /var/lib/containerd/... . Do the same here, otherwise the runner cannot bind
# those rootfs paths into sandboxes.
XDG_DATA_HOME="${XDG_DATA_HOME:-$HOME/.local/share}"
rm -f /var/lib/containerd
mkdir -p "$XDG_DATA_HOME/containerd" /var/lib/containerd
if ! mountpoint -q /var/lib/containerd 2>/dev/null; then
    mount --bind "$XDG_DATA_HOME/containerd" /var/lib/containerd
fi

export VORA_STATE_DIR="$STATE_DIR"
export VORA_SYSTEMD_MODE=user

exec "$RUNNER_BIN" --socket /run/vora/runner.sock "$@"
