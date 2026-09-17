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

# Vora binds the sandbox's resolver config over /etc/resolv.conf, so the path
# must exist in the image: base images do not ship one (Docker injects it at
# container start, which never happens for a containerd snapshot) and it cannot
# be created with RUN, because Docker bind-mounts a read-only resolv.conf into
# build containers. NetworkRestricted refuses to start without it.
COPY resolv.conf /etc/resolv.conf
