#!/usr/bin/env bash
# Release automation that calls GitHub without ever holding the token.
# Prereq: vora-api with VORA_MASTER_KEY and a 'github' alias secret (see README).
set -euo pipefail

tenant="${VORA_TENANT:-t1}"
api=(${VORA_API:+--api "$VORA_API"} ${VORA_API_KEY:+--api-key "$VORA_API_KEY"})

sbx=""
cleanup() {
  if [[ -n "$sbx" ]]; then
    vora "${api[@]}" sandbox destroy "$sbx" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# The sandbox may use the 'github' broker alias; nothing else leaves it.
sbx="$(vora "${api[@]}" sandbox create \
  --image vora/base \
  --backend "${VORA_BACKEND:-bwrap}" \
  --memory 256m --cpu 1 --pids 32 --ttl 5m \
  --network restricted --allow-domain api.github.com \
  --broker github | awk '{print $NF}')"
echo "sandbox: $sbx" >&2

echo "--- through the broker (the gateway attaches the token) --------------"
vora exec "$sbx" -- sh -c \
  'wget -q -O- "$VORA_BROKER_URL/creds/github/user" | head -c 200; echo'

echo "--- the sandbox itself holds no credential ----------------------------"
vora exec "$sbx" -- sh -c \
  'grep -r "$GITHUB_TOKEN" /proc/self/environ 2>/dev/null; env | grep -ci token || echo "no token in env or /proc"'

echo "--- direct call: no broker, no credential, egress denied --------------"
vora exec "$sbx" -- sh -c \
  'wget -q -O- https://api.github.com/user 2>&1 | head -1 || echo "denied as expected"'

echo "--- audit: the alias is recorded, never the secret ---------------------"
vora "${api[@]}" logs "$sbx" --egress
