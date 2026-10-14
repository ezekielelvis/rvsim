// Tests for the C ABI surface in rv.h: argument validation and lifecycle.
#include <gtest/gtest.h>

#include <array>
#include <cstdint>

#include "rv.h"

namespace {

rv_config default_config() {
  rv_config c{};
  c.abi_version = RV_ABI_VERSION;
  c.ram_base = 0x8000'0000;
  c.ram_size = 16ull << 20;
  c.clint_base = 0x0200'0000;
  c.reset_pc = 0x8000'0000;
  c.steps_per_tick = 10;
  return c;
}

class Machine : public ::testing::Test {
 protected:
  void SetUp() override {
    rv_config c = default_config();
    ASSERT_EQ(rv_create(&c, &m_), RV_OK);
    ASSERT_NE(m_, nullptr);
  }
  void TearDown() override { rv_destroy(m_); }
  rv_machine* m_ = nullptr;
};

TEST(Create, AcceptsDefaultConfig) {
  rv_config c = default_config();
  rv_machine* m = nullptr;
  ASSERT_EQ(rv_create(&c, &m), RV_OK);
  rv_destroy(m);
}

TEST(Create, RejectsNullArguments) {
  rv_config c = default_config();
  rv_machine* m = nullptr;
  EXPECT_EQ(rv_create(nullptr, &m), RV_E_INVAL);
  EXPECT_EQ(rv_create(&c, nullptr), RV_E_INVAL);
}

TEST(Create, RejectsAbiMismatch) {
  rv_config c = default_config();
  c.abi_version = RV_ABI_VERSION + 1;
  rv_machine* m = reinterpret_cast<rv_machine*>(0x1);
  EXPECT_EQ(rv_create(&c, &m), RV_E_ABI);
  EXPECT_EQ(m, nullptr);
}

struct BadConfigCase {
  const char* name;
  void (*mutate)(rv_config&);
};

class CreateRejects : public ::testing::TestWithParam<BadConfigCase> {};

TEST_P(CreateRejects, InvalidConfig) {
  rv_config c = default_config();
  GetParam().mutate(c);
  rv_machine* m = nullptr;
  EXPECT_EQ(rv_create(&c, &m), RV_E_INVAL);
  EXPECT_EQ(m, nullptr);
}

INSTANTIATE_TEST_SUITE_P(
    Config, CreateRejects,
    ::testing::Values(
        BadConfigCase{"ZeroRam", [](rv_config& c) { c.ram_size = 0; }},
        BadConfigCase{"RamSizeNotPageMultiple", [](rv_config& c) { c.ram_size = 4097; }},
        BadConfigCase{"RamBaseUnaligned", [](rv_config& c) { c.ram_base = 0x8000'0010; }},
        BadConfigCase{"RamWrapsAddressSpace",
                      [](rv_config& c) { c.ram_base = 0xFFFF'FFFF'FFFF'F000; c.ram_size = 0x2000; }},
        BadConfigCase{"ClintUnaligned", [](rv_config& c) { c.clint_base = 0x0200'1000; }},
        BadConfigCase{"ClintOverlapsRam", [](rv_config& c) { c.clint_base = 0x8000'0000; }},
        BadConfigCase{"ZeroStepsPerTick", [](rv_config& c) { c.steps_per_tick = 0; }},
        BadConfigCase{"UnknownFlag", [](rv_config& c) { c.flags = 1u << 31; }},
        BadConfigCase{"NonZeroReserved", [](rv_config& c) { c.reserved[3] = 1; }},
        BadConfigCase{"ToHostOutsideRam", [](rv_config& c) { c.htif_tohost = 0x1000; }},
        BadConfigCase{"ToHostUnaligned", [](rv_config& c) { c.htif_tohost = 0x8000'1004; }},
        BadConfigCase{"FromHostPastEndOfRam",
                      [](rv_config& c) { c.htif_fromhost = c.ram_base + c.ram_size; }}),
    [](const ::testing::TestParamInfo<BadConfigCase>& p) { return p.param.name; });

TEST_F(Machine, RunFromEmptyRamReportsBudget) {
  // Zeroed RAM decodes as illegal instructions; the hart traps forever but
  // every trap is a step, so the budget is still honoured.
  ASSERT_EQ(rv_run(m_, 1000), RV_OK);
  const rv_exit* e = rv_exit_info(m_);
  ASSERT_NE(e, nullptr);
  EXPECT_EQ(e->reason, RV_EXIT_BUDGET);
  EXPECT_EQ(e->steps, 1000u);
  EXPECT_EQ(e->instret, 0u);
  EXPECT_EQ(e->timer_step, e->steps);  // mtimecmp resets to 0: the timer is already due
}

TEST_F(Machine, ZeroBudgetExitsWithBudget) {
  ASSERT_EQ(rv_run(m_, 0), RV_OK);
  EXPECT_EQ(rv_exit_info(m_)->reason, RV_EXIT_BUDGET);
}

TEST_F(Machine, ExitPointerIsStableAcrossRuns) {
  const rv_exit* before = rv_exit_info(m_);
  ASSERT_EQ(rv_run(m_, 1), RV_OK);
  ASSERT_EQ(rv_run(m_, 1), RV_OK);
  EXPECT_EQ(rv_exit_info(m_), before);
}

TEST_F(Machine, CompleteMmioWithoutPendingAccessIsStateError) {
  EXPECT_EQ(rv_complete_mmio(m_, 0, 0), RV_E_STATE);
}

TEST_F(Machine, AdvanceIdleOutsideWfiIsStateError) {
  EXPECT_EQ(rv_advance_idle(m_, 10), RV_E_STATE);
}

TEST_F(Machine, X0IsHardwiredZero) {
  ASSERT_EQ(rv_set_reg(m_, RV_REG_X0, 0xdead), RV_OK);
  uint64_t v = 1;
  ASSERT_EQ(rv_get_reg(m_, RV_REG_X0, &v), RV_OK);
  EXPECT_EQ(v, 0u);
}

TEST_F(Machine, GprAndPcRoundTrip) {
  ASSERT_EQ(rv_set_reg(m_, 31, 0x0123'4567'89ab'cdef), RV_OK);
  ASSERT_EQ(rv_set_reg(m_, RV_REG_PC, 0x8000'1000), RV_OK);
  uint64_t v = 0;
  ASSERT_EQ(rv_get_reg(m_, 31, &v), RV_OK);
  EXPECT_EQ(v, 0x0123'4567'89ab'cdefu);
  ASSERT_EQ(rv_get_reg(m_, RV_REG_PC, &v), RV_OK);
  EXPECT_EQ(v, 0x8000'1000u);
}

TEST_F(Machine, PrivResetsToMachineAndRejectsReservedEncoding) {
  uint64_t v = 0;
  ASSERT_EQ(rv_get_reg(m_, RV_REG_PRIV, &v), RV_OK);
  EXPECT_EQ(v, 3u);
  EXPECT_EQ(rv_set_reg(m_, RV_REG_PRIV, 2), RV_E_INVAL);  // reserved (H)
  EXPECT_EQ(rv_set_reg(m_, RV_REG_PRIV, 1), RV_OK);
  EXPECT_EQ(rv_set_reg(m_, RV_REG_PRIV, 0), RV_OK);
}

TEST_F(Machine, UnknownRegisterIsRangeError) {
  uint64_t v = 0;
  EXPECT_EQ(rv_get_reg(m_, 34, &v), RV_E_RANGE);
  EXPECT_EQ(rv_set_reg(m_, RV_REG_CSR + 0x1000, 0), RV_E_RANGE);
  EXPECT_EQ(rv_get_reg(m_, RV_REG_CSR + 0x7c0, &v), RV_E_RANGE);  // unimplemented CSR
}

TEST_F(Machine, DebuggerCsrAccess) {
  uint64_t v = 1;
  ASSERT_EQ(rv_get_reg(m_, RV_REG_CSR + 0xf14, &v), RV_OK);  // mhartid
  EXPECT_EQ(v, 0u);
  EXPECT_EQ(rv_set_reg(m_, RV_REG_CSR + 0xf14, 1), RV_E_INVAL);  // read-only
  ASSERT_EQ(rv_set_reg(m_, RV_REG_CSR + 0x340, 0xabc), RV_OK);    // mscratch
  ASSERT_EQ(rv_get_reg(m_, RV_REG_CSR + 0x340, &v), RV_OK);
  EXPECT_EQ(v, 0xabcu);
}

TEST_F(Machine, PhysMemoryRoundTrip) {
  const std::array<uint8_t, 8> in{1, 2, 3, 4, 5, 6, 7, 8};
  ASSERT_EQ(rv_write_phys(m_, 0x8000'0ffc, in.data(), in.size()), RV_OK);
  std::array<uint8_t, 8> out{};
  ASSERT_EQ(rv_read_phys(m_, 0x8000'0ffc, out.data(), out.size()), RV_OK);
  EXPECT_EQ(in, out);

  uint64_t size = 0;
  const uint8_t* view = rv_ram_view(m_, &size);
  ASSERT_NE(view, nullptr);
  EXPECT_EQ(size, 16ull << 20);
  EXPECT_EQ(view[0xffc], 1);
}

TEST_F(Machine, PhysMemoryBoundsChecked) {
  uint8_t b = 0;
  const uint64_t end = 0x8000'0000 + (16ull << 20);
  EXPECT_EQ(rv_read_phys(m_, 0x7fff'ffff, &b, 1), RV_E_RANGE);
  EXPECT_EQ(rv_read_phys(m_, end, &b, 1), RV_E_RANGE);
  EXPECT_EQ(rv_read_phys(m_, end - 1, &b, 1), RV_OK);
  std::array<uint8_t, 2> two{};
  EXPECT_EQ(rv_write_phys(m_, end - 1, two.data(), 2), RV_E_RANGE);
  EXPECT_EQ(rv_read_phys(m_, UINT64_MAX, &b, 1), RV_E_RANGE);
}

TEST_F(Machine, RamStartsZeroed) {
  std::array<uint8_t, 64> out{};
  out.fill(0xff);
  ASSERT_EQ(rv_read_phys(m_, 0x8080'0000, out.data(), out.size()), RV_OK);
  for (uint8_t b : out) EXPECT_EQ(b, 0);
}

TEST_F(Machine, IrqLinesAcceptOnlyExternalLines) {
  EXPECT_EQ(rv_set_irq_lines(m_, RV_IRQ_MEIP | RV_IRQ_SEIP), RV_OK);
  EXPECT_EQ(rv_set_irq_lines(m_, 1ull << 7), RV_E_INVAL);  // MTIP is owned by the CLINT.
}

TEST_F(Machine, ResetRestoresArchitecturalState) {
  ASSERT_EQ(rv_set_reg(m_, 5, 42), RV_OK);
  ASSERT_EQ(rv_set_reg(m_, RV_REG_PC, 0x8000'2000), RV_OK);
  const uint8_t byte = 0xaa;
  ASSERT_EQ(rv_write_phys(m_, 0x8000'0000, &byte, 1), RV_OK);
  ASSERT_EQ(rv_reset(m_), RV_OK);
  uint64_t v = 1;
  ASSERT_EQ(rv_get_reg(m_, 5, &v), RV_OK);
  EXPECT_EQ(v, 0u);
  ASSERT_EQ(rv_get_reg(m_, RV_REG_PC, &v), RV_OK);
  EXPECT_EQ(v, 0x8000'0000u);
  uint8_t out = 0;
  ASSERT_EQ(rv_read_phys(m_, 0x8000'0000, &out, 1), RV_OK);
  EXPECT_EQ(out, 0xaa);  // RAM survives reset.
}

TEST_F(Machine, StateHashValidatesSelector) {
  uint64_t h = 0;
  EXPECT_EQ(rv_state_hash(m_, 0, &h), RV_E_INVAL);
  EXPECT_EQ(rv_state_hash(m_, 4, &h), RV_E_INVAL);
}

}  // namespace
