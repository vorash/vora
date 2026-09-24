#!/usr/bin/env bash
set -euo pipefail

for entry in 'vora-runner.system.service 6.5' 'vora-api.service 5.5'; do
  read -r unit ceiling <<<"$entry"
  report="$(systemd-analyze security --offline=yes --no-pager "deploy/systemd/$unit")"
  score="$(grep 'Overall exposure level' <<<"$report" | grep -oE '[0-9]+\.[0-9]+' | head -1)"
  echo "$unit: exposure $score (maximum $ceiling)"
  awk -v score="$score" -v ceiling="$ceiling" 'BEGIN { exit !(score <= ceiling) }'
done
