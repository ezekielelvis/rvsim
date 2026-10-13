// Control transfer: branches, JAL, JALR (Unpriv §2.5).
#include "asm.hpp"

namespace {

using namespace asm_;

struct BranchCase {
  const char* name;
  uint32_t insn;  // compares x1, x2; target +16
  uint64_t a, b;
  bool taken;
};

class Branch : public Cpu, public ::testing::WithParamInterface<BranchCase> {};

TEST_P(Branch, TakenOrNot) {
  const BranchCase& c = GetParam();
  program({c.insn});
  set_x(1, c.a);
  set_x(2, c.b);
  run(1);
  EXPECT_EQ(pc(), kRamBase + (c.taken ? 16 : 4));
}

constexpr uint64_t kNeg1 = ~0ull;

// clang-format off
INSTANTIATE_TEST_SUITE_P(RV64I, Branch, ::testing::Values(
  BranchCase{"beq_taken",   beq(1, 2, 16),  5, 5, true},
  BranchCase{"beq_not",     beq(1, 2, 16),  5, 6, false},
  BranchCase{"bne_taken",   bne(1, 2, 16),  5, 6, true},
  BranchCase{"blt_signed",  blt(1, 2, 16),  kNeg1, 1, true},
  BranchCase{"blt_equal",   blt(1, 2, 16),  3, 3, false},
  BranchCase{"bge_equal",   bge(1, 2, 16),  3, 3, true},
  BranchCase{"bge_signed",  bge(1, 2, 16),  kNeg1, 1, false},
  BranchCase{"bltu",        bltu(1, 2, 16), 1, kNeg1, true},
  BranchCase{"bltu_not",    bltu(1, 2, 16), kNeg1, 1, false},
  BranchCase{"bgeu",        bgeu(1, 2, 16), kNeg1, 1, true}),
  [](const ::testing::TestParamInfo<BranchCase>& p) { return p.param.name; });
// clang-format on

TEST_F(Cpu, BranchBackward) {
  program({kNop, kNop, beq(0, 0, -8)});
  run(3);
  EXPECT_EQ(pc(), kRamBase);
}

TEST_F(Cpu, JalLinksAndJumps) {
  program({jal(1, 0x100)});
  run(1);
  EXPECT_EQ(x(1), kRamBase + 4);
  EXPECT_EQ(pc(), kRamBase + 0x100);
}

TEST_F(Cpu, JalrClearsBitZeroAndLinks) {
  program({jalr(1, 2, 1)});
  set_x(2, kRamBase + 0x200);
  run(1);
  EXPECT_EQ(pc(), kRamBase + 0x200);  // (0x200 + 1) & ~1
  EXPECT_EQ(x(1), kRamBase + 4);
}

TEST_F(Cpu, JalrWithRdEqualRs1UsesOldValue) {
  program({jalr(1, 1, 8)});
  set_x(1, kRamBase + 0x100);
  run(1);
  EXPECT_EQ(pc(), kRamBase + 0x108);
  EXPECT_EQ(x(1), kRamBase + 4);
}

// Unpriv §2.5: a jump or taken branch to a target that is not IALIGN-aligned
// raises instruction-address-misaligned on the jump itself, with tval = target,
// and rd is not written.
TEST_F(Cpu, MisalignedJalTraps) {
  program({jal(1, 6)});
  run(1);
  expect_trap(0, kRamBase + 6, kRamBase);
  EXPECT_EQ(x(1), 0u);
}

TEST_F(Cpu, MisalignedJalrTraps) {
  program({jalr(1, 2, 0)});
  set_x(2, kRamBase + 0x102);
  run(1);
  expect_trap(0, kRamBase + 0x102, kRamBase);
  EXPECT_EQ(x(1), 0u);
}

TEST_F(Cpu, MisalignedTakenBranchTraps) {
  program({beq(0, 0, 6)});
  run(1);
  expect_trap(0, kRamBase + 6, kRamBase);
}

TEST_F(Cpu, MisalignedUntakenBranchDoesNotTrap) {
  program({bne(0, 0, 6)});
  run(1);
  EXPECT_EQ(pc(), kRamBase + 4);
}

TEST_F(Cpu, FetchOutsideRamIsAccessFault) {
  program({jalr(0, 1, 0)});
  set_x(1, 0x1000);
  run(2);
  expect_trap(1, 0x1000, 0x1000);
}

}  // namespace
