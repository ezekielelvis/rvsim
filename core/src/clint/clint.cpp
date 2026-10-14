#include "clint/clint.hpp"

#include "cpu/hart.hpp"
#include "machine.hpp"

namespace rv {
namespace {

constexpr uint64_t kMsipBit = 1ull << kIrqMSI;

// The 64-bit register containing offset (8-byte aligned), or 0 if unmapped.
uint64_t read_word(const rv_machine* m, uint64_t word) {
  switch (word) {
    // Only hart 0 exists: the upper half of this doubleword is hart 1's msip.
    case kClintMsip: return (m->hart.csr.mip & kMsipBit) ? 1 : 0;
    case kClintMtimecmp: return m->mtimecmp;
    case kClintMtime: return m->mtime();
    default: return 0;
  }
}

uint64_t size_mask(size_t len) { return len == 8 ? ~0ull : (1ull << (8 * len)) - 1; }

}  // namespace

bool clint_load(const rv_machine* m, uint64_t paddr, size_t len, uint64_t& out) {
  const uint64_t off = paddr - m->cfg.clint_base;
  if ((len != 4 && len != 8) || off % len != 0) return false;
  const unsigned shift = static_cast<unsigned>(off & 7) * 8;
  out = (read_word(m, off & ~7ull) >> shift) & size_mask(len);
  return true;
}

bool clint_store(rv_machine* m, uint64_t paddr, size_t len, uint64_t value) {
  const uint64_t off = paddr - m->cfg.clint_base;
  if ((len != 4 && len != 8) || off % len != 0) return false;
  const uint64_t word = off & ~7ull;
  const unsigned shift = static_cast<unsigned>(off & 7) * 8;
  const uint64_t mask = size_mask(len) << shift;
  const uint64_t merged = (read_word(m, word) & ~mask) | ((value << shift) & mask);
  switch (word) {
    case kClintMsip:
      if (merged & 1) {
        m->hart.csr.mip |= kMsipBit;
      } else {
        m->hart.csr.mip &= ~kMsipBit;
      }
      break;
    case kClintMtimecmp: m->mtimecmp = merged; break;
    case kClintMtime: m->mtime_offset = merged - m->steps / m->cfg.steps_per_tick; break;
    default: break;
  }
  return true;
}

}  // namespace rv
