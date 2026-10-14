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

# Suites that also have virtual-memory (-v-) variants.
has_v() {
  case $1 in
    rv64ui | rv64um | rv64ua | rv64uc) return 0 ;;
    *) return 1 ;;
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
    common=(-march="$march" -mabi=lp64 -static -mcmodel=medany -fvisibility=hidden -nostdlib -nostartfiles)
    # -p-: physical memory, M-mode environment.
    "$cc" "${common[@]}" -I"$src/env/p" -I"$src/isa/macros/scalar" -T"$src/env/p/link.ld" \
      -Wl,--no-warn-rwx-segments "$src/isa/$suite/$t.S" -o "$out/$suite-p-$t"
    count=$((count + 1))
    # -v-: the same test in U-mode under a demand-paging Sv39 supervisor.
    if has_v "$suite"; then
      name="$suite-v-$t"
      # Upstream seeds page placement from md5 of the name; any fixed
      # per-test value works, and cksum is portable.
      sum=$(printf %s "$name" | cksum | cut -d' ' -f1)
      entropy=$(printf '0x%07x' $((sum % 268435456)))
      # env/v/vm.c embeds one F instruction as data (to recognize FP tests on
      # non-FP hardware), so these builds need F in -march. The environment
      # has no other FP code, which the check below enforces.
      base=${march%%_*} exts=${march#*_}
      if [[ $base == *c ]]; then base="${base%c}fc"; else base="${base}f"; fi
      vcommon=(-march="${base}_${exts}" "${common[@]:1}")
      "$cc" "${vcommon[@]}" -DENTROPY="$entropy" -std=gnu99 -O2 \
        -I"$src/env/v" -I"$src/isa/macros/scalar" -T"$src/env/v/link.ld" \
        -Wl,--no-warn-rwx-segments "$src/env/v/entry.S" "$src"/env/v/*.c "$src/isa/$suite/$t.S" -o "$out/$name"
      count=$((count + 1))
    fi
  done
done
# The -v- binaries must contain no FP instructions except vm.c's embedded
# fssr, which is jumped over and only compared against.
objdump=${RISCV_PREFIX:-riscv64-unknown-elf-}objdump
for f in "$out"/*-v-*; do
  n=$("$objdump" -d "$f" | awk -F'\t' 'NF >= 3 && $3 ~ /^f/ && $3 !~ /^fence/' | wc -l | tr -d ' ')
  if [[ $n -gt 1 ]]; then
    echo "$f contains $n FP instructions" >&2
    exit 1
  fi
done
echo "built $count tests into $out"
