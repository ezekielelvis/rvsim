#!/usr/bin/env bash
# Verify (default) or install (--install) the tools rvsim needs.
#
#   scripts/toolchain.sh             report what is present; fail if a build tool is missing
#   scripts/toolchain.sh --strict    also fail if any later-phase tool is missing
#   scripts/toolchain.sh --install   install via Homebrew (macOS) or apt (Debian/Ubuntu),
#                                    and build the pinned Spike from source
#
# Spike is built from a pinned commit (not a package) so difftest results are
# reproducible on every machine. It installs into $RVSIM_TOOLS (default
# ~/.cache/rvsim-tools); put $RVSIM_TOOLS/bin on PATH.
set -euo pipefail

SPIKE_REPO=https://github.com/riscv-software-src/riscv-isa-sim.git
SPIKE_COMMIT=cae29438a126b9ff76ad50dc9266483b6c94b0e5  # master, 2026-09-12
SAIL_VERSION=0.14.1  # the version ACT4 (riscv-arch-test) is tested with
# Prebuilt riscv64-unknown-elf GCC for Ubuntu, as ACT4's CI uses: ACT4 needs
# GCC 15+, and Ubuntu 24.04's gcc-riscv64-unknown-elf package is 13.
GCC_RELEASE=2026.08.27
GO_MIN=1.26
RVSIM_TOOLS=${RVSIM_TOOLS:-$HOME/.cache/rvsim-tools}

mode=verify
case "${1:-}" in
  "") ;;
  --strict) mode=strict ;;
  --install) mode=install ;;
  *) echo "usage: $0 [--strict|--install]" >&2; exit 2 ;;
esac

os=$(uname -s)

install_packages() {
  if [[ $os == Darwin ]]; then
    brew install cmake dtc go mise z3
    brew tap riscv-software-src/riscv
    brew install riscv-software-src/riscv/riscv-gnu-toolchain
  elif command -v apt-get >/dev/null; then
    sudo apt-get update
    sudo apt-get install -y --no-install-recommends \
      build-essential cmake git curl xz-utils file device-tree-compiler \
      gcc-riscv64-linux-gnu libboost-regex-dev libboost-system-dev
    install_riscv_gcc
    command -v mise >/dev/null || curl -fsSL https://mise.run | sh
  else
    echo "unsupported host: install tools manually" >&2
    exit 1
  fi
}

# riscv64-unknown-elf GCC on Linux: riscv-collab's prebuilt release where one
# exists for this distro, otherwise the distro package (older than ACT4 needs).
install_riscv_gcc() {
  local stamp="$RVSIM_TOOLS/gcc.release"
  if [[ -x $RVSIM_TOOLS/bin/riscv64-unknown-elf-gcc && -f $stamp && $(cat "$stamp") == "$GCC_RELEASE" ]]; then
    echo "riscv64-unknown-elf-gcc $GCC_RELEASE already installed"
    return
  fi
  local id="" ver=""
  # shellcheck disable=SC1091
  [[ -f /etc/os-release ]] && { id=$(. /etc/os-release && echo "$ID"); ver=$(. /etc/os-release && echo "$VERSION_ID"); }
  if [[ $id == ubuntu && ($ver == 22.04 || $ver == 24.04) && $(uname -m) == x86_64 ]]; then
    local dl
    dl=$(mktemp -d)
    curl -fsSL --retry 5 -o "$dl/gcc.tar.xz" \
      "https://github.com/riscv-collab/riscv-gnu-toolchain/releases/download/$GCC_RELEASE/riscv64-elf-ubuntu-$ver-gcc.tar.xz"
    mkdir -p "$RVSIM_TOOLS"
    tar -xJf "$dl/gcc.tar.xz" -C "$RVSIM_TOOLS" --strip-components=1
    rm -rf "$dl"
    echo "$GCC_RELEASE" >"$stamp"
  else
    echo "no prebuilt toolchain for $id $ver $(uname -m); using the distro package (ACT4 needs GCC 15+)" >&2
    sudo apt-get install -y --no-install-recommends gcc-riscv64-unknown-elf
  fi
}

# Sail RISC-V reference model, used by ACT4 to compute expected results.
install_sail() {
  if [[ -x $RVSIM_TOOLS/bin/sail_riscv_sim && $("$RVSIM_TOOLS/bin/sail_riscv_sim" --version) == "$SAIL_VERSION" ]]; then
    echo "sail $SAIL_VERSION already installed"
    return
  fi
  local asset
  case "$os-$(uname -m)" in
    Darwin-arm64) asset=sail-riscv-Mac-arm64.tar.gz ;;
    Linux-x86_64) asset=sail-riscv-Linux-x86_64.tar.gz ;;
    Linux-aarch64) asset=sail-riscv-Linux-aarch64.tar.gz ;;
    *) echo "no Sail $SAIL_VERSION build for $os-$(uname -m)" >&2; exit 1 ;;
  esac
  local dl
  dl=$(mktemp -d)
  curl -fsSL -o "$dl/sail.tar.gz" "https://github.com/riscv/sail-riscv/releases/download/$SAIL_VERSION/$asset"
  mkdir -p "$RVSIM_TOOLS"
  tar -xzf "$dl/sail.tar.gz" -C "$RVSIM_TOOLS" --strip-components=1
  rm -rf "$dl"
}

build_spike() {
  local stamp="$RVSIM_TOOLS/spike.commit"
  if [[ -x $RVSIM_TOOLS/bin/spike && -f $stamp && $(cat "$stamp") == "$SPIKE_COMMIT" ]]; then
    echo "spike $SPIKE_COMMIT already built"
    return
  fi
  local src
  src=$(mktemp -d)
  git -C "$src" init -q
  git -C "$src" fetch -q --depth 1 "$SPIKE_REPO" "$SPIKE_COMMIT"
  git -C "$src" checkout -q FETCH_HEAD
  mkdir "$src/build"
  (cd "$src/build" && ../configure --prefix="$RVSIM_TOOLS" >/dev/null && make -j"$(getconf _NPROCESSORS_ONLN)" >/dev/null && make install >/dev/null)
  echo "$SPIKE_COMMIT" >"$stamp"
  rm -rf "$src"
}

if [[ $mode == install ]]; then
  install_packages
  build_spike
  install_sail
  mode=verify
fi

export PATH="$RVSIM_TOOLS/bin:$PATH"
missing_required=0
missing_later=0

# check <name> <required:yes|no> <note> <command...>
check() {
  local name=$1 required=$2 note=$3
  shift 3
  local out
  if out=$("$@" 2>&1 | head -n1) && [[ -n $out ]]; then
    printf '  %-26s %s\n' "$name" "$out"
  else
    printf '  %-26s MISSING (%s)\n' "$name" "$note"
    if [[ $required == yes ]]; then missing_required=1; else missing_later=1; fi
  fi
}

go_version_ok() {
  local v
  v=$(go env GOVERSION 2>/dev/null) || return 1
  v=${v#go}
  [[ $(printf '%s\n%s\n' "$GO_MIN" "$v" | sort -V | head -n1) == "$GO_MIN" ]] || return 1
  echo "go$v"
}

spike_version() {
  command -v spike >/dev/null || return 1
  local pinned=""
  [[ -f $RVSIM_TOOLS/spike.commit && $(command -v spike) == "$RVSIM_TOOLS/bin/spike" ]] &&
    pinned=" (pinned ${SPIKE_COMMIT:0:12})"
  echo "$(command -v spike)$pinned"
}

echo "build tools:"
check cmake yes "brew/apt install cmake" cmake --version
check "go >= $GO_MIN" yes "https://go.dev/dl" go_version_ok
check "c++ compiler" yes "Xcode CLT or build-essential" c++ --version

echo "RISC-V tools (Phase 1+):"
check riscv64-unknown-elf-gcc no "bare-metal tests" riscv64-unknown-elf-gcc --version
check spike no "difftest; run --install" spike_version
check dtc no "device tree validation (Phase 6)" dtc --version

sail_version() {
  command -v sail_riscv_sim >/dev/null || return 1
  local v
  v=$(sail_riscv_sim --version)
  [[ $v == "$SAIL_VERSION" ]] || { echo "version $v, want $SAIL_VERSION"; return 1; }
  echo "sail_riscv_sim $v"
}

echo "Architecture tests (ACT4):"
check sail no "ACT4 reference model; run --install" sail_version
check mise no "provisions ACT4's Ruby/uv; run --install" mise --version

echo "Linux image builds (Phase 6+):"
if [[ $os == Darwin ]]; then
  # Kernel builds need a case-sensitive filesystem and the linux-gnu
  # toolchain, neither of which macOS provides; images build in Docker.
  check docker no "image builds run in Docker on macOS" docker --version
else
  check riscv64-linux-gnu-gcc no "kernel/BusyBox builds" riscv64-linux-gnu-gcc --version
fi

if [[ $missing_required == 1 ]]; then
  echo "error: required build tools are missing" >&2
  exit 1
fi
if [[ $mode == strict && $missing_later == 1 ]]; then
  echo "error: some tools are missing (--strict)" >&2
  exit 1
fi
