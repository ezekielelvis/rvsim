// The interpreter: fetch, decode, execute, trap. One call to step() is one
// step of virtual time: either a retired instruction or a taken trap.
#include <algorithm>
#include <cstring>
#include <limits>

#include "clint/clint.hpp"
#include "csr/csr.hpp"
#include "decode/decode.hpp"
#include "machine.hpp"
#include "mmu/mmu.hpp"

namespace rv {
namespace {

// With the C extension IALIGN=16 (Unpriv §1.5). Jump and branch targets are
// always even (JALR clears bit 0), so this check cannot fire today; it stays
// because a misaligned-target trap is architectural if C is ever disabled.
constexpr uint64_t kIalignMask = 1;

// The 128-bit integers used by MULH* (GCC/Clang extension, hence __extension__).
__extension__ using i128 = __int128;
__extension__ using u128 = unsigned __int128;

constexpr uint64_t kNoException = ~0ull;
// Not an architectural exception: the access needs the VMM (ADR-022).
constexpr uint64_t kMmioExit = ~1ull;

struct Exception {
  uint64_t cause = kNoException;
  uint64_t tval = 0;
  bool raised() const { return cause != kNoException; }
  bool mmio_exit() const { return cause == kMmioExit; }
};

constexpr Exception ok() { return {}; }
constexpr Exception raise(uint64_t cause, uint64_t tval) { return {cause, tval}; }

enum class Step { Retired, Trapped, Halt, MmioExit, Error };

constexpr int64_t sext32(uint64_t v) { return static_cast<int32_t>(static_cast<uint32_t>(v)); }

// Division per Unpriv §13.2 Table 13.1: x/0 = all ones, x%0 = x; the signed
// overflow case (most negative / -1) yields the dividend and remainder 0.
// The special cases are handled before the C++ operator, whose behaviour
// would be undefined.
template <typename S>
S div_signed(S a, S b) {
  if (b == 0) return -1;
  if (a == std::numeric_limits<S>::min() && b == -1) return a;
  return a / b;
}
template <typename S>
S rem_signed(S a, S b) {
  if (b == 0) return a;
  if (a == std::numeric_limits<S>::min() && b == -1) return 0;
  return a % b;
}
template <typename U>
U div_unsigned(U a, U b) {
  return b == 0 ? std::numeric_limits<U>::max() : a / b;
}
template <typename U>
U rem_unsigned(U a, U b) {
  return b == 0 ? a : a % b;
}

template <bool kTrace>
void emit(rv_machine* m, const rv_trace_record& rec) {
  if constexpr (kTrace) m->trace[m->trace_len++] = rec;
}

// Trap entry (Priv §3.1.6.1, §3.1.7, §3.1.8, §3.1.14-16, §4.1.1). A trap
// from S- or U-mode whose cause is delegated in medeleg/mideleg goes to
// S-mode; everything else, and every trap taken in M-mode, goes to M-mode.
template <bool kTrace>
Step take_trap(rv_machine* m, Exception e, rv_trace_record& rec) {
  Hart& h = m->hart;
  Csrs& c = h.csr;
  const Priv from = h.priv;
  const bool interrupt = (e.cause & kInterruptBit) != 0;
  const uint64_t code = e.cause & ~kInterruptBit;
  const uint64_t deleg = interrupt ? c.mideleg : c.medeleg;
  const bool to_s = from <= kPrivS && code < 64 && ((deleg >> code) & 1) != 0;

  uint64_t s = c.mstatus;
  uint64_t tvec;
  if (to_s) {
    c.sepc = h.pc & ~1ull;
    c.scause = e.cause;
    c.stval = e.tval;
    s = (s & ~mstatus::kSPIE) | ((s & mstatus::kSIE) ? mstatus::kSPIE : 0);
    s &= ~mstatus::kSIE;
    s = (s & ~mstatus::kSPP) | (from == kPrivS ? mstatus::kSPP : 0);
    h.priv = kPrivS;
    tvec = c.stvec;
  } else {
    c.mepc = h.pc & ~1ull;
    c.mcause = e.cause;
    c.mtval = e.tval;
    s = (s & ~mstatus::kMPIE) | ((s & mstatus::kMIE) ? mstatus::kMPIE : 0);
    s &= ~mstatus::kMIE;
    s = (s & ~mstatus::kMPP) | (static_cast<uint64_t>(from) << mstatus::kMPPShift);
    h.priv = kPrivM;
    tvec = c.mtvec;
  }
  c.mstatus = s;
  // Vectored mode offsets only asynchronous interrupts (Priv §3.1.7).
  const uint64_t base = tvec & ~3ull;
  h.pc = ((tvec & 1) && interrupt) ? base + 4 * code : base;

  m->steps++;
  if constexpr (kTrace) {
    rec.kind = RV_TRACE_TRAP;
    rec.priv = from;
    rec.flags = 0;
    rec.rd = h.priv;
    rec.rd_value = e.cause;
    rec.csr_value = h.pc;
    rec.mem_addr = e.tval;
    emit<kTrace>(m, rec);
  }
  return Step::Trapped;
}

// The interrupt to take before the next instruction, or 0 (Priv §3.1.9).
// M-level (undelegated) interrupts are taken in any mode below M, or in M
// with mstatus.MIE set. Delegated interrupts are taken below S, or in S with
// sstatus.SIE set, and never in M. Higher-privilege interrupts go first;
// within a level the order is MEI, MSI, MTI, SEI, SSI, STI.
uint64_t pending_interrupt(const rv_machine* m) {
  const Hart& h = m->hart;
  const Csrs& c = h.csr;
  const uint64_t pend = mip_effective(m) & c.mie & ~m->irq_suppress;
  if (pend == 0) return 0;
  const uint64_t m_pend = pend & ~c.mideleg;
  const uint64_t s_pend = pend & c.mideleg;
  const bool m_enabled = h.priv < kPrivM || (c.mstatus & mstatus::kMIE) != 0;
  const bool s_enabled = h.priv < kPrivS || (h.priv == kPrivS && (c.mstatus & mstatus::kSIE) != 0);
  uint64_t take = 0;
  if (m_enabled && m_pend != 0) {
    take = m_pend;
  } else if (s_enabled && s_pend != 0) {
    take = s_pend;
  } else {
    return 0;
  }
  for (Irq irq : {kIrqMEI, kIrqMSI, kIrqMTI, kIrqSEI, kIrqSSI, kIrqSTI}) {
    if (take & (1ull << irq)) return kInterruptBit | irq;
  }
  return 0;
}

// Priv §3.3.3: a waiting hart resumes when any interrupt is pending and
// enabled in mie, regardless of the global enables and delegation.
bool wfi_wakeup(const rv_machine* m) {
  return (mip_effective(m) & m->hart.csr.mie & ~m->irq_suppress) != 0;
}

// Counters advance once per retired instruction (Spike's CPI = 1 model,
// ADR-015) unless inhibited, and an explicit write replaces the increment.
void bump_counters(Csrs& c) {
  if (c.mcycle_written) {
    c.mcycle_written = false;
  } else if ((c.mcountinhibit & 1) == 0) {
    c.mcycle++;
  }
  if (c.minstret_written) {
    c.minstret_written = false;
  } else if ((c.mcountinhibit & 4) == 0) {
    c.minstret++;
  }
}

// An access outside RAM and the CLINT goes to the VMM (ADR-022). The first
// time, it records the request and stops the instruction before any state
// changes; once rv_complete_mmio has supplied the result, re-executing the
// instruction reaches the same access and consumes it. MMIO accesses must be
// naturally aligned; other sizes and atomics raise access faults.
Exception mmio_access(rv_machine* m, uint64_t addr, uint64_t vaddr, size_t size, bool is_store,
                      uint64_t& value) {
  auto& io = m->mmio;
  if ((addr & (size - 1)) != 0) return raise(is_store ? kCauseStoreAccess : kCauseLoadAccess, vaddr);
  if (io.completed) {
    if (io.addr != addr || io.size != size || io.is_store != is_store) {
      return raise(kMmioExit, 1);  // re-execution diverged: an internal error
    }
    io.pending = io.completed = false;
    if (io.fault) return raise(is_store ? kCauseStoreAccess : kCauseLoadAccess, vaddr);
    if (!is_store) value = io.value;
    return ok();
  }
  io.pending = true;
  io.is_store = is_store;
  io.fault = false;
  io.size = static_cast<uint8_t>(size);
  io.addr = addr;
  io.value = is_store ? value : 0;
  return raise(kMmioExit, 0);
}

constexpr uint64_t kPageSize = 4096;

// Translates an access of `size` bytes at vaddr. An access that crosses a
// page boundary (possible only with RV_CFG_MISALIGNED_OK) translates both
// pages before anything is touched, so a fault leaves memory unchanged; it
// must then lie in RAM, where it is performed byte by byte.
struct Phys {
  uint64_t first;
  uint64_t second;  // the physical address of the byte at the page boundary
  size_t split;     // bytes on the first page; == size when not split
};

Exception translate_access(rv_machine* m, uint64_t vaddr, size_t size, Access a, Phys& p) {
  const Translation t = translate(m, vaddr, a);
  if (!t.ok()) return raise(t.cause, vaddr);
  p = {t.paddr, 0, size};
  const uint64_t room = kPageSize - (vaddr & (kPageSize - 1));
  if (room < size) {
    const Translation u = translate(m, vaddr + room, a);
    if (!u.ok()) return raise(u.cause, vaddr + room);
    p.second = u.paddr;
    p.split = room;
    const uint64_t fault = a == Access::Store ? kCauseStoreAccess : kCauseLoadAccess;
    if (!m->ram_contains(p.first, room) || !m->ram_contains(p.second, size - room)) return raise(fault, vaddr);
  }
  return ok();
}

template <typename T>
Exception load(rv_machine* m, uint64_t vaddr, T& out) {
  // Misaligned accesses trap unless configured otherwise (ADR-004, Unpriv §2.6).
  if ((vaddr & (sizeof(T) - 1)) != 0 && (m->cfg.flags & RV_CFG_MISALIGNED_OK) == 0) {
    return raise(kCauseMisalignedLoad, vaddr);
  }
  Phys p;
  if (Exception e = translate_access(m, vaddr, sizeof(T), Access::Load, p); e.raised()) return e;
  if (p.split != sizeof(T)) {
    auto* bytes = reinterpret_cast<uint8_t*>(&out);
    std::memcpy(bytes, m->ram_ptr(p.first), p.split);
    std::memcpy(bytes + p.split, m->ram_ptr(p.second), sizeof(T) - p.split);
    return ok();
  }
  const uint64_t addr = p.first;
  if (m->ram_contains(addr, sizeof(T))) {
    std::memcpy(&out, m->ram_ptr(addr), sizeof(T));
    return ok();
  }
  uint64_t v = 0;
  if (m->clint_contains(addr, sizeof(T))) {
    if (!clint_load(m, addr, sizeof(T), v)) return raise(kCauseLoadAccess, vaddr);
  } else if (Exception e = mmio_access(m, addr, vaddr, sizeof(T), false, v); e.raised()) {
    return e;
  }
  out = static_cast<T>(v);
  return ok();
}

// Writes RAM that has already been checked, then applies HTIF (ADR-013): a
// command is latched when a store writes the most significant byte of
// tohost and leaves tohost non-zero. Guests write the 64-bit command as two
// 32-bit stores, low word first, so latching on the low word would act on a
// half-written command.
template <typename T>
void store_ram(rv_machine* m, uint64_t addr, T value) {
  std::memcpy(m->ram_ptr(addr), &value, sizeof(T));
  const uint64_t tohost = m->cfg.htif_tohost;
  if (tohost != 0 && addr <= tohost + 7 && tohost + 7 < addr + sizeof(T)) {
    uint64_t v;
    std::memcpy(&v, m->ram_ptr(tohost), 8);
    m->htif_halt = v != 0;
  }
}

template <typename T>
Exception store(rv_machine* m, uint64_t vaddr, T value) {
  if ((vaddr & (sizeof(T) - 1)) != 0 && (m->cfg.flags & RV_CFG_MISALIGNED_OK) == 0) {
    return raise(kCauseMisalignedStore, vaddr);
  }
  Phys p;
  if (Exception e = translate_access(m, vaddr, sizeof(T), Access::Store, p); e.raised()) return e;
  if (p.split != sizeof(T)) {
    const auto* bytes = reinterpret_cast<const uint8_t*>(&value);
    std::memcpy(m->ram_ptr(p.first), bytes, p.split);
    std::memcpy(m->ram_ptr(p.second), bytes + p.split, sizeof(T) - p.split);
    return ok();
  }
  const uint64_t addr = p.first;
  if (m->ram_contains(addr, sizeof(T))) {
    store_ram(m, addr, value);
    return ok();
  }
  uint64_t v = static_cast<uint64_t>(value);
  if (m->clint_contains(addr, sizeof(T))) {
    return clint_store(m, addr, sizeof(T), v) ? ok() : raise(kCauseStoreAccess, vaddr);
  }
  return mmio_access(m, addr, vaddr, sizeof(T), true, v);
}

// Atomics require natural alignment regardless of RV_CFG_MISALIGNED_OK, and
// only RAM supports them (Unpriv §14.2, §14.4; Priv §3.6.3). LR translates
// and faults as a load; SC and AMOs as a store/AMO. On success paddr holds
// the physical address.
template <typename T>
Exception check_atomic(rv_machine* m, uint64_t vaddr, bool is_lr, uint64_t& paddr) {
  if ((vaddr & (sizeof(T) - 1)) != 0) {
    return raise(is_lr ? kCauseMisalignedLoad : kCauseMisalignedStore, vaddr);
  }
  const Translation t = translate(m, vaddr, is_lr ? Access::Load : Access::Store);
  if (!t.ok()) return raise(t.cause, vaddr);
  if (!m->ram_contains(t.paddr, sizeof(T))) return raise(is_lr ? kCauseLoadAccess : kCauseStoreAccess, vaddr);
  paddr = t.paddr;
  return ok();
}

template <bool kTrace>
class Executor {
 public:
  Executor(rv_machine* m, const Insn& d, rv_trace_record& rec)
      : m_(m), h_(m->hart), d_(d), rec_(rec), pc_(m->hart.pc), next_(pc_ + d.len) {}

  uint64_t next_pc() const { return next_; }

  Exception execute() {
    const uint64_t a = h_.x[d_.rs1];
    const uint64_t b = h_.x[d_.rs2];
    const uint64_t imm = static_cast<uint64_t>(d_.imm);
    const unsigned sh64 = static_cast<unsigned>(b & 63);
    const unsigned sh32 = static_cast<unsigned>(b & 31);
    const unsigned ish = static_cast<unsigned>(imm & 63);
    switch (d_.op) {
      case Op::Lui: wr(imm); return ok();
      case Op::Auipc: wr(pc_ + imm); return ok();
      case Op::Jal: return jump(pc_ + imm);
      case Op::Jalr: return jump((a + imm) & ~1ull);
      case Op::Beq: return branch(a == b);
      case Op::Bne: return branch(a != b);
      case Op::Blt: return branch(static_cast<int64_t>(a) < static_cast<int64_t>(b));
      case Op::Bge: return branch(static_cast<int64_t>(a) >= static_cast<int64_t>(b));
      case Op::Bltu: return branch(a < b);
      case Op::Bgeu: return branch(a >= b);
      case Op::Lb: return do_load<int8_t>(a + imm);
      case Op::Lh: return do_load<int16_t>(a + imm);
      case Op::Lw: return do_load<int32_t>(a + imm);
      case Op::Ld: return do_load<int64_t>(a + imm);
      case Op::Lbu: return do_load<uint8_t>(a + imm);
      case Op::Lhu: return do_load<uint16_t>(a + imm);
      case Op::Lwu: return do_load<uint32_t>(a + imm);
      case Op::Sb: return do_store<uint8_t>(a + imm, b);
      case Op::Sh: return do_store<uint16_t>(a + imm, b);
      case Op::Sw: return do_store<uint32_t>(a + imm, b);
      case Op::Sd: return do_store<uint64_t>(a + imm, b);
      case Op::Addi: wr(a + imm); return ok();
      case Op::Slti: wr(static_cast<int64_t>(a) < d_.imm); return ok();
      case Op::Sltiu: wr(a < imm); return ok();
      case Op::Xori: wr(a ^ imm); return ok();
      case Op::Ori: wr(a | imm); return ok();
      case Op::Andi: wr(a & imm); return ok();
      case Op::Slli: wr(a << ish); return ok();
      case Op::Srli: wr(a >> ish); return ok();
      case Op::Srai: wr(static_cast<uint64_t>(static_cast<int64_t>(a) >> ish)); return ok();
      case Op::Add: wr(a + b); return ok();
      case Op::Sub: wr(a - b); return ok();
      case Op::Sll: wr(a << sh64); return ok();
      case Op::Slt: wr(static_cast<int64_t>(a) < static_cast<int64_t>(b)); return ok();
      case Op::Sltu: wr(a < b); return ok();
      case Op::Xor: wr(a ^ b); return ok();
      case Op::Srl: wr(a >> sh64); return ok();
      case Op::Sra: wr(static_cast<uint64_t>(static_cast<int64_t>(a) >> sh64)); return ok();
      case Op::Or: wr(a | b); return ok();
      case Op::And: wr(a & b); return ok();
      // *W ops compute on the low 32 bits and sign-extend (Unpriv §5.2).
      case Op::Addiw: wrs(sext32(a + imm)); return ok();
      case Op::Slliw: wrs(sext32(static_cast<uint32_t>(a) << ish)); return ok();
      case Op::Srliw: wrs(sext32(static_cast<uint32_t>(a) >> ish)); return ok();
      case Op::Sraiw: wrs(static_cast<int32_t>(a) >> ish); return ok();
      case Op::Addw: wrs(sext32(a + b)); return ok();
      case Op::Subw: wrs(sext32(a - b)); return ok();
      case Op::Sllw: wrs(sext32(static_cast<uint32_t>(a) << sh32)); return ok();
      case Op::Srlw: wrs(sext32(static_cast<uint32_t>(a) >> sh32)); return ok();
      case Op::Sraw: wrs(static_cast<int32_t>(a) >> sh32); return ok();
      // M extension (Unpriv §13.1-13.2).
      case Op::Mul: wr(a * b); return ok();
      case Op::Mulh:
        wr(static_cast<uint64_t>((static_cast<i128>(static_cast<int64_t>(a)) * static_cast<int64_t>(b)) >> 64));
        return ok();
      case Op::Mulhsu:
        wr(static_cast<uint64_t>((static_cast<i128>(static_cast<int64_t>(a)) * static_cast<i128>(b)) >> 64));
        return ok();
      case Op::Mulhu: wr(static_cast<uint64_t>((static_cast<u128>(a) * b) >> 64)); return ok();
      case Op::Div: wrs(div_signed(static_cast<int64_t>(a), static_cast<int64_t>(b))); return ok();
      case Op::Divu: wr(div_unsigned(a, b)); return ok();
      case Op::Rem: wrs(rem_signed(static_cast<int64_t>(a), static_cast<int64_t>(b))); return ok();
      case Op::Remu: wr(rem_unsigned(a, b)); return ok();
      case Op::Mulw: wrs(sext32(a * b)); return ok();
      case Op::Divw: wrs(div_signed(static_cast<int32_t>(a), static_cast<int32_t>(b))); return ok();
      case Op::Divuw:
        wrs(sext32(div_unsigned(static_cast<uint32_t>(a), static_cast<uint32_t>(b))));
        return ok();
      case Op::Remw: wrs(rem_signed(static_cast<int32_t>(a), static_cast<int32_t>(b))); return ok();
      case Op::Remuw:
        wrs(sext32(rem_unsigned(static_cast<uint32_t>(a), static_cast<uint32_t>(b))));
        return ok();
      // A extension (Unpriv ch. 14).
      case Op::LrW: return lr<int32_t>(a);
      case Op::LrD: return lr<int64_t>(a);
      case Op::ScW: return sc<uint32_t>(a, b);
      case Op::ScD: return sc<uint64_t>(a, b);
      case Op::AmoswapW: return amo<uint32_t>(a, [b](uint32_t) { return static_cast<uint32_t>(b); });
      case Op::AmoaddW: return amo<uint32_t>(a, [b](uint32_t v) { return v + static_cast<uint32_t>(b); });
      case Op::AmoxorW: return amo<uint32_t>(a, [b](uint32_t v) { return v ^ static_cast<uint32_t>(b); });
      case Op::AmoandW: return amo<uint32_t>(a, [b](uint32_t v) { return v & static_cast<uint32_t>(b); });
      case Op::AmoorW: return amo<uint32_t>(a, [b](uint32_t v) { return v | static_cast<uint32_t>(b); });
      case Op::AmominW:
        return amo<uint32_t>(a, [b](uint32_t v) {
          return static_cast<int32_t>(v) < static_cast<int32_t>(b) ? v : static_cast<uint32_t>(b);
        });
      case Op::AmomaxW:
        return amo<uint32_t>(a, [b](uint32_t v) {
          return static_cast<int32_t>(v) > static_cast<int32_t>(b) ? v : static_cast<uint32_t>(b);
        });
      case Op::AmominuW: return amo<uint32_t>(a, [b](uint32_t v) { return std::min(v, static_cast<uint32_t>(b)); });
      case Op::AmomaxuW: return amo<uint32_t>(a, [b](uint32_t v) { return std::max(v, static_cast<uint32_t>(b)); });
      case Op::AmoswapD: return amo<uint64_t>(a, [b](uint64_t) { return b; });
      case Op::AmoaddD: return amo<uint64_t>(a, [b](uint64_t v) { return v + b; });
      case Op::AmoxorD: return amo<uint64_t>(a, [b](uint64_t v) { return v ^ b; });
      case Op::AmoandD: return amo<uint64_t>(a, [b](uint64_t v) { return v & b; });
      case Op::AmoorD: return amo<uint64_t>(a, [b](uint64_t v) { return v | b; });
      case Op::AmominD:
        return amo<uint64_t>(a, [b](uint64_t v) { return static_cast<int64_t>(v) < static_cast<int64_t>(b) ? v : b; });
      case Op::AmomaxD:
        return amo<uint64_t>(a, [b](uint64_t v) { return static_cast<int64_t>(v) > static_cast<int64_t>(b) ? v : b; });
      case Op::AmominuD: return amo<uint64_t>(a, [b](uint64_t v) { return std::min(v, b); });
      case Op::AmomaxuD: return amo<uint64_t>(a, [b](uint64_t v) { return std::max(v, b); });
      // A single in-order hart with no caches: FENCE and FENCE.I need no work.
      // FENCE.I will invalidate the decode cache in Phase 12.
      case Op::Fence:
      case Op::FenceI: return ok();
      // Ecall causes are 8 + privilege (Priv §3.1.15).
      case Op::Ecall: return raise(kCauseEcallU + static_cast<uint64_t>(h_.priv), 0);
      // tval = pc for breakpoints (Priv §3.1.16), matching Spike.
      case Op::Ebreak: return raise(kCauseBreakpoint, pc_);
      case Op::Csrrw:
      case Op::Csrrs:
      case Op::Csrrc:
      case Op::Csrrwi:
      case Op::Csrrsi:
      case Op::Csrrci: return csr();
      case Op::Mret: return mret();
      case Op::Sret: return sret();
      case Op::Wfi: return wfi();
      case Op::SfenceVma: return sfence_vma();
      case Op::Illegal: return illegal();
    }
    return illegal();
  }

 private:
  // Illegal instruction: tval holds the instruction bits (Priv §3.1.16), matching Spike.
  Exception illegal() const { return raise(kCauseIllegalInsn, d_.raw); }

  void wr(uint64_t v) {
    if (d_.rd == 0) return;
    h_.x[d_.rd] = v;
    if constexpr (kTrace) {
      rec_.flags |= RV_TF_RD;
      rec_.rd = d_.rd;
      rec_.rd_value = v;
    }
  }
  void wrs(int64_t v) { wr(static_cast<uint64_t>(v)); }

  Exception jump(uint64_t target) {
    if ((target & kIalignMask) != 0) return raise(kCauseMisalignedFetch, target);
    wr(next_);
    next_ = target;
    return ok();
  }

  Exception branch(bool taken) {
    if (!taken) return ok();
    const uint64_t target = pc_ + static_cast<uint64_t>(d_.imm);
    if ((target & kIalignMask) != 0) return raise(kCauseMisalignedFetch, target);
    next_ = target;
    return ok();
  }

  template <typename T>
  Exception do_load(uint64_t addr) {
    T v;
    if (Exception e = load(m_, addr, v); e.raised()) return e;
    wrs(static_cast<int64_t>(v));
    if constexpr (kTrace) {
      rec_.flags |= RV_TF_LOAD;
      rec_.mem_addr = addr;
      rec_.mem_size = sizeof(T);
    }
    return ok();
  }

  template <typename T>
  Exception do_store(uint64_t addr, uint64_t value) {
    const T v = static_cast<T>(value);
    if (Exception e = store(m_, addr, v); e.raised()) return e;
    if constexpr (kTrace) {
      rec_.flags |= RV_TF_STORE;
      rec_.mem_addr = addr;
      rec_.mem_size = sizeof(T);
      rec_.mem_value = v;
    }
    return ok();
  }

  // LR (Unpriv §14.2): load and register a reservation on the address.
  template <typename T>
  Exception lr(uint64_t addr) {
    uint64_t pa;
    if (Exception e = check_atomic<T>(m_, addr, true, pa); e.raised()) return e;
    T v;
    std::memcpy(&v, m_->ram_ptr(pa), sizeof(T));
    m_->reservation = pa;
    wrs(static_cast<int64_t>(v));
    if constexpr (kTrace) {
      rec_.flags |= RV_TF_LOAD;
      rec_.mem_addr = addr;
      rec_.mem_size = sizeof(T);
    }
    return ok();
  }

  // SC (Unpriv §14.2): store only if the reservation is still on this
  // address; rd = 0 on success, 1 on failure. Alignment and access faults are
  // raised even when the SC would fail, and every SC clears the reservation.
  // Like Spike, the reservation matches on address alone, and plain stores by
  // this hart do not clear it: the spec only requires failure after another
  // hart's store to the reservation set.
  template <typename T>
  Exception sc(uint64_t addr, uint64_t value) {
    uint64_t pa;
    if (Exception e = check_atomic<T>(m_, addr, false, pa); e.raised()) return e;
    const bool success = m_->reservation == pa;
    m_->reservation = kNoReservation;
    if (success) {
      store_ram(m_, pa, static_cast<T>(value));
      if constexpr (kTrace) {
        rec_.flags |= RV_TF_STORE;
        rec_.mem_addr = addr;
        rec_.mem_size = sizeof(T);
        rec_.mem_value = static_cast<T>(value);
      }
    }
    wr(success ? 0 : 1);
    return ok();
  }

  // AMO (Unpriv §14.4): rd gets the old value (sign-extended for .W), memory
  // gets op(old, rs2).
  template <typename T, typename F>
  Exception amo(uint64_t addr, F op) {
    uint64_t pa;
    if (Exception e = check_atomic<T>(m_, addr, false, pa); e.raised()) return e;
    T old;
    std::memcpy(&old, m_->ram_ptr(pa), sizeof(T));
    const T updated = op(old);
    store_ram(m_, pa, updated);
    wrs(sizeof(T) == 4 ? sext32(old) : static_cast<int64_t>(old));
    if constexpr (kTrace) {
      rec_.flags |= RV_TF_LOAD | RV_TF_STORE;
      rec_.mem_addr = addr;
      rec_.mem_size = sizeof(T);
      rec_.mem_value = updated;
    }
    return ok();
  }

  // Zicsr §9.1. CSRRW/CSRRWI with rd=x0 do not read; CSRRS/CSRRC(I) with
  // rs1=x0 / uimm=0 do not write, so read-only CSRs can be read with them.
  Exception csr() {
    const auto num = static_cast<uint16_t>(d_.imm);
    const bool imm_form = d_.op == Op::Csrrwi || d_.op == Op::Csrrsi || d_.op == Op::Csrrci;
    const uint64_t src = imm_form ? d_.rs1 : h_.x[d_.rs1];
    const bool is_rw = d_.op == Op::Csrrw || d_.op == Op::Csrrwi;
    const bool writes = is_rw || d_.rs1 != 0;

    if (!csr_access_allowed(m_, num, h_.priv, writes)) return illegal();
    uint64_t old = 0;
    // Our CSR reads have no side effects, so reading also checks existence
    // when rd=x0 suppresses the architectural read.
    if (!csr_read(m_, num, old)) return illegal();
    if (writes) {
      const bool is_rs = d_.op == Op::Csrrs || d_.op == Op::Csrrsi;
      const uint64_t base = csr_rmw_base(m_, num, old);
      const uint64_t v = is_rw ? src : is_rs ? (base | src) : (base & ~src);
      if (!csr_write(m_, num, v)) return illegal();
      if constexpr (kTrace) {
        const CsrLogTargets t = csr_log_targets(num);
        uint64_t now = 0;
        csr_read(m_, t.first, now);
        rec_.flags |= RV_TF_CSR;
        rec_.csr = t.first;
        rec_.csr_value = now;
        if (t.second != 0) {
          csr_read(m_, t.second, now);
          rec_.flags |= RV_TF_CSR2;
          rec_.csr2 = t.second;
          rec_.csr2_value = now;
        }
      }
    }
    wr(old);
    return ok();
  }

  // MRET (Priv §3.3.2): pop the M-mode interrupt stack.
  Exception mret() {
    if (h_.priv != kPrivM) return illegal();
    Csrs& c = h_.csr;
    const auto mpp = static_cast<Priv>((c.mstatus & mstatus::kMPP) >> mstatus::kMPPShift);
    uint64_t s = c.mstatus;
    s = (s & ~mstatus::kMIE) | ((s & mstatus::kMPIE) ? mstatus::kMIE : 0);
    s |= mstatus::kMPIE;
    s &= ~mstatus::kMPP;  // MPP <- U, the least-privileged supported mode
    if (mpp != kPrivM) s &= ~mstatus::kMPRV;
    c.mstatus = s;
    h_.priv = mpp;
    next_ = c.mepc;
    log_mstatus();
    return ok();
  }

  // SRET (Priv §3.3.2): pop the S-mode interrupt stack. Illegal in U-mode,
  // and in S-mode when mstatus.TSR is set (Priv §3.1.6.5). Leaving for a
  // mode below M clears MPRV (Priv 1.12 §3.1.6.3).
  Exception sret() {
    Csrs& c = h_.csr;
    if (h_.priv == kPrivU || (h_.priv == kPrivS && (c.mstatus & mstatus::kTSR) != 0)) return illegal();
    const Priv spp = (c.mstatus & mstatus::kSPP) ? kPrivS : kPrivU;
    uint64_t s = c.mstatus;
    s = (s & ~mstatus::kSIE) | ((s & mstatus::kSPIE) ? mstatus::kSIE : 0);
    s |= mstatus::kSPIE;
    s &= ~(mstatus::kSPP | mstatus::kMPRV);
    c.mstatus = s;
    h_.priv = spp;
    next_ = c.sepc;
    log_mstatus();
    return ok();
  }

  // WFI (Priv §3.3.3): with mstatus.TW set it is illegal below M-mode; with
  // S-mode implemented it is illegal in U-mode (ADR-009). Otherwise it
  // retires and the hart waits for an interrupt (see loop()).
  Exception wfi() {
    if (h_.priv < kPrivM && (h_.csr.mstatus & mstatus::kTW) != 0) return illegal();
    if (h_.priv == kPrivU) return illegal();
    m_->in_wfi = true;
    return ok();
  }

  // SFENCE.VMA (Priv §4.2.1, §3.1.6.5): illegal in U-mode, and in S-mode
  // when mstatus.TVM is set. rs1 = x0 means all addresses; rs2 = x0 means
  // all address spaces (and then global mappings are flushed too).
  Exception sfence_vma() {
    if (h_.priv == kPrivU) return illegal();
    if (h_.priv == kPrivS && (h_.csr.mstatus & mstatus::kTVM) != 0) return illegal();
    tlb_flush(m_, d_.rs1 != 0, h_.x[d_.rs1], d_.rs2 != 0, h_.x[d_.rs2] & 0xffff);
    return ok();
  }

  void log_mstatus() {
    if constexpr (kTrace) {
      uint64_t now = 0;
      csr_read(m_, csrno::kMstatus, now);
      rec_.flags |= RV_TF_CSR;
      rec_.csr = csrno::kMstatus;
      rec_.csr_value = now;
    }
  }

  rv_machine* m_;
  Hart& h_;
  const Insn& d_;
  rv_trace_record& rec_;
  const uint64_t pc_;
  uint64_t next_;
};

template <bool kTrace>
Step step(rv_machine* m) {
  Hart& h = m->hart;
  rv_trace_record rec{};
  if constexpr (kTrace) {
    rec.pc = h.pc;
    rec.priv = h.priv;
  }

  // Pending interrupts are taken between instructions (Priv §3.1.9), but not
  // while an instruction is finishing an MMIO access it already started.
  if (!m->mmio.completed) {
    if (const uint64_t irq = pending_interrupt(m); irq != 0) {
      return take_trap<kTrace>(m, raise(irq, 0), rec);
    }
  }

  // Fetch one parcel, then a second if the first says the instruction is 32
  // bits (Unpriv §1.5). RAM is the only executable region. A fault on the
  // second parcel reports that parcel's address in mtval while mepc points
  // at the instruction (Priv §3.1.16).
  // Each parcel is translated on its own, so an instruction that straddles
  // a page boundary can fault on its second page (Priv §4.3.2).
  const Translation t0 = translate(m, h.pc, Access::Fetch);
  if (!t0.ok()) return take_trap<kTrace>(m, raise(t0.cause, h.pc), rec);
  if (!m->ram_contains(t0.paddr, 2)) return take_trap<kTrace>(m, raise(kCauseFetchAccess, h.pc), rec);
  uint16_t lo;
  std::memcpy(&lo, m->ram_ptr(t0.paddr), 2);
  Insn d;
  if ((lo & 3) != 3) {
    d = decode16(lo);
  } else {
    const uint64_t pc2 = h.pc + 2;
    uint64_t pa2 = t0.paddr + 2;
    if ((pc2 & (kPageSize - 1)) == 0) {
      const Translation t1 = translate(m, pc2, Access::Fetch);
      if (!t1.ok()) return take_trap<kTrace>(m, raise(t1.cause, pc2), rec);
      pa2 = t1.paddr;
    }
    if (!m->ram_contains(pa2, 2)) return take_trap<kTrace>(m, raise(kCauseFetchAccess, pc2), rec);
    uint16_t hi;
    std::memcpy(&hi, m->ram_ptr(pa2), 2);
    d = decode32(static_cast<uint32_t>(lo) | (static_cast<uint32_t>(hi) << 16));
  }
  if constexpr (kTrace) {
    rec.insn = d.raw;
    rec.insn_len = d.len;
  }

  Executor<kTrace> ex(m, d, rec);
  if (Exception e = ex.execute(); e.raised()) {
    if (e.mmio_exit()) return e.tval == 0 ? Step::MmioExit : Step::Error;
    return take_trap<kTrace>(m, e, rec);
  }

  h.pc = ex.next_pc();
  bump_counters(h.csr);
  m->instret++;
  m->steps++;
  if constexpr (kTrace) {
    rec.kind = RV_TRACE_COMMIT;
    emit<kTrace>(m, rec);
  }
  return m->htif_halt ? Step::Halt : Step::Retired;
}

template <bool kTrace>
rv_exit_reason loop(rv_machine* m, uint64_t end) {
  while (m->steps < end) {
    // A hart in WFI stays idle until an interrupt is pending; the VMM decides
    // how much virtual time passes (rv_advance_idle).
    if (m->in_wfi) {
      if (!wfi_wakeup(m)) return RV_EXIT_WFI;
      m->in_wfi = false;
    }
    // Each step emits exactly one trace record; stop while there is room for none.
    if constexpr (kTrace) {
      if (m->trace_len == m->trace_cap) return RV_EXIT_TRACE_FULL;
    }
    switch (step<kTrace>(m)) {
      case Step::Halt: return RV_EXIT_HALT;
      case Step::MmioExit: return m->mmio.is_store ? RV_EXIT_MMIO_WRITE : RV_EXIT_MMIO_READ;
      case Step::Error: return RV_EXIT_ERROR;
      case Step::Retired:
      case Step::Trapped: break;
    }
  }
  return RV_EXIT_BUDGET;
}

}  // namespace

void reset_hart(rv_machine* m) {
  // Priv §3.4: M-mode, MIE=0, MPRV=0, pc = reset vector, mcause = 0
  // (no distinguishable reset causes). Everything else we also zero.
  m->hart = Hart{};
  m->hart.pc = m->cfg.reset_pc;
  m->steps = 0;
  m->instret = 0;
  m->htif_halt = false;
  m->in_wfi = false;
  m->mmio = rv_machine::Mmio{};
  for (TlbEntry& t : m->tlb) t = TlbEntry{};
  m->mtimecmp = 0;
  m->mtime_offset = 0;
  m->reservation = kNoReservation;
}

void run(rv_machine* m, uint64_t max_steps) {
  const uint64_t end = max_steps > UINT64_MAX - m->steps ? UINT64_MAX : m->steps + max_steps;
  const rv_exit_reason reason = m->trace != nullptr ? loop<true>(m, end) : loop<false>(m, end);

  m->exit = rv_exit{};
  m->exit.reason = reason;
  m->exit.pc = m->hart.pc;
  m->exit.steps = m->steps;
  m->exit.instret = m->instret;
  m->exit.timer_step = timer_step(m);
  switch (reason) {
    case RV_EXIT_HALT:
      m->htif_halt = false;
      m->exit.addr = m->cfg.htif_tohost;
      std::memcpy(&m->exit.value, m->ram_ptr(m->cfg.htif_tohost), 8);
      break;
    case RV_EXIT_MMIO_READ:
    case RV_EXIT_MMIO_WRITE:
      m->exit.addr = m->mmio.addr;
      m->exit.size = m->mmio.size;
      m->exit.value = m->mmio.is_store ? m->mmio.value : 0;
      break;
    case RV_EXIT_ERROR:
      // Re-executing an instruction to finish its MMIO access reached a
      // different access: a core invariant broke. The machine is unusable.
      m->poisoned = true;
      m->exit.value = RV_ERROR_MMIO_REPLAY;
      break;
    default: break;
  }
}

}  // namespace rv
