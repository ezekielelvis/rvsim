#include "decode/decode.hpp"

namespace rv {
namespace {

constexpr uint32_t bits(uint32_t x, unsigned hi, unsigned lo) {
  return (x >> lo) & ((1u << (hi - lo + 1)) - 1);
}

constexpr int64_t sext(uint64_t v, unsigned width) {
  const unsigned shift = 64 - width;
  return static_cast<int64_t>(v << shift) >> shift;
}

// Immediate formats, Unpriv §2.3 Figure 2.4.
constexpr int64_t imm_i(uint32_t r) { return sext(r >> 20, 12); }
constexpr int64_t imm_s(uint32_t r) { return sext((bits(r, 31, 25) << 5) | bits(r, 11, 7), 12); }
constexpr int64_t imm_b(uint32_t r) {
  return sext((bits(r, 31, 31) << 12) | (bits(r, 7, 7) << 11) | (bits(r, 30, 25) << 5) |
                  (bits(r, 11, 8) << 1),
              13);
}
constexpr int64_t imm_u(uint32_t r) { return sext(r & 0xfffff000u, 32); }
constexpr int64_t imm_j(uint32_t r) {
  return sext((bits(r, 31, 31) << 20) | (bits(r, 19, 12) << 12) | (bits(r, 20, 20) << 11) |
                  (bits(r, 30, 21) << 1),
              21);
}

constexpr uint32_t kOpLoad = 0b0000011;
constexpr uint32_t kOpMiscMem = 0b0001111;
constexpr uint32_t kOpImm = 0b0010011;
constexpr uint32_t kOpAuipc = 0b0010111;
constexpr uint32_t kOpImm32 = 0b0011011;
constexpr uint32_t kOpStore = 0b0100011;
constexpr uint32_t kOpOp = 0b0110011;
constexpr uint32_t kOpLui = 0b0110111;
constexpr uint32_t kOpOp32 = 0b0111011;
constexpr uint32_t kOpBranch = 0b1100011;
constexpr uint32_t kOpJalr = 0b1100111;
constexpr uint32_t kOpJal = 0b1101111;
constexpr uint32_t kOpSystem = 0b1110011;

Op decode_op(uint32_t r) {
  const uint32_t f3 = bits(r, 14, 12);
  const uint32_t f7 = bits(r, 31, 25);
  switch (bits(r, 6, 0)) {
    case kOpLui: return Op::Lui;
    case kOpAuipc: return Op::Auipc;
    case kOpJal: return Op::Jal;
    case kOpJalr: return f3 == 0 ? Op::Jalr : Op::Illegal;
    case kOpBranch: {
      constexpr Op t[8] = {Op::Beq, Op::Bne, Op::Illegal, Op::Illegal,
                           Op::Blt, Op::Bge, Op::Bltu,    Op::Bgeu};
      return t[f3];
    }
    case kOpLoad: {
      constexpr Op t[8] = {Op::Lb, Op::Lh, Op::Lw, Op::Ld, Op::Lbu, Op::Lhu, Op::Lwu, Op::Illegal};
      return t[f3];
    }
    case kOpStore: {
      constexpr Op t[8] = {Op::Sb,      Op::Sh,      Op::Sw,      Op::Sd,
                           Op::Illegal, Op::Illegal, Op::Illegal, Op::Illegal};
      return t[f3];
    }
    case kOpImm: {
      // RV64 shifts take a 6-bit shamt; imm[11:6] selects the variant (Unpriv §5.2.1).
      const uint32_t f6 = bits(r, 31, 26);
      switch (f3) {
        case 0: return Op::Addi;
        case 1: return f6 == 0 ? Op::Slli : Op::Illegal;
        case 2: return Op::Slti;
        case 3: return Op::Sltiu;
        case 4: return Op::Xori;
        case 5: return f6 == 0 ? Op::Srli : f6 == 0b010000 ? Op::Srai : Op::Illegal;
        case 6: return Op::Ori;
        default: return Op::Andi;
      }
    }
    case kOpOp: {
      if (f7 == 0) {
        constexpr Op t[8] = {Op::Add, Op::Sll, Op::Slt, Op::Sltu, Op::Xor, Op::Srl, Op::Or, Op::And};
        return t[f3];
      }
      if (f7 == 0b0100000) return f3 == 0 ? Op::Sub : f3 == 5 ? Op::Sra : Op::Illegal;
      return Op::Illegal;  // f7 == 1 is the M extension (Phase 2).
    }
    case kOpImm32:
      switch (f3) {
        case 0: return Op::Addiw;
        case 1: return f7 == 0 ? Op::Slliw : Op::Illegal;
        case 5: return f7 == 0 ? Op::Srliw : f7 == 0b0100000 ? Op::Sraiw : Op::Illegal;
        default: return Op::Illegal;
      }
    case kOpOp32:
      if (f7 == 0) return f3 == 0 ? Op::Addw : f3 == 1 ? Op::Sllw : f3 == 5 ? Op::Srlw : Op::Illegal;
      if (f7 == 0b0100000) return f3 == 0 ? Op::Subw : f3 == 5 ? Op::Sraw : Op::Illegal;
      return Op::Illegal;
    case kOpMiscMem:
      // Unused FENCE fields are reserved and ignored (Unpriv §2.7); FENCE.TSO
      // and PAUSE are FENCE encodings. FENCE.I ignores imm/rs1/rd (Zifencei §3.1).
      return f3 == 0 ? Op::Fence : f3 == 1 ? Op::FenceI : Op::Illegal;
    case kOpSystem:
      switch (f3) {
        case 0:
          switch (r) {
            case 0x00000073: return Op::Ecall;
            case 0x00100073: return Op::Ebreak;
            case 0x30200073: return Op::Mret;
            case 0x10200073: return Op::Sret;
            case 0x10500073: return Op::Wfi;
            default:
              // SFENCE.VMA rs1, rs2 (Priv §4.2.1): funct7 0001001, rd = 0.
              return (f7 == 0b0001001 && bits(r, 11, 7) == 0) ? Op::SfenceVma : Op::Illegal;
          }
        case 1: return Op::Csrrw;
        case 2: return Op::Csrrs;
        case 3: return Op::Csrrc;
        case 5: return Op::Csrrwi;
        case 6: return Op::Csrrsi;
        case 7: return Op::Csrrci;
        default: return Op::Illegal;
      }
    default: return Op::Illegal;
  }
}

}  // namespace

Insn decode32(uint32_t raw) {
  Insn d;
  d.raw = raw;
  d.len = 4;
  if ((raw & 3) != 3) return d;  // Compressed (Phase 2).
  d.op = decode_op(raw);
  d.rd = static_cast<uint8_t>(bits(raw, 11, 7));
  d.rs1 = static_cast<uint8_t>(bits(raw, 19, 15));
  d.rs2 = static_cast<uint8_t>(bits(raw, 24, 20));
  switch (bits(raw, 6, 0)) {
    case kOpLui:
    case kOpAuipc: d.imm = imm_u(raw); break;
    case kOpJal: d.imm = imm_j(raw); break;
    case kOpBranch: d.imm = imm_b(raw); break;
    case kOpStore: d.imm = imm_s(raw); break;
    case kOpSystem: d.imm = static_cast<int64_t>(bits(raw, 31, 20)); break;  // CSR number
    case kOpImm:
    case kOpImm32:
      // Shifts use only the shamt field of the immediate.
      d.imm = (bits(raw, 13, 12) == 1) ? static_cast<int64_t>(bits(raw, 25, 20)) : imm_i(raw);
      break;
    default: d.imm = imm_i(raw); break;
  }
  return d;
}

}  // namespace rv
