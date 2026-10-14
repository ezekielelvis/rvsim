// extern "C" entry points declared in rv.h. Argument validation lives here;
// execution is in cpu/execute.cpp.
#include <cstring>
#include <initializer_list>
#include <new>

#include "clint/clint.hpp"
#include "csr/csr.hpp"
#include "machine.hpp"
#include "mmu/mmu.hpp"
#include "rv.h"

namespace {

constexpr uint64_t kPageSize = 4096;

bool valid_config(const rv_config& c) {
  for (uint64_t r : c.reserved) {
    if (r != 0) return false;
  }
  if ((c.flags & ~RV_CFG_ALL_FLAGS) != 0) return false;
  if (c.ram_size == 0 || c.ram_size % kPageSize != 0) return false;
  if (c.ram_base % kPageSize != 0) return false;
  if (c.ram_base + c.ram_size < c.ram_base) return false;  // wraps
  if (c.clint_base % rv::kClintSize != 0) return false;
  if (c.steps_per_tick == 0) return false;
  // CLINT must not overlap RAM.
  if (c.clint_base < c.ram_base + c.ram_size && c.ram_base < c.clint_base + rv::kClintSize) {
    return false;
  }
  // HTIF mailboxes are 8-byte words in RAM.
  for (uint64_t a : {c.htif_tohost, c.htif_fromhost}) {
    if (a == 0) continue;
    if (a % 8 != 0 || a < c.ram_base || a - c.ram_base > c.ram_size - 8) return false;
  }
  return true;
}

void reset_state(rv_machine* m) {
  rv::reset_hart(m);
  m->irq_lines = 0;
  m->irq_suppress = 0;
  m->mmio = rv_machine::Mmio{};
  m->in_wfi = false;
  m->poisoned = false;
  m->exit = rv_exit{};
}

}  // namespace

extern "C" {

int rv_create(const rv_config* cfg, rv_machine** out) {
  if (cfg == nullptr || out == nullptr) return RV_E_INVAL;
  *out = nullptr;
  if (cfg->abi_version != RV_ABI_VERSION) return RV_E_ABI;
  if (!valid_config(*cfg)) return RV_E_INVAL;

  auto* m = new (std::nothrow) rv_machine;
  if (m == nullptr) return RV_E_NOMEM;
  m->cfg = *cfg;
  // calloc lets the OS hand out lazily-zeroed pages, so large RAM is cheap
  // until touched.
  m->ram = static_cast<uint8_t*>(std::calloc(1, cfg->ram_size));
  if (m->ram == nullptr) {
    delete m;
    return RV_E_NOMEM;
  }
  reset_state(m);
  *out = m;
  return RV_OK;
}

void rv_destroy(rv_machine* m) { delete m; }

int rv_reset(rv_machine* m) {
  if (m == nullptr) return RV_E_INVAL;
  reset_state(m);
  return RV_OK;
}

const rv_exit* rv_exit_info(const rv_machine* m) { return m == nullptr ? nullptr : &m->exit; }

int rv_run(rv_machine* m, uint64_t max_steps) {
  if (m == nullptr) return RV_E_INVAL;
  if (m->poisoned) return RV_E_POISONED;
  if (m->mmio.pending && !m->mmio.completed) return RV_E_STATE;
  rv::run(m, max_steps);
  return RV_OK;
}

int rv_complete_mmio(rv_machine* m, uint64_t read_value, int access_fault) {
  if (m == nullptr) return RV_E_INVAL;
  if (m->poisoned) return RV_E_POISONED;
  if (!m->mmio.pending || m->mmio.completed) return RV_E_STATE;
  m->mmio.completed = true;
  m->mmio.fault = access_fault != 0;
  if (!m->mmio.is_store) m->mmio.value = read_value;
  return RV_OK;
}

int rv_advance_idle(rv_machine* m, uint64_t steps) {
  if (m == nullptr) return RV_E_INVAL;
  if (m->poisoned) return RV_E_POISONED;
  if (!m->in_wfi) return RV_E_STATE;
  // Idle time advances virtual time (ADR-001) but retires nothing, so
  // mcycle and minstret do not move (ADR-015).
  m->steps = steps > UINT64_MAX - m->steps ? UINT64_MAX : m->steps + steps;
  return RV_OK;
}

int rv_set_irq_lines(rv_machine* m, uint64_t lines) {
  if (m == nullptr) return RV_E_INVAL;
  if ((lines & ~(RV_IRQ_SEIP | RV_IRQ_MEIP)) != 0) return RV_E_INVAL;
  m->irq_lines = lines;
  return RV_OK;
}

int rv_set_irq_mask(rv_machine* m, uint64_t suppress) {
  if (m == nullptr) return RV_E_INVAL;
  m->irq_suppress = suppress;
  return RV_OK;
}

int rv_get_reg(const rv_machine* m, uint32_t reg, uint64_t* out) {
  if (m == nullptr || out == nullptr) return RV_E_INVAL;
  if (reg < 32) {
    *out = m->hart.x[reg];
  } else if (reg == RV_REG_PC) {
    *out = m->hart.pc;
  } else if (reg == RV_REG_PRIV) {
    *out = m->hart.priv;
  } else if (reg >= RV_REG_CSR && reg <= RV_REG_CSR + 0xfff) {
    if (!rv::csr_read(m, static_cast<uint16_t>(reg - RV_REG_CSR), *out)) return RV_E_RANGE;
  } else {
    return RV_E_RANGE;
  }
  return RV_OK;
}

int rv_set_reg(rv_machine* m, uint32_t reg, uint64_t value) {
  if (m == nullptr) return RV_E_INVAL;
  if (reg < 32) {
    // Unpriv §2.1: x0 is hardwired to zero; writes are ignored.
    if (reg != 0) m->hart.x[reg] = value;
  } else if (reg == RV_REG_PC) {
    m->hart.pc = value;
  } else if (reg == RV_REG_PRIV) {
    // Priv §1.2: encoding 2 is reserved (no hypervisor extension).
    if (value != rv::kPrivM && value != rv::kPrivS && value != rv::kPrivU) return RV_E_INVAL;
    m->hart.priv = static_cast<rv::Priv>(value);
  } else if (reg >= RV_REG_CSR && reg <= RV_REG_CSR + 0xfff) {
    const auto csr = static_cast<uint16_t>(reg - RV_REG_CSR);
    uint64_t unused = 0;
    if (!rv::csr_read(m, csr, unused)) return RV_E_RANGE;
    if (!rv::csr_write(m, csr, value)) return RV_E_INVAL;  // read-only
  } else {
    return RV_E_RANGE;
  }
  return RV_OK;
}

int rv_read_phys(const rv_machine* m, uint64_t paddr, void* buf, size_t len) {
  if (m == nullptr || (buf == nullptr && len != 0)) return RV_E_INVAL;
  if (m->ram_contains(paddr, len)) {
    if (len != 0) std::memcpy(buf, m->ram + (paddr - m->cfg.ram_base), len);
    return RV_OK;
  }
  // The CLINT accepts aligned 4- and 8-byte accesses, as from the hart.
  uint64_t v = 0;
  if (m->clint_contains(paddr, len) && rv::clint_load(m, paddr, len, v)) {
    std::memcpy(buf, &v, len);
    return RV_OK;
  }
  return RV_E_RANGE;
}

int rv_write_phys(rv_machine* m, uint64_t paddr, const void* buf, size_t len) {
  if (m == nullptr || (buf == nullptr && len != 0)) return RV_E_INVAL;
  if (!m->ram_contains(paddr, len)) {
    uint64_t v = 0;
    std::memcpy(&v, buf, len < 8 ? len : 8);
    if (m->clint_contains(paddr, len) && rv::clint_store(m, paddr, len, v)) return RV_OK;
    return RV_E_RANGE;
  }
  if (len == 0) return RV_OK;
  std::memcpy(m->ram + (paddr - m->cfg.ram_base), buf, len);
  // A write by another agent (DMA, debugger) into the reservation set must
  // make a later SC fail (Unpriv §14.2). The reservation set is the aligned
  // doubleword containing the reserved address.
  const uint64_t set = m->reservation & ~7ull;
  if (m->reservation != rv::kNoReservation && paddr < set + 8 && set < paddr + len) {
    m->reservation = rv::kNoReservation;
  }
  return RV_OK;
}

const uint8_t* rv_ram_view(const rv_machine* m, uint64_t* size_out) {
  if (m == nullptr) return nullptr;
  if (size_out != nullptr) *size_out = m->cfg.ram_size;
  return m->ram;
}

int rv_translate(const rv_machine* m, uint64_t vaddr, uint32_t access, uint64_t* paddr_out) {
  if (m == nullptr || paddr_out == nullptr) return RV_E_INVAL;
  rv::Access a;
  switch (access) {
    case RV_ACC_READ: a = rv::Access::Load; break;
    case RV_ACC_WRITE: a = rv::Access::Store; break;
    case RV_ACC_EXEC: a = rv::Access::Fetch; break;
    default: return RV_E_INVAL;
  }
  const rv::Translation t = rv::translate_debug(m, vaddr, a);
  if (!t.ok()) return RV_E_RANGE;  // the access would fault
  *paddr_out = t.paddr;
  return RV_OK;
}

int rv_add_breakpoint(rv_machine* m, uint64_t, uint64_t, uint32_t, uint32_t* id_out) {
  if (m == nullptr || id_out == nullptr) return RV_E_INVAL;
  return RV_E_UNSUPPORTED;
}

int rv_remove_breakpoint(rv_machine* m, uint32_t) {
  if (m == nullptr) return RV_E_INVAL;
  return RV_E_UNSUPPORTED;
}

int rv_trace_enable(rv_machine* m, size_t capacity_bytes) {
  if (m == nullptr) return RV_E_INVAL;
  std::free(m->trace);
  m->trace = nullptr;
  m->trace_cap = 0;
  m->trace_len = 0;
  if (capacity_bytes == 0) return RV_OK;
  const size_t records = capacity_bytes / sizeof(rv_trace_record);
  if (records == 0) return RV_E_INVAL;
  m->trace = static_cast<rv_trace_record*>(std::malloc(records * sizeof(rv_trace_record)));
  if (m->trace == nullptr) return RV_E_NOMEM;
  m->trace_cap = records;
  return RV_OK;
}

int rv_trace_peek(const rv_machine* m, const uint8_t** data, size_t* len) {
  if (m == nullptr || data == nullptr || len == nullptr) return RV_E_INVAL;
  if (m->trace == nullptr) return RV_E_STATE;
  *data = reinterpret_cast<const uint8_t*>(m->trace);
  *len = m->trace_len * sizeof(rv_trace_record);
  return RV_OK;
}

int rv_trace_consume(rv_machine* m) {
  if (m == nullptr) return RV_E_INVAL;
  if (m->trace == nullptr) return RV_E_STATE;
  m->trace_len = 0;
  return RV_OK;
}

int rv_core_snapshot_size(const rv_machine* m, size_t* size_out) {
  if (m == nullptr || size_out == nullptr) return RV_E_INVAL;
  return RV_E_UNSUPPORTED;
}

int rv_core_snapshot_save(const rv_machine* m, void* buf, size_t) {
  if (m == nullptr || buf == nullptr) return RV_E_INVAL;
  return RV_E_UNSUPPORTED;
}

int rv_core_snapshot_load(rv_machine* m, const void* buf, size_t) {
  if (m == nullptr || buf == nullptr) return RV_E_INVAL;
  return RV_E_UNSUPPORTED;
}

int rv_state_hash(rv_machine* m, uint32_t what, uint64_t* hash_out) {
  if (m == nullptr || hash_out == nullptr) return RV_E_INVAL;
  if (what == 0 || (what & ~(RV_HASH_CORE | RV_HASH_RAM)) != 0) return RV_E_INVAL;
  return RV_E_UNSUPPORTED;
}

}  // extern "C"
