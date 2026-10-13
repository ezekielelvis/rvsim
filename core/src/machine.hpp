// Internal definition of rv_machine. Not part of the ABI; only rv.h is.
#pragma once

#include <cstdint>
#include <cstdlib>

#include "rv.h"

struct rv_machine {
  rv_config cfg{};
  rv_exit exit{};

  // Physical RAM: cfg.ram_size bytes, zero-initialized, owned by the core.
  uint8_t* ram = nullptr;

  // Architectural state.
  uint64_t x[32]{};
  uint64_t pc = 0;
  uint32_t priv = 3;  // Priv §3.4: reset enters M-mode.

  // Virtual time and retirement counters (ADR-001: steps are the time unit).
  uint64_t steps = 0;
  uint64_t instret = 0;

  // Interrupt inputs driven by the VMM.
  uint64_t irq_lines = 0;
  uint64_t irq_suppress = 0;

  // Run-loop state.
  bool mmio_pending = false;
  bool in_wfi = false;
  bool poisoned = false;

  ~rv_machine() { std::free(ram); }

  bool ram_contains(uint64_t paddr, size_t len) const {
    if (len > cfg.ram_size) return false;
    if (paddr < cfg.ram_base) return false;
    return paddr - cfg.ram_base <= cfg.ram_size - len;
  }
};
