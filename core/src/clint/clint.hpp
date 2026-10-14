// Core-local interruptor (CLINT): msip, mtimecmp and mtime for hart 0, in the
// SiFive layout that QEMU virt, Spike and OpenSBI's generic platform use.
// It lives in the core because guests read mtime constantly (ADR-001).
#pragma once

#include <cstddef>
#include <cstdint>

struct rv_machine;

namespace rv {

constexpr uint64_t kClintSize = 0x10000;
constexpr uint64_t kClintMsip = 0x0000;      // 32-bit; bit 0 drives mip.MSIP
constexpr uint64_t kClintMtimecmp = 0x4000;  // 64-bit
constexpr uint64_t kClintMtime = 0xbff8;     // 64-bit

// Accesses of 4 or 8 bytes at a naturally aligned offset inside a register
// are supported; the halves of mtimecmp/mtime can be accessed separately.
// Returns false (access fault) for other sizes. Unmapped offsets read as
// zero and ignore writes.
bool clint_load(const rv_machine* m, uint64_t paddr, size_t len, uint64_t& out);
bool clint_store(rv_machine* m, uint64_t paddr, size_t len, uint64_t value);

}  // namespace rv
