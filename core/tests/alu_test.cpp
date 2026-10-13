// RV64I integer computational instructions (Unpriv §2.4, §5.2).
#include "asm.hpp"

namespace {

using namespace asm_;

struct AluCase {
  const char* name;
  uint32_t insn;  // reads x1 (and x2), writes x3
  uint64_t a, b;
  uint64_t want;
};

class Alu : public Cpu, public ::testing::WithParamInterface<AluCase> {};

TEST_P(Alu, Computes) {
  const AluCase& c = GetParam();
  program({c.insn});
  set_x(1, c.a);
  set_x(2, c.b);
  EXPECT_EQ(run(1).reason, RV_EXIT_BUDGET);
  EXPECT_EQ(x(3), c.want);
  EXPECT_EQ(pc(), kRamBase + 4);
}

constexpr uint64_t kMin = 0x8000'0000'0000'0000;
constexpr uint64_t kMax = 0x7fff'ffff'ffff'ffff;
constexpr uint64_t kAll = ~0ull;

// clang-format off
INSTANTIATE_TEST_SUITE_P(RV64I, Alu, ::testing::Values(
  AluCase{"addi_neg",        addi(3, 1, -2048),            5, 0, static_cast<uint64_t>(5 - 2048)},
  AluCase{"addi_max",        addi(3, 1, 2047),             0, 0, 2047},
  AluCase{"addi_wrap",       addi(3, 1, 1),                kMax, 0, kMin},
  AluCase{"slti_true",       opimm(2, 3, 1, -1),           kMin, 0, 1},
  AluCase{"slti_false",      opimm(2, 3, 1, -1),           0, 0, 0},
  AluCase{"sltiu_imm_sext",  opimm(3, 3, 1, -1),           kAll - 1, 0, 1},  // imm is sign-extended, then compared unsigned
  AluCase{"seqz",            opimm(3, 3, 1, 1),            0, 0, 1},
  AluCase{"xori_not",        opimm(4, 3, 1, -1),           0x00ff, 0, ~0x00ffull},
  AluCase{"ori",             opimm(6, 3, 1, 0x0f0),        0x00f, 0, 0x0ff},
  AluCase{"andi_sext",       opimm(7, 3, 1, -16),          0x1234, 0, 0x1230},
  AluCase{"slli_63",         opimm(1, 3, 1, 63),           1, 0, kMin},
  AluCase{"srli_63",         opimm(5, 3, 1, 63),           kMin, 0, 1},
  AluCase{"srai_63",         opimm(5, 3, 1, 0x400 | 63),   kMin, 0, kAll},
  AluCase{"add",             op(0, 0, 3, 1, 2),            kMax, 1, kMin},
  AluCase{"sub_wrap",        op(0x20, 0, 3, 1, 2),         0, 1, kAll},
  AluCase{"sll_uses_6_bits", op(0, 1, 3, 1, 2),            1, 65, 2},
  AluCase{"slt",             op(0, 2, 3, 1, 2),            kAll, 1, 1},
  AluCase{"sltu",            op(0, 3, 3, 1, 2),            kAll, 1, 0},
  AluCase{"xor",             op(0, 4, 3, 1, 2),            0xff00, 0x0ff0, 0xf0f0},
  AluCase{"srl",             op(0, 5, 3, 1, 2),            kMin, 63, 1},
  AluCase{"sra",             op(0x20, 5, 3, 1, 2),         kMin, 64 + 4, 0xf800'0000'0000'0000},
  AluCase{"or",              op(0, 6, 3, 1, 2),            0xf0, 0x0f, 0xff},
  AluCase{"and",             op(0, 7, 3, 1, 2),            0xf0, 0x3c, 0x30},
  AluCase{"lui_neg",         lui(3, 0x80000),              0, 0, 0xffff'ffff'8000'0000},
  AluCase{"lui_pos",         lui(3, 0x7ffff),              0, 0, 0x7fff'f000},
  AluCase{"auipc",           auipc(3, 0xfffff),            0, 0, kRamBase - 0x1000},
  // *W: operate on the low 32 bits, sign-extend the result (Unpriv §5.2).
  AluCase{"addiw_wrap",      opimm32(0, 3, 1, 1),          0x7fff'ffff, 0, 0xffff'ffff'8000'0000},
  AluCase{"addiw_ignores_hi",opimm32(0, 3, 1, 0),          0x1234'5678'0000'0001, 0, 1},
  AluCase{"slliw",           opimm32(1, 3, 1, 31),         1, 0, 0xffff'ffff'8000'0000},
  AluCase{"srliw_0_sext",    opimm32(5, 3, 1, 0),          0xffff'ffff'8000'0000, 0, 0xffff'ffff'8000'0000},
  AluCase{"srliw_1",         opimm32(5, 3, 1, 1),          0xffff'ffff'8000'0000, 0, 0x4000'0000},
  AluCase{"sraiw",           opimm32(5, 3, 1, 0x400 | 31), 0x8000'0000, 0, kAll},
  AluCase{"addw",            op32(0, 0, 3, 1, 2),          0xffff'ffff, 1, 0},
  AluCase{"subw",            op32(0x20, 0, 3, 1, 2),       0, 1, kAll},
  AluCase{"sllw_uses_5_bits",op32(0, 1, 3, 1, 2),          1, 33, 2},
  AluCase{"srlw",            op32(0, 5, 3, 1, 2),          0xffff'ffff'0000'0000 | 0x8000'0000, 31, 1},
  AluCase{"sraw",            op32(0x20, 5, 3, 1, 2),       0x8000'0000, 31, kAll}),
  [](const ::testing::TestParamInfo<AluCase>& p) { return p.param.name; });
// clang-format on

TEST_F(Cpu, WritesToX0AreDiscarded) {
  program({addi(0, 0, 5), lui(0, 1), op(0, 0, 0, 1, 1)});
  set_x(1, 7);
  run(3);
  EXPECT_EQ(x(0), 0u);
}

TEST_F(Cpu, SourceEqualsDestination) {
  program({addi(1, 1, 1), op(0, 0, 1, 1, 1)});
  set_x(1, 20);
  run(2);
  EXPECT_EQ(x(1), 42u);
}

}  // namespace
