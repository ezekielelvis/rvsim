// Self-modifying code: instruction fetch always sees the latest RAM, however
// the code was changed. The decoded-instruction cache must never serve a
// stale instruction (Phase 12).
#include <gtest/gtest.h>

#include "asm.hpp"

namespace {

using namespace asm_;

class SelfModifyingCode : public Cpu {};

// Runs the instruction at `target` (addi x5, x5, 1) a few times, then
// overwrites it with addi x5, x5, 100 and runs it again.
TEST_F(SelfModifyingCode, StoresToExecutedCodeTakeEffect) {
  const uint64_t target = kRamBase + 0x100;
  program({addi(5, 5, 1), jalr(0, 1, 0)}, target);  // target: x5 += 1; ret
  program({
      addi(6, 0, 3),         // 0x00: x6 = 3 (loop count)
      jal(1, 0x100 - 0x04),  // 0x04: call target
      addi(6, 6, -1),        // 0x08
      bne(6, 0, -8),         // 0x0c: loop to the call
      store(2, 7, 8, 0),     // 0x10: sw x7, 0(x8): target becomes x5 += 100
      0x0000100f,            // 0x14: fence.i
      jal(1, 0x100 - 0x18),  // 0x18: call target
      jal(0, 0),             // 0x1c: halt (j .)
  });
  set_x(7, addi(5, 5, 100));
  set_x(8, target);
  run(30);
  EXPECT_EQ(x(5), 3u + 100u);
}

TEST_F(SelfModifyingCode, StoresAreSeenWithoutFenceI) {
  // rvsim makes stores visible to fetch immediately; fence.i is not needed
  // (a stronger guarantee than Unpriv §3 requires, kept by the cache).
  const uint64_t target = kRamBase + 0x40;
  program({addi(5, 5, 1), jalr(0, 1, 0)}, target);
  program({
      jal(1, 0x40),          // 0x00: call target
      store(2, 7, 8, 0),     // 0x04: sw x7, 0(x8)
      jal(1, 0x40 - 0x08),   // 0x08: call target
      jal(0, 0),
  });
  set_x(7, addi(5, 5, 7));
  set_x(8, target);
  run(20);
  EXPECT_EQ(x(5), 1u + 7u);
}

TEST_F(SelfModifyingCode, CompressedInstructionsToo) {
  // c.addi x5, 1 (0x0285) is rewritten as c.addi x5, 2 (0x0289) in a loop.
  program({0x00000000});
  const uint16_t c1 = 0x0285, c2 = 0x0289;
  const uint64_t target = kRamBase + 0x80;
  ASSERT_EQ(rv_write_phys(m, target, &c1, 2), RV_OK);
  const uint32_t ret = jalr(0, 1, 0);
  ASSERT_EQ(rv_write_phys(m, target + 2, &ret, 4), RV_OK);
  program({
      jal(1, 0x80),                  // 0x00: call target (+1)
      addi(7, 0, c2),                // 0x04: x7 = 0x0289
      store(1, 7, 8, 0),             // 0x08: sh x7, 0(x8)
      jal(1, 0x80 - 0x0c),           // 0x0c: call target (+2)
      jal(0, 0),
  });
  set_x(8, target);
  run(20);
  EXPECT_EQ(x(5), 3u);
}

TEST_F(SelfModifyingCode, HostWritesBetweenRunsTakeEffect) {
  program({addi(5, 5, 1), jal(0, -4)});
  run(10);
  EXPECT_EQ(x(5), 5u);  // 5 iterations of the 2-instruction loop
  const uint32_t w = addi(5, 5, 10);
  ASSERT_EQ(rv_write_phys(m, kRamBase, &w, 4), RV_OK);  // as a DMA or debugger would
  run(10);
  EXPECT_EQ(x(5), 55u);
}

}  // namespace
