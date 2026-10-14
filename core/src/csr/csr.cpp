#include "csr/csr.hpp"

#include "machine.hpp"

namespace rv {
namespace {

// Writable mstatus fields with M and U modes implemented (Priv §3.1.6).
constexpr uint64_t kMstatusWritable =
    mstatus::kMIE | mstatus::kMPIE | mstatus::kMPP | mstatus::kMPRV | mstatus::kTW;
// UXL is read-only 2 (XLEN=64) when U-mode is implemented (Priv §3.1.6.3).
// SXL is read-only zero because S-mode is not implemented yet.
constexpr uint64_t kMstatusFixed = 2ull << mstatus::kUXLShift;

// mie: only the M-level interrupts exist without S-mode (Priv §3.1.9).
constexpr uint64_t kMieWritable = (1ull << 3) | (1ull << 7) | (1ull << 11);

// MPP is WARL over the implemented modes (Priv §3.1.6.1). Unsupported S and
// reserved H encodings legalize to U, matching Spike.
uint64_t legalize_mpp(uint64_t v) {
  const uint64_t mpp = (v & mstatus::kMPP) >> mstatus::kMPPShift;
  if (mpp == kPrivM || mpp == kPrivU) return v;
  return v & ~mstatus::kMPP;
}

}  // namespace

bool csr_read(const rv_machine* m, uint16_t csr, uint64_t& out) {
  const Csrs& c = m->hart.csr;
  switch (csr) {
    case csrno::kMstatus: out = c.mstatus | kMstatusFixed; return true;
    case csrno::kMisa: out = kMisa; return true;
    case csrno::kMie: out = c.mie; return true;
    case csrno::kMtvec: out = c.mtvec; return true;
    case csrno::kMscratch: out = c.mscratch; return true;
    // IALIGN=16 with C: only mepc[0] is zero (Priv §3.1.14).
    case csrno::kMepc: out = c.mepc; return true;
    case csrno::kMcause: out = c.mcause; return true;
    case csrno::kMtval: out = c.mtval; return true;
    case csrno::kMip: out = c.mip; return true;
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
    case csrno::kMstatus: c.mstatus = legalize_mpp(v & kMstatusWritable); return true;
    case csrno::kMisa: return true;  // read-only WARL: writes are ignored
    case csrno::kMie: c.mie = v & kMieWritable; return true;
    // MODE values >= 2 are reserved; clearing bit 1 keeps MODE in {0,1} (Priv §3.1.7).
    case csrno::kMtvec: c.mtvec = v & ~2ull; return true;
    case csrno::kMscratch: c.mscratch = v; return true;
    case csrno::kMepc: c.mepc = v & ~1ull; return true;  // mepc[0] is always zero
    case csrno::kMcause: c.mcause = v; return true;
    case csrno::kMtval: c.mtval = v; return true;
    case csrno::kMip: return true;  // no software-writable bits without S-mode
    default: return false;          // unimplemented or read-only
  }
}

}  // namespace rv
