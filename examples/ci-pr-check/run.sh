#!/usr/bin/env bash
# Run an untrusted PR checkout's test suite inside a Vora sandbox.
# Usage: ./run.sh <pr-checkout-dir> [test-command...]
set -euo pipefail

checkout="${1:?usage: run.sh <pr-checkout-dir> [test-command...]}"
shift
test_cmd=("$@")
if [[ ${#test_cmd[@]} -eq 0 ]]; then
  test_cmd=(sh -c "npm ci --offline 2>/dev/null; npm test")
fi

image="${VORA_IMAGE:-vora/base}"
sbx=""
cleanup() {
  if [[ -n "$sbx" ]]; then
    vora sandbox destroy "$sbx" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# Budgets for hostile code: no network, small memory, few processes, bounded
# disk and a hard TTL. A pathological PR dies at the boundary, not on the host.
sbx="$(vora sandbox create \
  --image "$image" \
  --backend "${VORA_BACKEND:-bwrap}" \
  --memory 512m --cpu 1 --pids 64 --disk 1g --ttl 10m \
  --network none \
  | awk '{print $NF}')"
echo "sandbox: $sbx (running untrusted tests from $checkout)" >&2

# Ship the PR in. `vora cp` moves single files and only reaches /workspace and
# /root by design, so a checkout travels as a tarball and is unpacked inside.
tarball="$(mktemp -t pr-XXXXXX.tar)"
trap 'rm -f "$tarball"; cleanup' EXIT
tar -C "$checkout" -cf "$tarball" .
vora cp "$tarball" "$sbx":/workspace/pr.tar
vora exec "$sbx" -- mkdir -p /workspace/pr
vora exec "$sbx" --workdir /workspace -- tar -xf pr.tar -C pr

echo "--- test output -----------------------------------------------------" >&2
set +e
vora exec "$sbx" --workdir /workspace/pr -- "${test_cmd[@]}"
status=$?
set -e
echo "---------------------------------------------------------------------" >&2

# Explain the outcome, then leave the audit trail behind for the record.
if [[ $status -eq 0 ]]; then
  echo "exit: 0 (tests passed)" >&2
else
  reason="$(vora logs "$sbx" --execs | tail -n 1 | sed -n 's/.*reason=\([A-Z_]*\).*/\1/p')"
  echo "exit: $status (${reason:-no exec record})" >&2
fi
echo "--- resource usage --------------------------------------------------" >&2
vora stats "$sbx" >&2 || true
vora logs "$sbx" >&2 || true

exit "$status"
