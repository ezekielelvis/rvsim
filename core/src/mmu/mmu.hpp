// Sv39 address translation (Priv §4.3-4.4) and the TLB.
#pragma once

#include <cstdint>

struct rv_machine;

namespace rv {

enum class Access : uint8_t { Fetch, Load, Store };

// satp fields for RV64 (Priv §4.1.11).
namespace satp {
constexpr unsigned kModeShift = 60;
constexpr uint64_t kModeBare = 0;
constexpr uint64_t kModeSv39 = 8;
constexpr unsigned kAsidShift = 44;
constexpr uint64_t kAsidMask = 0xffffull << kAsidShift;  // ASIDLEN = 16
constexpr uint64_t kPpnMask = (1ull << 44) - 1;
}  // namespace satp

// PTE bits (Priv §4.4.1).
namespace pte {
constexpr uint64_t kV = 1u << 0, kR = 1u << 1, kW = 1u << 2, kX = 1u << 3;
constexpr uint64_t kU = 1u << 4, kG = 1u << 5, kA = 1u << 6, kD = 1u << 7;
constexpr unsigned kPpnShift = 10;
constexpr uint64_t kPpnMask = ((1ull << 44) - 1) << kPpnShift;
// Bits 63:54: N (Svnapot), PBMT (Svpbmt) and reserved bits. rvsim
// implements none of those extensions, so any of them set is a page fault.
constexpr uint64_t kReserved = ~0ull << 54;
}  // namespace pte

// menvcfg.ADUE (Svadu): hardware updates PTE A/D bits when set; otherwise a
// clear A, or a clear D on a store, raises a page fault (Svade).
constexpr uint64_t kMenvcfgADUE = 1ull << 61;
constexpr uint64_t kEnvcfgFIOM = 1ull << 0;

// A translation result: either a physical address or an exception cause
// (page fault, or access fault while reading a page-table entry).
struct Translation {
  uint64_t paddr = 0;
  uint64_t cause = ~0ull;  // ~0: success
  bool ok() const { return cause == ~0ull; }
};

// Translates vaddr for an access by the hart, with the privilege, satp and
// mstatus in effect (Priv §3.1.6.3: loads and stores honour MPRV). It may
// update PTE A/D bits and fills the TLB.
Translation translate(rv_machine* m, uint64_t vaddr, Access access);

// The same translation without side effects: no A/D update, no TLB use or
// fill. For the debugger (rv_translate).
Translation translate_debug(const rv_machine* m, uint64_t vaddr, Access access);

// SFENCE.VMA (Priv §4.2.1): vaddr/asid apply only when has_vaddr/has_asid.
void tlb_flush(rv_machine* m, bool has_vaddr, uint64_t vaddr, bool has_asid, uint64_t asid);

}  // namespace rv
