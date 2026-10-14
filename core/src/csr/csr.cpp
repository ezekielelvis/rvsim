#include "csr/csr.hpp"

#include "machine.hpp"
#include "mmu/mmu.hpp"

namespace rv {
namespace {

using namespace mstatus;

constexpr uint64_t bit(unsigned n) { return 1ull << n; }

// sstatus is a restricted view of mstatus (Priv §4.1.1). FS exists because
// S-mode is implemented, even without F (Priv §3.1.6.6, as in Spike).
constexpr uint64_t kSstatusWritable = kSIE | kSPIE | kSPP | kFS | kMXR | (kHasMmu ? kSUM : 0);
constexpr uint64_t kSstatusReadable = kSstatusWritable | kSUM | kMXR | (3ull << kUXLShift) | kSD;
constexpr uint64_t kMstatusWritable = kSstatusWritable | kMIE | kMPIE | kMPP | kMPRV | kTW | kTSR | kTVM;
// UXL and SXL are read-only 2: XLEN is always 64 (Priv §3.1.6.3).
constexpr uint64_t kMstatusFixed = (2ull << kUXLShift) | (2ull << kSXLShift);

// Delegable exceptions: everything that can occur except ECALL from M
// (medeleg[11] is read-only zero, Priv §3.1.8) and misaligned fetch, which
// cannot occur with IALIGN=16.
constexpr uint64_t kMedelegWritable =
    bit(kCauseFetchAccess) | bit(kCauseIllegalInsn) | bit(kCauseBreakpoint) | bit(kCauseMisalignedLoad) |
    bit(kCauseLoadAccess) | bit(kCauseMisalignedStore) | bit(kCauseStoreAccess) | bit(kCauseEcallU) |
    bit(kCauseEcallS) | bit(kCauseFetchPageFault) | bit(kCauseLoadPageFault) | bit(kCauseStorePageFault);

constexpr uint64_t kSupervisorIrqs = bit(kIrqSSI) | bit(kIrqSTI) | bit(kIrqSEI);
constexpr uint64_t kAllIrqs = kSupervisorIrqs | bit(kIrqMSI) | bit(kIrqMTI) | bit(kIrqMEI);
// M-mode can only delegate the supervisor interrupts (Priv §3.1.8).
constexpr uint64_t kMidelegWritable = kSupervisorIrqs;
// mip: SSIP, STIP and SEIP are writable by M-mode; MSIP and MTIP belong to
// the CLINT and MEIP to the PLIC (Priv §3.1.9).
constexpr uint64_t kMipWritable = kSupervisorIrqs;

constexpr uint64_t kCounterenWritable = 0x7;          // CY, TM, IR (Zicntr; no Zihpm)
constexpr uint64_t kCountinhibitWritable = 0x5;       // CY, IR (time cannot be inhibited)

uint64_t read_mstatus(const Csrs& c) {
  uint64_t v = c.mstatus | kMstatusFixed;
  if ((v & kFS) == kFS) v |= kSD;  // SD summarizes dirty FS/VS/XS (Priv §3.1.6.6)
  return v;
}

// MPP is WARL over the implemented modes; the reserved H encoding becomes U
// (Priv §3.1.6.1), as in Spike.
uint64_t legalize_mpp(uint64_t v) {
  const uint64_t mpp = (v & kMPP) >> kMPPShift;
  return mpp == 2 ? (v & ~kMPP) : v;
}

bool is_counter(uint16_t csr) { return csr >= csrno::kCycle && csr <= csrno::kInstret; }

}  // namespace

uint64_t mip_effective(const rv_machine* m) {
  const uint64_t lines = m->irq_lines & (bit(kIrqSEI) | bit(kIrqMEI));
  // CLINT: MTIP is pending while mtime >= mtimecmp.
  const uint64_t mtip = m->mtime() >= m->mtimecmp ? bit(kIrqMTI) : 0;
  return m->hart.csr.mip | lines | mtip;
}

bool csr_access_allowed(const rv_machine* m, uint16_t csr, unsigned priv, bool write) {
  if (write && ((csr >> 10) & 3) == 3) return false;
  if (priv < ((csr >> 8) & 3u)) return false;
  const Csrs& c = m->hart.csr;
  if (is_counter(csr)) {
    // Priv §3.1.11 and §4.1.5: below M, mcounteren gates the counters; in
    // U-mode scounteren must also allow them.
    const uint64_t b = bit(csr - csrno::kCycle);
    if (priv < kPrivM && (c.mcounteren & b) == 0) return false;
    if (priv == kPrivU && (c.scounteren & b) == 0) return false;
  }
  if (csr == csrno::kSatp && priv == kPrivS && (c.mstatus & kTVM) != 0) return false;
  return true;
}

bool csr_read(const rv_machine* m, uint16_t csr, uint64_t& out) {
  const Csrs& c = m->hart.csr;
  // There are no HPM counters, so mhpmcounter3-31 and mhpmevent3-31 do not
  // exist and raise illegal-instruction, as UDB and Sail model a hart whose
  // HPM_COUNTER_EN bits are all clear (ADR-019).
  switch (csr) {
    case csrno::kCycle: out = c.mcycle; return true;
    case csrno::kInstret: out = c.minstret; return true;
    case csrno::kTime: out = m->mtime(); return true;  // the CLINT's mtime
    case csrno::kSstatus: out = read_mstatus(c) & kSstatusReadable; return true;
    case csrno::kSie: out = c.mie & c.mideleg; return true;
    case csrno::kStvec: out = c.stvec; return true;
    case csrno::kScounteren: out = c.scounteren; return true;
    case csrno::kSenvcfg: out = c.senvcfg; return true;
    case csrno::kSscratch: out = c.sscratch; return true;
    case csrno::kSepc: out = c.sepc; return true;
    case csrno::kScause: out = c.scause; return true;
    case csrno::kStval: out = c.stval; return true;
    case csrno::kSip: out = mip_effective(m) & c.mideleg; return true;
    case csrno::kSatp: out = c.satp; return true;
    case csrno::kMstatus: out = read_mstatus(c); return true;
    case csrno::kMisa: out = kMisa; return true;
    case csrno::kMedeleg: out = c.medeleg; return true;
    case csrno::kMideleg: out = c.mideleg; return true;
    case csrno::kMie: out = c.mie; return true;
    case csrno::kMtvec: out = c.mtvec; return true;
    case csrno::kMcounteren: out = c.mcounteren; return true;
    case csrno::kMenvcfg: out = c.menvcfg; return true;
    case csrno::kMcountinhibit: out = c.mcountinhibit; return true;
    case csrno::kMscratch: out = c.mscratch; return true;
    // IALIGN=16 with C: only xepc[0] is zero (Priv §3.1.14).
    case csrno::kMepc: out = c.mepc; return true;
    case csrno::kMcause: out = c.mcause; return true;
    case csrno::kMtval: out = c.mtval; return true;
    case csrno::kMip: out = mip_effective(m); return true;
    case csrno::kMcycle: out = c.mcycle; return true;
    case csrno::kMinstret: out = c.minstret; return true;
    case csrno::kMvendorid: out = 0; return true;  // non-commercial implementation
    case csrno::kMarchid: out = 0; return true;    // not yet allocated
    case csrno::kMimpid: out = 0; return true;
    case csrno::kMhartid: out = m->cfg.mhartid; return true;
    case csrno::kMconfigptr: out = 0; return true;  // no configuration structure
    default: return false;
  }
}

bool csr_write(rv_machine* m, uint16_t csr, uint64_t v) {
  Csrs& c = m->hart.csr;
  switch (csr) {
    case csrno::kSstatus:
      c.mstatus = (c.mstatus & ~kSstatusWritable) | (v & kSstatusWritable);
      return true;
    case csrno::kSie:
      // Only delegated interrupts are visible in sie (Priv §4.1.3).
      c.mie = (c.mie & ~c.mideleg) | (v & c.mideleg & kSupervisorIrqs);
      return true;
    case csrno::kStvec:
      if ((v & 3) < 2) c.stvec = v;  // as mtvec
      return true;
    case csrno::kScounteren: c.scounteren = v & kCounterenWritable; return true;
    case csrno::kSenvcfg: c.senvcfg = v & kEnvcfgFIOM; return true;
    case csrno::kSscratch: c.sscratch = v; return true;
    case csrno::kSepc: c.sepc = v & ~1ull; return true;
    case csrno::kScause: c.scause = v; return true;
    case csrno::kStval: c.stval = v; return true;
    case csrno::kSip: {
      // Only SSIP is writable through sip, and only when delegated (Priv §4.1.3).
      const uint64_t mask = c.mideleg & bit(kIrqSSI);
      c.mip = (c.mip & ~mask) | (v & mask);
      return true;
    }
    // Bare and Sv39 are supported. A write naming another mode leaves MODE
    // unchanged but still writes ASID and PPN, as Spike does (Priv §4.1.11
    // makes MODE WARL). Changing satp flushes the TLB (Spike's behaviour; the
    // spec requires an SFENCE.VMA, which is then redundant).
    case csrno::kSatp: {
      const uint64_t mode = v >> satp::kModeShift;
      const bool valid = mode == satp::kModeBare || mode == satp::kModeSv39;
      const uint64_t mode_mask = 0xfull << satp::kModeShift;
      const uint64_t next = (valid ? (v & mode_mask) : (c.satp & mode_mask)) |
                            (v & (satp::kAsidMask | satp::kPpnMask));
      if (next != c.satp) tlb_flush(m, false, 0, false, 0);
      c.satp = next;
      return true;
    }
    case csrno::kMstatus:
      c.mstatus = legalize_mpp((c.mstatus & ~kMstatusWritable) | (v & kMstatusWritable));
      return true;
    case csrno::kMisa: return true;  // read-only WARL: writes are ignored
    case csrno::kMedeleg: c.medeleg = v & kMedelegWritable; return true;
    case csrno::kMideleg: c.mideleg = v & kMidelegWritable; return true;
    case csrno::kMie: c.mie = v & kAllIrqs; return true;
    // MODE values >= 2 are reserved (Priv §3.1.7). WARL: a write naming one
    // leaves the register unchanged, as in Sail (ADR-018).
    case csrno::kMtvec:
      if ((v & 3) < 2) c.mtvec = v;
      return true;
    case csrno::kMcounteren: c.mcounteren = v & kCounterenWritable; return true;
    // FIOM, and ADUE (Svadu) to choose hardware A/D updates (ADR-025).
    case csrno::kMenvcfg: c.menvcfg = v & (kEnvcfgFIOM | kMenvcfgADUE); return true;
    case csrno::kMcountinhibit: c.mcountinhibit = v & kCountinhibitWritable; return true;
    case csrno::kMscratch: c.mscratch = v; return true;
    case csrno::kMepc: c.mepc = v & ~1ull; return true;
    case csrno::kMcause: c.mcause = v; return true;
    case csrno::kMtval: c.mtval = v; return true;
    case csrno::kMip: c.mip = (c.mip & ~kMipWritable) | (v & kMipWritable); return true;
    case csrno::kMcycle:
      c.mcycle = v;
      c.mcycle_written = true;
      return true;
    case csrno::kMinstret:
      c.minstret = v;
      c.minstret_written = true;
      return true;
    default: return false;  // unimplemented or read-only
  }
}

uint64_t csr_rmw_base(const rv_machine* m, uint16_t csr, uint64_t read_value) {
  if (csr == csrno::kMip || csr == csrno::kSip) {
    // Use the software-writable SEIP bit, not the OR with the external line.
    const uint64_t line = m->irq_lines & bit(kIrqSEI) & ~m->hart.csr.mip;
    return read_value & ~line;
  }
  return read_value;
}

CsrLogTargets csr_log_targets(uint16_t csr) {
  switch (csr) {
    case csrno::kSstatus: return {csrno::kMstatus, 0};
    case csrno::kSie: return {csrno::kSie, csrno::kMie};
    case csrno::kSip: return {csrno::kMip, 0};
    default: return {csr, 0};
  }
}

}  // namespace rv
