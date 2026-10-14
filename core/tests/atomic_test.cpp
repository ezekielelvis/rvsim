// A extension: LR/SC reservations and AMOs (Unpriv ch. 14).
#include "asm.hpp"

namespace {

using namespace asm_;

// AMO encodings: funct5 | aq | rl | rs2 | rs1 | funct3 | rd | 0101111.
constexpr uint32_t amo(uint32_t f5, uint32_t f3, uint32_t rd, uint32_t rs1, uint32_t rs2) {
  return (f5 << 27) | (rs2 << 20) | (rs1 << 15) | (f3 << 12) | (rd << 7) | 0b0101111;
}
constexpr uint32_t lr_w(uint32_t rd, uint32_t rs1) { return amo(0b00010, 2, rd, rs1, 0); }
constexpr uint32_t lr_d(uint32_t rd, uint32_t rs1) { return amo(0b00010, 3, rd, rs1, 0); }
constexpr uint32_t sc_w(uint32_t rd, uint32_t rs1, uint32_t rs2) { return amo(0b00011, 2, rd, rs1, rs2); }
constexpr uint32_t sc_d(uint32_t rd, uint32_t rs1, uint32_t rs2) { return amo(0b00011, 3, rd, rs1, rs2); }

TEST_F(Cpu, LrScSucceeds) {
  program({lr_d(3, 1), addi(3, 3, 1), sc_d(4, 1, 3)});
  set_x(1, kData);
  set_mem<uint64_t>(kData, 41);
  set_x(4, 99);
  run(3);
  EXPECT_EQ(x(4), 0u);  // success
  EXPECT_EQ(mem<uint64_t>(kData), 42u);
}

TEST_F(Cpu, ScWithoutReservationFailsAndDoesNotStore) {
  program({sc_d(4, 1, 2)});
  set_x(1, kData);
  set_x(2, 7);
  run(1);
  EXPECT_EQ(x(4), 1u);
  EXPECT_EQ(mem<uint64_t>(kData), 0u);
}

TEST_F(Cpu, EveryScClearsTheReservation) {
  program({lr_d(3, 1), sc_d(4, 1, 2), sc_d(5, 1, 2)});
  set_x(1, kData);
  set_x(2, 7);
  run(3);
  EXPECT_EQ(x(4), 0u);
  EXPECT_EQ(x(5), 1u);
}

TEST_F(Cpu, ScToOtherAddressFailsAndClearsReservation) {
  program({lr_d(3, 1), sc_d(4, 2, 0), sc_d(5, 1, 0)});
  set_x(1, kData);
  set_x(2, kData + 8);
  run(3);
  EXPECT_EQ(x(4), 1u);
  EXPECT_EQ(x(5), 1u);
}

TEST_F(Cpu, LrWSignExtends) {
  program({lr_w(3, 1)});
  set_x(1, kData);
  set_mem<uint32_t>(kData, 0x8000'0000);
  run(1);
  EXPECT_EQ(x(3), 0xffff'ffff'8000'0000u);
}

TEST_F(Cpu, ScWStoresOnlyWord) {
  program({lr_w(3, 1), sc_w(4, 1, 2)});
  set_x(1, kData);
  set_x(2, 0x1122'3344'5566'7788);
  set_mem<uint64_t>(kData, ~0ull);
  run(2);
  EXPECT_EQ(x(4), 0u);
  EXPECT_EQ(mem<uint64_t>(kData), 0xffff'ffff'5566'7788u);
}

// ADR-012: like Spike, a plain store by this hart does not clear the
// reservation; only another agent's write (rv_write_phys) does.
TEST_F(Cpu, OwnStoreKeepsReservation) {
  program({lr_d(3, 1), store(3, 0, 1, 0), sc_d(4, 1, 2)});
  set_x(1, kData);
  run(3);
  EXPECT_EQ(x(4), 0u);
}

TEST_F(Cpu, ExternalWriteToReservationSetClearsReservation) {
  program({lr_d(3, 1), sc_d(4, 1, 2)});
  set_x(1, kData);
  run(1);
  set_mem<uint8_t>(kData + 7, 1);  // DMA-style write into the reserved doubleword
  run(1);
  EXPECT_EQ(x(4), 1u);
}

TEST_F(Cpu, ExternalWriteElsewhereKeepsReservation) {
  program({lr_d(3, 1), sc_d(4, 1, 2)});
  set_x(1, kData);
  run(1);
  set_mem<uint64_t>(kData + 8, 1);
  run(1);
  EXPECT_EQ(x(4), 0u);
}

TEST_F(Cpu, TrapDoesNotClearReservation) {
  program({lr_d(3, 1), kEcall});
  program({sc_d(4, 1, 2)}, kTrapVec);
  set_x(1, kData);
  run(3);
  EXPECT_EQ(x(4), 0u);
}

TEST_F(Cpu, ResetClearsReservation) {
  program({lr_d(3, 1)});
  set_x(1, kData);
  run(1);
  ASSERT_EQ(rv_reset(m), RV_OK);
  program({sc_d(4, 1, 2)});
  set_x(1, kData);
  run(1);
  EXPECT_EQ(x(4), 1u);
}

// Atomics need natural alignment even when misaligned plain accesses are allowed.
TEST_F(CpuMisalignedOK, MisalignedAtomicsTrap) {
  struct Case {
    uint32_t insn;
    uint64_t cause;
  } cases[] = {
      {lr_d(3, 1), 4},             // LR: load address misaligned
      {sc_d(3, 1, 2), 6},          // SC: store/AMO address misaligned, even without a reservation
      {amo(0, 2, 3, 1, 2), 6},     // AMOADD.W
  };
  for (const Case& c : cases) {
    ASSERT_EQ(rv_reset(m), RV_OK);
    set_csr(0x305, kTrapVec);
    program({c.insn});
    set_x(1, kData + 2);  // misaligned for both word and doubleword
    run(1);
    EXPECT_EQ(csr(0x342), c.cause) << std::hex << c.insn;
  }
}

TEST_F(Cpu, AtomicsOutsideRamFault) {
  struct Case {
    uint32_t insn;
    uint64_t cause;
  } cases[] = {
      {lr_d(3, 1), 5},          // load access fault
      {sc_d(3, 1, 2), 7},       // store/AMO access fault even though the SC would fail
      {amo(0b00001, 3, 3, 1, 2), 7},  // AMOSWAP.D: AMOs report store/AMO faults
  };
  for (const Case& c : cases) {
    ASSERT_EQ(rv_reset(m), RV_OK);
    set_csr(0x305, kTrapVec);
    program({c.insn});
    set_x(1, 0x1000);
    set_x(3, 55);
    run(1);
    expect_trap(c.cause, 0x1000, kRamBase);
    EXPECT_EQ(x(3), 55u);
  }
}

struct AmoCase {
  const char* name;
  uint32_t f5, f3;
  uint64_t mem, rs2;
  uint64_t want_mem;  // compared at the operand width
};

class Amo : public Cpu, public ::testing::WithParamInterface<AmoCase> {};

TEST_P(Amo, ReturnsOldValueAndUpdatesMemory) {
  const AmoCase& c = GetParam();
  program({amo(c.f5, c.f3, 3, 1, 2)});
  set_x(1, kData);
  set_x(2, c.rs2);
  set_mem<uint64_t>(kData, c.mem);
  run(1);
  if (c.f3 == 2) {
    EXPECT_EQ(x(3), static_cast<uint64_t>(static_cast<int64_t>(static_cast<int32_t>(c.mem))));
    EXPECT_EQ(mem<uint32_t>(kData), static_cast<uint32_t>(c.want_mem));
    EXPECT_EQ(mem<uint32_t>(kData + 4), static_cast<uint32_t>(c.mem >> 32));  // untouched
  } else {
    EXPECT_EQ(x(3), c.mem);
    EXPECT_EQ(mem<uint64_t>(kData), c.want_mem);
  }
}

constexpr uint64_t kNegW = 0xaaaa'aaaa'ffff'fff0;  // low word -16, upper word garbage

// clang-format off
INSTANTIATE_TEST_SUITE_P(RV64A, Amo, ::testing::Values(
  AmoCase{"swap_w",  0b00001, 2, kNegW, 5,  5},
  AmoCase{"add_w",   0b00000, 2, kNegW, 20, 4},
  AmoCase{"xor_w",   0b00100, 2, 0xf0,  0xff, 0x0f},
  AmoCase{"and_w",   0b01100, 2, 0xf0,  0x3c, 0x30},
  AmoCase{"or_w",    0b01000, 2, 0xf0,  0x0f, 0xff},
  AmoCase{"min_w",   0b10000, 2, kNegW, 1,  0xffff'fff0},   // signed: -16 < 1
  AmoCase{"max_w",   0b10100, 2, kNegW, 1,  1},
  AmoCase{"minu_w",  0b11000, 2, kNegW, 1,  1},             // unsigned: 0xfffffff0 > 1
  AmoCase{"maxu_w",  0b11100, 2, kNegW, 1,  0xffff'fff0},
  AmoCase{"min_w_uses_low_word_of_rs2", 0b10000, 2, 5, 0x1'0000'0003, 3},
  AmoCase{"swap_d",  0b00001, 3, 7, 9, 9},
  AmoCase{"add_d",   0b00000, 3, ~0ull, 2, 1},
  AmoCase{"min_d",   0b10000, 3, ~0ull, 1, ~0ull},
  AmoCase{"max_d",   0b10100, 3, ~0ull, 1, 1},
  AmoCase{"minu_d",  0b11000, 3, ~0ull, 1, 1},
  AmoCase{"maxu_d",  0b11100, 3, ~0ull, 1, ~0ull}),
  [](const ::testing::TestParamInfo<AmoCase>& p) { return p.param.name; });
// clang-format on

TEST_F(Cpu, AmoToToHostHalts) {
  program({amo(0b00001, 3, 0, 1, 2)});  // amoswap.d covers the whole of tohost
  set_x(1, kToHost);
  set_x(2, 1);
  EXPECT_EQ(run(10).reason, RV_EXIT_HALT);
}

TEST_F(Cpu, AmoTraceRecordsLoadAndStore) {
  ASSERT_EQ(rv_trace_enable(m, 4 * sizeof(rv_trace_record)), RV_OK);
  program({amo(0, 3, 3, 1, 2)});
  set_x(1, kData);
  set_x(2, 2);
  set_mem<uint64_t>(kData, 40);
  run(1);
  const uint8_t* data = nullptr;
  size_t len = 0;
  ASSERT_EQ(rv_trace_peek(m, &data, &len), RV_OK);
  ASSERT_EQ(len, sizeof(rv_trace_record));
  const auto* r = reinterpret_cast<const rv_trace_record*>(data);
  EXPECT_EQ(r->flags, RV_TF_RD | RV_TF_LOAD | RV_TF_STORE);
  EXPECT_EQ(r->rd_value, 40u);
  EXPECT_EQ(r->mem_value, 42u);
}

TEST(AtomicDecode, ReservedEncodingsAreIllegal) {
  rv_config c{};
  c.abi_version = RV_ABI_VERSION;
  c.ram_base = kRamBase;
  c.ram_size = kRamSize;
  c.clint_base = 0x0200'0000;
  c.reset_pc = kRamBase;
  c.steps_per_tick = 1;
  for (uint32_t insn : {amo(0b00010, 3, 3, 1, 2) /* LR with rs2 != 0 */,
                        amo(0b00101, 3, 3, 1, 2) /* AMOCAS.D (Zacas) */,
                        amo(0b00000, 1, 3, 1, 2) /* funct3 001 */}) {
    rv_machine* m = nullptr;
    ASSERT_EQ(rv_create(&c, &m), RV_OK);
    ASSERT_EQ(rv_write_phys(m, kRamBase, &insn, 4), RV_OK);
    ASSERT_EQ(rv_run(m, 1), RV_OK);
    uint64_t cause = 0;
    ASSERT_EQ(rv_get_reg(m, RV_REG_CSR + 0x342, &cause), RV_OK);
    EXPECT_EQ(cause, 2u) << std::hex << insn;
    rv_destroy(m);
  }
}

}  // namespace
