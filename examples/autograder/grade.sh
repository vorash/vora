#!/usr/bin/env bash
# Grade every submission script in a directory, one sandbox each.
# Usage: ./grade.sh <submissions-dir>
set -euo pipefail

dir="${1:?usage: grade.sh <submissions-dir>}"
fixture="What is 6 * 7?"     # the prompt every submission must answer
expected="42"                # the answer we expect on stdout

grade_one() {
  local submission="$1" name sbx out status
  name="$(basename "$submission")"
  sbx="$(vora sandbox create \
    --image vora/base \
    --backend "${VORA_BACKEND:-bwrap}" \
    --memory 64m --cpu 500m --pids 16 --disk 64m --ttl 2m \
    --network none | awk '{print $NF}')"
  # Isolated per grading attempt; the submission cannot see the grader.
  trap 'vora sandbox destroy "$sbx" >/dev/null 2>&1 || true' RETURN

  vora cp "$submission" "$sbx":/workspace/answer.sh
  vora exec "$sbx" -- chmod +x /workspace/answer.sh

  # The fixture goes in over stdin — nothing about the grader leaks in.
  set +e
  out="$(printf '%s\n' "$fixture" | vora exec "$sbx" --workdir /workspace -- sh ./answer.sh 2>/dev/null)"
  status=$?
  set -e

  local reason score
  # Semantic outcomes (OOMKilled, TimeoutExceeded, PidsLimitExceeded,
  # DiskQuotaExceeded …) land on the exec record's reason field.
  reason="$(vora logs "$sbx" --execs | tail -n 1 | sed -n 's/.*reason=\([A-Z_]*\).*/\1/p')"
  if [[ $status -ne 0 ]]; then
    score="0 (failed: ${reason})"
  elif [[ "$(echo "$out" | tr -d '[:space:]')" == *"$expected"* ]]; then
    score="1 (correct)"
  else
    score="0 (wrong answer: $(echo "$out" | head -c 60))"
  fi
  printf '%-20s %s\n' "$name" "$score"

  # Per-submission evidence for grade disputes: what ran, what it cost.
  vora logs "$sbx" --execs | sed 's/^/    /' || true
}

for submission in "$dir"/*; do
  grade_one "$submission"
done
