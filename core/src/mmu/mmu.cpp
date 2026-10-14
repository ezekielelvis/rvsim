#include "mmu/mmu.hpp"

#include <cstring>
#include <iterator>

#include "cpu/hart.hpp"
#include "machine.hpp"

namespace rv {
namespace {

constexpr unsigned kPageShift = 12;
constexpr unsigned kLevels = 3;       // Sv39
constexpr unsigned kVpnBits = 9;
constexpr unsigned kVaBits = 39;

uint64_t page_fault(Access a) {
  return a == Access::Fetch ? kCauseFetchPageFault : a == Access::Load ? kCauseLoadPageFault : kCauseStorePageFault;
}

uint64_t access_fault(Access a) {
  return a == Access::Fetch ? kCauseFetchAccess : a == Access::Load ? kCauseLoadAccess : kCauseStoreAccess;
}

// The translation context for one access.
struct Ctx {
  Priv priv;
  bool active;  // Sv39 translation applies
  uint64_t root;
  uint64_t asid;
  bool sum, mxr, adue;
};

Ctx context(const rv_machine* m, Access a) {
  const Csrs& c = m->hart.csr;
  Priv priv = m->hart.priv;
  // Priv §3.1.6.3: with MPRV set, M-mode loads and stores translate and
  // check permissions as if in mstatus.MPP. Instruction fetch is unaffected.
  if (a != Access::Fetch && priv == kPrivM && (c.mstatus & mstatus::kMPRV) != 0) {
    priv = static_cast<Priv>((c.mstatus & mstatus::kMPP) >> mstatus::kMPPShift);
  }
  Ctx x{};
  x.priv = priv;
  x.active = priv != kPrivM && (c.satp >> satp::kModeShift) == satp::kModeSv39;
  x.root = (c.satp & satp::kPpnMask) << kPageShift;
  x.asid = (c.satp & satp::kAsidMask) >> satp::kAsidShift;
  x.sum = (c.mstatus & mstatus::kSUM) != 0;
  x.mxr = (c.mstatus & mstatus::kMXR) != 0;
  x.adue = (c.menvcfg & kMenvcfgADUE) != 0;
  return x;
}

// Leaf permission check (Priv §4.3.1, §4.3.2 step 6).
bool permitted(uint64_t flags, Access a, const Ctx& x) {
  if (flags & pte::kU) {
    // S-mode may touch U pages only for loads/stores with SUM set, and may
    // never execute them.
    if (x.priv == kPrivS && (a == Access::Fetch || !x.sum)) return false;
  } else if (x.priv == kPrivU) {
    return false;
  }
  switch (a) {
    case Access::Fetch: return (flags & pte::kX) != 0;
    case Access::Load: return (flags & pte::kR) != 0 || (x.mxr && (flags & pte::kX) != 0);
    case Access::Store: return (flags & pte::kW) != 0;
  }
  return false;
}

uint64_t needed_ad(Access a) { return pte::kA | (a == Access::Store ? pte::kD : 0); }

// The page-table walk of Priv §4.3.2 for Sv39.
template <bool kSideEffects>
Translation walk(const rv_machine* cm, uint64_t vaddr, Access a, const Ctx& x) {
  // Bits 63:39 must all equal bit 38 (§4.4.1).
  const auto top = static_cast<int64_t>(vaddr) >> (kVaBits - 1);
  if (top != 0 && top != -1) return {0, page_fault(a)};

  uint64_t base = x.root;
  for (int level = kLevels - 1; level >= 0; --level) {
    const uint64_t vpn_i = (vaddr >> (kPageShift + kVpnBits * static_cast<unsigned>(level))) & 0x1ff;
    const uint64_t pte_addr = base + vpn_i * 8;
    // A PTE outside RAM is an access fault of the original access type.
    if (!cm->ram_contains(pte_addr, 8)) return {0, access_fault(a)};
    uint64_t e;
    std::memcpy(&e, cm->ram + (pte_addr - cm->cfg.ram_base), 8);
    const uint64_t ppn = (e & pte::kPpnMask) >> pte::kPpnShift;

    if ((e & pte::kReserved) != 0) return {0, page_fault(a)};
    if ((e & pte::kV) == 0 || ((e & pte::kR) == 0 && (e & pte::kW) != 0)) return {0, page_fault(a)};
    if ((e & (pte::kR | pte::kX)) == 0) {
      // Non-leaf: A, D and U are reserved and must be clear (§4.3.1).
      if (level == 0 || (e & (pte::kA | pte::kD | pte::kU)) != 0) return {0, page_fault(a)};
      base = ppn << kPageShift;
      continue;
    }
    if (!permitted(e, a, x)) return {0, page_fault(a)};
    // A superpage's PPN must be aligned to its size (§4.3.2 step 6).
    const unsigned span = kVpnBits * static_cast<unsigned>(level);
    if ((ppn & ((1ull << span) - 1)) != 0) return {0, page_fault(a)};
    const uint64_t ad = needed_ad(a);
    if ((e & ad) != ad) {
      if (!x.adue) return {0, page_fault(a)};  // Svade
      if constexpr (kSideEffects) {
        // Svadu: set A (and D for a store) in the PTE.
        e |= ad;
        auto* m = const_cast<rv_machine*>(cm);
        std::memcpy(m->ram + (pte_addr - m->cfg.ram_base), &e, 8);
      }
    }
    const uint64_t vpn = vaddr >> kPageShift;
    const uint64_t page = (ppn & ~((1ull << span) - 1)) | (vpn & ((1ull << span) - 1));
    if constexpr (kSideEffects) {
      auto* m = const_cast<rv_machine*>(cm);
      TlbEntry& t = m->tlb[vpn % std::size(m->tlb)];
      t.valid = true;
      t.vpn = vpn;
      t.ppn = page;
      t.asid = static_cast<uint16_t>(x.asid);
      t.flags = static_cast<uint8_t>(e);
      t.level = static_cast<uint8_t>(level);
    }
    return {(page << kPageShift) | (vaddr & ((1ull << kPageShift) - 1)), ~0ull};
  }
  return {0, page_fault(a)};  // not reached: level 0 is always a leaf or a fault
}

}  // namespace

Translation translate(rv_machine* m, uint64_t vaddr, Access a) {
  const Ctx x = context(m, a);
  if (!x.active) return {vaddr, ~0ull};
  const uint64_t vpn = vaddr >> kPageShift;
  const TlbEntry& t = m->tlb[vpn % std::size(m->tlb)];
  // A hit is used only if it already permits the access with A/D set;
  // otherwise re-walk, so the PTE in memory decides (as Spike does).
  if (t.valid && t.vpn == vpn && ((t.flags & pte::kG) != 0 || t.asid == x.asid) && permitted(t.flags, a, x) &&
      (t.flags & needed_ad(a)) == needed_ad(a)) {
    return {(t.ppn << kPageShift) | (vaddr & ((1ull << kPageShift) - 1)), ~0ull};
  }
  return walk<true>(m, vaddr, a, x);
}

Translation translate_debug(const rv_machine* m, uint64_t vaddr, Access a) {
  Ctx x = context(m, a);
  if (!x.active) return {vaddr, ~0ull};
  x.adue = true;  // report the mapping even if A/D are clear
  return walk<false>(m, vaddr, a, x);
}

void tlb_flush(rv_machine* m, bool has_vaddr, uint64_t vaddr, bool has_asid, uint64_t asid) {
  m->tlb_gen++;  // invalidates the fetch fast path
  const uint64_t vpn = vaddr >> kPageShift;
  for (TlbEntry& t : m->tlb) {
    if (!t.valid) continue;
    // Entries cache 4 KiB pieces of a leaf; a flush by address removes every
    // piece of the leaf (superpage) that maps vaddr.
    const unsigned span = kVpnBits * t.level;
    if (has_vaddr && (t.vpn >> span) != (vpn >> span)) continue;
    // Global mappings survive an ASID-specific flush (§4.2.1).
    if (has_asid && ((t.flags & pte::kG) != 0 || t.asid != asid)) continue;
    t.valid = false;
  }
}

}  // namespace rv
