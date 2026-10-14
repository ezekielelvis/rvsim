// Control and status registers (Priv ch. 2-4, Zicsr, Zicntr).
#pragma once

#include <cstdint>

struct rv_machine;

namespace rv {

namespace csrno {
// Unprivileged counters (Zicntr).
constexpr uint16_t kCycle = 0xc00;
constexpr uint16_t kTime = 0xc01;
constexpr uint16_t kInstret = 0xc02;
// Supervisor.
constexpr uint16_t kSstatus = 0x100;
constexpr uint16_t kSie = 0x104;
constexpr uint16_t kStvec = 0x105;
constexpr uint16_t kScounteren = 0x106;
constexpr uint16_t kSenvcfg = 0x10a;
constexpr uint16_t kSscratch = 0x140;
constexpr uint16_t kSepc = 0x141;
constexpr uint16_t kScause = 0x142;
constexpr uint16_t kStval = 0x143;
constexpr uint16_t kSip = 0x144;
constexpr uint16_t kSatp = 0x180;
// Machine.
constexpr uint16_t kMstatus = 0x300;
constexpr uint16_t kMisa = 0x301;
constexpr uint16_t kMedeleg = 0x302;
constexpr uint16_t kMideleg = 0x303;
constexpr uint16_t kMie = 0x304;
constexpr uint16_t kMtvec = 0x305;
constexpr uint16_t kMcounteren = 0x306;
constexpr uint16_t kMenvcfg = 0x30a;
constexpr uint16_t kMcountinhibit = 0x320;
constexpr uint16_t kMscratch = 0x340;
constexpr uint16_t kMepc = 0x341;
constexpr uint16_t kMcause = 0x342;
constexpr uint16_t kMtval = 0x343;
constexpr uint16_t kMip = 0x344;
constexpr uint16_t kMcycle = 0xb00;
constexpr uint16_t kMinstret = 0xb02;
constexpr uint16_t kMvendorid = 0xf11;
constexpr uint16_t kMarchid = 0xf12;
constexpr uint16_t kMimpid = 0xf13;
constexpr uint16_t kMhartid = 0xf14;
constexpr uint16_t kMconfigptr = 0xf15;
}  // namespace csrno

// misa: MXL=64, extensions A, C, I, M, S and U (Priv §3.1.1). Read-only,
// which WARL permits.
constexpr uint64_t kMisa = (2ull << 62) | (1ull << ('A' - 'A')) | (1ull << ('C' - 'A')) |
                           (1ull << ('I' - 'A')) | (1ull << ('M' - 'A')) | (1ull << ('S' - 'A')) |
                           (1ull << ('U' - 'A'));

// Sv39 is implemented (Phase 5), so SUM and menvcfg.FIOM are writable and
// page faults are delegable. ADR-016 described the pre-MMU state.
constexpr bool kHasMmu = true;

// Whether an access from privilege `priv` is permitted, apart from whether
// the CSR exists: the read-only and privilege bits of the address (Priv §2.1),
// counter enables (Priv §3.1.11, §4.1.5), and mstatus.TVM for satp (§3.1.6.5).
bool csr_access_allowed(const rv_machine* m, uint16_t csr, unsigned priv, bool write);

// Raw access without permission checks. read returns false if the CSR is not
// implemented; write returns false if it is not implemented or is read-only.
// write applies WARL legalization and side effects.
bool csr_read(const rv_machine* m, uint16_t csr, uint64_t& out);
bool csr_write(rv_machine* m, uint16_t csr, uint64_t value);

// The value a CSRRS/CSRRC read-modify-write starts from. It differs from
// csr_read only for mip/sip: the external SEIP signal is visible to reads but
// does not take part in the read-modify-write (Priv §3.1.9).
uint64_t csr_rmw_base(const rv_machine* m, uint16_t csr, uint64_t read_value);

// The CSRs a write is logged as in the commit log, matching Spike: sstatus
// and sip writes are logged as mstatus and mip; a sie write is logged as both
// sie and mie. second is 0 when there is only one.
struct CsrLogTargets {
  uint16_t first;
  uint16_t second;
};
CsrLogTargets csr_log_targets(uint16_t csr);

// mip as the interrupt logic sees it: software bits OR external lines.
uint64_t mip_effective(const rv_machine* m);

}  // namespace rv
