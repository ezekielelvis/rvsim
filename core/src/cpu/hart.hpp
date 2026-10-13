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

// mstatus fields, Priv §3.1.6 Figure 3.7.
namespace mstatus {
constexpr uint64_t kMIE = 1ull << 3;
constexpr uint64_t kMPIE = 1ull << 7;
constexpr unsigned kMPPShift = 11;
constexpr uint64_t kMPP = 3ull << kMPPShift;
constexpr uint64_t kMPRV = 1ull << 17;
constexpr uint64_t kTW = 1ull << 21;
constexpr unsigned kUXLShift = 32;
}  // namespace mstatus

struct Csrs {
  uint64_t mstatus = 0;  // writable fields only; UXL is merged on read
  uint64_t mtvec = 0;
  uint64_t mscratch = 0;
  uint64_t mepc = 0;
  uint64_t mcause = 0;
  uint64_t mtval = 0;
  uint64_t mie = 0;
  uint64_t mip = 0;
};

struct Hart {
  uint64_t x[32]{};
  uint64_t pc = 0;
  Priv priv = kPrivM;
  Csrs csr;
};

}  // namespace rv
