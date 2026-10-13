# Architecture Decision Records

Each entry records a decision that was proposed, approved, and is now binding.
Superseding a decision means adding a new ADR, not editing an old one.

---

## ADR-001: The C++/Go boundary ABI (`core/include/rv.h`)

**Status:** Accepted (Phase 0)

**Context.** The C++ core runs the per-instruction hot path; the Go VMM owns
devices, time, replay, and tooling. A cgo call costs tens of nanoseconds, so
crossings must be rare and must not allocate.

**Decision.** A KVM-style "run until exit" C ABI, with these refinements over
the initial sketch:

1. **Virtual time is measured in *steps*, not `instret`.** A step is a retired
   instruction, a taken trap, or one unit of idle time skipped during WFI.
   `mtime = steps / steps_per_tick`; `mcycle` counts steps; `minstret` stays
   architectural. Otherwise WFI could not advance time, and a guest stuck in
   a trap loop would never exhaust its budget.
2. **Both MMIO exits leave the access pending.** The instruction has not
   retired; `rv_complete_mmio(value, access_fault)` finishes it, optionally as
   a load/store access fault (cause 5/7). This supports device-failure fault
   injection and gives one uniform completion rule.
3. **Two external IRQ lines** (`RV_IRQ_MEIP`, `RV_IRQ_SEIP`) set as levels,
   matching the PLIC's M and S contexts. `mip.SEIP` reads as software bit OR
   external line (Priv §3.1.9).
4. **The exit struct lives in C memory** (`rv_exit_info`), stable for the
   machine's lifetime: no per-exit Go allocation or pointer passing.
5. **Go may write RAM only through `rv_write_phys`**, so the core can
   invalidate its decode cache and LR reservation. `rv_ram_view` is a
   read-only view for snapshots and hashing.
6. **Core snapshots exclude RAM** but include the TLB. Restoring with an empty
   TLB would diverge from an uninterrupted run whenever the guest has edited a
   PTE without `sfence.vma` yet.
7. **Tracing writes binary records to a C++ ring buffer**; Go formats Spike
   `--log-commits` text and drains it on `RV_EXIT_TRACE_FULL`.
8. **No asynchronous stop call.** The run loop caps each batch (~1M steps), so
   debugger interrupts are handled between batches.
9. **Layout is ABI.** `rv_exit` (56 bytes) and `rv_config` (104 bytes) are
   pinned with static assertions in both C and C++.

Addition made during Phase 0 implementation: `RV_E_UNSUPPORTED` (-10) for
entry points declared but not yet implemented, so callers can tell "not built
yet" from a real error. `RV_CFG_ALL_FLAGS` lets `rv_create` reject unknown
flags.

**Consequences.** Phase 0 baseline for an empty `rv_run` round trip:
31–57 ns/exit on an Apple Silicon laptop (noisy, unpinned runs) and ~37 ns/exit
in an arm64 Linux container, 0 allocations per exit. A controlled measurement
goes into `docs/ARCHITECTURE.md` in Phase 4.

---

## ADR-002: Memory map compatible with QEMU `virt`

**Status:** Accepted (Phase 0)

| Region | Base | Size |
|---|---|---|
| CLINT | `0x0200_0000` | 64 KiB |
| PLIC | `0x0C00_0000` | 64 MiB |
| UART 16550 | `0x1000_0000` | 256 B |
| virtio-mmio | `0x1000_1000` | 4 KiB per device |
| RAM | `0x8000_0000` | 256 MiB default |

OpenSBI's generic platform and stock Linux configs are well tested on this
layout, which removes a class of boot-time surprises.

---

## ADR-003: Page-table A/D bits are updated by hardware

**Status:** Accepted (Phase 0; implemented in Phase 5)

Priv 1.12 §4.3.1 allows either hardware update or a page fault when A is
clear (or D is clear on a store). We update in hardware (`RV_CFG_HW_AD_UPDATE`,
on by default): it avoids a trap on the first access to every page. Spike is
run with `_svadu` in its ISA string to match during difftest.

---

## ADR-004: Misaligned loads and stores trap

**Status:** Accepted (Phase 0; implemented in Phase 1)

Unpriv §2.6 lets misaligned accesses either complete or raise an
address-misaligned exception. We trap by default (`RV_CFG_MISALIGNED_OK`
off). This is Spike's default, OpenSBI emulates misaligned accesses for S/U
mode, and it avoids accesses that split across a page fault.

---

## ADR-005: Pinned tool and image versions

**Status:** Accepted (Phase 0)

| Tool | Version | Notes |
|---|---|---|
| Go | `go 1.26` in `go.mod` | Oldest supported release (Oct 2026). The brief's "1.25" was already EOL. |
| GoogleTest | v1.18.0 | CMake FetchContent |
| staticcheck | 2026.2.2 | `go tool` directive. 2026.2.1 crashes on Go 1.27.2 export data. |
| Spike | `cae29438a126` (2026-09-12) | Built from source by `scripts/toolchain.sh --install` |
| riscv64-unknown-elf-gcc | Homebrew `riscv-gnu-toolchain` / Ubuntu `gcc-riscv64-unknown-elf` | Has the `rv64imac/lp64` multilib |
| Linux | 6.12 LTS | Phase 6 |
| OpenSBI | v1.5.1 | Phase 6 |
| BusyBox | 1.36.1 | Phase 6 |

---

## ADR-006: Linux images build in a container on macOS

**Status:** Accepted (Phase 0)

macOS has no `riscv64-linux-gnu` toolchain package, and kernel builds fail on
case-insensitive APFS. Image builds (OpenSBI, kernel, BusyBox) run in a pinned
Debian container. The scripts accept either `docker` or `podman`. CI runs
natively on Ubuntu.
