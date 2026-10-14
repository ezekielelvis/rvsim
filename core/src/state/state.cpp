#include "state/state.hpp"

#include <type_traits>

#define XXH_INLINE_ALL
#include "xxhash.h"

namespace rv::state {
namespace {

// Feeds each field to XXH3 as little-endian bytes of its declared width
// (hosts are little-endian; see machine.hpp).
class Hasher {
 public:
  Hasher() { XXH3_64bits_reset(&st_); }

  template <class T>
  void operator()(const T& v) {
    if constexpr (std::is_enum_v<T>) {
      (*this)(static_cast<std::underlying_type_t<T>>(v));
    } else if constexpr (std::is_same_v<T, bool>) {
      const uint8_t b = v ? 1 : 0;
      XXH3_64bits_update(&st_, &b, 1);
    } else {
      static_assert(std::is_unsigned_v<T>, "state fields are unsigned integers");
      XXH3_64bits_update(&st_, &v, sizeof v);
    }
  }

  // Invalid entries are hashed as all-zero: their stale fields are not state.
  void tlb(const TlbEntry& e) {
    const TlbEntry canon = e.valid ? e : TlbEntry{};
    (*this)(canon.valid);
    (*this)(canon.flags);
    (*this)(canon.level);
    (*this)(canon.asid);
    (*this)(canon.vpn);
    (*this)(canon.ppn);
  }

  uint64_t digest() { return XXH3_64bits_digest(&st_); }

 private:
  XXH3_state_t st_;
};

}  // namespace

uint64_t hash_core(const rv_machine& m) {
  Hasher h;
  h(kCoreFormat);
  visit_core(m, h);
  return h.digest();
}

uint64_t hash_ram(const rv_machine& m) { return XXH3_64bits(m.ram, m.cfg.ram_size); }

uint64_t hash_both(const rv_machine& m) {
  const uint64_t pair[2] = {hash_core(m), hash_ram(m)};
  return XXH3_64bits(pair, sizeof pair);
}

}  // namespace rv::state
