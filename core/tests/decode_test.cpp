// Decoder: immediate extraction at the format limits and reserved encodings.
#include <gtest/gtest.h>

#include "asm.hpp"
#include "decode/decode.hpp"

namespace {

using namespace asm_;
using rv::decode32;
using rv::Op;

TEST(Decode, IImmediateLimits) {
  EXPECT_EQ(decode32(addi(1, 2, -2048)).imm, -2048);
  EXPECT_EQ(decode32(addi(1, 2, 2047)).imm, 2047);
  EXPECT_EQ(decode32(addi(1, 2, -1)).imm, -1);
}

TEST(Decode, SImmediateLimits) {
  EXPECT_EQ(decode32(store(3, 1, 2, -2048)).imm, -2048);
  EXPECT_EQ(decode32(store(3, 1, 2, 2047)).imm, 2047);
}

TEST(Decode, BImmediateLimits) {
  EXPECT_EQ(decode32(beq(1, 2, -4096)).imm, -4096);
  EXPECT_EQ(decode32(beq(1, 2, 4094)).imm, 4094);
  EXPECT_EQ(decode32(beq(1, 2, 2048)).imm, 2048);  // imm[11] lives in bit 7
}

TEST(Decode, JImmediateLimits) {
  EXPECT_EQ(decode32(jal(1, -(1 << 20))).imm, -(1 << 20));
  EXPECT_EQ(decode32(jal(1, (1 << 20) - 2)).imm, (1 << 20) - 2);
  EXPECT_EQ(decode32(jal(1, 2048)).imm, 2048);  // imm[11] lives in bit 20
}

TEST(Decode, UImmediateIsSignExtended) {
  EXPECT_EQ(decode32(lui(1, 0x80000)).imm, -0x8000'0000ll);
}

TEST(Decode, Fields) {
  const rv::Insn d = decode32(op(0x20, 0, 31, 30, 29));
  EXPECT_EQ(d.op, Op::Sub);
  EXPECT_EQ(d.rd, 31);
  EXPECT_EQ(d.rs1, 30);
  EXPECT_EQ(d.rs2, 29);
  EXPECT_EQ(d.len, 4);
}

TEST(Decode, ShiftImmediates) {
  EXPECT_EQ(decode32(opimm(1, 1, 2, 63)).op, Op::Slli);
  EXPECT_EQ(decode32(opimm(1, 1, 2, 63)).imm, 63);
  EXPECT_EQ(decode32(opimm(5, 1, 2, 0x400 | 63)).op, Op::Srai);
  EXPECT_EQ(decode32(opimm(5, 1, 2, 0x400 | 63)).imm, 63);
  // RV64: shamt[5] belongs to the shift amount; any other imm[11:6] is reserved.
  EXPECT_EQ(decode32(opimm(1, 1, 2, 0x200)).op, Op::Illegal);
  EXPECT_EQ(decode32(opimm(5, 1, 2, 0x200)).op, Op::Illegal);
  // *IW shifts take a 5-bit shamt; imm[5] set is reserved.
  EXPECT_EQ(decode32(opimm32(1, 1, 2, 32)).op, Op::Illegal);
  EXPECT_EQ(decode32(opimm32(5, 1, 2, 0x400 | 31)).op, Op::Sraiw);
}

TEST(Decode, ReservedEncodingsAreIllegal) {
  const uint32_t illegal[] = {
      0x00000000,          // all zeros (defined illegal, Unpriv §26)
      0xffffffff,          // all ones
      B(0, 1, 2, 2),       // branch funct3 010
      load(7, 1, 2, 0),    // load funct3 111
      store(4, 1, 2, 0),   // store funct3 100
      jalr(1, 2, 0) | (1u << 12),  // jalr funct3 != 0
      op(0x20, 1, 1, 2, 3),        // SLL with funct7 0100000
      op32(0, 2, 1, 2, 3),         // OP-32 funct3 010
      0x0000200f,                  // MISC-MEM funct3 010
      csr(4, 1, 0x300, 2),         // SYSTEM funct3 100
      0x00200073,                  // ECALL-space with imm 2 (URET, removed)
      0x00000173,                  // ECALL with rd != 0
  };
  for (uint32_t insn : illegal) {
    EXPECT_EQ(decode32(insn).op, Op::Illegal) << std::hex << insn;
  }
}

TEST(Decode, SystemInstructions) {
  EXPECT_EQ(decode32(kEcall).op, Op::Ecall);
  EXPECT_EQ(decode32(kEbreak).op, Op::Ebreak);
  EXPECT_EQ(decode32(kMret).op, Op::Mret);
  EXPECT_EQ(decode32(kSret).op, Op::Sret);
  EXPECT_EQ(decode32(kWfi).op, Op::Wfi);
  EXPECT_EQ(decode32(0x12a00073).op, Op::SfenceVma);  // sfence.vma x0, a0
  EXPECT_EQ(decode32(csrrw(1, 0xfff, 2)).imm, 0xfff);  // CSR numbers are unsigned
}

TEST(Decode, FenceVariantsAreFences) {
  EXPECT_EQ(decode32(kFence).op, Op::Fence);
  EXPECT_EQ(decode32(0x8330000f).op, Op::Fence);  // fence.tso
  EXPECT_EQ(decode32(0x0100000f).op, Op::Fence);  // pause
  EXPECT_EQ(decode32(kFenceI).op, Op::FenceI);
}

}  // namespace
