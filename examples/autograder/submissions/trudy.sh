#!/bin/sh
# A resource-exhaustion submission: fork bomb. Expect pids.max to stop it and
# the grade to read PidsLimitExceeded — the grader host is untouched.
read -r question
while :; do sh -c 'while :; do :; done' & done
