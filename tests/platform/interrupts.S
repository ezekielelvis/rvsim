# Bare-metal platform test (Phase 4): UART output, a CLINT timer interrupt,
# and a UART receive interrupt delivered through the PLIC, all in M-mode.
# Prints "hello\ntimer\nrx:<c>\ndone\n" and exits through HTIF.

.equ UART,         0x10000000
.equ UART_IER,     1
.equ UART_LSR,     5
.equ MTIMECMP,     0x02004000
.equ MTIME,        0x0200bff8
.equ PLIC,         0x0c000000
.equ UART_IRQ,     10
.equ PLIC_ENABLE0, PLIC + 0x2000
.equ PLIC_THRESH0, PLIC + 0x200000
.equ PLIC_CLAIM0,  PLIC + 0x200004

.section .text.init
.globl _start
_start:
  la    sp, stack_top
  la    t0, trap
  csrw  mtvec, t0

  la    a0, msg_hello
  call  puts

  # Timer: fire 1000 ticks from now.
  li    t0, MTIME
  ld    t1, 0(t0)
  addi  t1, t1, 1000
  li    t0, MTIMECMP
  sd    t1, 0(t0)
  li    t0, 0x80          # mie.MTIE
  csrs  mie, t0
  csrsi mstatus, 8        # mstatus.MIE
1:
  wfi
  ld    t1, timer_seen
  beqz  t1, 1b

  # UART receive interrupt through PLIC context 0 (M-mode).
  li    t0, UART
  li    t1, 1             # IER: received data available
  sb    t1, UART_IER(t0)
  li    t0, PLIC
  li    t1, 1
  sw    t1, UART_IRQ*4(t0)       # priority
  li    t0, PLIC_ENABLE0
  li    t1, 1 << UART_IRQ
  sw    t1, 0(t0)
  li    t0, PLIC_THRESH0
  sw    zero, 0(t0)
  li    t0, 0x800         # mie.MEIE
  csrs  mie, t0
2:
  wfi
  ld    t1, rx_seen
  beqz  t1, 2b

  la    a0, msg_done
  call  puts
  li    t1, 1             # HTIF: exit(0)
  la    t0, tohost
  sd    t1, 0(t0)
3:
  j     3b

# Trap handler: saves what it and puts clobber.
.balign 4
trap:
  addi  sp, sp, -64
  sd    ra, 0(sp)
  sd    t0, 8(sp)
  sd    t1, 16(sp)
  sd    t2, 24(sp)
  sd    a0, 32(sp)
  sd    a1, 40(sp)

  csrr  t0, mcause
  bgez  t0, fail          # exceptions are unexpected
  slli  t0, t0, 1
  srli  t0, t0, 1
  li    t1, 7
  beq   t0, t1, on_timer
  li    t1, 11
  beq   t0, t1, on_external
  j     fail

on_timer:
  li    t0, MTIMECMP      # silence the timer
  li    t1, -1
  sd    t1, 0(t0)
  li    t1, 1
  sd    t1, timer_seen, t0
  la    a0, msg_timer
  call  puts
  j     trap_return

on_external:
  li    t0, PLIC_CLAIM0
  lw    t2, 0(t0)         # claim
  sd    t2, 48(sp)        # keep the ID across puts
  li    t1, UART_IRQ
  bne   t2, t1, fail
  li    t1, UART
  lbu   a1, 0(t1)         # read the byte, clearing the UART interrupt
  la    a0, msg_rx
  call  puts
  mv    a0, a1
  call  putc
  li    a0, '\n'
  call  putc
  ld    t2, 48(sp)
  li    t0, PLIC_CLAIM0
  sw    t2, 0(t0)         # complete
  li    t1, 1
  sd    t1, rx_seen, t0

trap_return:
  ld    ra, 0(sp)
  ld    t0, 8(sp)
  ld    t1, 16(sp)
  ld    t2, 24(sp)
  ld    a0, 32(sp)
  ld    a1, 40(sp)
  addi  sp, sp, 64
  mret

fail:
  li    t1, 3             # HTIF: exit(1)
  la    t0, tohost
  sd    t1, 0(t0)
4:
  j     4b

# putc(a0): wait for THR empty, then send. Clobbers t0, t1.
putc:
  li    t0, UART
5:
  lbu   t1, UART_LSR(t0)
  andi  t1, t1, 0x20
  beqz  t1, 5b
  sb    a0, 0(t0)
  ret

# puts(a0): send a NUL-terminated string. Clobbers t0, t1, t2, a0.
puts:
  addi  sp, sp, -16
  sd    ra, 0(sp)
  mv    t2, a0
6:
  lbu   a0, 0(t2)
  beqz  a0, 7f
  call  putc
  addi  t2, t2, 1
  j     6b
7:
  ld    ra, 0(sp)
  addi  sp, sp, 16
  ret

.section .rodata
msg_hello: .asciz "hello\n"
msg_timer: .asciz "timer\n"
msg_rx:    .asciz "rx:"
msg_done:  .asciz "done\n"

.section .data
.balign 8
timer_seen: .dword 0
rx_seen:    .dword 0

.section .tohost, "aw", @progbits
.balign 64
.globl tohost
tohost:   .dword 0
.balign 64
.globl fromhost
fromhost: .dword 0

.section .bss
.balign 16
stack: .space 4096
stack_top:
