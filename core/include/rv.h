/* rv.h: the C ABI between the rvsim C++ core and the Go VMM.
 *
 * This header is the single source of truth for the boundary (ADR-001).
 *
 * Rules:
 *  - An rv_machine is not thread-safe. All calls on one machine come from one
 *    thread at a time (the Go run loop). No internal locking, no thread-locals.
 *  - No call blocks, calls back into the host, or lets a C++ exception escape
 *    (the core is built with -fno-exceptions).
 *  - After rv_create, nothing allocates except rv_trace_enable.
 *  - Every fallible call returns rv_status (0 = OK, negative = error).
 *  - Serialized data (snapshots, hashes) is little-endian and host-independent,
 *    so the same input gives the same bytes on arm64 and x86-64.
 */
#ifndef RVSIM_RV_H
#define RVSIM_RV_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define RV_ABI_VERSION 1u

typedef struct rv_machine rv_machine;

typedef enum {
  RV_OK            =  0,
  RV_E_INVAL       = -1,   /* bad argument */
  RV_E_RANGE       = -2,   /* address/register outside implemented space */
  RV_E_STATE       = -3,   /* not valid now (e.g. rv_run with MMIO pending) */
  RV_E_NOMEM       = -4,
  RV_E_ABI         = -5,   /* cfg->abi_version mismatch */
  RV_E_SNAP_VER    = -6,
  RV_E_SNAP_BAD    = -7,   /* bad magic, length or checksum */
  RV_E_BUFSZ       = -8,
  RV_E_POISONED    = -9,   /* a previous RV_EXIT_ERROR; only destroy/snapshot-load allowed */
  RV_E_UNSUPPORTED = -10,  /* declared in the ABI but not implemented yet */
  RV_E_INTERNAL    = -99
} rv_status;

/* ---------------- configuration ---------------- */
#define RV_CFG_HW_AD_UPDATE  (1u << 0) /* implement Svadu: menvcfg.ADUE is writable; else Svade only */
#define RV_CFG_MISALIGNED_OK (1u << 1) /* perform misaligned RAM accesses; else trap (Unpriv §2.6) */
#define RV_CFG_ALL_FLAGS     (RV_CFG_HW_AD_UPDATE | RV_CFG_MISALIGNED_OK)

typedef struct {
  uint32_t abi_version;     /* must be RV_ABI_VERSION */
  uint32_t flags;           /* RV_CFG_* */
  uint64_t ram_base;        /* physical, 4 KiB aligned */
  uint64_t ram_size;        /* bytes, multiple of 4 KiB, non-zero */
  uint64_t clint_base;      /* 64 KiB window, 64 KiB aligned */
  uint64_t reset_pc;
  uint64_t mhartid;
  uint64_t steps_per_tick;  /* mtime += 1 every N steps; >= 1 */
  uint64_t htif_tohost;     /* physical address; 0 = HTIF disabled */
  uint64_t htif_fromhost;
  uint64_t reserved[4];     /* must be zero */
} rv_config;

int  rv_create(const rv_config* cfg, rv_machine** out);
void rv_destroy(rv_machine* m);
int  rv_reset(rv_machine* m);  /* architectural reset (Priv §3.4); RAM untouched */

/* ---------------- run loop ---------------- */
typedef enum {
  RV_EXIT_NONE       = 0,
  RV_EXIT_MMIO_READ  = 1, /* addr,size valid; insn NOT retired; call rv_complete_mmio */
  RV_EXIT_MMIO_WRITE = 2, /* addr,size,value valid; insn NOT retired; call rv_complete_mmio */
  RV_EXIT_BUDGET     = 3, /* exactly max_steps steps taken */
  RV_EXIT_WFI        = 4, /* hart idle, no enabled interrupt pending; see timer_step */
  RV_EXIT_BREAKPOINT = 5, /* addr = matched address, value = breakpoint id */
  RV_EXIT_HALT       = 6, /* HTIF write to tohost; value = payload */
  RV_EXIT_TRACE_FULL = 7, /* drain with rv_trace_peek/rv_trace_consume, then rv_run */
  RV_EXIT_ERROR      = 8  /* value = rv_error_detail; machine poisoned */
} rv_exit_reason;

/* RV_EXIT_ERROR details. */
typedef enum {
  RV_ERROR_MMIO_REPLAY = 1 /* finishing an MMIO access reached a different access */
} rv_error_detail;

typedef struct {
  uint32_t reason;      /* rv_exit_reason */
  uint32_t size;        /* MMIO: 1, 2, 4 or 8 */
  uint64_t addr;
  uint64_t value;       /* MMIO_WRITE data (zero-extended), HALT payload, bp id, error detail */
  uint64_t pc;          /* faulting/exiting insn, or next pc for BUDGET/WFI */
  uint64_t steps;       /* virtual time since reset (retired + traps + idle) */
  uint64_t instret;     /* architectural minstret */
  uint64_t timer_step;  /* step at which MTIP rises (= steps if already pending); UINT64_MAX if never */
} rv_exit;

/* Stable for the machine's lifetime; overwritten by every rv_run. */
const rv_exit* rv_exit_info(const rv_machine* m);

/* Run until an exit or until max_steps steps. Pending interrupts are checked
 * before the first instruction, so IRQ changes between runs take effect at an
 * exact step. An exec breakpoint at the starting pc is not re-reported. */
int rv_run(rv_machine* m, uint64_t max_steps);

/* Finish a pending MMIO access. read_value is ignored for writes. If
 * access_fault != 0, raise a load/store access fault instead (cause 5/7, tval = addr). */
int rv_complete_mmio(rv_machine* m, uint64_t read_value, int access_fault);

/* Only after RV_EXIT_WFI: skip `steps` of idle virtual time. */
int rv_advance_idle(rv_machine* m, uint64_t steps);

/* ---------------- interrupts ---------------- */
#define RV_IRQ_SEIP (1ull << 9)
#define RV_IRQ_MEIP (1ull << 11)
int rv_set_irq_lines(rv_machine* m, uint64_t lines);    /* levels; only SEIP|MEIP accepted */
int rv_set_irq_mask(rv_machine* m, uint64_t suppress);  /* fault injection: hide these mip bits
                                                           from interrupt-taking logic */

/* ---------------- registers ---------------- */
enum {
  RV_REG_X0   = 0,       /* x0..x31 = 0..31 */
  RV_REG_PC   = 32,
  RV_REG_PRIV = 33,      /* 0 = U, 1 = S, 3 = M */
  RV_REG_CSR  = 0x10000  /* + 12-bit CSR number */
};
/* CSR writes apply WARL legalization and side effects (e.g. satp flushes the TLB),
 * but skip privilege checks and do not retire an instruction. */
int rv_get_reg(const rv_machine* m, uint32_t reg, uint64_t* out);
int rv_set_reg(rv_machine* m, uint32_t reg, uint64_t value);

/* ---------------- memory ---------------- */
/* RAM and CLINT only; RV_E_RANGE elsewhere. Writes invalidate decoded-insn
 * cache entries and an overlapping LR reservation. */
int rv_read_phys(const rv_machine* m, uint64_t paddr, void* buf, size_t len);
int rv_write_phys(rv_machine* m, uint64_t paddr, const void* buf, size_t len);

/* Read-only view of RAM, valid until rv_destroy; contents stable only between
 * rv_run calls. For snapshots and hashing. The host must never write through it. */
const uint8_t* rv_ram_view(const rv_machine* m, uint64_t* size_out);

#define RV_ACC_READ  1u
#define RV_ACC_WRITE 2u
#define RV_ACC_EXEC  4u
/* Translate with the current satp/priv/mstatus. No side effects: no A/D update,
 * no TLB fill, no trap. For the debugger. */
int rv_translate(const rv_machine* m, uint64_t vaddr, uint32_t access, uint64_t* paddr_out);

/* ---------------- breakpoints / watchpoints ---------------- */
#define RV_BP_EXEC  1u
#define RV_BP_READ  2u
#define RV_BP_WRITE 4u
#define RV_BP_PHYS  8u   /* match a physical rather than a virtual address */
int rv_add_breakpoint(rv_machine* m, uint64_t addr, uint64_t len, uint32_t kind, uint32_t* id_out);
int rv_remove_breakpoint(rv_machine* m, uint32_t id);

/* ---------------- tracing (record format in rv_trace.h, Phase 1) ---------------- */
int rv_trace_enable(rv_machine* m, size_t capacity_bytes);  /* 0 disables */
int rv_trace_peek(const rv_machine* m, const uint8_t** data, size_t* len);
int rv_trace_consume(rv_machine* m);

/* ---------------- snapshot & hashing ---------------- */
/* Core state only: GPRs, pc, priv, CSRs, CLINT, TLB, reservation, pending MMIO,
 * step/instret counters. RAM is saved by the VMM via rv_ram_view. Versioned, checksummed. */
int rv_core_snapshot_size(const rv_machine* m, size_t* size_out);
int rv_core_snapshot_save(const rv_machine* m, void* buf, size_t len);
int rv_core_snapshot_load(rv_machine* m, const void* buf, size_t len);

#define RV_HASH_CORE 1u
#define RV_HASH_RAM  2u
/* Fixed algorithm (XXH3-64 vendored, pinned) over a canonical little-endian
 * serialization; identical across hosts. */
int rv_state_hash(rv_machine* m, uint32_t what, uint64_t* hash_out);

#ifdef __cplusplus
}  /* extern "C" */
#define RV_STATIC_ASSERT static_assert
#else
#define RV_STATIC_ASSERT _Static_assert
#endif

/* Layout is part of the ABI: Go reads these structs directly through cgo. */
RV_STATIC_ASSERT(sizeof(rv_exit) == 56, "rv_exit layout is ABI");
RV_STATIC_ASSERT(offsetof(rv_exit, addr) == 8, "rv_exit layout is ABI");
RV_STATIC_ASSERT(offsetof(rv_exit, timer_step) == 48, "rv_exit layout is ABI");
RV_STATIC_ASSERT(sizeof(rv_config) == 104, "rv_config layout is ABI");
RV_STATIC_ASSERT(offsetof(rv_config, reserved) == 72, "rv_config layout is ABI");

#endif /* RVSIM_RV_H */
