// Execution breakpoints (rv_add_breakpoint / rv_remove_breakpoint).
#include <gtest/gtest.h>

#include "asm.hpp"

namespace {

using namespace asm_;

class Breakpoints : public Cpu {
 protected:
  uint32_t add(uint64_t addr, uint64_t len = 2, uint32_t kind = RV_BP_EXEC) {
    uint32_t id = 99;
    EXPECT_EQ(rv_add_breakpoint(m, addr, len, kind, &id), RV_OK);
    return id;
  }
};

TEST_F(Breakpoints, StopsBeforeTheInstructionAndResumesPastIt) {
  program({addi(5, 5, 1), addi(5, 5, 1), addi(5, 5, 1), addi(5, 5, 1)});
  const uint32_t id = add(kRamBase + 8);
  const rv_exit& e = run(100);
  EXPECT_EQ(e.reason, RV_EXIT_BREAKPOINT);
  EXPECT_EQ(e.addr, kRamBase + 8);
  EXPECT_EQ(e.value, id);
  EXPECT_EQ(pc(), kRamBase + 8);
  EXPECT_EQ(x(5), 2u);  // the instruction at the breakpoint has not run
  EXPECT_EQ(e.steps, 2u);
  // Resuming from the breakpoint's pc does not stop there again.
  const rv_exit& e2 = run(2);
  EXPECT_EQ(e2.reason, RV_EXIT_BUDGET);
  EXPECT_EQ(x(5), 4u);
}

TEST_F(Breakpoints, RemovedBreakpointsDoNotStop) {
  program({addi(5, 5, 1), addi(5, 5, 1), addi(5, 5, 1)});
  const uint32_t id = add(kRamBase + 4);
  ASSERT_EQ(rv_remove_breakpoint(m, id), RV_OK);
  EXPECT_EQ(run(3).reason, RV_EXIT_BUDGET);
  EXPECT_EQ(rv_remove_breakpoint(m, id), RV_E_INVAL);
}

TEST_F(Breakpoints, PhysicalBreakpointsMatchTheTranslatedPc) {
  program({addi(5, 5, 1), addi(5, 5, 1)});
  add(kRamBase + 4, 4, RV_BP_EXEC | RV_BP_PHYS);
  EXPECT_EQ(run(10).reason, RV_EXIT_BREAKPOINT);
  EXPECT_EQ(pc(), kRamBase + 4);
}

TEST_F(Breakpoints, BreakpointsDoNotChangeTheStateHash) {
  uint64_t a = 0, b = 0;
  ASSERT_EQ(rv_state_hash(m, RV_HASH_CORE, &a), RV_OK);
  add(kRamBase + 4);
  ASSERT_EQ(rv_state_hash(m, RV_HASH_CORE, &b), RV_OK);
  EXPECT_EQ(a, b);
}

TEST_F(Breakpoints, RejectsWatchpointsAndBadArguments) {
  uint32_t id = 0;
  EXPECT_EQ(rv_add_breakpoint(m, kData, 8, RV_BP_READ, &id), RV_E_UNSUPPORTED);
  EXPECT_EQ(rv_add_breakpoint(m, kData, 8, RV_BP_WRITE, &id), RV_E_UNSUPPORTED);
  EXPECT_EQ(rv_add_breakpoint(m, kData, 0, RV_BP_EXEC, &id), RV_E_INVAL);
  EXPECT_EQ(rv_add_breakpoint(m, ~0ull, 2, RV_BP_EXEC, &id), RV_E_INVAL);
  EXPECT_EQ(rv_add_breakpoint(m, kData, 2, 0, &id), RV_E_INVAL);
  EXPECT_EQ(rv_add_breakpoint(m, kData, 2, 64, &id), RV_E_INVAL);
  EXPECT_EQ(rv_add_breakpoint(m, kData, 2, RV_BP_EXEC, nullptr), RV_E_INVAL);
}

TEST_F(Breakpoints, TableHoldsSixtyFour) {
  for (unsigned i = 0; i < 64; ++i) add(kData + 4 * i);
  uint32_t id = 0;
  EXPECT_EQ(rv_add_breakpoint(m, kData + 1000, 2, RV_BP_EXEC, &id), RV_E_NOMEM);
}

}  // namespace
