#!/usr/bin/env bash
#
# bench.sh runs the benchmark suite and writes a report to docs/benchmarks/.
# Concurrency targets default to 10,50; raise VORA_BENCH_TARGETS only after the
# host is confirmed stable.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

OUT="docs/benchmarks/$(date +%Y-%m-%d)-local.md"
mkdir -p docs/benchmarks

{
  echo "# Vora benchmark report"
  echo
  echo "- Date: $(date -Is)"
  echo "- Host: $(uname -srm)"
  echo "- CPU: $(nproc) vCPU"
  echo "- Backend: bwrap"
  echo
  echo '```text'
  echo "== concurrency ladder (VORA_BENCH_TARGETS=${VORA_BENCH_TARGETS:-10,50}) =="
  go test -tags=benchmark -run TestConcurrencyLadder -v ./tests/benchmark/ 2>&1
  echo
  echo "== micro benchmarks =="
  go test -tags=benchmark -bench=. -run '^$' -benchtime="${BENCHTIME:-10x}" ./tests/benchmark/ 2>&1
  echo '```'
} | tee "$OUT"

echo
echo "wrote $OUT"
