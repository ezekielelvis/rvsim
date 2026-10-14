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

---

## ADR-007: No PMP; PMP CSRs raise illegal-instruction

**Status:** Accepted (Phase 1)

Priv 1.12 §3.7 allows zero PMP entries but does not say whether the PMP CSRs
then trap or read as zero. We implement no PMP, and accessing `pmpcfg*` or
`pmpaddr*` raises illegal-instruction. This matches Spike with
`--pmpregions=0`, so difftest needs no special handling. OpenSBI and the
riscv-tests environment probe PMP with trap-guarded accesses and handle
both outcomes.

---

## ADR-008: Values written to `mtval`

**Status:** Accepted (Phase 1)

Priv §3.1.16 allows `mtval` to be zero or informative for several causes. We
match Spike:

| Cause | mtval |
|---|---|
| Illegal instruction | The instruction bits |
| Breakpoint (EBREAK) | The pc of the EBREAK |
| Misaligned fetch | The misaligned target address |
| Misaligned / access-fault load or store | The faulting address |
| Instruction access fault | The fetch address |
| ECALL | 0 |

When a load or store is both misaligned and out of range, the
misaligned exception wins. Table 3.7 allows either order.

---

## ADR-009: WFI rules and idle exits

**Status:** Accepted (Phase 1; idle exits added in Phase 3)

Priv §3.3.3 lets WFI in a less-privileged mode either complete within a
bounded time or raise illegal-instruction. rvsim, like Spike:

- raises illegal-instruction below M-mode when `mstatus.TW` = 1;
- raises it in U-mode, since S-mode is implemented;
- otherwise retires WFI and leaves the hart idle.

An idle hart makes `rv_run` return `RV_EXIT_WFI` without using any steps. The
VMM then advances virtual time with `rv_advance_idle`. The hart resumes once
an interrupt is pending in `mip & mie`, whatever the global enables and
delegation say. If that interrupt is enabled it is taken, with `xepc` pointing
after the WFI.

---

## ADR-010: Difftest configures Spike to mirror rvsim's feature set

**Status:** Accepted (Phase 1)

Difftest compares per-instruction commit logs, so any configuration
difference shows up as a divergence. Spike is invoked with:

- `--isa` = exactly what rvsim implements in the current phase (Phase 1:
  `rv64i_zicsr_zifencei`; Phase 2: `rv64imac_zicsr_zifencei`), plus
  `_zicclsm` for tests that need
  misaligned access (rvsim runs those with `RV_CFG_MISALIGNED_OK`).
- `--priv` = the implemented modes (Phase 1: `MU`).
- `--pmpregions=0` (ADR-007).
- `--triggers=0`: rvsim has no Sdtrig. With triggers enabled, Spike's MRET
  also writes `tcontrol`. This was the first divergence difftest found.

Spike's boot-ROM commits (pc below RAM) are skipped. Spike keeps running
briefly after the HTIF exit store, so only rvsim's log length is compared.

**Phase 3 additions:** Spike runs with `_zicntr`, `--priv=MSU`, and a
device tree whose `mmu-type` is `riscv,sbare` until Phase 5 (ADR-016).
Commits that read `mvendorid`/`marchid`/`mimpid` are compared without the
value read, since implementation IDs rightly differ (Spike's `marchid` is 5).
Tests excluded from difftest only, with their reasons in
`tests/riscvtests/riscvtests.go`: `rv64mi-p-ma_fetch` (Spike's `misa.C` is
writable; rvsim's `misa` is read-only) and `rv64mi-p-illegal` (ADR-016).

**Phase 1 scope note:** the riscv-tests environment uses CSRs, `mret`,
U-mode and `ecall` before any RV64I test code runs. So Phase 1 includes a
minimal M/U privilege implementation: the M-mode trap CSRs, MRET, and
illegal-instruction for unimplemented CSRs. Phase 3 extends it to the full
M/S/U set.

---

## ADR-011: Trace record format v1 (`core/include/rv_trace.h`)

**Status:** Accepted (Phase 1)

Each step emits exactly one fixed-size 56-byte record: either a COMMIT
(written GPR, CSR write, memory access) or a TRAP (cause, tval, handler pc).
Because it is one record per step, the core can check for buffer space once
per step, before executing, and exit with `RV_EXIT_TRACE_FULL` without ever
splitting a step's records. Go formats COMMIT records as Spike
`--log-commits` text; TRAP records are kept for debugging (Spike does not
log traps). A COMMIT holds at most one CSR write; if a later extension needs
more (e.g. a `sstatus` write that Spike logs as several CSRs), the format
version is bumped.

---

## ADR-012: LR/SC reservation semantics

**Status:** Accepted (Phase 2)

Unpriv §14.2 leaves the reservation set's size, and most invalidation events,
to the implementation. rvsim follows Spike where it can:

- LR records the reserved physical address. An SC succeeds only if the
  reservation is on exactly that address (the size is not compared).
- **Every SC clears the reservation**, whether it succeeds or fails.
- An SC that would fail still checks alignment and RAM bounds, raising
  store/AMO misaligned or access-fault.
- Plain stores by this hart, traps, and xRET leave the reservation alone. The
  spec only requires an SC to fail after another hart's store to the
  reservation set, and lets xRET clear the reservation but does not require
  it (Priv §3.3.2).
- A write by another agent (`rv_write_phys`: DMA or debugger) into the
  aligned doubleword containing the reserved address clears it.
- `rv_reset` clears it.
- LR, SC and AMOs need natural alignment even when
  `RV_CFG_MISALIGNED_OK` is set. LR raises load-misaligned and load
  access-fault; SC and AMOs raise the store/AMO versions.

**Spike difference.** Spike also drops the reservation at the end of every
scheduling quantum. `sim_t::step` charges the full 5000-step quantum even
when the hart stops early at a trap, so the drop lands about 5000 steps after
the last trap. A Spike SC can therefore fail where rvsim's succeeds. Both are
legal, since an SC may fail for implementation-specific reasons. Difftest
handles exactly this case: when Spike's SC fails at the same pc where
rvsim's succeeded, it skips Spike's retry lines (at most 16) until the logs
agree again, and reports the count. Any other difference is still a
divergence. In Phase 2 this happens once, in `rv64ua-p-lrsc`.

---

## ADR-013: HTIF command latching and the console device

**Status:** Accepted (Phase 2)

HTIF is a host convention, not part of the ISA. Guests write the 64-bit
`tohost` command as two 32-bit stores, low word first: riscv-tests,
ACT4's halt macros and its console macro all do this. For console output the
low word alone is just the character, so acting on the first store would turn
an odd character into a bogus "exit" command. Spike avoids this only because
its host polls `tohost` between quanta.

rvsim **latches a command when a store writes the most significant byte of
`tohost`** and leaves `tohost` non-zero. A doubleword store or AMO covers the
whole word and latches at once. The core then exits with `RV_EXIT_HALT` and
the command in `value`. The Go side implements fesvr's commands:

| Command | Meaning | Host action |
|---|---|---|
| device 0, command 0, bit 0 set | exit with code `payload >> 1` | stop |
| device 1, command 1 | console putchar | write the byte, clear `tohost`, set `fromhost = 1<<56 \| 1<<48 \| 0x100 \| char` |

Other commands (for example the syscall proxy) are reported as unsupported.

---

## ADR-014: Architecture tests come from ACT4 with Sail 0.14.1

**Status:** Accepted (Phase 2)

riscv-arch-test has replaced RISCOF with the ACT4 framework. ACT4 generates
self-checking ELFs for a described implementation: it runs each test on the
Sail reference model, configured to match the implementation, and compiles
the expected results into the test.

- **rvsim's description** is in `tests/archtest/rvsim-rv64imac/`: a UDB
  configuration, `sail.json`, a linker script for 256 MiB of RAM at
  `0x8000_0000`, and HTIF `rvmodel_macros.h`. It was derived from ACT4's
  `sail-RVI20U64` config and edited to match rvsim: no F/D/Zicntr/Zihpm,
  misaligned loads, stores and atomics raise address-misaligned (ADR-004,
  ADR-012), and U-mode is implemented.
- **Unprivileged tests only for now** (`include_priv_tests: False`). The
  privileged parameters (counters, `mtvec` write behaviour, U-mode details)
  become accurate when the privileged tests are enabled in Phase 3.
- **Phase 3:** privileged tests are enabled. The config declares S, U, Zicntr,
  Svbare and a standard M-mode (`STANDARD_SM_SUPPORTED`). The interrupt
  suites (`InterruptsSm/S/U`) are excluded until the CLINT/PLIC interrupt
  hooks exist in Phase 4. Sdtrig stays excluded, as in ACT4's defaults.
- **Phase 4:** the interrupt suites run, using the real UART and PLIC
  (ADR-024).
- **Pinned versions:** ACT4 commit `d40cf3d` (2026-09-28) and Sail 0.14.1,
  the version that commit's own CI uses. Its README names 0.15, released
  2026-10-06.
- **Pass criterion:** a test passes only if it prints
  `RVCP-SUMMARY: TEST PASSED` and then exits with code 0.

---

## ADR-015: Counters follow Spike's CPI = 1 model

**Status:** Accepted (Phase 3). Refines ADR-001.

ADR-001 said `mcycle` counts steps. Spike, the difftest reference, advances
`mcycle` and `minstret` together, once per retired instruction. Traps and idle
time count for neither. riscv-tests read `cycle` and `instret` into registers,
so any other model shows up as a difftest divergence. So:

- `mcycle` and `minstret` advance once per retired instruction, unless
  `mcountinhibit.CY`/`IR` is set.
- An explicit CSR write to either one replaces that instruction's own
  increment, so software reads back what it wrote.
- Virtual time is unchanged from ADR-001. Steps still include traps and
  idle time, and `time`/`mtime` = steps / `steps_per_tick` (+ offset,
  ADR-020).
- `rv_exit.instret` counts retired instructions since reset. Writes to
  `minstret` do not affect it.
- Zicntr: `cycle`, `time` and `instret` are accessible below M-mode only
  when `mcounteren` allows (and, in U-mode, `scounteren` too).
  `mcountinhibit` has CY and IR.

---

## ADR-016: S-mode before the MMU

**Status:** Accepted (Phase 3; superseded for each field when Sv39 lands in
Phase 5)

S-mode exists from Phase 3, but address translation only arrives in Phase 5.
Until then:

- `satp` is read-only Bare. A write naming an unsupported mode has no effect
  (Priv §4.1.11).
- `mstatus.SUM` and `menvcfg`/`senvcfg.FIOM` are read-only zero. Priv
  §3.1.6.3 allows SUM to be when `satp.MODE` is read-only zero.
- `mstatus.MXR` and `TVM` are writable. Priv §3.1.6.3 and §3.1.6.5 make them
  read-only zero only when S-mode is absent. TVM traps `satp` accesses and
  SFENCE.VMA in S-mode.
- SFENCE.VMA is permitted in S-mode and M-mode when TVM = 0 (§3.1.6.5). It
  does nothing yet, because there is no TLB.

Spike without an MMU makes MXR and TVM read-only and SFENCE.VMA illegal.
Sail and the ACT4 certification tests follow the spec text, as rvsim does.
So `rv64mi-p-illegal` is excluded from difftest only (it passes in
compliance) until Phase 5, when both models have Sv39 and agree again.
Difftest gives Spike a device tree with `mmu-type = "riscv,sbare"` to match.

---

## ADR-017: Trace record format v2

**Status:** Accepted (Phase 3). Supersedes the layout in ADR-011.

Spike logs a `sie` write as two entries, `c260_sie` and `c772_mie`. Records grow
from 56 to 64 bytes, adding `csr2`/`csr2_value` and the `RV_TF_CSR2` flag
(`RV_TRACE_VERSION` 2). The CSR entries follow Spike's logging:

| CSR written | Logged as |
|---|---|
| `sstatus` | `mstatus` (full value) |
| `sip` | `mip` |
| `sie` | `sie`, then `mie` |
| any other | itself |

Difftest also removes Spike's `c777_mvip` entries. Spike implements `mip`
through the AIA `mvip` register, and rvsim has no AIA.

---

## ADR-018: A reserved xtvec MODE leaves the register unchanged

**Status:** Accepted (Phase 3)

MODE values 2 and 3 in `mtvec`/`stvec` are reserved (Priv §3.1.7), and WARL
allows any legal result. rvsim ignores such a write entirely
(`MTVEC_ILLEGAL_WRITE_BEHAVIOR: retain`). This is what Sail does, and so what
the ACT4 tests expect. Spike instead clears bit 1. No riscv-tests program
writes a reserved MODE.

---

## ADR-019: No hardware performance counters

**Status:** Accepted (Phase 3)

rvsim implements no HPM counters. `mhpmcounter3–31` and `mhpmevent3–31`
raise illegal-instruction, and so do `hpmcounter3–31` (no Zihpm).
`mcounteren`/`scounteren`/`mcountinhibit` have only the CY/TM/IR bits.
RISC-V's UDB defines `mhpmcounterN`/`mhpmeventN` only when
`HPM_COUNTER_EN[N]` is set, and Sail and the ACT4 tests follow that. Priv 1.12
§3.1.10 also allows read-only-zero registers, which is Spike's choice. OpenSBI
probes these CSRs with trap-guarded accesses, so both work.

Sail's `medeleg`/`mideleg` delegatable masks and `writable_fiom` in
`tests/archtest/rvsim-rv64imac/sail.json` are set to rvsim's own choices:
`medeleg` 0x3FE, `mideleg` 0x222, FIOM read-only zero without an MMU.

---

## ADR-020: The CLINT's MMIO window is in the core from Phase 3

**Status:** Accepted (Phase 3; brought forward from Phase 4)

ACT4's standard M-mode boot writes `mtimecmp`, so the privileged tests need
the CLINT's memory-mapped registers. The CLINT was always going to be in C++
(ADR-001), and needs no MMIO exits, so it arrived a phase early. It uses the
SiFive layout of QEMU virt, Spike and OpenSBI:

| Register | Offset | Notes |
|---|---|---|
| `msip` | `+0x0000` | 32-bit; bit 0 drives `mip.MSIP` |
| `mtimecmp` | `+0x4000` | 64-bit |
| `mtime` | `+0xBFF8` | 64-bit |

- Aligned 4- and 8-byte accesses work; byte and halfword accesses raise
  access faults, as do atomics. Unmapped offsets read as zero and ignore
  writes.
- `mtime` = steps / `steps_per_tick` + an offset, which a write to `mtime`
  sets. Virtual time stays deterministic.
- MTIP = `mtime >= mtimecmp`. `mtimecmp` resets to 0, as in Spike, so MTIP is
  pending until software programs it.
- `rv_exit.timer_step` is the step at which MTIP rises, or the current step
  if it is already pending.
- `rv_read_phys`/`rv_write_phys` reach the CLINT with the same rules.

---

## ADR-021: Go must relink when the C++ core changes

**Status:** Accepted (Phase 3)

Go's build cache tracks Go sources and flags, not the `librvcore.a` named in
`#cgo LDFLAGS`, and it keys a link on the *compiled* contents of its
dependencies. After a C++-only change, `go build`/`go test` could link
against stale core code without any error. This happened during Phase 3:
`bin/rvsim` kept running old CSR masks.

The fix: after every core build, CMake writes `vmm/machine/zz_corehash.go`
(gitignored). It sets a package variable to the library's SHA-256 in
`init()`, which puts the hash into the compiled package so dependents relink.
`rvsim version` prints it as the core build ID. CI builds from scratch, so
earlier pushed results were not affected.

---

## ADR-022: MMIO exits complete by re-executing the instruction

**Status:** Accepted (Phase 4)

A load or store outside RAM and the CLINT stops the instruction before it
changes any state. `rv_run` returns `RV_EXIT_MMIO_READ`/`WRITE` with the
address, size and (for writes) the value. After `rv_complete_mmio(value,
fault)`, the next `rv_run` re-executes the same instruction. When it reaches
the same access it takes the supplied result (or raises a load/store access
fault) and retires.

This is safe because no load or store has side effects before its memory
access. It also keeps the executor free of a separate "finish this
instruction" path, so the trace record, counters and step accounting are
written once, at retirement. Further rules:

- Pending interrupts are not taken between the exit and the instruction
  retiring.
- MMIO accesses must be naturally aligned, or they raise access faults,
  even with `RV_CFG_MISALIGNED_OK`. Atomics and instruction fetches outside
  RAM also fault without exiting.
- If re-execution ever reached a different access, the core would exit with
  `RV_EXIT_ERROR`/`RV_ERROR_MMIO_REPLAY` and refuse to run further.
- The VMM completes an access to an address with no device as an access fault.

---

## ADR-023: Host input reaches the guest only through scheduled events

**Status:** Accepted (Phase 4)

UART input (stdin, or `vm.ScheduleInput` in tests and replays) is queued by
the run loop and delivered one byte per scheduler event, `InputSpacing`
(10,000) steps apart. Two consequences:

- **Pacing:** a burst of host input arrives at a rate the guest can drain,
  rather than all at once into a 16-byte FIFO.
- **Flow control:** while the guest's receive FIFO is full, the next byte
  waits for the next slot instead of being lost to an overrun. Real UARTs
  have no flow control; this is a property of the virtual "host side",
  documented as such.

A reader goroutine only sends bytes over a channel. The run loop alone
decides when they are delivered: at the current virtual time if the hart is
running, or while the hart idles in WFI with nothing else to wake it, at
which point virtual time does not move while the loop waits. Phase 8 records
these delivery steps for replay.

---

## ADR-024: Device models

**Status:** Accepted (Phase 4)

**PLIC** (`riscv,plic0`, QEMU virt layout):
- 32 sources (ID 0 reserved), with 3-bit priorities.
- Hart 0 has two contexts: context 0 (M-mode) drives MEIP, context 1 (S-mode)
  drives SEIP.
- Sources have level-triggered gateways. A claim takes the highest priority
  above the threshold, ties going to the lowest ID.
- A completion for a source not enabled in that context is ignored
  (PLIC spec §9).
- Only 32-bit accesses are defined.
- The PLIC tells the core about MEIP/SEIP only when they change.

**UART** (16550A, `ns16550a`, byte registers, PLIC source 10):
- Transmission is instantaneous. A THRE interrupt is raised again after each
  transmit, cleared by reading IIR or writing THR, as in QEMU.
- With the FIFO enabled, received data below the trigger level reports
  immediately as a character timeout, instead of after four character times.
  Drivers treat the two the same way, and this keeps the device free of
  timers.
- Modem status reads as connected (DCD, DSR, CTS). Loopback mode is supported.

The run loop lives in a new `vmm/vm` package, keeping `vmm/machine` a thin
cgo wrapper that only it imports.

**ACT4 interrupt suites:** the `InterruptsSm`/`InterruptsS` tests raise
external interrupts through these real devices. The rvmodel macros enable the
UART's THRE interrupt, which goes through the PLIC, following ACT4's CVW
configuration. Supervisor software interrupts go through `mip.SSIP`.
