// M extension: multiply and divide, including every edge case in Unpriv §13.2
// Table 13.1 (division by zero and signed overflow).
#include "asm.hpp"

namespace {

using namespace asm_;

constexpr uint32_t mext(uint32_t f3) { return op(1, f3, 3, 1, 2); }
constexpr uint32_t mext32(uint32_t f3) { return op32(1, f3, 3, 1, 2); }

struct MulDivCase {
  const char* name;
  uint32_t insn;  // x3 = x1 op x2
  uint64_t a, b;
  uint64_t want;
};

class MulDiv : public Cpu, public ::testing::WithParamInterface<MulDivCase> {};

TEST_P(MulDiv, Computes) {
  const MulDivCase& c = GetParam();
  program({c.insn});
  set_x(1, c.a);
  set_x(2, c.b);
  run(1);
  EXPECT_EQ(x(3), c.want);
}

constexpr uint64_t kMin = 0x8000'0000'0000'0000;
constexpr uint64_t kAll = ~0ull;
constexpr uint64_t kMin32 = 0xffff'ffff'8000'0000;  // INT32_MIN sign-extended

// clang-format off
INSTANTIATE_TEST_SUITE_P(RV64M, MulDiv, ::testing::Values(
  MulDivCase{"mul",               mext(0), 0x1'0000'0001, 0x1'0000'0001, 0x2'0000'0001},
  MulDivCase{"mul_wraps",         mext(0), kMin, 2, 0},
  MulDivCase{"mulh_neg_neg",      mext(1), kAll, kAll, 0},                 // (-1)*(-1) = 1
  MulDivCase{"mulh_min_min",      mext(1), kMin, kMin, 0x4000'0000'0000'0000},
  MulDivCase{"mulh_neg_pos",      mext(1), kAll, 1, kAll},                 // -1 high half
  MulDivCase{"mulhsu_neg_max",    mext(2), kAll, kAll, kAll},              // -1 * (2^64-1)
  MulDivCase{"mulhsu_min_max",    mext(2), kMin, kAll, kMin},              // (-2^127 + 2^63) >> 64
  MulDivCase{"mulhu_max_max",     mext(3), kAll, kAll, kAll - 1},
  MulDivCase{"div",               mext(4), static_cast<uint64_t>(-7), 2, static_cast<uint64_t>(-3)},  // truncates toward zero
  MulDivCase{"div_by_zero",       mext(4), 42, 0, kAll},
  MulDivCase{"div_overflow",      mext(4), kMin, kAll, kMin},
  MulDivCase{"divu_by_zero",      mext(5), 42, 0, kAll},
  MulDivCase{"divu",              mext(5), kAll, 2, kAll >> 1},
  MulDivCase{"rem_sign_of_dividend", mext(6), static_cast<uint64_t>(-7), 2, static_cast<uint64_t>(-1)},
  MulDivCase{"rem_by_zero",       mext(6), 42, 0, 42},
  MulDivCase{"rem_overflow",      mext(6), kMin, kAll, 0},
  MulDivCase{"remu_by_zero",      mext(7), kAll, 0, kAll},
  MulDivCase{"remu",              mext(7), 7, 4, 3},
  MulDivCase{"mulw_sext",         mext32(0), 0x1'0000'ffff, 0x10000, 0xffff'ffff'ffff'0000},
  MulDivCase{"divw_ignores_upper",mext32(4), 0xdead'0000'0000'0006, 0xbeef'0000'0000'0003, 2},
  MulDivCase{"divw_by_zero",      mext32(4), 1, 0, kAll},
  MulDivCase{"divw_overflow",     mext32(4), 0x8000'0000, 0xffff'ffff, kMin32},
  MulDivCase{"divuw_by_zero",     mext32(5), 1, 0, kAll},                   // 0xffffffff sign-extended
  MulDivCase{"divuw",             mext32(5), 0xffff'fffe, 1, 0xffff'ffff'ffff'fffe},
  MulDivCase{"remw_by_zero",      mext32(6), 0x8000'0001, 0, 0xffff'ffff'8000'0001},
  MulDivCase{"remw_overflow",     mext32(6), 0x8000'0000, 0xffff'ffff, 0},
  MulDivCase{"remuw_by_zero",     mext32(7), 0x1'8000'0000, 0, kMin32},
  MulDivCase{"remuw",             mext32(7), 0xffff'ffff, 0x10, 0xf}),
  [](const ::testing::TestParamInfo<MulDivCase>& p) { return p.param.name; });
// clang-format on

TEST_F(Cpu, MulDivWithRdEqualSources) {
  program({op(1, 0, 1, 1, 1), op(1, 4, 1, 1, 1)});  // mul x1,x1,x1; div x1,x1,x1
  set_x(1, 7);
  run(2);
  EXPECT_EQ(x(1), 1u);
}

}  // namespace
