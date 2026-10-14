// Internal definition of rv_machine. Not part of the ABI; only rv.h is.
#pragma once

#include <bit>
#include <cstdint>
#include <cstdlib>

#include "cpu/hart.hpp"
#include "rv.h"
#include "rv_trace.h"

// Guest memory is little-endian; loads and stores memcpy host words directly.
static_assert(std::endian::native == std::endian::little, "big-endian hosts are not supported");

// One TLB entry: a 4 KiB piece of a leaf mapping (mmu/mmu.cpp).
struct TlbEntry {
  bool valid = false;
  uint8_t flags = 0;  // PTE bits 7:0 (V R W X U G A D)
  uint8_t level = 0;  // 0: 4 KiB leaf, 1: 2 MiB, 2: 1 GiB
  uint16_t asid = 0;
  uint64_t vpn = 0;  // vaddr >> 12
  uint64_t ppn = 0;  // physical page for this vpn
};

struct rv_machine {
  rv_config cfg{};
  rv_exit exit{};

  // Physical RAM: cfg.ram_size bytes, zero-initialized, owned by the core.
  uint8_t* ram = nullptr;

  rv::Hart hart;

  // Virtual time and retirement counters (ADR-001: steps are the time unit).
  uint64_t steps = 0;
  uint64_t instret = 0;

  // CLINT (clint/clint.hpp). mtimecmp resets to 0, as in Spike, so MTIP is
  // pending until software programs it. mtime is derived from virtual time;
  // a write to mtime moves this offset.
  uint64_t mtimecmp = 0;
  uint64_t mtime_offset = 0;

  // Sv39 TLB: direct-mapped by vpn.
  TlbEntry tlb[256];

  // LR/SC reservation: the reserved physical address, or kNoReservation.
  uint64_t reservation = ~0ull;

  // Interrupt inputs driven by the VMM.
  uint64_t irq_lines = 0;
  uint64_t irq_suppress = 0;

  // Trace buffer (rv_trace.h). Allocated by rv_trace_enable.
  rv_trace_record* trace = nullptr;
  size_t trace_cap = 0;  // in records
  size_t trace_len = 0;

  // An MMIO access the VMM must perform (ADR-022). `pending`: rv_run exited
  // with it; `completed`: rv_complete_mmio supplied the result, and the
  // instruction is re-executed on the next rv_run to retire with it.
  struct Mmio {
    bool pending = false;
    bool completed = false;
    bool is_store = false;
    bool fault = false;
    uint8_t size = 0;
    uint64_t addr = 0;
    uint64_t value = 0;  // store data, or the completed load result
  } mmio;

  // Run-loop state.
  bool in_wfi = false;
  bool poisoned = false;
  bool htif_halt = false;  // a store made tohost non-zero

  ~rv_machine() {
    std::free(ram);
    std::free(trace);
  }

  bool ram_contains(uint64_t paddr, size_t len) const {
    if (len > cfg.ram_size) return false;
    if (paddr < cfg.ram_base) return false;
    return paddr - cfg.ram_base <= cfg.ram_size - len;
  }

  uint8_t* ram_ptr(uint64_t paddr) { return ram + (paddr - cfg.ram_base); }

  bool clint_contains(uint64_t paddr, size_t len) const {
    if (paddr < cfg.clint_base) return false;
    return paddr - cfg.clint_base < 0x10000 && len <= 0x10000 - (paddr - cfg.clint_base);
  }

  // mtime is derived from virtual time (ADR-001).
  uint64_t mtime() const { return steps / cfg.steps_per_tick + mtime_offset; }
};

namespace rv {

constexpr uint64_t kNoReservation = ~0ull;

// Runs until an exit; fills m->exit. Defined in cpu/execute.cpp.
void run(rv_machine* m, uint64_t max_steps);

// Resets architectural state (Priv §3.4). Defined in cpu/execute.cpp.
void reset_hart(rv_machine* m);

// The step at which MTIP rises (mtime reaches mtimecmp), or the current step
// if it is already pending; UINT64_MAX if that is beyond the step counter.
inline uint64_t timer_step(const rv_machine* m) {
  const uint64_t now = m->mtime();
  if (now >= m->mtimecmp) return m->steps;
  const uint64_t spt = m->cfg.steps_per_tick;
  const uint64_t ticks = m->steps / spt + (m->mtimecmp - now);
  if (ticks < m->steps / spt || ticks > UINT64_MAX / spt) return UINT64_MAX;
  return ticks * spt;
}

}  // namespace rv
