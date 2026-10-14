#!/usr/bin/env bash
# Fetch riscv-tests at a pinned commit and build the suites rvsim targets.
#
#   tests/compliance/build-riscv-tests.sh [OUT_DIR]   (default: build/riscv-tests)
#
# Tests are compiled for the ISA rvsim implements, not upstream's rv64g:
# the upstream flags would emit F/D setup code that a no-FPU hart rejects.
set -euo pipefail

RISCV_TESTS_REPO=https://github.com/riscv-software-src/riscv-tests.git
RISCV_TESTS_COMMIT=bcffa2b3188b040c611f90dc0b6e422f54775a09  # master, 2026-09-25

root=$(cd "$(dirname "$0")/../.." && pwd)
out=${1:-$root/build/riscv-tests}
src=$root/build/src/riscv-tests
cc=${RISCV_PREFIX:-riscv64-unknown-elf-}gcc

# Suites to build, and the -march each is assembled with. (A function rather
# than an associative array: macOS ships bash 3.2.)
# Each suite is assembled for exactly the extension it tests, as upstream
# does, so e.g. rv64ui exercises 32-bit encodings only.
suites="rv64ui rv64um rv64ua rv64uc rv64mi rv64si"
suite_march() {
  case $1 in
    rv64ui) echo rv64i_zicsr_zifencei ;;
    rv64um) echo rv64im_zicsr_zifencei ;;
    rv64ua) echo rv64ia_zicsr_zifencei ;;
    rv64uc) echo rv64ic_zicsr_zifencei ;;
    rv64mi | rv64si) echo rv64imac_zicsr_zifencei ;;
    *) echo "unknown suite $1" >&2; exit 1 ;;
  esac
}

# Tests for extensions rvsim does not implement (Zacas).
skipped() {
  case $1 in
    amocas_*) return 0 ;;
    *) return 1 ;;
  esac
}

if [[ ! -f $src/.rvsim-commit || $(cat "$src/.rvsim-commit") != "$RISCV_TESTS_COMMIT" ]]; then
  rm -rf "$src"
  mkdir -p "$src"
  git -C "$src" init -q
  git -C "$src" fetch -q --depth 1 "$RISCV_TESTS_REPO" "$RISCV_TESTS_COMMIT"
  git -C "$src" checkout -q FETCH_HEAD
  git -C "$src" submodule update -q --init --depth 1 env
  echo "$RISCV_TESTS_COMMIT" >"$src/.rvsim-commit"
fi

mkdir -p "$out"
count=0
for suite in $suites; do
  march=$(suite_march "$suite")
  # The Makefrag lists the suite's tests as "<suite>_sc_tests = a b c \ ...".
  tests=$(sed -n "/^${suite}_sc_tests = /,/^\$/p" "$src/isa/$suite/Makefrag" |
    sed "s/^${suite}_sc_tests = //" | tr -d '\\' | tr -s ' \t\n' ' ')
  for t in $tests; do
    if skipped "$t"; then continue; fi
    "$cc" -march="$march" -mabi=lp64 -static -mcmodel=medany -fvisibility=hidden \
      -nostdlib -nostartfiles \
      -I"$src/env/p" -I"$src/isa/macros/scalar" -T"$src/env/p/link.ld" \
      "$src/isa/$suite/$t.S" -o "$out/$suite-p-$t"
    count=$((count + 1))
  done
done
echo "built $count tests into $out"
