#!/usr/bin/env bash
# Generate the RISC-V Architectural Certification Tests (ACT4) for rvsim.
#
#   tests/archtest/build-archtest.sh [WORKDIR]   (default: build/archtest)
#
# ACT4 generates self-checking ELFs for a described implementation: it runs
# each test on the Sail reference model, configured to match rvsim, and
# compiles the expected results into the ELF. The ELFs land in
# WORKDIR/rvsim-rv64imac/elfs and are run by `go test ./tests/archtest`.
#
# Needs: mise (installs ACT4's pinned Ruby, uv and bundler), the pinned Sail
# from scripts/toolchain.sh --install, and riscv64-unknown-elf-gcc.
set -euo pipefail

ACT_REPO=https://github.com/riscv-non-isa/riscv-arch-test.git
ACT_COMMIT=d40cf3db24cdb1611b26e07f37d97382d1e08d8c  # act4 branch, 2026-09-28

root=$(cd "$(dirname "$0")/../.." && pwd)
work=${1:-$root/build/archtest}
src=$root/build/src/riscv-arch-test
config=$root/tests/archtest/rvsim-rv64imac/test_config.yaml
export PATH="${RVSIM_TOOLS:-$HOME/.cache/rvsim-tools}/bin:$PATH"

for tool in mise sail_riscv_sim riscv64-unknown-elf-gcc; do
  command -v "$tool" >/dev/null || { echo "missing $tool; run scripts/toolchain.sh --install" >&2; exit 1; }
done

if [[ ! -f $src/.rvsim-commit || $(cat "$src/.rvsim-commit") != "$ACT_COMMIT" ]]; then
  rm -rf "$src"
  mkdir -p "$src"
  git -C "$src" init -q
  git -C "$src" fetch -q --depth 1 "$ACT_REPO" "$ACT_COMMIT"
  git -C "$src" checkout -q FETCH_HEAD
  echo "$ACT_COMMIT" >"$src/.rvsim-commit"
fi

cd "$src"
mise trust -q .mise.toml
mise install -q

# The UDB gem fetches a prebuilt z3 into ~/.cache/udb, but on macOS it fetches
# the Linux aarch64 build, which dlopen rejects. Substitute Homebrew's z3
# (dlopen ignores the .so name).
if [[ $(uname -s) == Darwin ]]; then
  for lib in "${XDG_CACHE_HOME:-$HOME/.cache}"/udb/z3/*/*/libz3.so; do
    [[ -e $lib ]] || continue
    if ! file -L "$lib" | grep -q Mach-O; then
      z3=$(brew --prefix z3 2>/dev/null)/lib/libz3.dylib
      [[ -f $z3 ]] || { echo "need a native z3: brew install z3" >&2; exit 1; }
      ln -sf "$z3" "$lib"
    fi
  done
fi
jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
# ACT4's default exclusions (Sdtrig), plus the interrupt suites: raising
# external, timer and software interrupts needs the CLINT/PLIC MMIO that
# arrives in Phase 4.
exclude=SdtrigSm,SdtrigS,SdtrigU,InterruptsSm,InterruptsS,InterruptsU
mise exec -- make CONFIG_FILES="$config" WORKDIR="$work" EXCLUDE_EXTENSIONS="$exclude" \
  FAST=True CLEAN_INTERMEDIATES=True -j"$jobs"
echo "ACT4 ELFs: $(find "$work/rvsim-rv64imac/elfs" -name '*.elf' | wc -l | tr -d ' ') in $work/rvsim-rv64imac/elfs"
