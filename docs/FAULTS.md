# Fault injection

rvsim injects faults into a running guest from a YAML spec: bit flips,
corrupted registers, bad MMIO reads, lost or late interrupts, failing or
corrupting disk requests, and dropped or garbled serial bytes. Faults are
deterministic. Probabilistic faults use a seeded generator, every fault
applied is logged, and `rvsim record` stores them in the recording, so
`rvsim replay` reproduces a faulty run exactly. ADR-031 has the design.

```sh
rvsim boot   -faults spec.yaml [boot flags]          # inject, print each fault to stderr
rvsim record -faults spec.yaml -o run.rvrec [flags]  # inject and record
rvsim replay run.rvrec                               # re-apply the recorded faults
```

`-faults` also works with `rvsim snapshot` and `rvsim gdb`.

## Spec format

```yaml
seed: 42            # seeds chance triggers and random values (default 0)
faults:
  - name: my-fault  # optional; used in the log (default fault1, fault2, ...)
    when: {...}     # exactly one trigger
    do: {...}       # exactly one action
    times: 1        # injections (default 1; 0 = unlimited, for chance triggers)
```

Unknown fields are errors. Numbers can be decimal or hex (`0x...`).

### Triggers (`when`)

| Trigger | Fires |
|---|---|
| `step: N` | at virtual time N (steps) |
| `instret: N` | when N instructions have retired; the run loop stops exactly there |
| `pc: {addr: A, hit: N}` | the Nth time (default 1) the hart is about to execute the instruction at virtual address A. With `times` > 1, it also fires on every later arrival |
| `access: {device: D \| addr: A, nth: N, kind: read\|write\|any}` | on the Nth (default 1) MMIO access to device D (`uart`, `plic`, `finisher`, `virtio-blk`) or address A |
| `chance: {p: P}` | on each event of the action's kind, with probability P (0 < P ≤ 1) |

### Actions (`do`)

**Immediate actions** happen when the trigger fires:

| Action | Effect |
|---|---|
| `flip_bit: {addr: A, bit: B}` | flips bit B (0–7) of the RAM byte at physical address A |
| `reg: {reg: R, set: V}` | sets register R (`x0`–`x31`, ABI names, or `pc`) to V |
| `reg: {reg: R, xor: V}` | XORs register R with V |
| `reg: {reg: R, random: true}` | sets register R to a random value |

**Event actions** apply to the next event of their kind after the trigger
fires (or, with `chance`, to the event that fired it):

| Action | Event | Effect |
|---|---|---|
| `mmio_read: {value: V}` or `{random: true}` | an MMIO read, optionally narrowed with `device:` or `addr:` | the read returns V or a random value. The device is still read, so its side effects happen. With an `access` read trigger, it applies to that very access |
| `irq: {source: S, drop: true}` | the next assertion of PLIC source S (UART is 10, virtio-blk is 1) | the interrupt is lost until the device deasserts and asserts again |
| `irq: {source: S, delay: N}` | the same | the interrupt arrives N steps late, if the device still asserts it |
| `blk: {fail: true}` | the next virtio-blk request | completes with `VIRTIO_BLK_S_IOERR` |
| `blk: {corrupt: true, offset: O, xor: X}` | the next virtio-blk read | byte O of the data is XORed with X (default `0xff`) |
| `uart: {dir: tx\|rx, drop: true}` | the next byte the guest sends (`tx`) or receives (`rx`) | the byte is lost |
| `uart: {dir: tx\|rx, garble: true, xor: X}` | the same | the byte is XORed with X (default random) |

### The log

Each fault applied prints, and records, its concrete effect:

```
rvsim: fault "dead-sector" at step 244755265: failed a virtio-blk request with an I/O error
rvsim: fault "noisy-uart" at step 2042360: garbled UART tx byte 0x20 (xor 0x5b)
```

A recording stores these as `FAULT` records (tag `0x10`): the step, the
event and which event of that kind at that step, and the resolved values.
Replay applies them at the same points without re-evaluating triggers or the
random generator.

## Example 1: a noisy serial line

[`faults/flaky-console.yaml`](faults/flaky-console.yaml) garbles 2% of the
guest's UART output, every run the same way:

```yaml
seed: 2026
faults:
  - name: noisy-uart
    when: {chance: {p: 0.02}}
    do: {uart: {dir: tx, garble: true}}
    times: 0
```

```sh
rvsim boot -max-steps 150000000 -faults docs/faults/flaky-console.yaml \
  -firmware build/images/fw_jump.bin -kernel build/images/Image -initrd build/images/rootfs.cpio
```

OpenSBI's banner and the kernel log arrive with occasional wrong bytes, for
example `1latform CPPC Device`. stderr lists all 130 garbled bytes, each with
its step and XOR pattern. Running the command again produces byte-for-byte
the same output.

## Example 2: a disk that loses writes, recorded and replayed

[`faults/dead-sector.yaml`](faults/dead-sector.yaml) fails two disk requests
after step 200,000,000:

```yaml
faults:
  - name: dead-sector
    when: {step: 200000000}
    do: {blk: {fail: true}}
    times: 2
```

Record an interactive session booting from the ext4 disk, then replay it:

```sh
rvsim record -faults docs/faults/dead-sector.yaml -o dead.rvrec \
  -firmware build/images/fw_jump.bin -kernel build/images/Image -disk build/images/rootfs.ext4
# at the shell:  dd if=/dev/vda of=/dev/null bs=4096 skip=6000 count=1; poweroff -f
rvsim replay -q dead.rvrec
```

In the recorded session, the `dd` ran before step 200 M and succeeded. The
next requests after the trigger were the journal writes made during
`poweroff`, so those failed:

```
[    2.305832] I/O error, dev vda, sector 32770 op 0x1:(WRITE) flags 0x9800 phys_seg 1 prio class 2
[    2.306024] Buffer I/O error on dev vda, logical block 16385, lost sync page write
```

Which requests a fault hits depends on when you typed. The recording
captures that, and the replay reproduces the same faults on the same
requests:

```
rvsim: fault "dead-sector" at step 244755265: failed a virtio-blk request with an I/O error
rvsim: fault "dead-sector" at step 244837530: failed a virtio-blk request with an I/O error
rvsim: replay identical: 24 checkpoints, 245019897 steps, 65304927 instructions (exit)
```

The `TestBlkFailIsAnIOErrorInTheGuest` test arms the same fault just before
a read: `dd` then fails with `dd: /dev/vda: I/O error`.

## Example 3: a corrupted kernel argument

[`faults/null-execve.yaml`](faults/null-execve.yaml) clears `a0` on entry to
`kernel_execve`, the function the kernel uses to start `/init`:

```yaml
faults:
  - name: null-filename
    when: {pc: {addr: 0xffffffff8009aa80}}   # nm build/images/vmlinux | grep ' kernel_execve$'
    do: {reg: {reg: a0, set: 0}}
```

```sh
rvsim boot -max-steps 300000000 -faults docs/faults/null-execve.yaml \
  -firmware build/images/fw_jump.bin -kernel build/images/Image -initrd build/images/rootfs.cpio
```

```
rvsim: fault "null-filename" at step 67011418: set a0 to 0x0
[    0.530547] Run /init as init process
[    0.530916] Unable to handle kernel NULL pointer dereference at virtual address 0000000000000000
[    0.531063] Oops [#1]
[    0.531446] epc : strlen+0x6/0x18
[    0.531611]  ra : getname_kernel+0x24/0xbc
```

The NULL filename reaches `getname_kernel` and then `strlen`. With
`rvsim gdb` and a `pc` breakpoint on `getname_kernel`, you can inspect the
moment before the crash. With `rvsim record` and `rvsim replay -gdb`, you
can step backwards from the oops to the corrupted register.
