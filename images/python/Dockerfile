# Python workload image used by the v0.1 demo.
FROM python:3.13-slim

# Mount points used by Vora: /workspace (rw) and /vora (rw, agent + socket).
RUN mkdir -p /workspace /vora

# Vora binds the sandbox's resolver config over /etc/resolv.conf, so the path
# must exist in the image (see images/base/Dockerfile for why it is copied).
COPY resolv.conf /etc/resolv.conf
