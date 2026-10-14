#include "state/state.hpp"

#include <cstring>
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

// Invalid TLB entries are stored and hashed as all-zero.
TlbEntry canonical(const TlbEntry& e) { return e.valid ? e : TlbEntry{}; }

// Serializes fields at their declared widths. With buf == nullptr it only
// counts.
class Writer {
 public:
  explicit Writer(uint8_t* buf) : buf_(buf) {}
  template <class T>
  void operator()(const T& v) {
    if constexpr (std::is_enum_v<T>) {
      (*this)(static_cast<std::underlying_type_t<T>>(v));
    } else if constexpr (std::is_same_v<T, bool>) {
      put(static_cast<uint8_t>(v ? 1 : 0));
    } else {
      static_assert(std::is_unsigned_v<T>, "state fields are unsigned integers");
      put(v);
    }
  }
  void tlb(const TlbEntry& e) {
    const TlbEntry c = canonical(e);
    (*this)(c.valid);
    (*this)(c.flags);
    (*this)(c.level);
    (*this)(c.asid);
    (*this)(c.vpn);
    (*this)(c.ppn);
  }
  size_t size() const { return n_; }

 private:
  template <class T>
  void put(T v) {
    if (buf_ != nullptr) std::memcpy(buf_ + n_, &v, sizeof v);
    n_ += sizeof v;
  }
  uint8_t* buf_;
  size_t n_ = 0;
};

// Deserializes what Writer wrote; bools must be 0 or 1.
class Reader {
 public:
  Reader(const uint8_t* buf, size_t len) : buf_(buf), len_(len) {}
  template <class T>
  void operator()(T& v) {
    if constexpr (std::is_enum_v<T>) {
      std::underlying_type_t<T> u{};
      (*this)(u);
      v = static_cast<T>(u);
    } else if constexpr (std::is_same_v<T, bool>) {
      uint8_t b = 0;
      get(b);
      if (b > 1) ok_ = false;
      v = b == 1;
    } else {
      get(v);
    }
  }
  void tlb(TlbEntry& e) {
    (*this)(e.valid);
    (*this)(e.flags);
    (*this)(e.level);
    (*this)(e.asid);
    (*this)(e.vpn);
    (*this)(e.ppn);
  }
  bool ok() const { return ok_ && pos_ == len_; }

 private:
  template <class T>
  void get(T& v) {
    if (len_ - pos_ < sizeof v) {
      ok_ = false;
      return;
    }
    std::memcpy(&v, buf_ + pos_, sizeof v);
    pos_ += sizeof v;
  }
  const uint8_t* buf_;
  size_t len_;
  size_t pos_ = 0;
  bool ok_ = true;
};

constexpr char kMagic[8] = {'R', 'V', 'C', 'O', 'R', 'E', 0, 0};
constexpr size_t kHeader = 8 + 4 + 4;
constexpr size_t kTrailer = 8;

size_t payload_size(const rv_machine& m) {
  Writer w(nullptr);
  visit_core(m, w);
  return w.size();
}

}  // namespace

size_t snapshot_size(const rv_machine& m) { return kHeader + payload_size(m) + kTrailer; }

int snapshot_save(const rv_machine& m, uint8_t* buf, size_t len) {
  const size_t payload = payload_size(m);
  if (len < kHeader + payload + kTrailer) return RV_E_BUFSZ;
  std::memcpy(buf, kMagic, 8);
  const uint32_t fmt = kCoreFormat, plen = static_cast<uint32_t>(payload);
  std::memcpy(buf + 8, &fmt, 4);
  std::memcpy(buf + 12, &plen, 4);
  Writer w(buf + kHeader);
  visit_core(m, w);
  const uint64_t sum = XXH3_64bits(buf, kHeader + payload);
  std::memcpy(buf + kHeader + payload, &sum, 8);
  return RV_OK;
}

int snapshot_load(rv_machine& m, const uint8_t* buf, size_t len) {
  if (len < kHeader + kTrailer || std::memcmp(buf, kMagic, 8) != 0) return RV_E_SNAP_BAD;
  uint32_t fmt = 0, plen = 0;
  std::memcpy(&fmt, buf + 8, 4);
  std::memcpy(&plen, buf + 12, 4);
  if (fmt != kCoreFormat) return RV_E_SNAP_VER;
  if (len != kHeader + plen + kTrailer) return RV_E_SNAP_BAD;
  uint64_t sum = 0;
  std::memcpy(&sum, buf + kHeader + plen, 8);
  if (sum != XXH3_64bits(buf, kHeader + plen)) return RV_E_SNAP_BAD;

  // Parse into a scratch machine first: a malformed snapshot, or one taken
  // on a differently configured machine, must leave m untouched.
  rv_machine tmp;
  Reader check(buf + kHeader, plen);
  visit_core(tmp, check);
  if (!check.ok()) return RV_E_SNAP_BAD;
  const rv_config &a = tmp.cfg, &b = m.cfg;
  if (a.ram_base != b.ram_base || a.ram_size != b.ram_size || a.clint_base != b.clint_base ||
      a.reset_pc != b.reset_pc || a.mhartid != b.mhartid || a.steps_per_tick != b.steps_per_tick ||
      a.htif_tohost != b.htif_tohost || a.htif_fromhost != b.htif_fromhost || a.flags != b.flags) {
    return RV_E_INVAL;
  }
  Reader r(buf + kHeader, plen);
  visit_core(m, r);
  m.exit = rv_exit{};
  m.trace_len = 0;
  return RV_OK;
}

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
