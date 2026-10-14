// CLINT: msip, mtimecmp, mtime and MTIP (SiFive layout at cfg.clint_base).
#include "asm.hpp"

namespace {

using namespace asm_;

constexpr uint64_t kClint = 0x0200'0000;
constexpr uint64_t kMsip = kClint;
constexpr uint64_t kMtimecmp = kClint + 0x4000;
constexpr uint64_t kMtime = kClint + 0xbff8;
constexpr uint32_t kMip = 0x344, kTime = 0xc01;
constexpr uint64_t kMSI = 1u << 3, kMTI = 1u << 7;

uint64_t phys64(rv_machine* m, uint64_t addr) {
  uint64_t v = 0;
  EXPECT_EQ(rv_read_phys(m, addr, &v, 8), RV_OK);
  return v;
}

TEST_F(Cpu, MsipDrivesMachineSoftwareInterrupt) {
  program({store(2, 2, 1, 0), load(2, 3, 1, 0), store(2, 0, 1, 0)});
  set_x(1, kMsip);
  set_x(2, 0xffff'ffff);  // only bit 0 is implemented
  run(1);
  EXPECT_EQ(csr(kMip) & kMSI, kMSI);
  run(1);
  EXPECT_EQ(x(3), 1u);
  run(1);
  EXPECT_EQ(csr(kMip) & kMSI, 0u);
}

TEST_F(Cpu, MsipIsReadOnlyThroughMip) {
  set_csr(kMip, kMSI);
  EXPECT_EQ(csr(kMip) & kMSI, 0u);
}

TEST_F(Cpu, MtimecmpControlsMtip) {
  // mtimecmp resets to 0, so the timer is due immediately.
  EXPECT_EQ(csr(kMip) & kMTI, kMTI);
  program({store(3, 2, 1, 0), kNop, kNop});
  set_x(1, kMtimecmp);
  set_x(2, 3);  // steps_per_tick = 1 in this fixture
  run(1);
  EXPECT_EQ(csr(kMip) & kMTI, 0u);  // mtime = 1 < 3
  run(2);
  EXPECT_EQ(csr(kMip) & kMTI, kMTI);  // mtime = 3
}

TEST_F(Cpu, MtimecmpHalvesAreIndependentlyWritable) {
  program({store(2, 2, 1, 0), store(2, 3, 1, 4), load(2, 4, 1, 4), load(3, 5, 1, 0)});
  set_x(1, kMtimecmp);
  set_x(2, 0x1111'2222);
  set_x(3, 0x3333'4444);
  run(4);
  EXPECT_EQ(phys64(m, kMtimecmp), 0x3333'4444'1111'2222u);
  EXPECT_EQ(x(4), 0x3333'4444u);
  EXPECT_EQ(x(5), 0x3333'4444'1111'2222u);
}

TEST_F(Cpu, MtimeFollowsVirtualTimeAndCanBeWritten) {
  program({kNop, kNop, load(3, 3, 1, 0), store(3, 2, 1, 0), load(3, 4, 1, 0)});
  set_x(1, kMtime);
  set_x(2, 1000);
  run(3);
  EXPECT_EQ(x(3), 2u);  // two steps before the load
  run(2);
  EXPECT_EQ(x(4), 1001u);  // written 1000, then one more step
  EXPECT_EQ(csr(kTime), 1002u);
}

TEST_F(Cpu, TimerStepReportsTheDeadline) {
  const uint64_t deadline = 50;
  ASSERT_EQ(rv_write_phys(m, kMtimecmp, &deadline, 8), RV_OK);
  EXPECT_EQ(rv_run(m, 0), RV_OK);
  EXPECT_EQ(rv_exit_info(m)->timer_step, 50u);
  const uint64_t never = ~0ull;
  ASSERT_EQ(rv_write_phys(m, kMtimecmp, &never, 8), RV_OK);
  EXPECT_EQ(rv_run(m, 0), RV_OK);
  EXPECT_EQ(rv_exit_info(m)->timer_step, UINT64_MAX);  // beyond the step counter
}

TEST_F(Cpu, ClintRejectsByteAccessesAndAtomics) {
  program({load(0, 3, 1, 0)});
  set_x(1, kMtimecmp);
  run(1);
  expect_trap(5, kMtimecmp, kRamBase);

  ASSERT_EQ(rv_reset(m), RV_OK);
  set_csr(0x305, kTrapVec);
  const uint32_t amoadd_d = (0u << 27) | (2u << 20) | (1u << 15) | (3u << 12) | (3u << 7) | 0b0101111;
  program({amoadd_d});
  set_x(1, kMtimecmp);
  run(1);
  expect_trap(7, kMtimecmp, kRamBase);  // atomics need RAM
}

TEST_F(Cpu, ClintUnmappedOffsetsReadZeroAndIgnoreWrites) {
  program({store(3, 2, 1, 0), load(3, 3, 1, 0)});
  set_x(1, kClint + 0x100);
  set_x(2, 0x1234);
  set_x(3, 99);
  run(2);
  EXPECT_EQ(x(3), 0u);
  EXPECT_EQ(pc(), kRamBase + 8);
}

TEST_F(Cpu, DebuggerCanAccessClint) {
  const uint32_t one = 1;
  ASSERT_EQ(rv_write_phys(m, kMsip, &one, 4), RV_OK);
  EXPECT_EQ(csr(kMip) & kMSI, kMSI);
  uint8_t byte = 0;
  EXPECT_EQ(rv_read_phys(m, kMsip, &byte, 1), RV_E_RANGE);
}

TEST_F(Cpu, ResetClearsClint) {
  const uint64_t v = 77;
  ASSERT_EQ(rv_write_phys(m, kMtimecmp, &v, 8), RV_OK);
  ASSERT_EQ(rv_write_phys(m, kMtime, &v, 8), RV_OK);
  ASSERT_EQ(rv_reset(m), RV_OK);
  EXPECT_EQ(phys64(m, kMtimecmp), 0u);
  EXPECT_EQ(phys64(m, kMtime), 0u);
}

}  // namespace
