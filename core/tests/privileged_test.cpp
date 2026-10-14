// M/S/U privilege: delegation, interrupts, xRET, WFI, counters and the
// supervisor CSR views (Priv ch. 3-4).
#include "asm.hpp"

namespace {

using namespace asm_;

// CSR numbers.
constexpr uint32_t kSstatus = 0x100, kSie = 0x104, kStvec = 0x105, kScounteren = 0x106;
constexpr uint32_t kSepc = 0x141, kScause = 0x142, kStval = 0x143, kSip = 0x144, kSatp = 0x180;
constexpr uint32_t kMstatus = 0x300, kMedeleg = 0x302, kMideleg = 0x303, kMie = 0x304;
constexpr uint32_t kMcounteren = 0x306, kMcountinhibit = 0x320, kMepc = 0x341, kMcause = 0x342;
constexpr uint32_t kMip = 0x344, kMcycle = 0xb00, kMinstret = 0xb02, kMhpmcounter3 = 0xb03;
constexpr uint32_t kCycle = 0xc00, kTime = 0xc01, kInstret = 0xc02;

// mstatus bits.
constexpr uint64_t kSIE = 1u << 1, kMIE = 1u << 3, kSPIE = 1u << 5, kMPIE = 1u << 7;
constexpr uint64_t kSPP = 1u << 8, kMPPs = 1u << 11, kMPRV = 1u << 17, kTW = 1u << 21, kTSR = 1u << 22;

// Interrupt bits.
constexpr uint64_t kSSI = 1u << 1, kMTI = 1u << 7, kSEI = 1u << 9, kMEI = 1u << 11;
constexpr uint64_t kIntr = 1ull << 63;

constexpr uint64_t kSTrapVec = kRamBase + 0x9000;

class Priv : public Cpu {
 protected:
  void SetUp() override {
    Cpu::SetUp();
    set_csr(kStvec, kSTrapVec);
  }
  void set_priv(uint64_t p) { ASSERT_EQ(rv_set_reg(m, RV_REG_PRIV, p), RV_OK); }
};

// ---------------------------------------------------------------- delegation

TEST_F(Priv, DelegatableExceptionMask) {
  set_csr(kMedeleg, ~0ull);
  // Everything that can occur except misaligned fetch (impossible with C)
  // and ECALL from M (read-only zero, Priv §3.1.8). No page faults yet.
  EXPECT_EQ(csr(kMedeleg), 0x3feu);
  set_csr(kMideleg, ~0ull);
  EXPECT_EQ(csr(kMideleg), 0x222u);  // SSI, STI, SEI
}

TEST_F(Priv, DelegatedExceptionFromUserGoesToSupervisor) {
  program({kEcall});
  set_csr(kMedeleg, 1u << 8);
  set_csr(kMstatus, kSIE);
  set_priv(0);
  run(1);
  EXPECT_EQ(priv(), 1u);
  EXPECT_EQ(pc(), kSTrapVec);
  EXPECT_EQ(csr(kScause), 8u);
  EXPECT_EQ(csr(kSepc), kRamBase);
  EXPECT_EQ(csr(kStval), 0u);
  EXPECT_EQ(csr(kMcause), 0u);  // M-mode trap state untouched
  // Priv §4.1.1: SPP <- U, SPIE <- SIE, SIE <- 0.
  EXPECT_EQ(csr(kMstatus) & (kSPP | kSPIE | kSIE), kSPIE);
}

TEST_F(Priv, DelegatedExceptionFromSupervisorSetsSpp) {
  const uint32_t insn = csrrw(0, 0x7c0, 0);  // unimplemented CSR
  program({insn});
  set_csr(kMedeleg, 1u << 2);
  set_priv(1);
  run(1);
  EXPECT_EQ(priv(), 1u);
  EXPECT_EQ(csr(kScause), 2u);
  EXPECT_EQ(csr(kStval), insn);
  EXPECT_EQ(csr(kMstatus) & kSPP, kSPP);
}

TEST_F(Priv, DelegationNeverLowersPrivilege) {
  // Priv §3.1.8: a trap in M-mode is handled in M-mode even if delegated.
  program({kEbreak});
  set_csr(kMedeleg, 1u << 3);
  run(1);
  expect_trap(3, kRamBase, kRamBase);
}

TEST_F(Priv, UndelegatedExceptionFromUserGoesToMachine) {
  program({kEcall});
  set_priv(0);
  run(1);
  expect_trap(8, 0, kRamBase);
  EXPECT_EQ(csr(kMstatus) & (3u << 11), 0u);  // MPP = U
}

// --------------------------------------------------------------------- xRET

TEST_F(Priv, SretReturnsToSppAndPopsSie) {
  program({kSret});
  set_csr(kSepc, kRamBase + 0x40);
  set_csr(kMstatus, kSPIE | kSPP | kMPRV);
  set_priv(1);
  run(1);
  EXPECT_EQ(priv(), 1u);
  EXPECT_EQ(pc(), kRamBase + 0x40);
  // SIE <- SPIE, SPIE <- 1, SPP <- U; MPRV <- 0 (Priv 1.12 §3.1.6.3).
  EXPECT_EQ(csr(kMstatus) & (kSIE | kSPIE | kSPP | kMPRV), kSIE | kSPIE);
}

TEST_F(Priv, SretToUser) {
  program({kSret});
  set_csr(kSepc, kRamBase + 0x40);
  set_priv(1);
  run(1);
  EXPECT_EQ(priv(), 0u);
}

TEST_F(Priv, SretIsLegalInMachineMode) {
  program({kSret});
  set_csr(kSepc, kRamBase + 0x40);
  set_csr(kMstatus, kSPP);
  run(1);
  EXPECT_EQ(priv(), 1u);
  EXPECT_EQ(pc(), kRamBase + 0x40);
}

TEST_F(Priv, SretIllegalInUserAndWithTsr) {
  program({kSret});
  set_priv(0);
  run(1);
  expect_trap(2, kSret, kRamBase);

  ASSERT_EQ(rv_reset(m), RV_OK);
  set_csr(0x305, kTrapVec);
  program({kSret});
  set_csr(kMstatus, kTSR);
  set_priv(1);
  run(1);
  expect_trap(2, kSret, kRamBase);
}

TEST_F(Priv, MretToSupervisorClearsMprv) {
  program({kMret});
  set_csr(kMepc, kRamBase + 0x40);
  set_csr(kMstatus, kMPPs | kMPRV);
  run(1);
  EXPECT_EQ(priv(), 1u);
  EXPECT_EQ(csr(kMstatus) & kMPRV, 0u);
}

// --------------------------------------------------------------- interrupts

TEST_F(Priv, MachineInterruptNeedsMieInMachineMode) {
  program({kNop, kNop});
  set_csr(kMip, kSSI);  // undelegated: an M-level interrupt
  set_csr(kMie, kSSI);
  run(1);
  EXPECT_EQ(pc(), kRamBase + 4);  // MIE = 0: not taken
  set_csr(kMstatus, kMIE);
  run(1);
  expect_trap(kIntr | 1, 0, kRamBase + 4);
  EXPECT_EQ(csr(kMstatus) & (kMIE | kMPIE), kMPIE);
}

TEST_F(Priv, MachineInterruptIgnoresMieBelowMachineMode) {
  program({kNop});
  set_csr(kMip, kSSI);
  set_csr(kMie, kSSI);
  set_priv(1);  // MIE = 0, but S-mode is below M
  run(1);
  expect_trap(kIntr | 1, 0, kRamBase);
  EXPECT_EQ(csr(kMstatus) & (3u << 11), kMPPs);
}

TEST_F(Priv, DelegatedInterruptRules) {
  program({kNop, kNop, kNop});
  set_csr(kMideleg, kSSI);
  set_csr(kMip, kSSI);
  set_csr(kMie, kSSI);
  set_csr(kMstatus, kSIE | kMIE);
  run(1);  // M-mode: delegated interrupts are never taken
  EXPECT_EQ(pc(), kRamBase + 4);
  set_priv(1);
  set_csr(kMstatus, 0);  // S-mode with SIE = 0: not taken
  run(1);
  EXPECT_EQ(pc(), kRamBase + 8);
  set_priv(0);  // U-mode: taken regardless of SIE
  run(1);
  EXPECT_EQ(priv(), 1u);
  EXPECT_EQ(pc(), kSTrapVec);
  EXPECT_EQ(csr(kScause), kIntr | 1);
  EXPECT_EQ(csr(kSepc), kRamBase + 8);
}

TEST_F(Priv, InterruptPriority) {
  // Priv §3.1.9: MEI > MSI > MTI > SEI > SSI > STI.
  program({kNop});
  set_csr(kMip, kSSI);
  ASSERT_EQ(rv_set_irq_lines(m, RV_IRQ_SEIP | RV_IRQ_MEIP), RV_OK);
  set_csr(kMie, kSSI | kSEI | kMEI | kMTI);
  set_csr(kMstatus, kMIE);
  run(1);
  EXPECT_EQ(csr(kMcause), kIntr | 11);
  ASSERT_EQ(rv_set_irq_lines(m, RV_IRQ_SEIP), RV_OK);
  set_csr(kMstatus, kMIE);
  run(1);
  EXPECT_EQ(csr(kMcause), kIntr | 7);  // MTIP is pending: mtimecmp resets to 0
  set_csr(kMie, kSSI | kSEI);
  set_csr(kMstatus, kMIE);
  run(1);
  EXPECT_EQ(csr(kMcause), kIntr | 9);
}

TEST_F(Priv, MachineLevelBeatsSupervisorLevel) {
  // An enabled M-level interrupt goes first even if its number is lower.
  program({kNop});
  set_csr(kMideleg, kSEI);
  ASSERT_EQ(rv_set_irq_lines(m, RV_IRQ_SEIP), RV_OK);
  set_csr(kMip, kSSI);  // SSI stays at M-level
  set_csr(kMie, kSSI | kSEI);
  set_priv(0);
  run(1);
  EXPECT_EQ(priv(), 3u);
  EXPECT_EQ(csr(kMcause), kIntr | 1);
}

TEST_F(Priv, VectoredInterruptsAndSupervisorVectoring) {
  program({kNop});
  set_csr(0x305, kTrapVec | 1);
  set_csr(kMip, kSSI);
  set_csr(kMie, kSSI);
  set_csr(kMstatus, kMIE);
  run(1);
  EXPECT_EQ(pc(), kTrapVec + 4 * 1);

  ASSERT_EQ(rv_reset(m), RV_OK);
  program({kNop});
  set_csr(kStvec, kSTrapVec | 1);
  set_csr(kMideleg, kSSI);
  set_csr(kMip, kSSI);
  set_csr(kMie, kSSI);
  set_priv(0);
  run(1);
  EXPECT_EQ(pc(), kSTrapVec + 4 * 1);
}

TEST_F(Priv, SuppressedInterruptIsNotTaken) {
  program({kNop});
  ASSERT_EQ(rv_set_irq_lines(m, RV_IRQ_MEIP), RV_OK);
  set_csr(kMie, kMEI);
  set_csr(kMstatus, kMIE);
  ASSERT_EQ(rv_set_irq_mask(m, kMEI), RV_OK);  // fault injection: drop MEI
  run(1);
  EXPECT_EQ(pc(), kRamBase + 4);
  ASSERT_EQ(rv_set_irq_mask(m, 0), RV_OK);
  run(1);
  EXPECT_EQ(csr(kMcause), kIntr | 11);
}

TEST_F(Priv, TimerInterruptPendingAfterReset) {
  // The CLINT's mtimecmp resets to 0, so mtime >= mtimecmp from the start.
  EXPECT_EQ(csr(kMip) & kMTI, kMTI);
  EXPECT_EQ(rv_run(m, 0), RV_OK);
  EXPECT_EQ(rv_exit_info(m)->timer_step, rv_exit_info(m)->steps);
}

// Priv §3.1.9: reads see SEIP as software bit OR external signal, but only
// the software bit takes part in CSRRS/CSRRC.
TEST_F(Priv, SeipReadVersusReadModifyWrite) {
  program({csrrs(3, kMip, 0), csrrsi(4, kMip, 2 /* SSIP */)});
  ASSERT_EQ(rv_set_irq_lines(m, RV_IRQ_SEIP), RV_OK);
  run(2);
  EXPECT_EQ(x(3) & kSEI, kSEI);
  EXPECT_EQ(x(4) & kSEI, kSEI);
  ASSERT_EQ(rv_set_irq_lines(m, 0), RV_OK);
  EXPECT_EQ(csr(kMip) & (kSEI | kSSI), kSSI);  // the RMW did not latch SEIP
}

TEST_F(Priv, MipWritableBits) {
  set_csr(kMip, ~0ull);
  EXPECT_EQ(csr(kMip), 0x222u | kMTI);  // SSIP/STIP/SEIP written; MTIP from the CLINT
}

// ---------------------------------------------------------------------- WFI

TEST_F(Priv, WfiIdlesUntilAnInterruptIsPending) {
  program({kWfi, addi(5, 0, 1)});
  set_csr(kMie, kMEI);  // MIE = 0: the interrupt will wake WFI but not trap
  const rv_exit& e = run(10);
  EXPECT_EQ(e.reason, RV_EXIT_WFI);
  EXPECT_EQ(e.pc, kRamBase + 4);  // WFI retired
  EXPECT_EQ(e.steps, 1u);
  EXPECT_EQ(run(10).reason, RV_EXIT_WFI);  // still idle, no steps used
  EXPECT_EQ(rv_exit_info(m)->steps, 1u);

  ASSERT_EQ(rv_advance_idle(m, 100), RV_OK);
  EXPECT_EQ(csr(kTime), 101u);       // idle time advances mtime (steps_per_tick = 1)...
  EXPECT_EQ(csr(kMcycle), 1u);       // ...but not mcycle (ADR-015)
  ASSERT_EQ(rv_set_irq_lines(m, RV_IRQ_MEIP), RV_OK);
  run(1);
  EXPECT_EQ(x(5), 1u);  // woke and continued without trapping
  EXPECT_EQ(rv_advance_idle(m, 1), RV_E_STATE);
}

TEST_F(Priv, WfiCompletesWhenInterruptAlreadyPending) {
  program({kWfi, addi(5, 0, 1)});
  set_csr(kMip, kSSI);
  set_csr(kMie, kSSI);  // pending and enabled in mie; MIE = 0
  EXPECT_EQ(run(2).reason, RV_EXIT_BUDGET);
  EXPECT_EQ(x(5), 1u);
}

TEST_F(Priv, WfiTakesTheWakingInterrupt) {
  program({kWfi});
  set_csr(kMie, kMEI);
  set_csr(kMstatus, kMIE);
  EXPECT_EQ(run(10).reason, RV_EXIT_WFI);
  ASSERT_EQ(rv_set_irq_lines(m, RV_IRQ_MEIP), RV_OK);
  run(1);
  expect_trap(kIntr | 11, 0, kRamBase + 4);  // mepc is after the WFI
}

TEST_F(Priv, WfiPrivilegeRules) {
  // S-mode with TW = 0: legal. TW = 1: illegal below M. U-mode: illegal (ADR-009).
  program({kWfi});
  set_priv(1);
  EXPECT_EQ(run(1).reason, RV_EXIT_BUDGET);
  EXPECT_EQ(pc(), kRamBase + 4);

  for (uint64_t p : {1u, 0u}) {
    ASSERT_EQ(rv_reset(m), RV_OK);
    set_csr(0x305, kTrapVec);
    program({kWfi});
    set_csr(kMstatus, p == 1 ? kTW : 0);
    set_priv(p);
    run(1);
    expect_trap(2, kWfi, kRamBase);
  }

  ASSERT_EQ(rv_reset(m), RV_OK);
  program({kWfi});
  set_csr(kMstatus, kTW);  // TW does not affect M-mode
  run(1);
  EXPECT_EQ(pc(), kRamBase + 4);
}

// ----------------------------------------------------------------- counters

TEST_F(Priv, CountersCountRetiredInstructionsOnly) {
  program({kNop, kEcall});
  program({csrrs(3, kCycle, 0), csrrs(4, kInstret, 0), csrrs(5, kTime, 0)}, kTrapVec);
  run(5);
  // nop retired; ecall trapped (no count); the first csrr reads 1.
  EXPECT_EQ(x(3), 1u);
  EXPECT_EQ(x(4), 2u);
  EXPECT_EQ(x(5), 4u);  // time follows steps, which include the trap
}

TEST_F(Priv, ExplicitCounterWriteReplacesIncrement) {
  program({csrrw(0, kMinstret, 1), csrrs(3, kMinstret, 0), csrrw(0, kMcycle, 1), csrrs(4, kMcycle, 0)});
  set_x(1, 100);
  run(4);
  EXPECT_EQ(x(3), 100u);
  EXPECT_EQ(x(4), 100u);
}

TEST_F(Priv, CountinhibitStopsCounters) {
  set_csr(kMcountinhibit, ~0ull);
  EXPECT_EQ(csr(kMcountinhibit), 5u);  // CY and IR; time cannot be inhibited
  program({kNop, kNop});
  run(2);
  EXPECT_EQ(csr(kMcycle), 0u);
  EXPECT_EQ(csr(kMinstret), 0u);
}

TEST_F(Priv, CounterAccessBelowMachineMode) {
  const uint32_t rd = csrrs(3, kCycle, 0);
  // S-mode needs mcounteren.CY.
  program({rd});
  set_priv(1);
  run(1);
  expect_trap(2, rd, kRamBase);

  ASSERT_EQ(rv_reset(m), RV_OK);
  set_csr(0x305, kTrapVec);
  program({rd});
  set_csr(kMcounteren, 1);
  set_priv(1);
  run(1);
  EXPECT_EQ(pc(), kRamBase + 4);

  // U-mode needs scounteren.CY as well.
  ASSERT_EQ(rv_reset(m), RV_OK);
  set_csr(0x305, kTrapVec);
  program({rd});
  set_csr(kMcounteren, 1);
  set_priv(0);
  run(1);
  expect_trap(2, rd, kRamBase);

  ASSERT_EQ(rv_reset(m), RV_OK);
  program({rd});
  set_csr(kMcounteren, 1);
  set_csr(kScounteren, 1);
  set_priv(0);
  run(1);
  EXPECT_EQ(pc(), kRamBase + 4);
}

TEST_F(Priv, HpmCountersDoNotExist) {
  // ADR-019: no HPM counters, so their CSRs are absent.
  for (uint32_t c : {kMhpmcounter3, 0xb1fu, 0x323u /* mhpmevent3 */, 0x33fu}) {
    ASSERT_EQ(rv_reset(m), RV_OK);
    set_csr(0x305, kTrapVec);
    const uint32_t insn = csrrs(3, c, 0);
    program({insn});
    run(1);
    expect_trap(2, insn, kRamBase);
  }
}

// --------------------------------------------------------- supervisor views

TEST_F(Priv, SstatusIsARestrictedViewOfMstatus) {
  set_csr(kMstatus, kMIE | kMPIE | kSIE | kTSR);
  EXPECT_EQ(csr(kSstatus), (2ull << 32) | kSIE);  // UXL is visible, M fields are not
  set_csr(kSstatus, ~0ull);
  const uint64_t ms = csr(kMstatus);
  EXPECT_EQ(ms & (kMIE | kMPIE | kTSR), kMIE | kMPIE | kTSR);  // untouched
  EXPECT_EQ(ms & (kSIE | kSPIE | kSPP | (3u << 13)), kSIE | kSPIE | kSPP | (3u << 13));
  EXPECT_EQ(csr(kSstatus) >> 63, 1u);  // SD: FS is dirty
}

TEST_F(Priv, SieAndSipShowOnlyDelegatedInterrupts) {
  set_csr(kMie, kSSI | kSEI | kMEI);
  set_csr(kMip, kSSI);
  EXPECT_EQ(csr(kSie), 0u);
  EXPECT_EQ(csr(kSip), 0u);
  set_csr(kMideleg, kSSI);
  EXPECT_EQ(csr(kSie), kSSI);
  EXPECT_EQ(csr(kSip), kSSI);
  set_csr(kSie, 0);
  EXPECT_EQ(csr(kMie), kSEI | kMEI);  // only the delegated bit changed
  set_csr(kSip, 0);
  EXPECT_EQ(csr(kMip) & kSSI, 0u);
  set_csr(kMip, 1u << 5);   // STIP via mip
  set_csr(kMideleg, 1u << 5);
  set_csr(kSip, 0);         // STIP is read-only in sip
  EXPECT_EQ(csr(kSip), 1u << 5);
}

TEST_F(Priv, SieWriteIsLoggedAsSieAndMie) {
  ASSERT_EQ(rv_trace_enable(m, 4 * sizeof(rv_trace_record)), RV_OK);
  program({csrrsi(0, kSie, 2)});
  set_csr(kMideleg, kSSI);
  set_priv(1);
  run(1);
  const uint8_t* data = nullptr;
  size_t len = 0;
  ASSERT_EQ(rv_trace_peek(m, &data, &len), RV_OK);
  const auto* r = reinterpret_cast<const rv_trace_record*>(data);
  EXPECT_EQ(r->flags, RV_TF_CSR | RV_TF_CSR2);
  EXPECT_EQ(r->csr, kSie);
  EXPECT_EQ(r->csr2, kMie);
  EXPECT_EQ(r->csr2_value, kSSI);
}

TEST_F(Priv, SstatusWriteIsLoggedAsMstatus) {
  ASSERT_EQ(rv_trace_enable(m, 4 * sizeof(rv_trace_record)), RV_OK);
  program({csrrsi(0, kSstatus, 2)});
  set_priv(1);
  run(1);
  const uint8_t* data = nullptr;
  size_t len = 0;
  ASSERT_EQ(rv_trace_peek(m, &data, &len), RV_OK);
  const auto* r = reinterpret_cast<const rv_trace_record*>(data);
  EXPECT_EQ(r->flags, RV_TF_CSR);
  EXPECT_EQ(r->csr, kMstatus);
}

// ------------------------------------------------- no MMU yet (ADR-016)

TEST_F(Priv, SatpIsReadOnlyBareWithoutMmu) {
  program({csrrw(0, kSatp, 1)});
  set_x(1, (8ull << 60) | 0x1234);  // Sv39: unsupported, so the write is ignored
  set_priv(1);
  run(1);
  EXPECT_EQ(pc(), kRamBase + 4);
  EXPECT_EQ(csr(kSatp), 0u);
}

TEST_F(Priv, SatpIsNotAccessibleFromUser) {
  const uint32_t insn = csrrs(3, kSatp, 0);
  program({insn});
  set_priv(0);
  run(1);
  expect_trap(2, insn, kRamBase);
}

// Priv §3.1.6.5: SFENCE.VMA and satp are permitted in S-mode when TVM = 0,
// and raise illegal-instruction when TVM = 1. SFENCE.VMA is illegal in U-mode.
TEST_F(Priv, SfenceVmaAndTvm) {
  constexpr uint32_t kSfence = 0x12000073u;
  for (uint64_t p : {3u, 1u}) {
    ASSERT_EQ(rv_reset(m), RV_OK);
    program({kSfence});
    set_priv(p);
    run(1);
    EXPECT_EQ(pc(), kRamBase + 4) << "priv " << p;
  }
  struct Case {
    uint32_t insn;
    uint64_t priv;
  } traps[] = {{kSfence, 0}, {kSfence, 1}, {csrrs(3, kSatp, 0), 1}};
  for (const Case& c : traps) {
    ASSERT_EQ(rv_reset(m), RV_OK);
    set_csr(0x305, kTrapVec);
    program({c.insn});
    if (c.priv == 1) set_csr(kMstatus, 1u << 20);  // TVM
    set_priv(c.priv);
    run(1);
    expect_trap(2, c.insn, kRamBase);
  }
  // TVM does not restrict M-mode.
  ASSERT_EQ(rv_reset(m), RV_OK);
  program({kSfence, csrrs(3, kSatp, 0)});
  set_csr(kMstatus, 1u << 20);
  run(2);
  EXPECT_EQ(pc(), kRamBase + 8);
}

TEST_F(Priv, StvecReservedModeWriteIsIgnored) {
  set_csr(kStvec, kSTrapVec | 1);
  set_csr(kStvec, kRamBase | 2);  // ADR-018
  EXPECT_EQ(csr(kStvec), kSTrapVec | 1);
}

}  // namespace
