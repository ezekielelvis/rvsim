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
    brew install cmake dtc go
    brew tap riscv-software-src/riscv
    brew install riscv-software-src/riscv/riscv-gnu-toolchain
  elif command -v apt-get >/dev/null; then
    sudo apt-get update
    sudo apt-get install -y --no-install-recommends \
      build-essential cmake git device-tree-compiler \
      gcc-riscv64-unknown-elf gcc-riscv64-linux-gnu \
      libboost-regex-dev libboost-system-dev
  else
    echo "unsupported host: install tools manually" >&2
    exit 1
  fi
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
