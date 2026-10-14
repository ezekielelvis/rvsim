# rvmodel_macros.h
# DUT-specific macro definitions for rvsim (HTIF tohost/fromhost, as for Sail).
# Jordan Carlin jcarlin@hmc.edu October 2025, Sadhvi Narayana sanarayanan@hmc.edu February 2026
# SPDX-License-Identifier: BSD-3-Clause

#ifndef _RVMODEL_MACROS_H
#define _RVMODEL_MACROS_H

#define CLINT_BASE_ADDRESS 0x02000000

#define RVMODEL_DATA_SECTION \
        .pushsection .tohost,"aw",@progbits;                \
        .balign 8; .global tohost; tohost: .dword 0;         \
        .balign 8; .global fromhost; fromhost: .dword 0;     \
        .popsection

// rvsim implements a standard M-mode (mtvec, mstatus, ...), so the framework
// can install its trap handlers at boot.
#define STANDARD_SM_SUPPORTED

##### STARTUP #####

# Perform boot operations. Can be empty.
#define RVMODEL_BOOT

// Custom RVMODEL_BOOT_TO_MMODE overrides default RVTEST_BOOT_TO_MMODE
// if defined.  For most DUTs, the default should work and this macro
// should not be defined.  If no M-mode or CSRs are implemented, define this
// macro as blank to bypass the boot process.  If a nonconforming
// M-mode is implemented, define this macro to set up the necessary
// state in a fashion similar to RVTEST_BOOT_TO_MMODE.
//#define RVMODEL_BOOT_TO_MMODE  // rvsim has a conforming M-mode: use the default boot

##### TERMINATION #####

// SAIL uses HTIF (Host-Target Interface) to terminate simulation.
// Writing to 'tohost' with value 1 indicates success, 3 indicates failure.

# Terminate test with a pass indication.
# When the test is run in simulation, this should end the simulation.
#define RVMODEL_HALT_PASS  \
  li x1, 1                ;\
  la t0, tohost           ;\
  write_tohost_pass:      ;\
    sw x1, 0(t0)          ;\
    sw x0, 4(t0)          ;\
    j write_tohost_pass   ;\


# Terminate test with a fail indication.
# When the test is run in simulation, this should end the simulation.
#define RVMODEL_HALT_FAIL \
  li x1, 3                ;\
  la t0, tohost           ;\
  write_tohost_fail:      ;\
    sw x1, 0(t0)          ;\
    sw x0, 4(t0)          ;\
    j write_tohost_fail   ;\


##### IO #####

# Initialization steps needed prior to writing to the console
# _R1, _R2, and _R3 can be used as temporary registers if needed.
# Do not modify any other registers (or make sure to restore them).
#define RVMODEL_IO_INIT(_R1, _R2, _R3)


# Prints a null-terminated string using a DUT specific mechanism.
# A pointer to the string is passed in _STR_PTR.
# _R1, _R2, and _R3 can be used as temporary registers if needed.
# Do not modify any other registers (or make sure to restore them).
#define RVMODEL_IO_WRITE_STR(_R1, _R2, _R3, _STR_PTR)               \
1:                           ;                       \
  lbu _R1, 0(_STR_PTR)        ;/* Load byte */        \
  beqz _R1, 3f                ;/* Exit if null */     \
2: /* htif_putc */           ;                      \
  la _R2, tohost       ;   \
  sw _R1, 0(_R2)     ; \
  /* device=1 (terminal), cmd=1 (output) */ \
  li _R1, 0x01010000 ;\
  sw _R1, 4(_R2)   ;\
  addi _STR_PTR, _STR_PTR, 1 ;/* Next char */        \
  j 1b                       ;/* Loop */             \
3:

##### Access Fault #####

#define RVMODEL_ACCESS_FAULT_ADDRESS 0x00000000

##### Machine Timer #####

#define RVMODEL_MTIMECMP_ADDRESS  0x02004000  /* CLINT mtimecmp */
#define RVMODEL_MTIME_ADDRESS     0x0200BFF8  /* CLINT mtime */

// rvsim's mtime advances once every steps_per_tick (10 by default) steps.
#define RVMODEL_MAX_CYCLES_PER_TIMER_TICK 10

// Enough ticks to reach the lower privilege mode before the timer fires.
#define RVMODEL_TIMER_INT_SOON_DELAY 5000

##### Interrupt Latency #####

// Interrupts are taken before the next instruction once pending and enabled.
#define RVMODEL_INTERRUPT_LATENCY 10

##### Machine Interrupts #####

#define CLINT_BASE_ADDRESS   0x02000000
#define RVMODEL_MSIP_ADDRESS (CLINT_BASE_ADDRESS + 0x0)

// External interrupts use the real UART and PLIC (QEMU virt layout), as in
// the CVW configuration: enabling the UART's THRE interrupt with THR empty
// raises source 10, which the PLIC routes to MEIP (context 0) or SEIP
// (context 1).
#define PLIC_BASE_ADDRESS    0x0c000000
#define PLIC_ENABLE_ADDRESS  0x0c002000
#define PLIC_THRESH_ADDRESS  0x0c200000
#define PLIC_CLAIM_ADDRESS   0x0c200004
#define PLIC_SENABLE_ADDRESS 0x0c002080
#define PLIC_STHRESH_ADDRESS 0x0c201000
#define PLIC_SCLAIM_ADDRESS  0x0c201004

#define NS16550_BASE_ADDRESS 0x10000000
#define UART_INT_SRC         10

#define RVMODEL_SET_MEXT_INT(_R1, _R2)          \
  li _R1, 7;                                     \
  li _R2, PLIC_BASE_ADDRESS;                     \
  sw _R1, (4*UART_INT_SRC)(_R2);                 \
  li _R1, (1 << UART_INT_SRC);                   \
  li _R2, PLIC_ENABLE_ADDRESS;                   \
  sw _R1, 0(_R2);                                \
  li _R2, PLIC_THRESH_ADDRESS;                   \
  sw zero, 0(_R2);                               \
  li _R1, 0x02;                                  \
  li _R2, NS16550_BASE_ADDRESS;                  \
  sb _R1, 1(_R2);

#define RVMODEL_CLR_MEXT_INT(_R1, _R2)          \
  li _R2, NS16550_BASE_ADDRESS;                  \
  sb zero, 1(_R2);                               \
  li _R2, PLIC_CLAIM_ADDRESS;                    \
  lw _R1, 0(_R2);                                \
  sw _R1, 0(_R2);                                \
  li _R2, PLIC_ENABLE_ADDRESS;                   \
  sw zero, 0(_R2);

##### Supervisor Interrupts #####

#define RVMODEL_SET_SEXT_INT(_R1, _R2)          \
  li _R1, 7;                                     \
  li _R2, PLIC_BASE_ADDRESS;                     \
  sw _R1, (4*UART_INT_SRC)(_R2);                 \
  li _R1, (1 << UART_INT_SRC);                   \
  li _R2, PLIC_SENABLE_ADDRESS;                  \
  sw _R1, 0(_R2);                                \
  li _R2, PLIC_STHRESH_ADDRESS;                  \
  sw zero, 0(_R2);                               \
  li _R1, 0x02;                                  \
  li _R2, NS16550_BASE_ADDRESS;                  \
  sb _R1, 1(_R2);

#define RVMODEL_CLR_SEXT_INT(_R1, _R2)          \
  li _R2, NS16550_BASE_ADDRESS;                  \
  sb zero, 1(_R2);                               \
  li _R2, PLIC_SCLAIM_ADDRESS;                   \
  lw _R1, 0(_R2);                                \
  sw _R1, 0(_R2);                                \
  li _R2, PLIC_SENABLE_ADDRESS;                  \
  sw zero, 0(_R2);

// RVMODEL_SET_SSW_INT / RVMODEL_CLR_SSW_INT are intentionally undefined:
// there is no supervisor software interrupt controller, so the test
// environment raises and clears SSI through mip.SSIP.

#endif // _RVMODEL_MACROS_H
