#!/usr/bin/env bash
#
# install-system.sh installs the release binaries and the system units on a
# host, for the first hand-converged machine before vora-ansible exists there.
# It is the manual equivalent of that repo's `vora_runner` (and `vora_api`)
# roles; once the playbook manages the host, stop using this script.
#
# Idempotent, and it never overwrites an existing /etc/vora/*.env: those hold
# the DSNs, join token and master key, and belong to the operator.
#
# Usage:
#   sudo scripts/install-system.sh --from dist [--api]
#
# Prerequisites it does not install (that is the runtime role's job):
#   bubblewrap, iproute2, nftables, util-linux, a dedicated vora user with
#   lingering systemd --user manager and rootless containerd; optional runsc.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FROM="$REPO_ROOT/dist"
WITH_API=0
BIN_DIR=/usr/local/bin
ETC=/etc/vora
STATE=/var/lib/vora

while [[ $# -gt 0 ]]; do
  case "$1" in
    --from) FROM="${2:-}"; shift 2 ;;
    --api) WITH_API=1; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || { echo "error: run as root (sudo)" >&2; exit 1; }
if ! id -u vora >/dev/null 2>&1; then
  useradd --system --user-group --home-dir /var/lib/vora --shell /usr/bin/nologin vora
fi
if ! getent group vora-api-tls >/dev/null; then
  groupadd --system vora-api-tls
fi
loginctl enable-linger vora
vora_uid="$(id -u vora)"
containerd_socket="/run/user/$vora_uid/containerd-rootless/containerd.sock"
if [[ ! -S "$containerd_socket" ]]; then
  echo "error: rootless containerd for vora is required at $containerd_socket; refusing host-root containerd access" >&2
  exit 1
fi
if [[ -d "$STATE" && "$(stat -c %U "$STATE")" != vora && -n "$(ls -A "$STATE")" ]]; then
  echo "error: existing $STATE is not owned by vora; migrate its ownership and rootless containerd before installing" >&2
  exit 1
fi
for bin in vora vora-runner vora-init; do
  [[ -x "$FROM/$bin" ]] || { echo "error: $FROM/$bin missing (run scripts/release.sh first)" >&2; exit 1; }
done
if [[ "$WITH_API" -eq 1 && ! -x "$FROM/vora-api" ]]; then
  echo "error: $FROM/vora-api missing" >&2; exit 1
fi

if [[ "$(< /proc/sys/net/ipv4/ip_forward)" != 1 ]]; then
  echo "warning: NetworkFull/NetworkPrivate need net.ipv4.ip_forward=1 set by the host administrator (runner cannot change protected kernel tunables)" >&2
fi

echo "==> installing binaries into $BIN_DIR"
install -d -m 0755 "$BIN_DIR"
for bin in vora vora-runner vora-init; do
  install -m 0755 "$FROM/$bin" "$BIN_DIR/$bin"
done
if [[ "$WITH_API" -eq 1 ]]; then
  install -m 0755 "$FROM/vora-api" "$BIN_DIR/vora-api"
fi

echo "==> creating $ETC and $STATE"
install -d -m 0755 "$ETC"
install -d -m 0750 -o vora -g vora "$STATE"
install -d -m 0755 "$ETC/tls"


# Environment files: created once, never overwritten. The units require them,
# so an unconfigured host fails to start instead of running with defaults.
if [[ "$(stat -c %a "$ETC")" != 755 || "$(stat -c %a "$ETC/tls")" != 755 ]]; then
  echo "error: $ETC and $ETC/tls must be traversable (0755); private keys must be protected by their own file modes" >&2
  exit 1
fi
if [[ ! -f "$ETC/runner.env" ]]; then
  echo "==> writing $ETC/runner.env (template; edit before starting)"
  cat >"$ETC/runner.env" <<EOF
# Vora runner configuration. See vora/docs/adr and README.
# Audit trail: PostgreSQL/Supabase DSN, or unset for per-sandbox JSONL.
#VORA_AUDIT_DSN=postgres://user:pass@host:5432/vora
# Image store: the system containerd socket (the rootless socket is dev-only).
VORA_CONTAINERD_ADDRESS=$containerd_socket
VORA_CONTAINERD_NAMESPACE=vora
VORA_IMAGES_MODE=auto
# Metrics are unauthenticated: keep them on loopback.
VORA_METRICS_ADDR=127.0.0.1:9101
# Registration with vora-api (runner dials out):
#VORA_RUNNER_ADDR=0.0.0.0:9200
#VORA_RUNNER_ADVERTISE_ADDR=vora-mini:9200
#VORA_API_GRPC=api.example.com:9301
#VORA_API_JOIN_TOKEN=
# TLS for the API->runner channel (required for credential brokering):
#VORA_RUNNER_TLS_CERT=/etc/vora/tls/runner.crt
#VORA_RUNNER_TLS_KEY=/etc/vora/tls/runner.key
# Set private key ownership to vora:vora with mode 0600.
#VORA_RUNNER_TLS_CA=/etc/vora/tls/ca.crt
#VORA_API_REGISTRY_TLS_CERT=/etc/vora/tls/registry-client.crt
#VORA_API_REGISTRY_TLS_KEY=/etc/vora/tls/registry-client.key
# Set the client key ownership to vora:vora with mode 0600.
#VORA_API_REGISTRY_TLS_CA=/etc/vora/tls/ca.crt
EOF
  chmod 0600 "$ETC/runner.env"
else
  echo "==> keeping existing $ETC/runner.env"
fi

if [[ "$WITH_API" -eq 1 && ! -f "$ETC/api.env" ]]; then
  echo "==> writing $ETC/api.env (template; edit before starting)"
  cat >"$ETC/api.env" <<'EOF'
# vora-api configuration. Keep this file 0600 root:root.
VORA_API_HTTP_ADDR=127.0.0.1:9300
VORA_API_GRPC_ADDR=127.0.0.1:9301
# Control-plane store (PostgreSQL/Supabase) and audit DSN:
#VORA_API_DSN=postgres://user:pass@host:5432/vora
#VORA_AUDIT_DSN=postgres://user:pass@host:5432/vora
# Runner registration and the first admin key:
#VORA_API_JOIN_TOKEN=
#VORA_API_BOOTSTRAP_KEY=
# Credential brokering (>=32 bytes). Required for `vora secrets`:
#VORA_MASTER_KEY=
# Public HTTP TLS (reverse proxy or certbot in front is also fine):
#VORA_API_HTTP_TLS_CERT=/etc/vora/tls/api.crt
#VORA_API_HTTP_TLS_KEY=/etc/vora/tls/api.key
# Registry gRPC TLS:
#VORA_API_REGISTRY_TLS_CERT=/etc/vora/tls/registry.crt
#VORA_API_REGISTRY_TLS_KEY=/etc/vora/tls/registry.key
# Set API private key group to vora-api-tls with mode 0640.
#VORA_API_REGISTRY_TLS_CA=/etc/vora/tls/ca.crt
EOF
  chmod 0600 "$ETC/api.env"
elif [[ "$WITH_API" -eq 1 ]]; then
  echo "==> keeping existing $ETC/api.env"
fi

echo "==> installing systemd units"
install -m 0644 "$REPO_ROOT/deploy/systemd/vora-runner.system.service" /etc/systemd/system/vora-runner.service
if [[ "$WITH_API" -eq 1 ]]; then
  install -m 0644 "$REPO_ROOT/deploy/systemd/vora-api.service" /etc/systemd/system/vora-api.service
fi
systemctl daemon-reload

echo "==> enabling and starting services"
systemctl enable --now vora-runner.service
if [[ "$WITH_API" -eq 1 ]]; then
  systemctl enable --now vora-api.service
fi

cat <<EOF

installed $( "$BIN_DIR/vora" version 2>/dev/null || echo "vora (version unknown)" )

  socket   /var/run/vora/runner.sock        (systemctl status vora-runner)
  state    $STATE
  config   $ETC/runner.env$( [[ "$WITH_API" -eq 1 ]] && echo " and $ETC/api.env" )
  metrics  http://127.0.0.1:9101/metrics

Next: fill in the env files, then:
  sudo systemctl restart vora-runner$( [[ "$WITH_API" -eq 1 ]] && echo " vora-api" )
  sudo -u root vora sandbox list
EOF
