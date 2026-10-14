// MMIO exits (ADR-022): accesses outside RAM and the CLINT go to the VMM.
#include <cstring>

#include "asm.hpp"

namespace {

using namespace asm_;

constexpr uint64_t kUart = 0x1000'0000;

TEST_F(Cpu, MmioLoadExitsWithoutChangingState) {
  program({load(2, 3, 1, 4)});  // lw x3, 4(x1)
  set_x(1, kUart);
  set_x(3, 99);
  const rv_exit& e = run(10);
  EXPECT_EQ(e.reason, RV_EXIT_MMIO_READ);
  EXPECT_EQ(e.addr, kUart + 4);
  EXPECT_EQ(e.size, 4u);
  EXPECT_EQ(e.pc, kRamBase);  // not retired
  EXPECT_EQ(e.steps, 0u);
  EXPECT_EQ(e.instret, 0u);
  EXPECT_EQ(x(3), 99u);
  EXPECT_EQ(pc(), kRamBase);
}

TEST_F(Cpu, RunRefusesUntilMmioIsCompleted) {
  program({load(2, 3, 1, 0)});
  set_x(1, kUart);
  run(10);
  EXPECT_EQ(rv_run(m, 10), RV_E_STATE);
  ASSERT_EQ(rv_complete_mmio(m, 0, 0), RV_OK);
  EXPECT_EQ(rv_complete_mmio(m, 0, 0), RV_E_STATE);  // already completed
}

struct LoadExtCase {
  const char* name;
  uint32_t f3;
  uint64_t device_value;
  uint64_t want;
};

class MmioLoad : public Cpu, public ::testing::WithParamInterface<LoadExtCase> {};

TEST_P(MmioLoad, CompletionRetiresWithExtension) {
  const LoadExtCase& c = GetParam();
  program({load(c.f3, 3, 1, 0), kNop});
  set_x(1, kUart);
  ASSERT_EQ(run(10).reason, RV_EXIT_MMIO_READ);
  ASSERT_EQ(rv_complete_mmio(m, c.device_value, 0), RV_OK);
  const rv_exit& e = run(1);
  EXPECT_EQ(e.reason, RV_EXIT_BUDGET);
  EXPECT_EQ(x(3), c.want);
  EXPECT_EQ(pc(), kRamBase + 4);
  EXPECT_EQ(e.steps, 1u);
  EXPECT_EQ(e.instret, 1u);
}

// clang-format off
INSTANTIATE_TEST_SUITE_P(Widths, MmioLoad, ::testing::Values(
  LoadExtCase{"lb",  0, 0x80,          0xffff'ffff'ffff'ff80},
  LoadExtCase{"lbu", 4, 0x80,          0x80},
  LoadExtCase{"lh",  1, 0x8001,        0xffff'ffff'ffff'8001},
  LoadExtCase{"lhu", 5, 0x8001,        0x8001},
  LoadExtCase{"lw",  2, 0x8000'0000,   0xffff'ffff'8000'0000},
  LoadExtCase{"lwu", 6, 0x8000'0000,   0x8000'0000},
  LoadExtCase{"ld",  3, 0x1234'5678'9abc'def0, 0x1234'5678'9abc'def0},
  // The device value is truncated to the access width.
  LoadExtCase{"lbu_truncates", 4, 0x1ff, 0xff}),
  [](const ::testing::TestParamInfo<LoadExtCase>& p) { return p.param.name; });
// clang-format on

TEST_F(Cpu, MmioStoreExitsWithValue) {
  program({store(0, 2, 1, 0), kNop});  // sb x2, 0(x1)
  set_x(1, kUart);
  set_x(2, 0x1234'5641);
  const rv_exit& e = run(10);
  EXPECT_EQ(e.reason, RV_EXIT_MMIO_WRITE);
  EXPECT_EQ(e.addr, kUart);
  EXPECT_EQ(e.size, 1u);
  EXPECT_EQ(e.value, 0x41u);  // truncated to the access, zero-extended
  ASSERT_EQ(rv_complete_mmio(m, 0, 0), RV_OK);
  EXPECT_EQ(run(1).instret, 1u);
  EXPECT_EQ(pc(), kRamBase + 4);
}

TEST_F(Cpu, MmioFaultCompletionRaisesAccessFault) {
  program({load(3, 3, 1, 0)});
  set_x(1, 0x4000'0000);  // nothing there
  ASSERT_EQ(run(10).reason, RV_EXIT_MMIO_READ);
  ASSERT_EQ(rv_complete_mmio(m, 0, 1), RV_OK);
  run(1);
  expect_trap(5, 0x4000'0000, kRamBase);

  ASSERT_EQ(rv_reset(m), RV_OK);
  set_csr(0x305, kTrapVec);
  program({store(3, 2, 1, 0)});
  set_x(1, 0x4000'0000);
  ASSERT_EQ(run(10).reason, RV_EXIT_MMIO_WRITE);
  ASSERT_EQ(rv_complete_mmio(m, 0, 1), RV_OK);
  run(1);
  expect_trap(7, 0x4000'0000, kRamBase);
}

TEST_F(Cpu, InterruptsWaitForTheMmioInstructionToRetire) {
  program({load(2, 3, 1, 0), kNop});
  set_x(1, kUart);
  ASSERT_EQ(run(10).reason, RV_EXIT_MMIO_READ);
  // An interrupt arrives while the VMM services the access.
  ASSERT_EQ(rv_set_irq_lines(m, RV_IRQ_MEIP), RV_OK);
  set_csr(0x304, 1u << 11);
  set_csr(0x300, 1u << 3);
  ASSERT_EQ(rv_complete_mmio(m, 7, 0), RV_OK);
  run(1);
  EXPECT_EQ(x(3), 7u);              // the load retired first...
  EXPECT_EQ(pc(), kRamBase + 4);
  run(1);
  expect_trap((1ull << 63) | 11, 0, kRamBase + 4);  // ...then the interrupt
}

TEST_F(Cpu, MisalignedMmioTrapsAsMisaligned) {
  program({load(2, 3, 1, 2)});
  set_x(1, kUart);
  run(1);
  expect_trap(4, kUart + 2, kRamBase);  // ADR-004: the misaligned check comes first
}

TEST_F(CpuMisalignedOK, MisalignedMmioAccessFaults) {
  program({load(2, 3, 1, 2)});
  set_x(1, kUart);
  EXPECT_EQ(run(1).reason, RV_EXIT_BUDGET);
  expect_trap(5, kUart + 2, kRamBase);  // MMIO accesses are never split
}

TEST_F(Cpu, AtomicsAndFetchesOutsideRamDoNotExit) {
  const uint32_t amoswap_w = (1u << 27) | (2u << 20) | (1u << 15) | (2u << 12) | (3u << 7) | 0b0101111;
  program({amoswap_w});
  set_x(1, kUart);
  EXPECT_EQ(run(1).reason, RV_EXIT_BUDGET);
  EXPECT_EQ(csr(0x342), 7u);  // store/AMO access fault

  ASSERT_EQ(rv_reset(m), RV_OK);
  set_csr(0x305, kTrapVec);
  program({jalr(0, 1, 0)});
  set_x(1, kUart);
  EXPECT_EQ(run(2).reason, RV_EXIT_BUDGET);
  expect_trap(1, kUart, kUart);  // instruction access fault
}

TEST_F(Cpu, MmioTraceRecordIsWrittenOnRetirement) {
  ASSERT_EQ(rv_trace_enable(m, 4 * sizeof(rv_trace_record)), RV_OK);
  program({load(2, 3, 1, 0)});
  set_x(1, kUart);
  run(10);
  const uint8_t* data = nullptr;
  size_t len = 0;
  ASSERT_EQ(rv_trace_peek(m, &data, &len), RV_OK);
  EXPECT_EQ(len, 0u);  // the exit itself is not a step
  ASSERT_EQ(rv_complete_mmio(m, 5, 0), RV_OK);
  run(1);
  ASSERT_EQ(rv_trace_peek(m, &data, &len), RV_OK);
  ASSERT_EQ(len, sizeof(rv_trace_record));
  rv_trace_record r;
  std::memcpy(&r, data, sizeof r);
  EXPECT_EQ(r.flags, RV_TF_RD | RV_TF_LOAD);
  EXPECT_EQ(r.mem_addr, kUart);
  EXPECT_EQ(r.rd_value, 5u);
}

TEST_F(Cpu, CountersIgnoreTheExit) {
  program({load(2, 3, 1, 0), csrrs(4, 0xb02, 0)});
  set_x(1, kUart);
  run(10);
  ASSERT_EQ(rv_complete_mmio(m, 0, 0), RV_OK);
  run(2);
  EXPECT_EQ(x(4), 1u);  // minstret counted the load once
}

TEST_F(Cpu, ResetDiscardsPendingMmio) {
  program({load(2, 3, 1, 0)});
  set_x(1, kUart);
  run(10);
  ASSERT_EQ(rv_reset(m), RV_OK);
  EXPECT_EQ(rv_run(m, 0), RV_OK);
}

}  // namespace
