// rv_state_hash: what it covers and what it ignores.
#include <gtest/gtest.h>

#include <functional>
#include <vector>

#include "asm.hpp"
#include "machine.hpp"

namespace {


class StateHash : public Cpu {
 protected:
  uint64_t hash(uint32_t what) {
    uint64_t h = 0;
    EXPECT_EQ(rv_state_hash(m, what, &h), RV_OK);
    return h;
  }
  uint64_t core() { return hash(RV_HASH_CORE); }
  uint64_t ram() { return hash(RV_HASH_RAM); }
};

TEST_F(StateHash, IsStableAndRepeatable) {
  const uint64_t c = core(), r = ram();
  EXPECT_EQ(core(), c);
  EXPECT_EQ(ram(), r);
  EXPECT_NE(c, r);
}

TEST_F(StateHash, MatchesAnIdenticalMachine) {
  rv_machine* other = nullptr;
  rv_config c = m->cfg;
  ASSERT_EQ(rv_create(&c, &other), RV_OK);
  ASSERT_EQ(rv_set_reg(other, RV_REG_CSR + 0x305, kTrapVec), RV_OK);
  uint64_t a = 0, b = 0;
  ASSERT_EQ(rv_state_hash(other, RV_HASH_CORE | RV_HASH_RAM, &b), RV_OK);
  ASSERT_EQ(rv_state_hash(m, RV_HASH_CORE | RV_HASH_RAM, &a), RV_OK);
  EXPECT_EQ(a, b);
  rv_destroy(other);
}

TEST_F(StateHash, CoreStateChangesTheCoreHashOnly) {
  struct Change {
    const char* name;
    std::function<void()> apply;
  };
  const Change changes[] = {
      {"gpr", [this] { set_x(7, 1); }},
      {"pc", [this] { set_x(RV_REG_PC, kRamBase + 4); }},
      {"priv", [this] { set_x(RV_REG_PRIV, 1); }},
      {"csr", [this] { set_csr(0x340, 5); }},  // mscratch
      {"satp", [this] { set_csr(0x180, 8ull << 60); }},
      {"mtimecmp", [this] { m->mtimecmp = 99; }},
      {"steps", [this] { m->steps++; }},
      {"reservation", [this] { m->reservation = kData; }},
      {"irq lines", [this] { ASSERT_EQ(rv_set_irq_lines(m, 1u << 9), RV_OK); }},
      {"valid tlb entry", [this] { m->tlb[3] = TlbEntry{true, 0xcf, 0, 0, 3, 0x80000}; }},
      {"pending mmio", [this] { m->mmio.pending = true; }},
  };
  for (const auto& ch : changes) {
    TearDown();
    SetUp();
    const uint64_t c = core(), r = ram();
    ch.apply();
    EXPECT_NE(core(), c) << ch.name;
    EXPECT_EQ(ram(), r) << ch.name;
  }
}

TEST_F(StateHash, RamChangesTheRamHashOnly) {
  const uint64_t c = core(), r = ram();
  set_mem<uint8_t>(kRamBase + kRamSize - 1, 1);
  EXPECT_EQ(core(), c);
  EXPECT_NE(ram(), r);
}

TEST_F(StateHash, IgnoresStaleFieldsOfInvalidTlbEntries) {
  const uint64_t c = core();
  m->tlb[7] = TlbEntry{false, 0xff, 2, 9, 0x1234, 0x5678};
  EXPECT_EQ(core(), c);
}

TEST_F(StateHash, IgnoresOutputs) {
  const uint64_t c = core();
  m->exit.reason = RV_EXIT_HALT;
  m->exit.pc = 0x1234;
  ASSERT_EQ(rv_trace_enable(m, 4096), RV_OK);
  EXPECT_EQ(core(), c);
}

TEST_F(StateHash, CombinedHashCoversBoth) {
  const uint64_t both = hash(RV_HASH_CORE | RV_HASH_RAM);
  set_mem<uint8_t>(kData, 1);
  const uint64_t after_ram = hash(RV_HASH_CORE | RV_HASH_RAM);
  set_x(5, 1);
  const uint64_t after_core = hash(RV_HASH_CORE | RV_HASH_RAM);
  EXPECT_NE(both, after_ram);
  EXPECT_NE(after_ram, after_core);
}

TEST_F(StateHash, RejectsBadArguments) {
  uint64_t h = 0;
  EXPECT_EQ(rv_state_hash(nullptr, RV_HASH_CORE, &h), RV_E_INVAL);
  EXPECT_EQ(rv_state_hash(m, RV_HASH_CORE, nullptr), RV_E_INVAL);
  EXPECT_EQ(rv_state_hash(m, 0, &h), RV_E_INVAL);
  EXPECT_EQ(rv_state_hash(m, 4, &h), RV_E_INVAL);
}

}  // namespace

namespace {

using namespace asm_;

class Snapshot : public StateHash {
 protected:
  std::vector<uint8_t> save() {
    size_t n = 0;
    EXPECT_EQ(rv_core_snapshot_size(m, &n), RV_OK);
    std::vector<uint8_t> buf(n);
    EXPECT_EQ(rv_core_snapshot_save(m, buf.data(), buf.size()), RV_OK);
    return buf;
  }
  // A second machine with the same configuration and the same RAM.
  rv_machine* twin() {
    rv_machine* o = nullptr;
    rv_config c = m->cfg;
    EXPECT_EQ(rv_create(&c, &o), RV_OK);
    std::vector<uint8_t> ram(kRamSize);
    EXPECT_EQ(rv_read_phys(m, kRamBase, ram.data(), ram.size()), RV_OK);
    EXPECT_EQ(rv_write_phys(o, kRamBase, ram.data(), ram.size()), RV_OK);
    return o;
  }
};

TEST_F(Snapshot, RestoredMachineContinuesIdentically) {
  // A loop that counts in x5 and stores to memory, with traps enabled.
  program({addi(5, 5, 1), store(3, 5, 6, 0), addi(6, 6, 8), jal(0, -12)});
  set_x(6, kData);
  set_csr(0x340, 0x1234);  // mscratch
  run(1000);
  const auto snap = save();
  rv_machine* o = twin();
  ASSERT_EQ(rv_core_snapshot_load(o, snap.data(), snap.size()), RV_OK);
  uint64_t a = 0, b = 0;
  ASSERT_EQ(rv_state_hash(m, RV_HASH_CORE | RV_HASH_RAM, &a), RV_OK);
  ASSERT_EQ(rv_state_hash(o, RV_HASH_CORE | RV_HASH_RAM, &b), RV_OK);
  EXPECT_EQ(a, b);
  run(5000);
  ASSERT_EQ(rv_run(o, 5000), RV_OK);
  ASSERT_EQ(rv_state_hash(m, RV_HASH_CORE | RV_HASH_RAM, &a), RV_OK);
  ASSERT_EQ(rv_state_hash(o, RV_HASH_CORE | RV_HASH_RAM, &b), RV_OK);
  EXPECT_EQ(a, b);
  rv_destroy(o);
}

TEST_F(Snapshot, CarriesTlbReservationAndPendingMmio) {
  m->tlb[9] = TlbEntry{true, 0xcf, 1, 7, 9, 0x80200};
  m->reservation = kData;
  m->mmio = {true, false, true, false, 4, 0x1000'0000, 42};
  const auto snap = save();
  rv_machine* o = twin();
  ASSERT_EQ(rv_core_snapshot_load(o, snap.data(), snap.size()), RV_OK);
  EXPECT_EQ(o->tlb[9].ppn, 0x80200u);
  EXPECT_EQ(o->reservation, kData);
  EXPECT_TRUE(o->mmio.pending);
  EXPECT_EQ(o->mmio.value, 42u);
  rv_destroy(o);
}

TEST_F(Snapshot, RejectsDamageAndLeavesTheMachineAlone) {
  set_x(5, 77);
  auto snap = save();
  rv_machine* o = twin();
  const uint64_t before = [&] {
    uint64_t h = 0;
    EXPECT_EQ(rv_state_hash(o, RV_HASH_CORE, &h), RV_OK);
    return h;
  }();
  auto bad = snap;
  bad[40] ^= 1;
  EXPECT_EQ(rv_core_snapshot_load(o, bad.data(), bad.size()), RV_E_SNAP_BAD);
  EXPECT_EQ(rv_core_snapshot_load(o, snap.data(), snap.size() - 1), RV_E_SNAP_BAD);
  bad = snap;
  bad[0] = 'X';
  EXPECT_EQ(rv_core_snapshot_load(o, bad.data(), bad.size()), RV_E_SNAP_BAD);
  bad = snap;
  bad[8] = 99;  // format version
  EXPECT_EQ(rv_core_snapshot_load(o, bad.data(), bad.size()), RV_E_SNAP_VER);
  uint64_t after = 0;
  ASSERT_EQ(rv_state_hash(o, RV_HASH_CORE, &after), RV_OK);
  EXPECT_EQ(after, before);
  std::vector<uint8_t> small(snap.size() - 1);
  EXPECT_EQ(rv_core_snapshot_save(m, small.data(), small.size()), RV_E_BUFSZ);
  rv_destroy(o);
}

TEST_F(Snapshot, RejectsADifferentlyConfiguredMachine) {
  const auto snap = save();
  rv_machine* o = nullptr;
  rv_config c = m->cfg;
  c.ram_size = 2 * kRamSize;
  ASSERT_EQ(rv_create(&c, &o), RV_OK);
  EXPECT_EQ(rv_core_snapshot_load(o, snap.data(), snap.size()), RV_E_INVAL);
  rv_destroy(o);
}

TEST_F(Snapshot, LoadClearsPoison) {
  const auto snap = save();
  m->poisoned = true;
  ASSERT_EQ(rv_core_snapshot_load(m, snap.data(), snap.size()), RV_OK);
  EXPECT_FALSE(m->poisoned);
  EXPECT_EQ(rv_run(m, 1), RV_OK);
}

}  // namespace
