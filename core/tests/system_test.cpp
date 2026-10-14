// Zicsr, traps, MRET, WFI, step accounting and tracing (Priv ch. 2-3).
#include <vector>

#include "asm.hpp"

namespace {

using namespace asm_;

constexpr uint32_t kMstatus = 0x300, kMisa = 0x301, kMie = 0x304, kMtvec = 0x305;
constexpr uint32_t kMscratch = 0x340, kMepc = 0x341, kMhartid = 0xf14;
constexpr uint64_t kMIE = 1u << 3, kMPIE = 1u << 7, kMPP = 3u << 11, kMPRV = 1u << 17;
constexpr uint64_t kUXL64 = 2ull << 32;
constexpr uint64_t kSXL64 = 2ull << 34;
constexpr uint64_t kXL = kUXL64 | kSXL64;  // UXL = SXL = 64, read-only

// Zicsr §9.1 read-modify-write semantics.
TEST_F(Cpu, CsrInstructionsReturnOldValue) {
  program({csrrw(3, kMscratch, 1), csrrs(4, kMscratch, 2), csrrc(5, kMscratch, 1),
           csrrwi(6, kMscratch, 31), csrrsi(7, kMscratch, 0), csrrci(8, kMscratch, 1)});
  set_x(1, 0xf0);
  set_x(2, 0x0f);
  set_csr(kMscratch, 0x1234);
  run(6);
  EXPECT_EQ(x(3), 0x1234u);
  EXPECT_EQ(x(4), 0xf0u);
  EXPECT_EQ(x(5), 0xffu);
  EXPECT_EQ(x(6), 0x0fu);
  EXPECT_EQ(x(7), 31u);
  EXPECT_EQ(x(8), 31u);
  EXPECT_EQ(csr(kMscratch), 30u);
}

TEST_F(Cpu, ReadOnlyCsrCanBeReadWithCsrrsX0) {
  program({csrrs(3, kMhartid, 0)});
  set_x(3, 99);
  run(1);
  EXPECT_EQ(x(3), 0u);
  EXPECT_EQ(pc(), kRamBase + 4);
}

// Priv §2.1: writing a read-only CSR is illegal, even with rd = x0.
TEST_F(Cpu, WritingReadOnlyCsrIsIllegal) {
  const uint32_t insn = csrrw(0, kMhartid, 1);
  program({insn});
  run(1);
  expect_trap(2, insn, kRamBase);
}

TEST_F(Cpu, CsrrsiWithNonZeroUimmToReadOnlyCsrIsIllegal) {
  const uint32_t insn = csrrsi(3, kMhartid, 1);
  program({insn});
  run(1);
  expect_trap(2, insn, kRamBase);
}

// Without PMP, debug triggers or AIA these CSRs do not exist (ADR-007).
TEST_F(Cpu, UnimplementedCsrIsIllegal) {
  for (uint32_t c : {0x7a5u /* tcontrol */, 0x7a0u /* tselect */, 0x3b0u /* pmpaddr0 */,
                     0x309u /* mvip */, 0xc03u /* hpmcounter3: no Zihpm */}) {
    ASSERT_EQ(rv_reset(m), RV_OK);
    set_csr(kMtvec, kTrapVec);
    const uint32_t insn = csrrw(0, c, 0);
    program({insn});
    run(1);
    expect_trap(2, insn, kRamBase);
  }
}

TEST_F(Cpu, MstatusLegalization) {
  // Priv §3.1.6: UXL/SXL read 2. Without an MMU, SUM is read-only zero; MXR
  // and TVM exist because S-mode does (ADR-016). FS exists with S-mode, and
  // SD reflects FS=Dirty.
  program({csrrw(0, kMstatus, 1), csrrs(3, kMstatus, 0)});
  set_x(1, ~0ull);
  run(2);
  const uint64_t writable = (1u << 1) | (1u << 3) | (1u << 5) | (1u << 7) | (1u << 8) | (3u << 11) |
                            (3u << 13) | (1u << 17) | (1u << 19) | (1u << 20) | (1u << 21) | (1u << 22);
  EXPECT_EQ(x(3), kXL | writable | (1ull << 63));
  set_csr(kMstatus, 2u << 11);  // MPP = 2 (reserved H) legalizes to U
  EXPECT_EQ(csr(kMstatus), kXL);
  set_csr(kMstatus, 1u << 11);  // MPP = S is legal
  EXPECT_EQ(csr(kMstatus), kXL | (1u << 11));
}

TEST_F(Cpu, MisaIsReadOnly) {
  const uint64_t misa = csr(kMisa);
  EXPECT_EQ(misa >> 62, 2u);  // MXL = 64
  program({csrrw(0, kMisa, 0)});
  run(1);
  EXPECT_EQ(csr(kMisa), misa);
}

TEST_F(Cpu, MtvecModeAndMepcAlignment) {
  set_csr(kMtvec, kTrapVec | 1);
  set_csr(kMtvec, kRamBase | 3);  // reserved MODE: the write is ignored (ADR-018)
  EXPECT_EQ(csr(kMtvec), kTrapVec | 1);
  set_csr(kMepc, kRamBase | 3);
  EXPECT_EQ(csr(kMepc), kRamBase | 2);  // IALIGN=16: only bit 0 is zero
  set_csr(kMie, ~0ull);
  EXPECT_EQ(csr(kMie), 0xaaau);  // SSIE, MSIE, STIE, MTIE, SEIE, MEIE
}

TEST_F(Cpu, EcallFromMachineMode) {
  program({kEcall});
  set_csr(kMstatus, kMIE);
  run(1);
  expect_trap(11, 0, kRamBase);
  // Priv §3.1.6.1: MPIE <- MIE, MIE <- 0, MPP <- previous privilege.
  EXPECT_EQ(csr(kMstatus) & (kMIE | kMPIE | kMPP), kMPIE | kMPP);
}

TEST_F(Cpu, EcallFromUserModeAndMretBack) {
  // M: set mepc to user code, MPP = U, mret; U: ecall.
  program({kMret});
  program({kEcall}, kRamBase + 0x100);
  set_csr(kMepc, kRamBase + 0x100);
  set_csr(kMstatus, kMPIE | kMPRV);  // MPP = U
  run(1);
  EXPECT_EQ(priv(), 0u);
  EXPECT_EQ(pc(), kRamBase + 0x100);
  // Priv §3.3.2: MIE <- MPIE, MPIE <- 1, MPP <- U, MPRV <- 0 when leaving M.
  EXPECT_EQ(csr(kMstatus), kXL | kMIE | kMPIE);
  run(1);
  expect_trap(8, 0, kRamBase + 0x100);
  EXPECT_EQ(csr(kMstatus) & kMPP, 0u);  // came from U
  EXPECT_EQ(csr(kMstatus) & kMPIE, kMPIE);
}

TEST_F(Cpu, MretToMachineKeepsMprv) {
  program({kMret});
  set_csr(kMepc, kRamBase + 0x40);
  set_csr(kMstatus, kMPP | kMPRV);
  run(1);
  EXPECT_EQ(priv(), 3u);
  EXPECT_EQ(csr(kMstatus) & kMPRV, kMPRV);
  EXPECT_EQ(csr(kMstatus) & kMPP, 0u);
}

TEST_F(Cpu, PrivilegedInstructionsFromUserModeAreIllegal) {
  for (uint32_t insn : {kMret, csrrs(3, kMscratch, 0), kWfi}) {
    ASSERT_EQ(rv_reset(m), RV_OK);
    set_csr(kMtvec, kTrapVec);
    program({insn});
    ASSERT_EQ(rv_set_reg(m, RV_REG_PRIV, 0), RV_OK);
    run(1);
    expect_trap(2, insn, kRamBase);
    EXPECT_EQ(csr(kMstatus) & kMPP, 0u);
  }
}

TEST_F(Cpu, DefinedIllegalEncodings) {
  // All-zero and all-one encodings are defined illegal.
  for (uint32_t insn : {0u, ~0u}) {
    ASSERT_EQ(rv_reset(m), RV_OK);
    set_csr(kMtvec, kTrapVec);
    program({insn});
    run(1);
    expect_trap(2, insn, kRamBase);
  }
}

TEST_F(Cpu, EbreakSetsTvalToPc) {
  program({kNop, kEbreak});
  run(2);
  expect_trap(3, kRamBase + 4, kRamBase + 4);
}

TEST_F(Cpu, WfiInMachineModeCompletes) {
  program({kWfi});
  run(1);
  EXPECT_EQ(pc(), kRamBase + 4);
}

TEST_F(Cpu, VectoredMtvecSendsExceptionsToBase) {
  program({kEcall});
  set_csr(kMtvec, kTrapVec | 1);
  run(1);
  EXPECT_EQ(pc(), kTrapVec);  // vectoring applies to interrupts only (Priv §3.1.7)
}

// ADR-001: a step is a retired instruction or a taken trap; minstret counts
// only retired instructions.
TEST_F(Cpu, BudgetCountsStepsExactly) {
  program({kNop, kNop, kEcall});
  program({kNop}, kTrapVec);
  const rv_exit& e = run(4);
  EXPECT_EQ(e.reason, RV_EXIT_BUDGET);
  EXPECT_EQ(e.steps, 4u);
  EXPECT_EQ(e.instret, 3u);
  EXPECT_EQ(e.pc, kTrapVec + 4);
  EXPECT_EQ(run(5).steps, 9u);
}

TEST_F(Cpu, TrapLoopStillExhaustsBudget) {
  set_csr(kMtvec, 0x1000);  // outside RAM: every fetch faults
  program({jalr(0, 0, 0x100)});  // jump to 0x100: fetch fault
  const rv_exit& e = run(1000);
  EXPECT_EQ(e.reason, RV_EXIT_BUDGET);
  EXPECT_EQ(e.steps, 1000u);
  EXPECT_EQ(e.instret, 1u);
}

std::vector<rv_trace_record> drain(rv_machine* m) {
  const uint8_t* data = nullptr;
  size_t len = 0;
  EXPECT_EQ(rv_trace_peek(m, &data, &len), RV_OK);
  const auto* r = reinterpret_cast<const rv_trace_record*>(data);
  std::vector<rv_trace_record> out(r, r + len / sizeof(rv_trace_record));
  EXPECT_EQ(rv_trace_consume(m), RV_OK);
  return out;
}

TEST_F(Cpu, TraceRecordsCommitsAndTraps) {
  ASSERT_EQ(rv_trace_enable(m, 16 * sizeof(rv_trace_record)), RV_OK);
  program({addi(5, 0, 7), store(3, 5, 1, 0), load(2, 6, 1, 0), csrrw(0, kMscratch, 5), kEcall});
  set_x(1, kData);
  run(5);
  const auto r = drain(m);
  ASSERT_EQ(r.size(), 5u);

  EXPECT_EQ(r[0].kind, RV_TRACE_COMMIT);
  EXPECT_EQ(r[0].pc, kRamBase);
  EXPECT_EQ(r[0].insn_len, 4);
  EXPECT_EQ(r[0].flags, RV_TF_RD);
  EXPECT_EQ(r[0].rd, 5);
  EXPECT_EQ(r[0].rd_value, 7u);

  EXPECT_EQ(r[1].flags, RV_TF_STORE);
  EXPECT_EQ(r[1].mem_addr, kData);
  EXPECT_EQ(r[1].mem_size, 8);
  EXPECT_EQ(r[1].mem_value, 7u);

  EXPECT_EQ(r[2].flags, RV_TF_RD | RV_TF_LOAD);
  EXPECT_EQ(r[2].mem_size, 4);

  EXPECT_EQ(r[3].flags, RV_TF_CSR);
  EXPECT_EQ(r[3].csr, kMscratch);
  EXPECT_EQ(r[3].csr_value, 7u);

  EXPECT_EQ(r[4].kind, RV_TRACE_TRAP);
  EXPECT_EQ(r[4].priv, 3);
  EXPECT_EQ(r[4].rd_value, 11u);  // cause
  EXPECT_EQ(r[4].csr_value, kTrapVec);
}

TEST_F(Cpu, TraceFullExitsBeforeOverflow) {
  ASSERT_EQ(rv_trace_enable(m, 2 * sizeof(rv_trace_record)), RV_OK);
  program({kNop, kNop, kNop});
  const rv_exit& e = run(3);
  EXPECT_EQ(e.reason, RV_EXIT_TRACE_FULL);
  EXPECT_EQ(e.steps, 2u);
  EXPECT_EQ(drain(m).size(), 2u);
  EXPECT_EQ(run(1).reason, RV_EXIT_BUDGET);
  EXPECT_EQ(drain(m).size(), 1u);
}

TEST_F(Cpu, TraceEnableValidatesCapacity) {
  EXPECT_EQ(rv_trace_enable(m, sizeof(rv_trace_record) - 1), RV_E_INVAL);
  const uint8_t* data = nullptr;
  size_t len = 0;
  EXPECT_EQ(rv_trace_peek(m, &data, &len), RV_E_STATE);
  EXPECT_EQ(rv_trace_enable(m, 0), RV_OK);
}

}  // namespace
