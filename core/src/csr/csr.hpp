// Control and status registers (Priv ch. 2-3, Zicsr).
#pragma once

#include <cstdint>

struct rv_machine;

namespace rv {

namespace csrno {
constexpr uint16_t kMstatus = 0x300;
constexpr uint16_t kMisa = 0x301;
constexpr uint16_t kMie = 0x304;
constexpr uint16_t kMtvec = 0x305;
constexpr uint16_t kMscratch = 0x340;
constexpr uint16_t kMepc = 0x341;
constexpr uint16_t kMcause = 0x342;
constexpr uint16_t kMtval = 0x343;
constexpr uint16_t kMip = 0x344;
constexpr uint16_t kMvendorid = 0xf11;
constexpr uint16_t kMarchid = 0xf12;
constexpr uint16_t kMimpid = 0xf13;
constexpr uint16_t kMhartid = 0xf14;
constexpr uint16_t kMconfigptr = 0xf15;
}  // namespace csrno

// misa: MXL=64, extensions A, C, I, M and U (Priv §3.1.1). Read-only, which
// WARL permits.
constexpr uint64_t kMisa = (2ull << 62) | (1ull << ('A' - 'A')) | (1ull << ('C' - 'A')) |
                           (1ull << ('I' - 'A')) | (1ull << ('M' - 'A')) | (1ull << ('U' - 'A'));

// Privilege and read-only checks from the CSR address itself (Priv §2.1):
// csr[11:10] == 0b11 marks read-only, csr[9:8] is the lowest privilege allowed.
constexpr bool csr_permitted(uint16_t csr, unsigned priv, bool write) {
  if (write && ((csr >> 10) & 3) == 3) return false;
  return priv >= ((csr >> 8) & 3u);
}

// Raw access without privilege checks. read returns false if the CSR is not
// implemented; write returns false if it is not implemented or is read-only.
// write applies WARL legalization.
bool csr_read(const rv_machine* m, uint16_t csr, uint64_t& out);
bool csr_write(rv_machine* m, uint16_t csr, uint64_t value);

}  // namespace rv
