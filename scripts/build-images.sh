#!/usr/bin/env bash
#
# build-images.sh builds the read-only sandbox rootfs cache.
#
# Each images/<name>/Dockerfile is built with Docker, then exported to
#   $VORA_STATE_DIR/images/<name>/latest/rootfs
# and tagged with the source image id in
#   $VORA_STATE_DIR/images/<name>/latest/image-id
#
# The runner binds this rootfs read-only into sandboxes, so it can be shared
# by every sandbox that uses the image.
#
# Optional args limit which images to build (e.g. ./scripts/build-images.sh base).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -n "${VORA_STATE_DIR:-}" ]]; then
  STATE_DIR="$VORA_STATE_DIR"
elif [[ "$(id -u)" -eq 0 ]]; then
  STATE_DIR="/var/lib/vora"
else
  STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/vora"
fi
IMAGES_DIR="$STATE_DIR/images"

command -v docker >/dev/null 2>&1 || { echo "error: docker not found" >&2; exit 1; }

build_image() {
  local name="$1" dockerfile="$2"
  local ctx tag dir
  ctx="$(dirname "$dockerfile")"
  tag="vora/${name}:latest"
  dir="$IMAGES_DIR/$name/latest"

  if [[ ! -f "$dockerfile" ]]; then
    echo "error: missing $dockerfile" >&2
    exit 1
  fi

  echo "==> building $tag"

  local df_sha
  df_sha="$(sha256sum "$dockerfile" | awk '{print $1}')"
  local tag_id=""
  if docker image inspect "$tag" >/dev/null 2>&1; then
    tag_id="$(docker image inspect --format '{{.Id}}' "$tag")"
  fi
  if [[ -d "$dir/rootfs" && -f "$dir/dockerfile-sha" && -f "$dir/image-id" \
        && "$(cat "$dir/dockerfile-sha")" == "$df_sha" \
        && "$tag_id" == "$(cat "$dir/image-id")" ]]; then
    echo "    up to date ($tag_id)"
    return
  fi

  # --network=host: the default bridge resolves IPv6-only for some CDNs and
  # cannot route it, which breaks apk/apt during the build.
  docker build --network=host --quiet -t "$tag" -f "$dockerfile" "$ctx" >/dev/null

  local image_id
  image_id="$(docker image inspect --format '{{.Id}}' "$tag")"

  echo "==> exporting rootfs for $name:latest"
  mkdir -p "$IMAGES_DIR"
  local tmp
  tmp="$(mktemp -d "$IMAGES_DIR/.build-${name}-XXXXXX")"
  trap 'rm -rf "${tmp:-}"' RETURN

  local cid
  cid="$(docker create "$tag" /bin/true)"
  mkdir -p "$tmp/rootfs"
  # --no-same-owner: the cache is owned by the invoking user.
  docker export "$cid" | tar --no-same-owner -xpf - -C "$tmp/rootfs"
  docker rm -f "$cid" >/dev/null

  rm -rf "$dir"
  mkdir -p "$dir"
  mv "$tmp/rootfs" "$dir/rootfs"
  printf '%s\n' "$image_id" > "$dir/image-id"
  printf '%s\n' "$df_sha" > "$dir/dockerfile-sha"
  echo "    wrote $dir/rootfs"
}

main() {
  shopt -s nullglob
  local found=0 names=()
  if [[ $# -gt 0 ]]; then
    names=("$@")
  else
    local dockerfile
    for dockerfile in "$REPO_ROOT"/images/*/Dockerfile; do
      names+=("$(basename "$(dirname "$dockerfile")")")
    done
  fi
  local name
  for name in "${names[@]}"; do
    build_image "$name" "$REPO_ROOT/images/$name/Dockerfile"
    found=1
  done
  if [[ "$found" -eq 0 ]]; then
    echo "error: no images found under $REPO_ROOT/images/*/Dockerfile" >&2
    exit 1
  fi
  echo "images cached in $IMAGES_DIR"
}

main "$@"
