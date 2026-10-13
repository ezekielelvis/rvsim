// Loads, stores, access faults, misalignment, and HTIF (Unpriv §2.6).
#include "asm.hpp"

namespace {

using namespace asm_;

struct LoadCase {
  const char* name;
  uint32_t f3;
  uint64_t want;  // loading from kData, which holds 0x8899aabbccddeeff
};

class Load : public Cpu, public ::testing::WithParamInterface<LoadCase> {};

TEST_P(Load, ExtendsCorrectly) {
  program({load(GetParam().f3, 3, 1, 0)});
  set_mem<uint64_t>(kData, 0x8899aabb'ccddeeff);
  set_x(1, kData);
  run(1);
  EXPECT_EQ(x(3), GetParam().want);
}

// clang-format off
INSTANTIATE_TEST_SUITE_P(RV64I, Load, ::testing::Values(
  LoadCase{"lb",  0, 0xffffffff'ffffffff},
  LoadCase{"lh",  1, 0xffffffff'ffffeeff},
  LoadCase{"lw",  2, 0xffffffff'ccddeeff},
  LoadCase{"ld",  3, 0x8899aabb'ccddeeff},
  LoadCase{"lbu", 4, 0xff},
  LoadCase{"lhu", 5, 0xeeff},
  LoadCase{"lwu", 6, 0xccddeeff}),
  [](const ::testing::TestParamInfo<LoadCase>& p) { return p.param.name; });
// clang-format on

TEST_F(Cpu, LoadNegativeOffset) {
  program({load(3, 3, 1, -8)});
  set_mem<uint64_t>(kData, 42);
  set_x(1, kData + 8);
  run(1);
  EXPECT_EQ(x(3), 42u);
}

TEST_F(Cpu, StoresWriteOnlyTheirWidth) {
  program({store(0, 2, 1, 0), store(1, 2, 1, 2), store(2, 2, 1, 4)});
  set_x(1, kData);
  set_x(2, 0x11223344'55667788);
  run(3);
  EXPECT_EQ(mem<uint64_t>(kData), 0x55667788'77880088u);
  set_mem<uint64_t>(kData, 0);
  program({store(3, 2, 1, 0)}, kRamBase + 12);
  run(1);
  EXPECT_EQ(mem<uint64_t>(kData), 0x11223344'55667788u);
}

TEST_F(Cpu, LoadOutsideRamIsAccessFault) {
  program({load(3, 3, 1, 0)});
  set_x(1, 0x1000'0000);
  set_x(3, 7);
  run(1);
  expect_trap(5, 0x1000'0000, kRamBase);
  EXPECT_EQ(x(3), 7u);  // rd unchanged
}

TEST_F(Cpu, LastWordOfRamIsAccessible) {
  program({load(2, 3, 1, 0)});
  set_x(1, kRamBase + kRamSize - 4);
  set_mem<uint32_t>(kRamBase + kRamSize - 4, 0x1234);
  run(1);
  EXPECT_EQ(x(3), 0x1234u);
}

// Misalignment is checked before access faults; Priv §3.1.15 (Table 3.7) allows either order.
TEST_F(Cpu, MisalignedCheckPrecedesAccessFault) {
  program({load(3, 3, 1, 0)});
  set_x(1, kRamBase + kRamSize - 4);
  run(1);
  expect_trap(4, kRamBase + kRamSize - 4, kRamBase);
}

TEST_F(CpuMisalignedOK, AccessStraddlingEndOfRamIsAccessFault) {
  program({load(3, 3, 1, 0)});
  set_x(1, kRamBase + kRamSize - 4);
  run(1);
  expect_trap(5, kRamBase + kRamSize - 4, kRamBase);
}

TEST_F(Cpu, StoreOutsideRamIsAccessFault) {
  program({store(3, 2, 1, 0)});
  set_x(1, 0x1000);
  run(1);
  expect_trap(7, 0x1000, kRamBase);
}

// ADR-004: misaligned accesses trap by default.
TEST_F(Cpu, MisalignedLoadTraps) {
  program({load(2, 3, 1, 1)});
  set_x(1, kData);
  run(1);
  expect_trap(4, kData + 1, kRamBase);
}

TEST_F(Cpu, MisalignedStoreTraps) {
  program({store(1, 2, 1, 1)});
  set_x(1, kData);
  run(1);
  expect_trap(6, kData + 1, kRamBase);
  EXPECT_EQ(mem<uint64_t>(kData), 0u);
}

TEST_F(CpuMisalignedOK, MisalignedAccessesComplete) {
  program({store(3, 2, 1, 3), load(2, 3, 1, 5)});
  set_x(1, kData);
  set_x(2, 0x11223344'55667788);
  run(2);
  EXPECT_EQ(mem<uint64_t>(kData + 3), 0x11223344'55667788u);
  EXPECT_EQ(x(3), 0x33445566u);
}

TEST_F(Cpu, StoreToToHostHalts) {
  program({store(2, 2, 1, 0), kNop});
  set_x(1, kToHost);
  set_x(2, 1);
  const rv_exit& e = run(100);
  EXPECT_EQ(e.reason, RV_EXIT_HALT);
  EXPECT_EQ(e.addr, kToHost);
  EXPECT_EQ(e.value, 1u);
  EXPECT_EQ(e.pc, kRamBase + 4);  // the store retired
  EXPECT_EQ(e.instret, 1u);
}

TEST_F(Cpu, StoreToUpperHalfOfToHostHalts) {
  program({store(2, 2, 1, 4)});  // sw to tohost+4
  set_x(1, kToHost);
  set_x(2, 3);
  EXPECT_EQ(run(10).reason, RV_EXIT_HALT);
  EXPECT_EQ(rv_exit_info(m)->value, 3ull << 32);
}

TEST_F(Cpu, StoreNextToToHostDoesNotHalt) {
  set_mem<uint64_t>(kToHost, 1);  // tohost already non-zero, but this store misses it
  program({store(3, 2, 1, -8), store(3, 2, 1, 8)});
  set_x(1, kToHost);
  set_x(2, 1);
  EXPECT_EQ(run(2).reason, RV_EXIT_BUDGET);
}

TEST_F(Cpu, StoringZeroToToHostDoesNotHalt) {
  program({store(3, 0, 1, 0), kNop});
  set_x(1, kToHost);
  EXPECT_EQ(run(2).reason, RV_EXIT_BUDGET);
}

TEST_F(Cpu, FenceAndFenceIAreNops) {
  program({kFence, kFenceI});
  run(2);
  EXPECT_EQ(pc(), kRamBase + 8);
}

TEST_F(Cpu, SelfModifyingCodeIsSeen) {
  // Overwrite the instruction at +12 with addi x5, x0, 9, then fence.i and run it.
  program({store(2, 2, 1, 0), kFenceI, kNop, addi(5, 0, 1)});
  set_x(1, kRamBase + 12);
  set_x(2, addi(5, 0, 9));
  run(4);
  EXPECT_EQ(x(5), 9u);
}

}  // namespace
