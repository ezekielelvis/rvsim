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

struct rv_machine {
  rv_config cfg{};
  rv_exit exit{};

  // Physical RAM: cfg.ram_size bytes, zero-initialized, owned by the core.
  uint8_t* ram = nullptr;

  rv::Hart hart;

  // Virtual time and retirement counters (ADR-001: steps are the time unit).
  uint64_t steps = 0;
  uint64_t instret = 0;

  // LR/SC reservation: the reserved physical address, or kNoReservation.
  uint64_t reservation = ~0ull;

  // Interrupt inputs driven by the VMM.
  uint64_t irq_lines = 0;
  uint64_t irq_suppress = 0;

  // Trace buffer (rv_trace.h). Allocated by rv_trace_enable.
  rv_trace_record* trace = nullptr;
  size_t trace_cap = 0;  // in records
  size_t trace_len = 0;

  // Run-loop state.
  bool mmio_pending = false;
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
};

namespace rv {

constexpr uint64_t kNoReservation = ~0ull;

// Runs until an exit; fills m->exit. Defined in cpu/execute.cpp.
void run(rv_machine* m, uint64_t max_steps);

// Resets architectural state (Priv §3.4). Defined in cpu/execute.cpp.
void reset_hart(rv_machine* m);

}  // namespace rv
