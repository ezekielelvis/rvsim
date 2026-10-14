// Architectural state of one hart.
#pragma once

#include <cstdint>

namespace rv {

enum Priv : uint8_t { kPrivU = 0, kPrivS = 1, kPrivM = 3 };

// Exception causes, Priv §3.1.15 Table 3.6.
enum Cause : uint64_t {
  kCauseMisalignedFetch = 0,
  kCauseFetchAccess = 1,
  kCauseIllegalInsn = 2,
  kCauseBreakpoint = 3,
  kCauseMisalignedLoad = 4,
  kCauseLoadAccess = 5,
  kCauseMisalignedStore = 6,
  kCauseStoreAccess = 7,
  kCauseEcallU = 8,
  kCauseEcallS = 9,
  kCauseEcallM = 11,
};

constexpr uint64_t kInterruptBit = 1ull << 63;

// Interrupt numbers / mip and mie bit positions (Priv §3.1.9).
enum Irq : unsigned {
  kIrqSSI = 1,
  kIrqMSI = 3,
  kIrqSTI = 5,
  kIrqMTI = 7,
  kIrqSEI = 9,
  kIrqMEI = 11,
};

// mstatus fields, Priv §3.1.6 Figure 3.7.
namespace mstatus {
constexpr uint64_t kSIE = 1ull << 1;
constexpr uint64_t kMIE = 1ull << 3;
constexpr uint64_t kSPIE = 1ull << 5;
constexpr uint64_t kMPIE = 1ull << 7;
constexpr uint64_t kSPP = 1ull << 8;
constexpr unsigned kMPPShift = 11;
constexpr uint64_t kMPP = 3ull << kMPPShift;
constexpr uint64_t kFS = 3ull << 13;
constexpr uint64_t kMPRV = 1ull << 17;
constexpr uint64_t kSUM = 1ull << 18;
constexpr uint64_t kMXR = 1ull << 19;
constexpr uint64_t kTVM = 1ull << 20;
constexpr uint64_t kTW = 1ull << 21;
constexpr uint64_t kTSR = 1ull << 22;
constexpr unsigned kUXLShift = 32;
constexpr unsigned kSXLShift = 34;
constexpr uint64_t kSD = 1ull << 63;
}  // namespace mstatus

struct Csrs {
  uint64_t mstatus = 0;  // writable fields only; UXL/SXL/SD are merged on read
  uint64_t medeleg = 0;
  uint64_t mideleg = 0;
  uint64_t mie = 0;
  uint64_t mip = 0;  // software-writable bits (SSIP, STIP, SEIP) and CLINT bits (MSIP, MTIP)
  uint64_t mtvec = 0;
  uint64_t stvec = 0;
  uint64_t mscratch = 0;
  uint64_t sscratch = 0;
  uint64_t mepc = 0;
  uint64_t sepc = 0;
  uint64_t mcause = 0;
  uint64_t scause = 0;
  uint64_t mtval = 0;
  uint64_t stval = 0;
  uint64_t satp = 0;
  uint64_t mcounteren = 0;
  uint64_t scounteren = 0;
  uint64_t mcountinhibit = 0;
  uint64_t mcycle = 0;
  uint64_t minstret = 0;
  // An explicit write to mcycle/minstret replaces that instruction's own
  // increment (Spike's model; the write is what software observes).
  bool mcycle_written = false;
  bool minstret_written = false;
};

struct Hart {
  uint64_t x[32]{};
  uint64_t pc = 0;
  Priv priv = kPrivM;
  Csrs csr;
};

}  // namespace rv
