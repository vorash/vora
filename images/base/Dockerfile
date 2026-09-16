# Minimal sandbox base image: just enough tooling to run and debug workloads.
FROM alpine:3.20

RUN apk add --no-cache \
    bash \
    coreutils \
    ca-certificates \
    curl \
    git \
    python3 \
    py3-pip

# Mount points used by Vora: /workspace (rw) and /vora (rw, agent + socket).
RUN mkdir -p /workspace /vora
