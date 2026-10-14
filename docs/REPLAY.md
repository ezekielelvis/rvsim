# Record and replay

rvsim is deterministic: the guest's execution is a function of its images,
its configuration, and its external input, which is the bytes typed at the
UART and when they arrived (in virtual time, counted in steps). `rvsim
record` captures that input, and `rvsim replay` reproduces the session
exactly, checking state hashes along the way. ADR-028 has the design.

## Record

```sh
rvsim record -o session.rvrec \
  -firmware build/images/fw_jump.bin -kernel build/images/Image \
  -disk build/images/rootfs.ext4
```

Use it like `rvsim boot`, with the same flags. Type at the console. The
recording ends when the guest powers off, at `-max-steps`, or on Ctrl-C.

- `-checkpoint N`: record state hashes every N steps (default 10,000,000).
- `-keep-disk out.img`: the session runs on a private copy of the disk image,
  so the original is never modified. This saves the copy as the session
  left it.

## Replay

```sh
rvsim replay session.rvrec        # prints the console output
rvsim replay -q session.rvrec     # just the verdict
```

Image paths come from the recording, and each image's SHA-256 must match.
On another machine, point at local copies with `-firmware`, `-kernel`,
`-initrd` and `-disk`. A different core build produces a warning, not an
error.

Exit status 0 means every checkpoint and the final state matched:

```
rvsim: replay identical: 178 checkpoints, 890185552 steps, 81301656 instructions (exit)
```

## Debugging a divergence

When replay diverges, it reports:
- the first checkpoint whose hashes differ;
- which part diverged: `core` (registers, CSRs, TLB, counters), `ram`, or
  `vmm` (devices, scheduler, input queue);
- the window between the last matching checkpoint and that one.

For example:

```
rvsim: replay diverged at checkpoint 46 (step 470000000): [vmm a0d1… != recorded 5ca6…];
       state last matched at step 460000000, so the divergence is in steps (460000000, 470000000]
rvsim: to trace the window: rvsim replay -q -trace window.log -trace-from 460000000 session.rvrec
```

1. **Narrow the window.** Record again with a smaller `-checkpoint`, if you
   can reproduce the session.
2. **Trace it.** `-trace-from` replays silently up to the last good step,
   then writes a Spike-format commit log until replay stops at the
   divergence. Compare it with a trace of the reference run, for example a
   replay under the previous rvsim build. The first differing line is the
   divergence.
3. **Find the cause.**
   - A `vmm`-only difference points at device or input state. These are the
     usual suspects: input delivered at a different point within a step,
     an event fired at a different time.
   - A `core` or `ram` difference with an identical `vmm` points at an
     instruction. Use Spike difftest (`make difftest`) to find the first
     instruction rvsim gets wrong.

## Format

`.rvrec` files are little-endian:

```
"RVSIMREC" | version u16 | header length u32 | header (JSON)
record*:  tag u8 | Δstep uvarint | length uvarint | payload
```

`Δstep` is relative to the previous record. The header records:
- the core ABI and build;
- the machine and disk configuration, bootargs, and the checkpoint
  interval;
- the role, path, size and SHA-256 of every image, including the generated
  device tree.

| Tag | Record | Payload |
|---|---|---|
| `0x01` | INPUT | the bytes queued for the UART at this step |
| `0x02` | INPUT_EOF | none: host input ended |
| `0x03` | CHECKPOINT | `instret u64`, `core u64`, `ram u64`, `vmm u64` |
| `0x04` | END | as CHECKPOINT, then `code u64` and the reason (`exit`, `reset`, `limit`, `interrupted`) |
| `0x10–0x1f` | reserved for fault injection | |

Records are flushed as they are written. A recording cut short replays up to
its last complete record, with a warning.
