#!/usr/bin/env bash
# Builds the rvsim boot images (OpenSBI fw_jump, Linux Image, BusyBox
# initramfs) with Buildroot inside a Debian container (ADR-006, ADR-026).
#
#   tests/images/build-images.sh [output-dir]    # default: build/images
#
# The build tree lives in a container volume, not a host bind mount: the
# kernel sources need a case-sensitive filesystem. Rerunning is incremental.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
out=${1:-$root/build/images}
engine=${CONTAINER_ENGINE:-$(command -v podman || command -v docker || true)}
if [[ -z $engine ]]; then
  echo "build-images: podman or docker is required" >&2
  exit 1
fi
image=docker.io/library/debian:bookworm-slim
volume=rvsim-images-work

mkdir -p "$out"
"$engine" volume inspect "$volume" >/dev/null 2>&1 || "$engine" volume create "$volume" >/dev/null
"$engine" run --rm \
  -v "$here:/src:ro" -v "$volume:/work" -v "$out:/out" ${JOBS:+-e JOBS="$JOBS"} \
  "$image" bash /src/container-build.sh
ls -l "$out"
