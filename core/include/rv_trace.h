/* rv_trace.h: binary trace records produced by the core (see rv_trace_enable).
 *
 * The core appends fixed-size records to a C-owned buffer. When fewer than
 * one record's worth of space remains, rv_run exits with RV_EXIT_TRACE_FULL
 * before executing the next instruction. The VMM reads the buffer with
 * rv_trace_peek, formats it (e.g. as Spike --log-commits text), and calls
 * rv_trace_consume to empty it.
 */
#ifndef RVSIM_RV_TRACE_H
#define RVSIM_RV_TRACE_H

#include <stddef.h>
#include <stdint.h>

#define RV_TRACE_VERSION 2u

enum {
  RV_TRACE_COMMIT = 1, /* an instruction retired */
  RV_TRACE_TRAP   = 2  /* an exception or interrupt was taken (Spike does not log these) */
};

/* Which optional fields of a COMMIT record are valid. */
#define RV_TF_RD    1u  /* rd, rd_value: a GPR other than x0 was written */
#define RV_TF_CSR   2u  /* csr, csr_value: a CSR was written (explicitly or by xRET) */
#define RV_TF_LOAD  4u  /* mem_addr, mem_size */
#define RV_TF_STORE 8u  /* mem_addr, mem_size, mem_value */
#define RV_TF_CSR2  16u /* csr2, csr2_value: a second CSR changed by the same write (e.g. sie -> mie) */

typedef struct {
  uint64_t pc;
  uint32_t insn;      /* raw instruction bits; 16-bit instructions zero-extended */
  uint8_t  kind;      /* RV_TRACE_COMMIT or RV_TRACE_TRAP */
  uint8_t  priv;      /* privilege the instruction ran in (TRAP: privilege trapped from) */
  uint8_t  insn_len;  /* 2 or 4; 0 if the fetch itself faulted */
  uint8_t  flags;     /* RV_TF_* (COMMIT only) */
  uint8_t  rd;        /* COMMIT: destination GPR. TRAP: privilege trapped into */
  uint8_t  mem_size;  /* 1, 2, 4 or 8 */
  uint16_t csr;
  uint16_t csr2;      /* csr < csr2 when both are present */
  uint16_t reserved;
  uint64_t rd_value;  /* TRAP: xcause */
  uint64_t csr_value; /* TRAP: handler pc */
  uint64_t mem_addr;  /* TRAP: xtval */
  uint64_t mem_value;
  uint64_t csr2_value;
} rv_trace_record;

#ifdef __cplusplus
static_assert(sizeof(rv_trace_record) == 64, "rv_trace_record layout is ABI");
#else
_Static_assert(sizeof(rv_trace_record) == 64, "rv_trace_record layout is ABI");
#endif

#endif /* RVSIM_RV_TRACE_H */
