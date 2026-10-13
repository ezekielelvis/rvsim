// Tiny RV64 instruction encoder and a machine fixture for unit tests.
#pragma once

#include <gtest/gtest.h>

#include <cstdint>
#include <initializer_list>
#include <vector>

#include "rv.h"
#include "rv_trace.h"

namespace asm_ {

constexpr uint32_t u(int64_t v) { return static_cast<uint32_t>(v); }

// Formats, Unpriv §2.2-2.3.
constexpr uint32_t R(uint32_t f7, uint32_t rs2, uint32_t rs1, uint32_t f3, uint32_t rd, uint32_t op) {
  return (f7 << 25) | (rs2 << 20) | (rs1 << 15) | (f3 << 12) | (rd << 7) | op;
}
constexpr uint32_t I(int32_t imm, uint32_t rs1, uint32_t f3, uint32_t rd, uint32_t op) {
  return (u(imm & 0xfff) << 20) | (rs1 << 15) | (f3 << 12) | (rd << 7) | op;
}
constexpr uint32_t S(int32_t imm, uint32_t rs2, uint32_t rs1, uint32_t f3, uint32_t op) {
  return (u((imm >> 5) & 0x7f) << 25) | (rs2 << 20) | (rs1 << 15) | (f3 << 12) | (u(imm & 0x1f) << 7) | op;
}
constexpr uint32_t B(int32_t imm, uint32_t rs2, uint32_t rs1, uint32_t f3) {
  return (u((imm >> 12) & 1) << 31) | (u((imm >> 5) & 0x3f) << 25) | (rs2 << 20) | (rs1 << 15) |
         (f3 << 12) | (u((imm >> 1) & 0xf) << 8) | (u((imm >> 11) & 1) << 7) | 0b1100011;
}
constexpr uint32_t U(int32_t imm20, uint32_t rd, uint32_t op) { return (u(imm20 & 0xfffff) << 12) | (rd << 7) | op; }
constexpr uint32_t J(int32_t imm, uint32_t rd) {
  return (u((imm >> 20) & 1) << 31) | (u((imm >> 1) & 0x3ff) << 21) | (u((imm >> 11) & 1) << 20) |
         (u((imm >> 12) & 0xff) << 12) | (rd << 7) | 0b1101111;
}

constexpr uint32_t lui(uint32_t rd, int32_t imm20) { return U(imm20, rd, 0b0110111); }
constexpr uint32_t auipc(uint32_t rd, int32_t imm20) { return U(imm20, rd, 0b0010111); }
constexpr uint32_t jal(uint32_t rd, int32_t off) { return J(off, rd); }
constexpr uint32_t jalr(uint32_t rd, uint32_t rs1, int32_t off) { return I(off, rs1, 0, rd, 0b1100111); }
constexpr uint32_t beq(uint32_t a, uint32_t b, int32_t off) { return B(off, b, a, 0); }
constexpr uint32_t bne(uint32_t a, uint32_t b, int32_t off) { return B(off, b, a, 1); }
constexpr uint32_t blt(uint32_t a, uint32_t b, int32_t off) { return B(off, b, a, 4); }
constexpr uint32_t bge(uint32_t a, uint32_t b, int32_t off) { return B(off, b, a, 5); }
constexpr uint32_t bltu(uint32_t a, uint32_t b, int32_t off) { return B(off, b, a, 6); }
constexpr uint32_t bgeu(uint32_t a, uint32_t b, int32_t off) { return B(off, b, a, 7); }
constexpr uint32_t load(uint32_t f3, uint32_t rd, uint32_t rs1, int32_t off) { return I(off, rs1, f3, rd, 0b0000011); }
constexpr uint32_t store(uint32_t f3, uint32_t rs2, uint32_t rs1, int32_t off) { return S(off, rs2, rs1, f3, 0b0100011); }
constexpr uint32_t opimm(uint32_t f3, uint32_t rd, uint32_t rs1, int32_t imm) { return I(imm, rs1, f3, rd, 0b0010011); }
constexpr uint32_t addi(uint32_t rd, uint32_t rs1, int32_t imm) { return opimm(0, rd, rs1, imm); }
constexpr uint32_t op(uint32_t f7, uint32_t f3, uint32_t rd, uint32_t rs1, uint32_t rs2) { return R(f7, rs2, rs1, f3, rd, 0b0110011); }
constexpr uint32_t opimm32(uint32_t f3, uint32_t rd, uint32_t rs1, int32_t imm) { return I(imm, rs1, f3, rd, 0b0011011); }
constexpr uint32_t op32(uint32_t f7, uint32_t f3, uint32_t rd, uint32_t rs1, uint32_t rs2) { return R(f7, rs2, rs1, f3, rd, 0b0111011); }
constexpr uint32_t csr(uint32_t f3, uint32_t rd, uint32_t csrno, uint32_t rs1) { return (csrno << 20) | (rs1 << 15) | (f3 << 12) | (rd << 7) | 0b1110011; }
constexpr uint32_t csrrw(uint32_t rd, uint32_t c, uint32_t rs1) { return csr(1, rd, c, rs1); }
constexpr uint32_t csrrs(uint32_t rd, uint32_t c, uint32_t rs1) { return csr(2, rd, c, rs1); }
constexpr uint32_t csrrc(uint32_t rd, uint32_t c, uint32_t rs1) { return csr(3, rd, c, rs1); }
constexpr uint32_t csrrwi(uint32_t rd, uint32_t c, uint32_t uimm) { return csr(5, rd, c, uimm); }
constexpr uint32_t csrrsi(uint32_t rd, uint32_t c, uint32_t uimm) { return csr(6, rd, c, uimm); }
constexpr uint32_t csrrci(uint32_t rd, uint32_t c, uint32_t uimm) { return csr(7, rd, c, uimm); }

constexpr uint32_t kNop = 0x00000013;
constexpr uint32_t kEcall = 0x00000073;
constexpr uint32_t kEbreak = 0x00100073;
constexpr uint32_t kMret = 0x30200073;
constexpr uint32_t kSret = 0x10200073;
constexpr uint32_t kWfi = 0x10500073;
constexpr uint32_t kFence = 0x0ff0000f;
constexpr uint32_t kFenceI = 0x0000100f;

}  // namespace asm_

constexpr uint64_t kRamBase = 0x8000'0000;
constexpr uint64_t kRamSize = 1ull << 20;
constexpr uint64_t kTrapVec = kRamBase + 0x8000;  // mtvec in tests
constexpr uint64_t kToHost = kRamBase + 0x10000;
constexpr uint64_t kData = kRamBase + 0x20000;  // scratch data area

// Machine with 1 MiB of RAM at 0x8000'0000, mtvec = kTrapVec, HTIF at kToHost.
class Cpu : public ::testing::Test {
 protected:
  void SetUp() override {
    rv_config c{};
    c.abi_version = RV_ABI_VERSION;
    c.flags = flags_;
    c.ram_base = kRamBase;
    c.ram_size = kRamSize;
    c.clint_base = 0x0200'0000;
    c.reset_pc = kRamBase;
    c.steps_per_tick = 1;
    c.htif_tohost = kToHost;
    ASSERT_EQ(rv_create(&c, &m), RV_OK);
    set_csr(0x305, kTrapVec);
  }
  void TearDown() override { rv_destroy(m); }

  void program(std::initializer_list<uint32_t> insns, uint64_t at = kRamBase) {
    for (uint32_t w : insns) {
      ASSERT_EQ(rv_write_phys(m, at, &w, 4), RV_OK);
      at += 4;
    }
  }
  const rv_exit& run(uint64_t steps) {
    EXPECT_EQ(rv_run(m, steps), RV_OK);
    return *rv_exit_info(m);
  }
  uint64_t x(uint32_t r) const {
    uint64_t v = 0;
    EXPECT_EQ(rv_get_reg(m, r, &v), RV_OK);
    return v;
  }
  void set_x(uint32_t r, uint64_t v) { ASSERT_EQ(rv_set_reg(m, r, v), RV_OK); }
  uint64_t pc() const { return x(RV_REG_PC); }
  uint64_t priv() const { return x(RV_REG_PRIV); }
  uint64_t csr(uint32_t n) const { return x(RV_REG_CSR + n); }
  void set_csr(uint32_t n, uint64_t v) { ASSERT_EQ(rv_set_reg(m, RV_REG_CSR + n, v), RV_OK); }
  template <typename T>
  T mem(uint64_t addr) const {
    T v{};
    EXPECT_EQ(rv_read_phys(m, addr, &v, sizeof v), RV_OK);
    return v;
  }
  template <typename T>
  void set_mem(uint64_t addr, T v) {
    ASSERT_EQ(rv_write_phys(m, addr, &v, sizeof v), RV_OK);
  }
  // Expect that the last step trapped into M-mode with this cause and tval,
  // from an instruction at epc.
  void expect_trap(uint64_t cause, uint64_t tval, uint64_t epc) {
    EXPECT_EQ(pc(), kTrapVec);
    EXPECT_EQ(priv(), 3u);
    EXPECT_EQ(csr(0x342), cause) << "mcause";
    EXPECT_EQ(csr(0x343), tval) << "mtval";
    EXPECT_EQ(csr(0x341), epc) << "mepc";
  }

  uint32_t flags_ = 0;
  rv_machine* m = nullptr;
};

// Same machine with misaligned loads/stores performed instead of trapping.
class CpuMisalignedOK : public Cpu {
 protected:
  void SetUp() override {
    flags_ = RV_CFG_MISALIGNED_OK;
    Cpu::SetUp();
  }
};
