# rvsim

A deterministic RISC-V full-system emulator. rvsim runs OpenSBI and Linux
to a BusyBox shell on an RV64IMAC hart with an Sv39 MMU, a CLINT, a PLIC, a
16550 UART and a virtio block device. Given the same images and the same
input, it executes exactly the same instructions every time: on any
machine, at any speed, under a debugger or not.

The emulator is a C++ interpreter core (`core/`) driven by a Go VMM
(`vmm/`) that owns the devices, virtual time and the run loop.

![demo: booting Linux, recording a session and replaying it](docs/demo.gif)

*(The demo GIF is a placeholder until one is recorded.)*

## Why determinism, and why fault injection

Most bugs in systems software are hard because they are rare. A race needs
just the wrong interleaving. A driver bug needs a device to misbehave in a
way real hardware seldom does. A crash happens once in a thousand boots, on
someone else's machine.

rvsim makes those runs reproducible:

- **Determinism.** Virtual time is counted in executed steps, not wall
  time. The run loop delivers every external input (keystrokes, the end of
  input) at an exact step and records it. Nothing reads the host clock or a
  random source. A run is therefore a function of its inputs.
- **Record and replay.** `rvsim record` captures a session; `rvsim replay`
  reproduces it, checking hashes of the whole machine state as it goes. A
  bug seen once can be replayed, traced and debugged as often as needed,
  even on another computer.
- **Fault injection.** Bit flips, corrupted registers, bad device reads,
  lost interrupts, failing or corrupted disk requests, noisy serial lines.
  They are triggered at exact points or with seeded probability, and
  recorded, so a faulty run replays exactly too. The rare failures software
  must survive become ordinary, repeatable test cases.
- **Snapshots and reverse debugging.** Snapshot the machine and restore it
  later, exactly. Jump to any step of a recording. Step backwards in GDB.

## Quick start

You need Go 1.26+, CMake 3.24+, a C++20 compiler, and Podman or Docker
(to build the Linux images).

```sh
git clone https://github.com/ezekielelvis/rvsim && cd rvsim
make go-build      # the C++ core and the rvsim binary (bin/rvsim)
make images        # OpenSBI, Linux and BusyBox with Buildroot in a container (slow the first time)
bin/rvsim boot -firmware build/images/fw_jump.bin -kernel build/images/Image -initrd build/images/rootfs.cpio
```

You get a root shell on the console in a few seconds. Type `poweroff` to
leave.

Then try:

```sh
# Boot from the ext4 disk image instead (writes go to a copy with record/snapshot):
bin/rvsim boot -firmware build/images/fw_jump.bin -kernel build/images/Image -disk build/images/rootfs.ext4

# Record a session, then replay it and check it is identical:
bin/rvsim record -o s.rvrec -firmware build/images/fw_jump.bin -kernel build/images/Image -initrd build/images/rootfs.cpio
bin/rvsim replay s.rvrec

# Debug the kernel with GDB, backwards too, on a recording:
bin/rvsim replay -gdb localhost:1234 s.rvrec &
gdb-multiarch build/images/vmlinux -ex 'target remote localhost:1234'

# Inject faults:
bin/rvsim boot -faults docs/faults/flaky-console.yaml -firmware build/images/fw_jump.bin -kernel build/images/Image -initrd build/images/rootfs.cpio
```

## Features

- **CPU:**
  - RV64IMAC with Zicsr, Zifencei and Zicntr, and M/S/U privilege modes;
  - Sv39 paging, with Svade/Svadu and an ASID-tagged TLB;
  - checked against the riscv-tests, Spike (instruction-by-instruction
    difftest) and the riscv-arch-test ACT4 suites (Sail).
- **Platform:** the QEMU `virt` memory map:
  - CLINT, PLIC and a 16550A UART;
  - a virtio-mmio block device;
  - the SiFive test finisher, for power-off.
  
  It boots unmodified OpenSBI (`fw_jump`) and Linux 6.12, with a device tree
  generated to match.
- **Determinism:** virtual time and paced, recorded input. Two runs with the
  same inputs match hash for hash.
- **Record and replay** (`record`, `replay`) with periodic state-hash
  checkpoints and a precise divergence report.
- **Snapshots** (`snapshot`, `restore`) of the whole machine: core, RAM,
  devices, pending events and disk. Compressed, with zero pages skipped.
  Replays can jump to any step.
- **GDB stub** (`gdb`, `replay -gdb`):
  - registers with a target description, and memory through the page tables;
  - breakpoints, stepping and Ctrl-C;
  - `reverse-stepi` and `reverse-continue` on recordings.
- **Fault injection** (`-faults spec.yaml`): seven kinds of fault, with
  step, instruction-count, pc, device-access and probability triggers. All
  recorded and replayable.
- **Tracing** (`-trace`, `trace`): Spike-compatible commit logs.
- **Performance:** about 60–70 MIPS while booting Linux, about 1.3 s to the
  shell on a laptop, thanks to a decoded-instruction cache and a fetch fast
  path.

Out of scope: JIT compilation, multiple harts, F/D/V extensions,
networking and graphics.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): the C++/Go split, the exit
  model, virtual time, the memory map and performance.
- [docs/REPLAY.md](docs/REPLAY.md): recording, replaying, snapshots, and
  debugging a divergence.
- [docs/FAULTS.md](docs/FAULTS.md): the fault spec format, with worked
  examples.
- [docs/DECISIONS.md](docs/DECISIONS.md): every design decision, as ADRs.
- `rvsim <command> -h`: help for each command (`run`, `boot`, `record`,
  `replay`, `snapshot`, `restore`, `gdb`, `trace`, `fault`).

## Testing

```sh
make test           # C++ unit tests, Go tests, vet and staticcheck
make sanitize       # the C++ tests under ASan and UBSan
scripts/toolchain.sh --install   # cross GCC, Spike, Sail (for the next three)
make compliance difftest archtest
make linux          # boot Linux; record, replay, snapshot, fault-inject and debug it (needs make images)
make perf           # interpreter MIPS and boot time
```

CI runs all of these on Linux and macOS. It also replays, on macOS, a Linux
session recorded on Ubuntu.
