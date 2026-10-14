// C extension (Unpriv ch. 16, RV64C). Encodings come from the GNU assembler
// (riscv64-unknown-elf-as -march=rv64imac) or, for branches and jumps,
// were checked by disassembling them with objdump.
#include <cstring>

#include "asm.hpp"
#include "decode/decode.hpp"

namespace {

using namespace asm_;
using rv::decode16;
using rv::Op;

struct ExpandCase {
  const char* name;
  uint16_t raw;
  Op op;
  uint8_t rd, rs1, rs2;
  int64_t imm;
};

class Expand : public ::testing::TestWithParam<ExpandCase> {};

TEST_P(Expand, MatchesEquivalent32BitInstruction) {
  const ExpandCase& c = GetParam();
  const rv::Insn d = decode16(c.raw);
  EXPECT_EQ(d.op, c.op);
  EXPECT_EQ(d.rd, c.rd);
  EXPECT_EQ(d.rs1, c.rs1);
  EXPECT_EQ(d.rs2, c.rs2);
  EXPECT_EQ(d.imm, c.imm);
  EXPECT_EQ(d.len, 2);
  EXPECT_EQ(d.raw, c.raw);
}

// x8=s0 x9=s1 x10=a0 x11=a1 x12=a2 x15=a5 x5=t0 x1=ra x2=sp
// clang-format off
INSTANTIATE_TEST_SUITE_P(RV64C, Expand, ::testing::Values(
  ExpandCase{"addi4spn_max",  0x1fe0, Op::Addi,  8, 2, 0, 1020},   // c.addi4spn s0, sp, 1020
  ExpandCase{"addi4spn_min",  0x005c, Op::Addi, 15, 2, 0, 4},      // c.addi4spn a5, sp, 4
  ExpandCase{"lw",            0x5de8, Op::Lw,   10, 11, 0, 124},   // c.lw a0, 124(a1)
  ExpandCase{"ld",            0x7de8, Op::Ld,   10, 11, 0, 248},   // c.ld a0, 248(a1)
  ExpandCase{"sw",            0xc0b0, Op::Sw,    0, 9, 12, 64},    // c.sw a2, 64(s1)
  ExpandCase{"sd",            0xe0d0, Op::Sd,    0, 9, 12, 128},   // c.sd a2, 128(s1)
  ExpandCase{"nop",           0x0001, Op::Addi,  0, 0, 0, 0},
  ExpandCase{"addi_neg",      0x1501, Op::Addi, 10, 10, 0, -32},
  ExpandCase{"addi_pos",      0x057d, Op::Addi, 10, 10, 0, 31},
  ExpandCase{"addiw",         0x357d, Op::Addiw,10, 10, 0, -1},
  ExpandCase{"li",            0x5281, Op::Addi,  5, 0, 0, -32},
  ExpandCase{"addi16sp_neg",  0x7101, Op::Addi,  2, 2, 0, -512},
  ExpandCase{"addi16sp_pos",  0x617d, Op::Addi,  2, 2, 0, 496},
  ExpandCase{"lui_neg",       0x7501, Op::Lui,  10, 0, 0, -0x20000},   // c.lui a0, 0xfffe0
  ExpandCase{"lui_pos",       0x657d, Op::Lui,  10, 0, 0, 0x1f000},
  ExpandCase{"srli_63",       0x917d, Op::Srli, 10, 10, 0, 63},
  ExpandCase{"srai",          0x8485, Op::Srai,  9, 9, 0, 1},
  ExpandCase{"andi",          0x9bfd, Op::Andi, 15, 15, 0, -1},
  ExpandCase{"sub",           0x8c05, Op::Sub,   8, 8, 9, 0},
  ExpandCase{"xor",           0x8c25, Op::Xor,   8, 8, 9, 0},
  ExpandCase{"or",            0x8c45, Op::Or,    8, 8, 9, 0},
  ExpandCase{"and",           0x8c65, Op::And,   8, 8, 9, 0},
  ExpandCase{"subw",          0x9d0d, Op::Subw, 10, 10, 11, 0},
  ExpandCase{"addw",          0x9d2d, Op::Addw, 10, 10, 11, 0},
  ExpandCase{"j_min",         0xb001, Op::Jal,   0, 0, 0, -2048},
  ExpandCase{"j_max",         0xaffd, Op::Jal,   0, 0, 0, 2046},
  ExpandCase{"beqz_min",      0xd001, Op::Beq,   0, 8, 0, -256},
  ExpandCase{"bnez_max",      0xec7d, Op::Bne,   0, 8, 0, 254},
  ExpandCase{"slli_63",       0x12fe, Op::Slli,  5, 5, 0, 63},
  ExpandCase{"lwsp_max",      0x50fe, Op::Lw,    1, 2, 0, 252},
  ExpandCase{"ldsp_max",      0x70fe, Op::Ld,    1, 2, 0, 504},
  ExpandCase{"jr",            0x8082, Op::Jalr,  0, 1, 0, 0},
  ExpandCase{"mv",            0x852e, Op::Add,  10, 0, 11, 0},
  ExpandCase{"ebreak",        0x9002, Op::Ebreak, 0, 0, 0, 0},
  ExpandCase{"jalr",          0x9282, Op::Jalr,  1, 5, 0, 0},
  ExpandCase{"add",           0x952e, Op::Add,  10, 10, 11, 0},
  ExpandCase{"swsp_max",      0xdf86, Op::Sw,    0, 2, 1, 252},
  ExpandCase{"sdsp_max",      0xff86, Op::Sd,    0, 2, 1, 504}),
  [](const ::testing::TestParamInfo<ExpandCase>& p) { return p.param.name; });
// clang-format on

struct BitCase {
  uint16_t raw;
  Op op;
  uint8_t rd, rs1, rs2;
  int64_t imm;
};

constexpr BitCase kBitCases[] = {
#include "compressed_bits.inc"
};

TEST(Compressed, EveryImmediateBitLandsInPlace) {
  for (const BitCase& c : kBitCases) {
    const rv::Insn d = decode16(c.raw);
    EXPECT_EQ(d.op, c.op) << std::hex << c.raw;
    EXPECT_EQ(d.rd, c.rd) << std::hex << c.raw;
    EXPECT_EQ(d.rs1, c.rs1) << std::hex << c.raw;
    EXPECT_EQ(d.rs2, c.rs2) << std::hex << c.raw;
    EXPECT_EQ(d.imm, c.imm) << std::hex << c.raw;
  }
}

TEST(Compressed, ReservedAndUnimplementedEncodingsAreIllegal) {
  const uint16_t illegal[] = {
      0x0000,  // all zeros: defined illegal
      0x0004,  // c.addi4spn with nzuimm = 0
      0x2000,  // c.fld (no D)
      0x8000,  // quadrant 0 funct3 100: reserved
      0xa000,  // c.fsd
      0x2001,  // c.addiw x0: reserved
      0x6101,  // c.addi16sp with nzimm = 0
      0x6501,  // c.lui a0, 0: reserved
      0x6001,  // c.lui x0, 0: reserved
      0x9c45,  // reserved slot after c.subw/c.addw
      0x9c65,
      0x2002,  // c.fldsp
      0x4002,  // c.lwsp x0: reserved
      0x6002,  // c.ldsp x0: reserved
      0x8002,  // c.jr x0: reserved
      0xa002,  // c.fsdsp
  };
  for (uint16_t raw : illegal) {
    EXPECT_EQ(decode16(raw).op, Op::Illegal) << std::hex << raw;
  }
}

TEST(Compressed, HintsDecodeAsNoEffectInstructions) {
  // Writes to x0 are HINTs; they decode and execute without effect.
  EXPECT_EQ(decode16(0x0005).op, Op::Addi);  // c.addi x0, 1
  EXPECT_EQ(decode16(0x4005).op, Op::Addi);  // c.li x0, 1
  EXPECT_EQ(decode16(0x6005).op, Op::Lui);   // c.lui x0, 1
  EXPECT_EQ(decode16(0x8006).op, Op::Add);   // c.mv x0, ra
  EXPECT_EQ(decode16(0x9006).op, Op::Add);   // c.add x0, ra
  EXPECT_EQ(decode16(0x0006).op, Op::Slli);  // c.slli x0, 1
  EXPECT_EQ(decode16(0x8001).op, Op::Srli);  // c.srli s0, 0 (shamt 0)
}

void put16(rv_machine* m, uint64_t addr, std::initializer_list<uint16_t> parcels) {
  for (uint16_t p : parcels) {
    ASSERT_EQ(rv_write_phys(m, addr, &p, 2), RV_OK);
    addr += 2;
  }
}

TEST_F(Cpu, CompressedAndFullWidthInstructionsInterleave) {
  // c.li a0, -32; addi a0, a0, 1 (at +2, 2-byte aligned); c.addi a0, 31
  put16(m, kRamBase, {0x5501});
  program({addi(10, 10, 1)}, kRamBase + 2);
  put16(m, kRamBase + 6, {0x057d});
  run(3);
  EXPECT_EQ(x(10), 0u);
  EXPECT_EQ(pc(), kRamBase + 8);
}

TEST_F(Cpu, CompressedJalrLinksPcPlus2) {
  put16(m, kRamBase, {0x9282});  // c.jalr t0
  set_x(5, kRamBase + 0x100);
  run(1);
  EXPECT_EQ(pc(), kRamBase + 0x100);
  EXPECT_EQ(x(1), kRamBase + 2);
}

TEST_F(Cpu, JumpToOddHalfwordIsAllowedWithC) {
  program({jalr(0, 1, 0)});
  set_x(1, kRamBase + 0x102);
  run(1);
  EXPECT_EQ(pc(), kRamBase + 0x102);  // IALIGN=16: no misaligned-fetch trap
}

TEST_F(Cpu, IllegalCompressedReportsSixteenBits) {
  put16(m, kRamBase, {0x0000});
  run(1);
  expect_trap(2, 0, kRamBase);
  put16(m, kRamBase + 0x10, {0x8002});  // c.jr x0
  set_csr(0x305, kTrapVec);
  ASSERT_EQ(rv_set_reg(m, RV_REG_PC, kRamBase + 0x10), RV_OK);
  run(1);
  expect_trap(2, 0x8002, kRamBase + 0x10);
}

TEST_F(Cpu, CompressedEbreak) {
  put16(m, kRamBase, {0x0001, 0x9002});
  run(2);
  expect_trap(3, kRamBase + 2, kRamBase + 2);
}

// Priv §3.1.16: for a fetch fault on the second parcel of an instruction,
// mtval holds that parcel's address and mepc the instruction's.
TEST_F(Cpu, FetchStraddlingEndOfRamFaultsOnSecondParcel) {
  const uint64_t last = kRamBase + kRamSize - 2;
  put16(m, last, {0x0013});  // low parcel of a 32-bit addi
  program({jalr(0, 1, 0)});
  set_x(1, last);
  run(2);
  expect_trap(1, last + 2, last);
}

TEST_F(Cpu, CompressedAtEndOfRamExecutes) {
  const uint64_t last = kRamBase + kRamSize - 2;
  put16(m, last, {0x0505});  // c.addi a0, 1
  program({jalr(0, 1, 0)});
  set_x(1, last);
  run(2);
  EXPECT_EQ(x(10), 1u);
}

TEST_F(Cpu, MepcKeepsBit1WithC) {
  set_csr(0x341, kRamBase | 3);
  EXPECT_EQ(csr(0x341), kRamBase | 2);
}

TEST_F(Cpu, CompressedTraceRecordsSixteenBitInsn) {
  ASSERT_EQ(rv_trace_enable(m, 4 * sizeof(rv_trace_record)), RV_OK);
  put16(m, kRamBase, {0x5501});
  run(1);
  const uint8_t* data = nullptr;
  size_t len = 0;
  ASSERT_EQ(rv_trace_peek(m, &data, &len), RV_OK);
  rv_trace_record r;
  std::memcpy(&r, data, sizeof r);
  EXPECT_EQ(r.insn, 0x5501u);
  EXPECT_EQ(r.insn_len, 2);
}

TEST_F(Cpu, MisaReportsImplementedExtensions) {
  const uint64_t ext = csr(0x301) & ((1u << 26) - 1);
  EXPECT_EQ(ext, (1u << 0) | (1u << 2) | (1u << 8) | (1u << 12) | (1u << 18) | (1u << 20));  // A C I M S U
}

}  // namespace
