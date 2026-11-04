#!/usr/bin/env bash
#
# test-system-unit.sh boots the shipped system runner unit
# (deploy/systemd/vora-runner.system.service) the way a production host runs
# it, and drives sandboxes through it. It is the end-to-end check for the
# non-root topology (vorash/vora#102): a dedicated vora user with ambient
# CAP_SYS_ADMIN/CAP_NET_ADMIN, a lingering user manager, rootless containerd,
# and /var/lib/vora on XFS with prjquota, as on vora-mini.
#
# It changes the host (packages, a vora user, a loop mount, systemd units), so
# it is meant for a disposable CI VM only.
#
# Usage:
#   sudo scripts/test-system-unit.sh setup     # host prerequisites (runtime role)
#   sudo scripts/test-system-unit.sh install   # scripts/install-system.sh
#   sudo scripts/test-system-unit.sh test      # sandboxes through the unit
#   sudo scripts/test-system-unit.sh diagnose  # dump logs after a failure
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE=/var/lib/vora
SOCKET=/run/vora/runner.sock

# The images released with v0.9.1, digest-pinned so the check is reproducible.
IMAGES=(
  "ghcr.io/vorash/vora-base:v0.9.1@sha256:19d31922d418c7a2fdb58ba1f062816d765ece5bfa11f6ae30acb51c3f30848d"
  "ghcr.io/vorash/vora-node:v0.9.1@sha256:200aed9b8b703abfd47151bb0409a61a81e320d162e8690b4f81cd5fdb0ae160"
  "ghcr.io/vorash/vora-python:v0.9.1@sha256:5880d6176322e658c98f3f63b1b80fa614e70ac5708bd022da85bfb9b3de3431"
)
NETWORKS=(none restricted full private)

# nerdctl's rootless containerd scripts (Ubuntu packages containerd and
# rootlesskit but not these), pinned to v2.4.0.
NERDCTL_COMMIT=bbc533ce69509508731f7993ef1cc078bcb99245
ROOTLESS_SH_SHA256=7ec0fe765e13d3a0d93fa3c6cf2a00a82e9de41c634f4fb8c7b2e8edbf8c4f7a
SETUPTOOL_SHA256=caa27e0cc1cdd21c8ab1cbd63c0875eda1e4be15852b9c6a907e481c1d054d5e

[[ "$(id -u)" -eq 0 ]] || { echo "error: run as root" >&2; exit 1; }

vora_uid() { id -u vora; }

# as_vora runs a command as vora inside its user session, the way the runner
# unit reaches the user manager and the rootless containerd.
as_vora() {
  local uid
  uid="$(vora_uid)"
  (cd / && sudo -u vora env -i HOME="$STATE" USER=vora LOGNAME=vora XDG_RUNTIME_DIR="/run/user/$uid" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
    PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin "$@")
}

# A second runner on the same host, as vora, with its own state, socket,
# metrics port and runtime directory: the two-runner layout docs/release.md
# describes, both allocating in the host network namespace (vorash/vora#110).
SOCKET_B=/run/vora-b/runner.sock

# stdin from /dev/null: `vora exec` forwards piped stdin to the sandbox.
# RUNNER_SOCKET selects the second runner.
cli() { timeout 180 vora --socket "${RUNNER_SOCKET:-$SOCKET}" "$@" </dev/null; }

fetch_pinned() {
  local name="$1" sum="$2"
  if echo "$sum  /usr/local/bin/$name" | sha256sum --check --quiet 2>/dev/null; then
    return 0
  fi
  curl -fsSL "https://raw.githubusercontent.com/containerd/nerdctl/$NERDCTL_COMMIT/extras/rootless/$name" -o "/usr/local/bin/$name"
  echo "$sum  /usr/local/bin/$name" | sha256sum --check --quiet
  chmod 0755 "/usr/local/bin/$name"
}

setup() {
  echo "==> host: $(sed -n 's/^PRETTY_NAME=//p' /etc/os-release), kernel $(uname -r), $(systemctl --version | head -1)"
  echo "==> apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo n/a)"
  local pkgs=(bubblewrap uidmap dbus-user-session rootlesskit slirp4netns
    containerd xfsprogs quota nftables iproute2 util-linux)
  if ! dpkg -s "${pkgs[@]}" >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -q
    apt-get install -y -q "${pkgs[@]}"
  fi
  echo "==> bwrap $(bwrap --version), $(containerd --version), $(rootlesskit --version)"
  ls -l /etc/apparmor.d/bwrap-userns-restrict /etc/apparmor.d/rootlesskit || true

  # As on vora-mini: forwarding is set by the host, not by the runner.
  sysctl -q -w net.ipv4.ip_forward=1

  # /var/lib/vora on XFS with project quotas, as on vora-mini.
  truncate -s 6G /var/tmp/vora-state.img
  mkfs.xfs -q /var/tmp/vora-state.img
  mkdir -p "$STATE"
  mount -o loop,prjquota /var/tmp/vora-state.img "$STATE"
  findmnt -no FSTYPE,OPTIONS "$STATE"

  # The vora user with subordinate ids, a lingering manager and rootless
  # containerd: the prerequisites install-system.sh expects.
  useradd --system --user-group --home-dir "$STATE" --shell "$(command -v nologin)" vora
  chown vora:vora "$STATE"
  chmod 0750 "$STATE"
  usermod --add-subuids 200000-265535 --add-subgids 200000-265535 vora
  # CI images put the runner user's XDG_* dirs in /etc/environment, which
  # pam_env hands to every user manager; vora's must use its own.
  if grep -q '^XDG_' /etc/environment; then
    echo "==> dropping from /etc/environment: $(grep '^XDG_' /etc/environment | tr '\n' ' ')"
    sed -i '/^XDG_/d' /etc/environment
  fi
  loginctl enable-linger vora
  systemctl start "user@$(vora_uid).service"

  fetch_pinned containerd-rootless.sh "$ROOTLESS_SH_SHA256"
  fetch_pinned containerd-rootless-setuptool.sh "$SETUPTOOL_SHA256"
  as_vora containerd-rootless-setuptool.sh install
  test -S "/run/user/$(vora_uid)/containerd/containerd.sock"
}

install_unit() {
  # VORA_BIN_DIR: prebuilt static binaries (a local container run); CI builds.
  local bin="${VORA_BIN_DIR:-}"
  if [[ -z "$bin" ]]; then
    (cd "$REPO_ROOT" && make binaries)
    bin="$REPO_ROOT/bin"
  fi
  "$REPO_ROOT/scripts/install-system.sh" --from "$bin"
  # Force containerd images: in auto mode a containerd failure falls back to
  # the image cache and would hide a rootfs path problem.
  sed -i 's/^VORA_IMAGES_MODE=.*/VORA_IMAGES_MODE=containerd/' /etc/vora/runner.env
  systemctl restart vora-runner.service
  wait_socket
}

wait_socket() { wait_runner vora-runner.service "$SOCKET"; }

wait_runner() {
  local unit="$1" socket="$2"
  for _ in $(seq 60); do
    [[ -S "$socket" ]] && systemctl is-active --quiet "$unit" && return 0
    sleep 1
  done
  echo "error: $unit did not come up" >&2
  return 1
}

start_runner_b() {
  sed -e 's|VORA_STATE_DIR=/var/lib/vora |VORA_STATE_DIR=/var/lib/vora/runner-b |' \
      -e "s|VORA_SOCKET=$SOCKET|VORA_SOCKET=$SOCKET_B|" \
      -e 's|^EnvironmentFile=.*|EnvironmentFile=/etc/vora/runner-b.env|' \
      -e 's|^ReadWritePaths=.*|ReadWritePaths=/var/lib/vora /run/vora-b|' \
      -e 's|^BindPaths=.*|BindPaths=/run/vora-b/netns:/run/netns|' \
      -e 's|^RuntimeDirectory=.*|RuntimeDirectory=vora-b vora-b/netns|' \
      /etc/systemd/system/vora-runner.service >/etc/systemd/system/vora-runner-b.service
  grep -q "VORA_STATE_DIR=/var/lib/vora/runner-b .*VORA_SOCKET=$SOCKET_B" /etc/systemd/system/vora-runner-b.service || {
    echo "error: vora-runner-b.service did not get its own state dir and socket" >&2; return 1; }
  sed 's|^VORA_METRICS_ADDR=.*|VORA_METRICS_ADDR=127.0.0.1:9102|' /etc/vora/runner.env >/etc/vora/runner-b.env
  chmod 0600 /etc/vora/runner-b.env
  systemctl daemon-reload
  systemctl start vora-runner-b.service
  wait_runner vora-runner-b.service "$SOCKET_B"
}

fail=0
check() {
  local what="$1"; shift
  if "$@"; then
    echo "ok   $what"
  else
    echo "FAIL $what"
    fail=1
  fi
}

create() {
  local image="$1" network="$2" out
  local args=(sandbox create --image "$image" --network "$network" --ttl 10m)
  case "$network" in
    restricted) args+=(--allow-domain example.com) ;;
    private) args+=(--tenant ci) ;;
  esac
  out="$(cli --json "${args[@]}")" || return 1
  sed -n 's/.*"id": *"\([^"]*\)".*/\1/p' <<<"$out" | head -1
}

run_sandbox() {
  local image="$1" network="$2" id
  id="$(create "$image" "$network")" || return 1
  [[ -n "$id" ]] || return 1
  local rc=0
  cli exec "$id" -- sh -c 'echo ok && ls / >/dev/null' | grep -qx ok || rc=1
  case "$image" in
    *vora-node*) cli exec "$id" -- node --version >/dev/null || rc=1 ;;
    *vora-python*) cli exec "$id" -- python3 --version >/dev/null || rc=1 ;;
  esac
  cli sandbox destroy "$id" >/dev/null || rc=1
  return "$rc"
}

# A sandbox cannot write past its disk limit: with project quotas the kernel
# refuses the write, where accounting mode would only notice later.
quota_enforced() {
  local id out rc=0
  id="$(cli --json sandbox create --image "${IMAGES[0]}" --disk 16Mi --ttl 10m | sed -n 's/.*"id": *"\([^"]*\)".*/\1/p' | head -1)"
  [[ -n "$id" ]] || return 1
  # Only the quota's refusal counts, not a missing dd or a failed exec. XFS
  # project quotas report ENOSPC (the 6 GiB state filesystem is far from
  # full), ext4 reports EDQUOT.
  out="$(cli exec "$id" -- dd if=/dev/zero of=/workspace/fill bs=1M count=64 2>&1)" || true
  grep -qE 'No space left on device|Disk quota exceeded' <<<"$out" || rc=1
  cli sandbox destroy "$id" >/dev/null || true
  return "$rc"
}

# The runner holds CAP_SYS_ADMIN/CAP_NET_ADMIN/CAP_NET_BIND_SERVICE ambient. A
# netns sandbox is its child and must hold none of them (V-31).
sandbox_has_no_capabilities() {
  local id rc=0
  id="$(create "${IMAGES[0]}" full)" && [[ -n "$id" ]] || return 1
  [[ "$(cli exec "$id" -- grep -cE '^Cap(Inh|Prm|Eff|Amb):[[:space:]]+0+$' /proc/self/status)" == 4 ]] || rc=1
  cli sandbox destroy "$id" >/dev/null || rc=1
  return "$rc"
}

# Only the capabilities the unit grants, nothing inherited from root.
runner_capabilities() {
  local pid
  pid="$(systemctl show -p MainPID --value vora-runner.service)"
  # CAP_NET_BIND_SERVICE (10), CAP_NET_ADMIN (12), CAP_SYS_ADMIN (21)
  grep -qx $'CapEff:\t0000000000201400' "/proc/$pid/status"
}

# Egress through the runner-side resolver and proxy: full reaches the internet,
# restricted reaches its allowlist (example.com, see create) and nothing else.
egress_policy() {
  local id rc=0 code
  id="$(create "${IMAGES[0]}" full)" && [[ -n "$id" ]] || return 1
  code="$(cli exec "$id" -- curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://example.com)" || rc=1
  [[ "$code" == 200 ]] || rc=1
  cli sandbox destroy "$id" >/dev/null || rc=1
  id="$(create "${IMAGES[0]}" restricted)" && [[ -n "$id" ]] || return 1
  code="$(cli exec "$id" -- curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://example.com)" || rc=1
  [[ "$code" == 200 ]] || rc=1
  if cli exec "$id" -- curl -sS -o /dev/null --max-time 10 https://github.com 2>/dev/null; then
    rc=1
  fi
  cli sandbox destroy "$id" >/dev/null || rc=1
  return "$rc"
}

# The 10.x addresses a sandbox holds, read from inside it.
sandbox_addrs() {
  cli exec "$1" -- sh -c "grep -B1 '32 host' /proc/net/fib_trie | grep -o '10\.[0-9.]*' | sort -u"
}

# Each live sandbox's host veth carries the sandbox id as its alias.
veth_labelled() {
  local id
  for id in "$@"; do
    ip -j link show | grep -q "\"ifalias\":\"$id\"" || return 1
  done
}

# Two runners allocate in the host network namespace side by side: every
# sandbox works, the same tenant lands in a different /24 on each runner, and
# destroying one runner's sandboxes or restarting it leaves the other's intact.
two_runners_coexist() {
  local a_full a_priv b_full b_priv a_net b_net rc=0
  start_runner_b || return 1
  a_full="$(create "${IMAGES[0]}" full)" && [[ -n "$a_full" ]] || rc=1
  a_priv="$(create "${IMAGES[0]}" private)" && [[ -n "$a_priv" ]] || rc=1
  b_full="$(RUNNER_SOCKET=$SOCKET_B create "${IMAGES[0]}" full)" && [[ -n "$b_full" ]] || rc=1
  b_priv="$(RUNNER_SOCKET=$SOCKET_B create "${IMAGES[0]}" private)" && [[ -n "$b_priv" ]] || rc=1
  if [[ "$rc" -eq 0 ]]; then
    cli exec "$a_full" -- echo ok | grep -qx ok || rc=1
    cli exec "$a_priv" -- echo ok | grep -qx ok || rc=1
    RUNNER_SOCKET=$SOCKET_B cli exec "$b_full" -- echo ok | grep -qx ok || rc=1
    RUNNER_SOCKET=$SOCKET_B cli exec "$b_priv" -- echo ok | grep -qx ok || rc=1
    veth_labelled "$a_full" "$a_priv" "$b_full" "$b_priv" || { echo "a sandbox veth is missing its owner label" >&2; rc=1; }
    # Both runners used --tenant ci and must still hold different /24s.
    a_net="$(sandbox_addrs "$a_priv" | cut -d. -f1-3)"
    b_net="$(RUNNER_SOCKET=$SOCKET_B sandbox_addrs "$b_priv" | cut -d. -f1-3)"
    [[ -n "$a_net" && -n "$b_net" && "$a_net" != "$b_net" ]] || { echo "tenant /24: A ${a_net:-?} B ${b_net:-?}" >&2; rc=1; }
    cli sandbox destroy "$a_full" >/dev/null || rc=1
    cli sandbox destroy "$a_priv" >/dev/null || rc=1
    systemctl restart vora-runner.service && wait_socket || rc=1
    RUNNER_SOCKET=$SOCKET_B cli exec "$b_full" -- echo ok | grep -qx ok || { echo "runner B's full sandbox broke" >&2; rc=1; }
    RUNNER_SOCKET=$SOCKET_B cli exec "$b_priv" -- echo ok | grep -qx ok || { echo "runner B's private sandbox broke" >&2; rc=1; }
    RUNNER_SOCKET=$SOCKET_B cli sandbox destroy "$b_full" >/dev/null || rc=1
    RUNNER_SOCKET=$SOCKET_B cli sandbox destroy "$b_priv" >/dev/null || rc=1
  fi
  systemctl stop vora-runner-b.service || rc=1
  return "$rc"
}

journal() { journalctl -u vora-runner.service --no-pager -o cat; }

# At boot the system runner can start before vora's rootless containerd, which
# it cannot be ordered after. In containerd images mode it must refuse to run
# without the socket and recover through Restart= once containerd is up.
runner_waits_for_containerd() {
  local since rc=0
  since="$(date '+%Y-%m-%d %H:%M:%S')"
  as_vora systemctl --user stop containerd
  systemctl restart vora-runner.service || true
  sleep 6
  journalctl -u vora-runner.service --since "$since" --no-pager -o cat | grep -q 'no containerd socket' || rc=1
  as_vora systemctl --user start containerd
  sleep 5
  wait_socket || return 1
  run_sandbox "${IMAGES[0]}" none || rc=1
  return "$rc"
}

# Image files owned by non-root image users live under vora's subordinate ids.
# systemd's StateDirectory= ownership fix-up must not rewrite them on restart.
subuid_owners_survive_restart() {
  local before after
  before="$(find "$STATE" -xdev -uid +199999 -printf '%U %p\n' 2>/dev/null | sort | head -200)"
  if [[ -z "$before" ]]; then
    echo "note: no subordinate-id owned files under $STATE; ownership check is vacuous"
  fi
  systemctl restart vora-runner.service
  wait_socket
  after="$(find "$STATE" -xdev -uid +199999 -printf '%U %p\n' 2>/dev/null | sort | head -200)"
  [[ "$before" == "$after" ]]
}

test_unit() {
  echo "==> unit: $(systemctl show -p User,ActiveState vora-runner.service | tr '\n' ' ')"
  check "runner runs as vora" test "$(ps -o user= -p "$(systemctl show -p MainPID --value vora-runner.service)")" = vora
  check "bwrap backend registered" grep -q '"backend":"bwrap"' <(journal)
  for image in "${IMAGES[@]}"; do
    for network in "${NETWORKS[@]}"; do
      check "sandbox ${image%%@*} network=$network" run_sandbox "$image" "$network"
    done
  done
  check "runner holds only its three capabilities" runner_capabilities
  check "netns sandbox holds no capabilities" sandbox_has_no_capabilities
  check "two runners share the host network namespace" two_runners_coexist
  check "egress: full reaches the internet, restricted only its allowlist" egress_policy
  check "runner reports project-quota mode" grep -qx 'vora_disk_quota_enforced 1' <(curl -fsS http://127.0.0.1:9101/metrics)
  check "disk quota is kernel-enforced" quota_enforced
  check "runner waits for rootless containerd" runner_waits_for_containerd
  check "subordinate-id ownership survives a restart" subuid_owners_survive_restart
  check "no bwrap capability abort" bash -c '! journalctl -u vora-runner.service --no-pager | grep -q "Unexpected capabilities"'
  return "$fail"
}

diagnose() {
  set +e
  local uid
  uid="$(vora_uid 2>/dev/null)"
  echo "==> vora-runner journal"; journal | tail -200
  echo "==> vora-runner unit"; systemctl status vora-runner.service --no-pager
  echo "==> vora-runner-b journal"; journalctl -u vora-runner-b.service --no-pager -o cat | tail -100
  echo "==> host links"; ip -j link show | grep -o '"ifname":"vora-[^"]*"\|"ifalias":"[^"]*"' | paste - - || true
  echo "==> vora user manager journal"; journalctl _UID="$uid" --no-pager -o short | tail -150
  echo "==> rootless containerd"; journalctl --no-pager -o cat _UID="$uid" _SYSTEMD_USER_UNIT=containerd.service | tail -60
  as_vora systemctl --user status containerd --no-pager 2>&1
  echo "==> /etc/vora/runner.env"; cat /etc/vora/runner.env
  echo "==> apparmor denials"; journalctl -k --no-pager | grep -i 'apparmor="DENIED"' | tail -30
  return 0
}

case "${1:-}" in
  setup) setup ;;
  install) install_unit ;;
  test) test_unit ;;
  diagnose) diagnose ;;
  *) sed -n '2,17p' "$0"; exit 2 ;;
esac
