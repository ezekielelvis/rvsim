// The canonical list of core state, shared by rv_state_hash and (Phase 9)
// core snapshots, so the two can never disagree about what state is.
#pragma once

#include <cstdint>

#include "machine.hpp"

namespace rv::state {

// Version of the canonical serialization. Bump it whenever a field is added,
// removed or reordered: hashes and snapshots change meaning.
constexpr uint32_t kCoreFormat = 1;

// visit_core presents every piece of core state that determines future
// execution to v, in a fixed order. M is rv_machine or const rv_machine;
// V provides operator()(T&) for each unsigned integer and bool type, plus
// tlb(TlbEntry&). Excluded: RAM (hashed and saved separately), the trace
// buffer and the last exit report (outputs, not state).
template <class M, class V>
void visit_core(M& m, V& v) {
  auto& c = m.cfg;
  v(c.ram_base);
  v(c.ram_size);
  v(c.clint_base);
  v(c.reset_pc);
  v(c.mhartid);
  v(c.steps_per_tick);
  v(c.htif_tohost);
  v(c.htif_fromhost);
  v(c.flags);

  auto& h = m.hart;
  for (auto& x : h.x) v(x);
  v(h.pc);
  v(h.priv);
  auto& r = h.csr;
  v(r.mstatus);
  v(r.medeleg);
  v(r.mideleg);
  v(r.mie);
  v(r.mip);
  v(r.mtvec);
  v(r.stvec);
  v(r.mscratch);
  v(r.sscratch);
  v(r.mepc);
  v(r.sepc);
  v(r.mcause);
  v(r.scause);
  v(r.mtval);
  v(r.stval);
  v(r.satp);
  v(r.mcounteren);
  v(r.scounteren);
  v(r.mcountinhibit);
  v(r.menvcfg);
  v(r.senvcfg);
  v(r.mcycle);
  v(r.minstret);
  v(r.mcycle_written);
  v(r.minstret_written);

  v(m.steps);
  v(m.instret);
  v(m.mtimecmp);
  v(m.mtime_offset);
  for (auto& e : m.tlb) v.tlb(e);
  v(m.reservation);
  v(m.irq_lines);
  v(m.irq_suppress);

  auto& io = m.mmio;
  v(io.pending);
  v(io.completed);
  v(io.is_store);
  v(io.fault);
  v(io.size);
  v(io.addr);
  v(io.value);

  v(m.in_wfi);
  v(m.poisoned);
  v(m.htif_halt);
}

// Hashes of the core state and of RAM (XXH3-64, seed 0).
uint64_t hash_core(const rv_machine& m);
uint64_t hash_ram(const rv_machine& m);
// XXH3-64 of {hash_core, hash_ram} as two little-endian u64s.
uint64_t hash_both(const rv_machine& m);

}  // namespace rv::state
