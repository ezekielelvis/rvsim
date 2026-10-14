// Sv39 translation, permission checks, A/D handling and the TLB
// (Priv §4.3-4.4).
#include <cstring>

#include "asm.hpp"

namespace {

using namespace asm_;

// PTE bits.
constexpr uint64_t V = 1, R = 2, W = 4, X = 8, U = 16, G = 32, A = 64, D = 128;
constexpr uint64_t kSatp = 0x180, kMstatus = 0x300, kMedeleg = 0x302, kMenvcfg = 0x30a;
constexpr uint64_t kSUM = 1u << 18, kMXR = 1u << 19, kMPRV = 1u << 17;
constexpr uint64_t kADUE = 1ull << 61;
constexpr uint64_t kSfence = 0x12000073;  // sfence.vma x0, x0

// Physical layout inside the fixture's 1 MiB of RAM.
constexpr uint64_t kTables = kRamBase + 0x40000;
constexpr uint64_t kData = kRamBase + 0x30000;
constexpr uint64_t kData2 = kRamBase + 0x31000;

class Mmu : public Cpu {
 protected:
  void SetUp() override {
    Cpu::SetUp();
    next_table_ = kTables + 0x1000;
    root_ = kTables;
    // Code and trap vector, identity-mapped for S-mode.
    map(kRamBase, kRamBase, R | X | A | D);
    map(kTrapVec, kTrapVec, R | X | A | D);
  }

  // Maps vaddr to paddr with a leaf at `level` (0: 4 KiB, 1: 2 MiB, 2: 1 GiB).
  void map(uint64_t va, uint64_t pa, uint64_t flags, int level = 0) {
    uint64_t table = root_;
    for (int l = 2; l > level; --l) {
      const uint64_t slot = table + ((va >> (12 + 9 * l)) & 0x1ff) * 8;
      uint64_t e = mem<uint64_t>(slot);
      if ((e & V) == 0) {
        e = ((next_table_ >> 12) << 10) | V;
        set_mem<uint64_t>(slot, e);
        next_table_ += 0x1000;
      }
      table = (e >> 10) << 12;
    }
    set_mem<uint64_t>(table + ((va >> (12 + 9 * level)) & 0x1ff) * 8, ((pa >> 12) << 10) | flags | V);
  }

  // The address of the leaf (or other) PTE for va at `level`, after map().
  uint64_t pte_addr(uint64_t va, int level = 0) {
    uint64_t table = root_;
    for (int l = 2; l > level; --l) {
      const uint64_t e = mem<uint64_t>(table + ((va >> (12 + 9 * l)) & 0x1ff) * 8);
      table = (e >> 10) << 12;
    }
    return table + ((va >> (12 + 9 * level)) & 0x1ff) * 8;
  }

  void enable(uint64_t asid = 0) { set_csr(kSatp, (8ull << 60) | (asid << 44) | (root_ >> 12)); }
  void to_s() { ASSERT_EQ(rv_set_reg(m, RV_REG_PRIV, 1), RV_OK); }
  void to_u() { ASSERT_EQ(rv_set_reg(m, RV_REG_PRIV, 0), RV_OK); }

  uint64_t root_ = 0;
  uint64_t next_table_ = 0;
};

TEST_F(Mmu, BareModeDoesNotTranslate) {
  program({load(3, 3, 1, 0)});
  set_mem<uint64_t>(kData, 42);
  set_x(1, kData);
  to_s();
  run(1);
  EXPECT_EQ(x(3), 42u);
}

TEST_F(Mmu, MachineModeIgnoresSatp) {
  program({load(3, 3, 1, 0)});
  set_mem<uint64_t>(kData, 42);
  set_x(1, kData);
  enable();
  run(1);
  EXPECT_EQ(x(3), 42u);
}

TEST_F(Mmu, FourKiBMapping) {
  map(0x1000, kData, R | W | A | D);
  program({load(3, 3, 1, 8), store(3, 2, 1, 16)});
  set_mem<uint64_t>(kData + 8, 42);
  set_x(1, 0x1000);
  set_x(2, 7);
  enable();
  to_s();
  run(2);
  EXPECT_EQ(x(3), 42u);
  EXPECT_EQ(mem<uint64_t>(kData + 16), 7u);
}

struct FaultCase {
  const char* name;
  uint64_t flags;   // leaf flags for VA 0x1000
  int priv;         // 1 = S, 0 = U
  uint32_t insn;    // accesses 0(x1)
  uint64_t cause;
  uint64_t mstatus; // extra mstatus bits
};

class MmuFault : public Mmu, public ::testing::WithParamInterface<FaultCase> {};

TEST_P(MmuFault, RaisesPageFault) {
  const FaultCase& c = GetParam();
  map(0x1000, kData, c.flags);
  if (c.priv == 0) map(kRamBase, kRamBase, R | X | U | A | D);  // let U run the code
  program({c.insn});
  set_x(1, 0x1000);
  set_csr(kMstatus, c.mstatus);
  enable();
  c.priv == 0 ? to_u() : to_s();
  run(1);
  expect_trap(c.cause, 0x1000, kRamBase);
}

constexpr uint32_t kLd = load(3, 3, 1, 0), kSd = store(3, 2, 1, 0);

// clang-format off
INSTANTIATE_TEST_SUITE_P(Sv39, MmuFault, ::testing::Values(
  FaultCase{"store_read_only",       R | A | D,       1, kSd, 15, 0},
  FaultCase{"load_no_read",          X | A,           1, kLd, 13, 0},
  FaultCase{"user_page_from_s",      R | W | U | A | D, 1, kLd, 13, 0},
  FaultCase{"s_page_from_user",      R | W | A | D,   0, kLd, 13, 0},
  FaultCase{"svade_a_clear",         R | W,           1, kLd, 13, 0},
  FaultCase{"svade_d_clear_store",   R | W | A,       1, kSd, 15, 0}),
  [](const ::testing::TestParamInfo<FaultCase>& p) { return p.param.name; });
// clang-format on

TEST_F(Mmu, InvalidPteFaults) {
  map(0x1000, kData, R | A);
  set_mem<uint64_t>(pte_addr(0x1000), mem<uint64_t>(pte_addr(0x1000)) & ~V);
  program({kLd});
  set_x(1, 0x1000);
  enable();
  to_s();
  run(1);
  expect_trap(13, 0x1000, kRamBase);
}

TEST_F(Mmu, WriteWithoutReadIsReserved) {
  map(0x1000, kData, W | A | D);
  program({kSd});
  set_x(1, 0x1000);
  enable();
  to_s();
  run(1);
  expect_trap(15, 0x1000, kRamBase);
}

TEST_F(Mmu, ReservedHighBitsFault) {
  map(0x1000, kData, R | A);
  set_mem<uint64_t>(pte_addr(0x1000), mem<uint64_t>(pte_addr(0x1000)) | (1ull << 54));
  program({kLd});
  set_x(1, 0x1000);
  enable();
  to_s();
  run(1);
  expect_trap(13, 0x1000, kRamBase);
}

TEST_F(Mmu, NonLeafWithReservedBitsFaults) {
  map(0x1000, kData, R | A);
  const uint64_t mid = pte_addr(0x1000, 1);  // level-1 pointer
  set_mem<uint64_t>(mid, mem<uint64_t>(mid) | A);
  program({kLd});
  set_x(1, 0x1000);
  enable();
  to_s();
  run(1);
  expect_trap(13, 0x1000, kRamBase);
}

TEST_F(Mmu, PointerAtLastLevelFaults) {
  map(0x1000, kData, R | A);
  set_mem<uint64_t>(pte_addr(0x1000), ((kData >> 12) << 10) | V);  // R=W=X=0 at level 0
  program({kLd});
  set_x(1, 0x1000);
  enable();
  to_s();
  run(1);
  expect_trap(13, 0x1000, kRamBase);
}

TEST_F(Mmu, Superpages) {
  // 2 MiB leaf at VA 0x4000_0000 -> PA RAM base (2 MiB aligned).
  map(0x4000'0000, kRamBase, R | W | A | D, 1);
  // 1 GiB leaf at VA 0xC000_0000 -> PA 0x8000_0000 (1 GiB aligned).
  map(0xC000'0000, kRamBase, R | W | A | D, 2);
  program({load(3, 3, 1, 0), load(3, 4, 2, 0)});
  set_mem<uint64_t>(kData, 77);
  set_x(1, 0x4000'0000 + (kData - kRamBase));
  set_x(2, 0xC000'0000 + (kData - kRamBase));
  enable();
  to_s();
  run(2);
  EXPECT_EQ(x(3), 77u);
  EXPECT_EQ(x(4), 77u);
}

TEST_F(Mmu, MisalignedSuperpageFaults) {
  map(0x4000'0000, kRamBase + 0x1000, R | A, 1);  // PPN[0] != 0
  program({kLd});
  set_x(1, 0x4000'0000);
  enable();
  to_s();
  run(1);
  expect_trap(13, 0x4000'0000, kRamBase);
}

TEST_F(Mmu, SumAllowsSupervisorDataAccessToUserPages) {
  map(0x1000, kData, R | W | U | A | D);
  program({kLd, kSd});
  set_mem<uint64_t>(kData, 5);
  set_x(1, 0x1000);
  set_x(2, 6);
  set_csr(kMstatus, kSUM);
  enable();
  to_s();
  run(2);
  EXPECT_EQ(x(3), 5u);
  EXPECT_EQ(mem<uint64_t>(kData), 6u);
}

TEST_F(Mmu, SupervisorCannotExecuteUserPagesEvenWithSum) {
  map(kRamBase, kRamBase, R | X | U | A | D);
  program({kNop});
  set_csr(kMstatus, kSUM);
  enable();
  to_s();
  run(1);
  expect_trap(12, kRamBase, kRamBase);
}

TEST_F(Mmu, MxrMakesExecutablePagesReadable) {
  map(0x1000, kData, X | A);
  program({kLd});
  set_mem<uint64_t>(kData, 9);
  set_x(1, 0x1000);
  set_csr(kMstatus, kMXR);
  enable();
  to_s();
  run(1);
  EXPECT_EQ(x(3), 9u);
}

TEST_F(Mmu, NonCanonicalAddressFaults) {
  program({kLd});
  set_x(1, 0x40'0000'0000);  // bit 38 set, bits 63:39 clear
  enable();
  to_s();
  run(1);
  expect_trap(13, 0x40'0000'0000, kRamBase);
}

TEST_F(Mmu, SvaduSetsAccessedAndDirty) {
  map(0x1000, kData, R | W);
  program({kLd, kSd});
  set_x(1, 0x1000);
  set_csr(kMenvcfg, kADUE);
  enable();
  to_s();
  run(1);
  EXPECT_EQ(mem<uint64_t>(pte_addr(0x1000)) & (A | D), A);
  run(1);
  EXPECT_EQ(mem<uint64_t>(pte_addr(0x1000)) & (A | D), A | D);
  EXPECT_EQ(pc(), kRamBase + 8);
}

TEST_F(Mmu, PteOutsideRamIsAccessFault) {
  program({kLd});
  set_x(1, 0x1000);
  set_csr(kSatp, (8ull << 60) | (0x1000'0000 >> 12));  // root in MMIO space
  to_s();
  // The code page itself cannot be fetched either, so fetch faults first.
  run(1);
  expect_trap(1, kRamBase, kRamBase);
}

TEST_F(Mmu, MprvTranslatesMachineLoads) {
  map(0x1000, kData, R | A);
  program({kLd});
  set_mem<uint64_t>(kData, 31);
  set_x(1, 0x1000);
  set_csr(kMstatus, kMPRV | (1u << 11));  // MPP = S
  enable();
  run(1);  // M-mode: fetch is untranslated, the load is translated as S
  EXPECT_EQ(x(3), 31u);
}

TEST_F(Mmu, StaleTlbUntilSfence) {
  map(0x1000, kData, R | A);
  program({kLd, kLd, kSfence, kLd});
  set_mem<uint64_t>(kData, 1);
  set_mem<uint64_t>(kData2, 2);
  set_x(1, 0x1000);
  enable();
  to_s();
  run(1);
  EXPECT_EQ(x(3), 1u);
  map(0x1000, kData2, R | A);  // remap without a fence
  run(1);
  EXPECT_EQ(x(3), 1u);  // the TLB still holds the old translation
  run(2);
  EXPECT_EQ(x(3), 2u);
}

TEST_F(Mmu, SfenceByAddressFlushesWholeSuperpage) {
  map(0x4000'0000, kRamBase, R | A, 1);
  const uint32_t sfence_a2 = 0x12000073u | (12u << 15);  // sfence.vma a2, x0
  program({load(3, 3, 1, 0), sfence_a2, load(3, 3, 1, 0)});
  set_mem<uint64_t>(kData, 1);
  set_x(1, 0x4000'0000 + (kData - kRamBase));
  set_x(12, 0x4000'0000);  // a different 4 KiB page of the same superpage
  enable();
  to_s();
  run(1);
  EXPECT_EQ(x(3), 1u);
  set_mem<uint64_t>(pte_addr(0x4000'0000, 1), 0);  // unmap
  run(2);
  expect_trap(13, x(1), kRamBase + 8);
}

TEST_F(Mmu, AsidFlushKeepsGlobalMappings) {
  map(0x1000, kData, R | A | G);
  map(0x2000, kData2, R | A);
  const uint32_t sfence_asid = 0x12000073u | (13u << 20);  // sfence.vma x0, a3
  program({kLd, load(3, 4, 2, 0), sfence_asid, kLd, load(3, 4, 2, 0)});
  set_mem<uint64_t>(kData, 1);
  set_mem<uint64_t>(kData2, 2);
  set_x(1, 0x1000);
  set_x(2, 0x2000);
  set_x(13, 5);
  enable(5);
  to_s();
  run(2);
  map(0x1000, kData2, R | A | G);  // both remapped behind the TLB's back
  map(0x2000, kData, R | A);
  run(3);
  EXPECT_EQ(x(3), 1u);  // global: still cached
  EXPECT_EQ(x(4), 1u);  // ASID 5: flushed, so the new mapping is used
}

TEST_F(Mmu, SatpWriteFlushesTlb) {
  map(0x1000, kData, R | A);
  program({kLd, csrrw(0, kSatp, 5), kLd});
  set_mem<uint64_t>(kData, 1);
  set_mem<uint64_t>(kData2, 2);
  set_x(1, 0x1000);
  enable();
  set_x(5, (8ull << 60) | (1ull << 44) | (root_ >> 12));  // same table, new ASID
  to_s();
  run(1);
  map(0x1000, kData2, R | A);
  run(2);
  EXPECT_EQ(x(3), 2u);
}

TEST_F(Mmu, FetchStraddlingPagesFaultsOnSecondPage) {
  // A 32-bit instruction in the last two bytes of a mapped page; the next
  // page is unmapped. mtval names the second page, mepc the instruction.
  map(0x10000, kData, R | X | A);
  const uint64_t pc = 0x10000 + 0xffe;
  set_mem<uint16_t>(kData + 0xffe, 0x0013);  // low half of addi x0, x0, 0
  program({jalr(0, 1, 0)});
  set_x(1, pc);
  enable();
  to_s();
  run(2);
  expect_trap(12, pc + 2, pc);
}

TEST_F(Mmu, FetchStraddlingMappedPagesWorks) {
  map(0x10000, kData, R | X | A);
  map(0x11000, kData2, R | X | A);  // physically discontiguous
  const uint32_t li = addi(5, 0, 42);
  set_mem<uint16_t>(kData + 0xffe, static_cast<uint16_t>(li));
  set_mem<uint16_t>(kData2, static_cast<uint16_t>(li >> 16));
  program({jalr(0, 1, 0)});
  set_x(1, 0x10ffe);
  enable();
  to_s();
  run(2);
  EXPECT_EQ(x(5), 42u);
}

TEST_F(Mmu, PageFaultsCanBeDelegated) {
  program({kLd});
  set_x(1, 0x1000);
  set_csr(kMedeleg, 1u << 13);
  set_csr(0x105, kRamBase + 0x9000);  // stvec
  map(kRamBase + 0x9000, kRamBase + 0x9000, R | X | A);
  enable();
  to_s();
  run(1);
  EXPECT_EQ(x(RV_REG_PRIV), 1u);
  EXPECT_EQ(csr(0x142), 13u);
  EXPECT_EQ(csr(0x143), 0x1000u);
}

TEST_F(Mmu, DebugTranslateHasNoSideEffects) {
  map(0x1000, kData, R | W);  // A and D clear
  enable();
  to_s();
  uint64_t pa = 0;
  ASSERT_EQ(rv_translate(m, 0x1008, RV_ACC_WRITE, &pa), RV_OK);
  EXPECT_EQ(pa, kData + 8);
  EXPECT_EQ(mem<uint64_t>(pte_addr(0x1000)) & (A | D), 0u);
  EXPECT_EQ(rv_translate(m, 0x5000, RV_ACC_READ, &pa), RV_E_RANGE);
  EXPECT_EQ(rv_translate(m, 0x1000, RV_ACC_EXEC, &pa), RV_E_RANGE);  // not executable
}

TEST_F(Mmu, MmioThroughAMapping) {
  map(0x7000, 0x1000'0000, R | W | A | D);
  program({load(4, 3, 1, 5)});
  set_x(1, 0x7000);
  enable();
  to_s();
  const rv_exit& e = run(1);
  EXPECT_EQ(e.reason, RV_EXIT_MMIO_READ);
  EXPECT_EQ(e.addr, 0x1000'0005u);  // the physical address
}

TEST_F(Mmu, AtomicsTranslateAndFaultAsStores) {
  map(0x1000, kData, R | A);  // read-only
  const uint32_t amoadd_d = (2u << 20) | (1u << 15) | (3u << 12) | (3u << 7) | 0b0101111;
  const uint32_t lr_d = (0b00010u << 27) | (1u << 15) | (3u << 12) | (3u << 7) | 0b0101111;
  program({lr_d, amoadd_d});
  set_mem<uint64_t>(kData, 11);
  set_x(1, 0x1000);
  enable();
  to_s();
  run(1);
  EXPECT_EQ(x(3), 11u);  // LR only needs read permission
  run(1);
  expect_trap(15, 0x1000, kRamBase + 4);
}

class MmuMisalignedOK : public Mmu {
 protected:
  void SetUp() override {
    flags_ = RV_CFG_MISALIGNED_OK;
    Mmu::SetUp();
  }
};

TEST_F(MmuMisalignedOK, AccessAcrossPagesUsesBothTranslations) {
  map(0x1000, kData2, R | W | A | D);  // VA 0x1000 -> data2
  map(0x2000, kData, R | W | A | D);   // VA 0x2000 -> data (physically before data2)
  set_mem<uint8_t>(kData2 + 0xffe, 0x11);
  set_mem<uint8_t>(kData2 + 0xfff, 0x22);
  set_mem<uint8_t>(kData + 0, 0x33);
  set_mem<uint8_t>(kData + 1, 0x44);
  program({load(2, 3, 1, 0), store(2, 2, 1, 0)});
  set_x(1, 0x1ffe);
  set_x(2, 0x55667788);
  enable();
  to_s();
  run(1);
  EXPECT_EQ(x(3), 0x44332211u);
  run(1);
  EXPECT_EQ(mem<uint8_t>(kData2 + 0xffe), 0x88);
  EXPECT_EQ(mem<uint8_t>(kData + 1), 0x55);
}

TEST_F(MmuMisalignedOK, AcrossPagesFaultsOnSecondPageWithoutWriting) {
  map(0x1000, kData2, R | W | A | D);  // VA 0x2000 is unmapped
  program({store(2, 2, 1, 0)});
  set_x(1, 0x1ffe);
  set_x(2, 0x55667788);
  enable();
  to_s();
  run(1);
  expect_trap(15, 0x2000, kRamBase);
  EXPECT_EQ(mem<uint16_t>(kData2 + 0xffe), 0u);  // nothing written
}

}  // namespace
