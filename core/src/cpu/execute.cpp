// The interpreter: fetch, decode, execute, trap. One call to step() is one
// step of virtual time: either a retired instruction or a taken trap.
#include <cstring>

#include "csr/csr.hpp"
#include "decode/decode.hpp"
#include "machine.hpp"

namespace rv {
namespace {

// IALIGN=32 until the C extension arrives (Unpriv §1.5): a jump or taken
// branch to an address that is not 4-byte aligned raises
// instruction-address-misaligned (Unpriv §2.5).
constexpr uint64_t kIalignMask = 3;

constexpr uint64_t kNoException = ~0ull;

struct Exception {
  uint64_t cause = kNoException;
  uint64_t tval = 0;
  bool raised() const { return cause != kNoException; }
};

constexpr Exception ok() { return {}; }
constexpr Exception raise(uint64_t cause, uint64_t tval) { return {cause, tval}; }

enum class Step { Retired, Trapped, Halt };

constexpr int64_t sext32(uint64_t v) { return static_cast<int32_t>(static_cast<uint32_t>(v)); }

template <bool kTrace>
void emit(rv_machine* m, const rv_trace_record& rec) {
  if constexpr (kTrace) m->trace[m->trace_len++] = rec;
}

// Trap entry into M-mode (Priv §3.1.6.1, §3.1.7, §3.1.14-16). Delegation to
// S-mode arrives with S-mode in Phase 3.
template <bool kTrace>
Step take_trap(rv_machine* m, Exception e, rv_trace_record& rec) {
  Hart& h = m->hart;
  Csrs& c = h.csr;
  const Priv from = h.priv;

  c.mepc = h.pc & ~1ull;
  c.mcause = e.cause;
  c.mtval = e.tval;
  uint64_t s = c.mstatus;
  s = (s & ~mstatus::kMPIE) | ((s & mstatus::kMIE) ? mstatus::kMPIE : 0);
  s &= ~mstatus::kMIE;
  s = (s & ~mstatus::kMPP) | (static_cast<uint64_t>(from) << mstatus::kMPPShift);
  c.mstatus = s;
  h.priv = kPrivM;

  const uint64_t base = c.mtvec & ~3ull;
  const bool interrupt = (e.cause >> 63) != 0;
  // Vectored mode offsets only asynchronous interrupts (Priv §3.1.7).
  h.pc = ((c.mtvec & 1) && interrupt) ? base + 4 * (e.cause & ~(1ull << 63)) : base;

  m->steps++;
  if constexpr (kTrace) {
    rec.kind = RV_TRACE_TRAP;
    rec.priv = from;
    rec.flags = 0;
    rec.rd = kPrivM;
    rec.rd_value = e.cause;
    rec.csr_value = h.pc;
    rec.mem_addr = e.tval;
    emit<kTrace>(m, rec);
  }
  return Step::Trapped;
}

template <typename T>
Exception load(rv_machine* m, uint64_t addr, T& out) {
  // Misaligned accesses trap unless configured otherwise (ADR-004, Unpriv §2.6).
  if ((addr & (sizeof(T) - 1)) != 0 && (m->cfg.flags & RV_CFG_MISALIGNED_OK) == 0) {
    return raise(kCauseMisalignedLoad, addr);
  }
  if (!m->ram_contains(addr, sizeof(T))) return raise(kCauseLoadAccess, addr);
  std::memcpy(&out, m->ram_ptr(addr), sizeof(T));
  return ok();
}

template <typename T>
Exception store(rv_machine* m, uint64_t addr, T value) {
  if ((addr & (sizeof(T) - 1)) != 0 && (m->cfg.flags & RV_CFG_MISALIGNED_OK) == 0) {
    return raise(kCauseMisalignedStore, addr);
  }
  if (!m->ram_contains(addr, sizeof(T))) return raise(kCauseStoreAccess, addr);
  std::memcpy(m->ram_ptr(addr), &value, sizeof(T));
  // HTIF: a store that leaves tohost non-zero asks the host to act.
  const uint64_t tohost = m->cfg.htif_tohost;
  if (tohost != 0 && addr < tohost + 8 && tohost < addr + sizeof(T)) {
    uint64_t v;
    std::memcpy(&v, m->ram_ptr(tohost), 8);
    m->htif_halt = v != 0;
  }
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
      case Op::Wfi:
        // Priv §3.3.3: WFI may complete immediately. Spike makes it illegal in
        // U-mode; we match. Idle exits (RV_EXIT_WFI) arrive in Phase 3.
        return h_.priv == kPrivU ? illegal() : ok();
      case Op::Sret:       // no S-mode yet
      case Op::SfenceVma:  // no S-mode yet
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

  // Zicsr §9.1. CSRRW/CSRRWI with rd=x0 do not read; CSRRS/CSRRC(I) with
  // rs1=x0 / uimm=0 do not write, so read-only CSRs can be read with them.
  Exception csr() {
    const auto num = static_cast<uint16_t>(d_.imm);
    const bool imm_form = d_.op == Op::Csrrwi || d_.op == Op::Csrrsi || d_.op == Op::Csrrci;
    const uint64_t src = imm_form ? d_.rs1 : h_.x[d_.rs1];
    const bool is_rw = d_.op == Op::Csrrw || d_.op == Op::Csrrwi;
    const bool writes = is_rw || d_.rs1 != 0;

    if (!csr_permitted(num, h_.priv, writes)) return illegal();
    uint64_t old = 0;
    // Our CSR reads have no side effects, so reading also checks existence
    // when rd=x0 suppresses the architectural read.
    if (!csr_read(m_, num, old)) return illegal();
    if (writes) {
      const bool is_rs = d_.op == Op::Csrrs || d_.op == Op::Csrrsi;
      const uint64_t v = is_rw ? src : is_rs ? (old | src) : (old & ~src);
      if (!csr_write(m_, num, v)) return illegal();
      if constexpr (kTrace) {
        uint64_t now = 0;
        csr_read(m_, num, now);
        rec_.flags |= RV_TF_CSR;
        rec_.csr = num;
        rec_.csr_value = now;
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
    uint64_t epc = 0;
    csr_read(m_, csrno::kMepc, epc);
    next_ = epc;
    if constexpr (kTrace) {
      uint64_t now = 0;
      csr_read(m_, csrno::kMstatus, now);
      rec_.flags |= RV_TF_CSR;
      rec_.csr = csrno::kMstatus;
      rec_.csr_value = now;
    }
    return ok();
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

  // Fetch. RAM is the only executable region.
  if (!m->ram_contains(h.pc, 4)) return take_trap<kTrace>(m, raise(kCauseFetchAccess, h.pc), rec);
  uint32_t raw;
  std::memcpy(&raw, m->ram_ptr(h.pc), 4);
  const Insn d = decode32(raw);
  if constexpr (kTrace) {
    rec.insn = raw;
    rec.insn_len = d.len;
  }

  Executor<kTrace> ex(m, d, rec);
  if (Exception e = ex.execute(); e.raised()) return take_trap<kTrace>(m, e, rec);

  h.pc = ex.next_pc();
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
    // Each step emits exactly one trace record; stop while there is room for none.
    if constexpr (kTrace) {
      if (m->trace_len == m->trace_cap) return RV_EXIT_TRACE_FULL;
    }
    if (step<kTrace>(m) == Step::Halt) return RV_EXIT_HALT;
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
}

void run(rv_machine* m, uint64_t max_steps) {
  const uint64_t end = max_steps > UINT64_MAX - m->steps ? UINT64_MAX : m->steps + max_steps;
  const rv_exit_reason reason = m->trace != nullptr ? loop<true>(m, end) : loop<false>(m, end);

  m->exit = rv_exit{};
  m->exit.reason = reason;
  m->exit.pc = m->hart.pc;
  m->exit.steps = m->steps;
  m->exit.instret = m->instret;
  m->exit.timer_step = UINT64_MAX;  // No CLINT yet (Phase 4).
  if (reason == RV_EXIT_HALT) {
    m->htif_halt = false;
    m->exit.addr = m->cfg.htif_tohost;
    std::memcpy(&m->exit.value, m->ram_ptr(m->cfg.htif_tohost), 8);
  }
}

}  // namespace rv
