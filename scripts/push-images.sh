#!/usr/bin/env bash
#
# push-images.sh builds the sandbox rootfs images and publishes them to a
# registry so a runner can pull them by reference (ADR 009). containerd pulls
# an image when it is not present locally, so a registry ref is all a host
# needs: no docker-export cache, no tarballs shipped to the machine.
#
# Each images/<name>/Dockerfile becomes <registry>/vora-<name>:<tag>. Pass
# --latest to also move :latest (off by default: a dispatched rebuild of an old
# tag must not drag the convenience tag backwards). The script prints the
# digests; pin those in vora-ansible (group_vars/all/all.yml) instead of the tag.
#
# Usage:
#   scripts/push-images.sh --tag v0.4.0 [--registry ghcr.io/vorash] [--latest]
#   scripts/push-images.sh --tag v0.4.0 --load        # build locally, no push
#   GHCR_USER=... GHCR_TOKEN=... scripts/push-images.sh --tag v0.4.0 --login
#
# Public packages need no credentials on the pulling side; keep them public
# unless every runner is configured with registry credentials.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

REGISTRY="ghcr.io/vorash"
TAG=""
IMAGES=()
LOAD=0
LOGIN=0
LATEST=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --registry) REGISTRY="${2:-}"; shift 2 ;;
    --tag) TAG="${2:-}"; shift 2 ;;
    --images) IFS=, read -r -a IMAGES <<<"${2:-}"; shift 2 ;;
    --load) LOAD=1; shift ;;
    --login) LOGIN=1; shift ;;
    --latest) LATEST=1; shift ;;
    -h|--help) sed -n '2,23p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

command -v docker >/dev/null 2>&1 || { echo "error: docker not found" >&2; exit 1; }

# image_has_file reports whether a path is present in the image **layers**.
# `docker export` cannot answer this: Docker injects /etc/resolv.conf into every
# container at start, so an exported rootfs shows a file the image does not
# have.
#
# The archive layout is not fixed. Docker with the containerd image store writes
# an OCI layout (gzipped blobs under blobs/sha256/), the classic store writes
# <id>/layer.tar entries, and either may be compressed or not. Every regular
# entry is therefore tried as a tar, and a change that yields no parsable layer
# at all is reported as an error rather than as "file missing", so a future
# layout change cannot silently fail every publish.
image_has_file() {
  local ref="$1" want="$2" tmp entry layers=0
  tmp="$(mktemp -d)"
  docker save "$ref" -o "$tmp/image.tar"
  while IFS= read -r entry; do
    case "$entry" in */) continue ;; esac
    tar -xOf "$tmp/image.tar" "$entry" >"$tmp/entry" 2>/dev/null || continue
    if tar -tzf "$tmp/entry" >"$tmp/list" 2>/dev/null || tar -tf "$tmp/entry" >"$tmp/list" 2>/dev/null; then
      layers=$((layers + 1))
      if grep -qx "$want" "$tmp/list"; then
        rm -rf "$tmp"
        return 0
      fi
    fi
  done < <(tar -tf "$tmp/image.tar")
  rm -rf "$tmp"
  if [[ "$layers" -eq 0 ]]; then
    echo "error: no layer archive found in 'docker save $ref' output; cannot verify $want" >&2
    return 2
  fi
  return 1
}

if [[ -z "$TAG" ]]; then
  TAG="$(git describe --tags --always 2>/dev/null || echo dev)"
fi
if [[ "$TAG" == *-dirty ]]; then
  echo "error: refusing to publish images from a dirty tree ($TAG)" >&2
  exit 1
fi

if [[ ${#IMAGES[@]} -eq 0 ]]; then
  for df in "$REPO_ROOT"/images/*/Dockerfile; do
    IMAGES+=("$(basename "$(dirname "$df")")")
  done
fi

if [[ "$LOGIN" -eq 1 && "$LOAD" -eq 0 ]]; then
  : "${GHCR_USER:?GHCR_USER must be set with --login}"
  : "${GHCR_TOKEN:?GHCR_TOKEN must be set with --login}"
  echo "==> docker login $REGISTRY (user $GHCR_USER)"
  printf '%s' "$GHCR_TOKEN" | docker login "$REGISTRY" --username "$GHCR_USER" --password-stdin >/dev/null
fi

digests=()
for name in "${IMAGES[@]}"; do
  dockerfile="$REPO_ROOT/images/$name/Dockerfile"
  [[ -f "$dockerfile" ]] || { echo "error: missing $dockerfile" >&2; exit 1; }
  local_tag="vora/${name}:latest"
  remote="$REGISTRY/vora-${name}"

  echo "==> building $local_tag"  # --network=host: the default bridge resolves IPv6-only for some CDNs.
  docker build --network=host --quiet -t "$local_tag" -f "$dockerfile" "$(dirname "$dockerfile")" >/dev/null

  # The restricted path binds the sandbox resolver config over /etc/resolv.conf,
  # so the mount point must exist in the image itself.
  status=0
  image_has_file "$local_tag" 'etc/resolv.conf' || status=$?
  case "$status" in
    0) echo "    /etc/resolv.conf present in the image layers" ;;
    1)
      echo "error: $name has no /etc/resolv.conf in its layers; NetworkRestricted would refuse it" >&2
      exit 1
      ;;
    *) exit 1 ;; # image_has_file already explained the problem
  esac

  docker tag "$local_tag" "$remote:$TAG"
  if [[ "$LATEST" -eq 1 ]]; then
    docker tag "$local_tag" "$remote:latest"
  fi
  if [[ "$LOAD" -eq 1 ]]; then
    echo "    built and tagged $remote:$TAG (not pushed)"
    continue
  fi

  echo "==> pushing $remote:$TAG"
  docker push "$remote:$TAG" >/dev/null
  if [[ "$LATEST" -eq 1 ]]; then
    docker push "$remote:latest" >/dev/null
  fi

  digest="$(docker inspect --format '{{index .RepoDigests 0}}' "$remote:$TAG" 2>/dev/null || true)"
  if [[ -z "$digest" ]]; then
    echo "error: no repo digest for $remote:$TAG" >&2
    exit 1
  fi
  digests+=("$digest")
done

echo
if [[ "$LOAD" -eq 1 ]]; then
  echo "images built for $TAG (not pushed):"
  for name in "${IMAGES[@]}"; do
    echo "  $REGISTRY/vora-${name}:$TAG"
  done
else
  echo "images for $TAG:"
  for d in "${digests[@]}"; do
    echo "  $d"
  done
fi
