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
constexpr uint32_t kOpAmo = 0b0101111;
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
      if (f7 == 1) {
        constexpr Op t[8] = {Op::Mul, Op::Mulh, Op::Mulhsu, Op::Mulhu,
                             Op::Div, Op::Divu, Op::Rem,    Op::Remu};
        return t[f3];
      }
      return Op::Illegal;
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
      if (f7 == 1) {
        constexpr Op t[8] = {Op::Mulw, Op::Illegal, Op::Illegal, Op::Illegal,
                             Op::Divw, Op::Divuw,   Op::Remw,    Op::Remuw};
        return t[f3];
      }
      return Op::Illegal;
    case kOpAmo: {
      if (f3 != 2 && f3 != 3) return Op::Illegal;
      const bool d = f3 == 3;
      switch (bits(r, 31, 27)) {
        // LR has no rs2 operand; the field must be zero (Unpriv §14.2).
        case 0b00010: return bits(r, 24, 20) != 0 ? Op::Illegal : d ? Op::LrD : Op::LrW;
        case 0b00011: return d ? Op::ScD : Op::ScW;
        case 0b00001: return d ? Op::AmoswapD : Op::AmoswapW;
        case 0b00000: return d ? Op::AmoaddD : Op::AmoaddW;
        case 0b00100: return d ? Op::AmoxorD : Op::AmoxorW;
        case 0b01100: return d ? Op::AmoandD : Op::AmoandW;
        case 0b01000: return d ? Op::AmoorD : Op::AmoorW;
        case 0b10000: return d ? Op::AmominD : Op::AmominW;
        case 0b10100: return d ? Op::AmomaxD : Op::AmomaxW;
        case 0b11000: return d ? Op::AmominuD : Op::AmominuW;
        case 0b11100: return d ? Op::AmomaxuD : Op::AmomaxuW;
        default: return Op::Illegal;  // e.g. AMOCAS (Zacas), not implemented
      }
    }
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
  if ((raw & 3) != 3) return d;  // compressed: use decode16
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
    case kOpAmo: d.imm = 0; break;
    default: d.imm = imm_i(raw); break;
  }
  return d;
}

// --- C extension (Unpriv ch. 16, RV64C) -------------------------------------

namespace {

// Compressed register fields rd'/rs1'/rs2' name x8-x15.
constexpr uint8_t creg(uint32_t three_bits) { return static_cast<uint8_t>(8 + three_bits); }

Insn make(Op op, uint32_t rd, uint32_t rs1, uint32_t rs2, int64_t imm) {
  Insn d;
  d.op = op;
  d.rd = static_cast<uint8_t>(rd);
  d.rs1 = static_cast<uint8_t>(rs1);
  d.rs2 = static_cast<uint8_t>(rs2);
  d.imm = imm;
  return d;
}

// Immediate layouts, Unpriv §16.3 Tables 16.1-16.3.
constexpr int64_t ci_imm(uint32_t r) { return sext((bits(r, 12, 12) << 5) | bits(r, 6, 2), 6); }
constexpr uint32_t ci_shamt(uint32_t r) { return (bits(r, 12, 12) << 5) | bits(r, 6, 2); }
constexpr int64_t cj_imm(uint32_t r) {
  return sext((bits(r, 12, 12) << 11) | (bits(r, 11, 11) << 4) | (bits(r, 10, 9) << 8) |
                  (bits(r, 8, 8) << 10) | (bits(r, 7, 7) << 6) | (bits(r, 6, 6) << 7) |
                  (bits(r, 5, 3) << 1) | (bits(r, 2, 2) << 5),
              12);
}
constexpr int64_t cb_imm(uint32_t r) {
  return sext((bits(r, 12, 12) << 8) | (bits(r, 11, 10) << 3) | (bits(r, 6, 5) << 6) |
                  (bits(r, 4, 3) << 1) | (bits(r, 2, 2) << 5),
              9);
}
constexpr uint32_t cl_w_imm(uint32_t r) { return (bits(r, 12, 10) << 3) | (bits(r, 6, 6) << 2) | (bits(r, 5, 5) << 6); }
constexpr uint32_t cl_d_imm(uint32_t r) { return (bits(r, 12, 10) << 3) | (bits(r, 6, 5) << 6); }

Insn decode_q0(uint32_t r) {
  const uint32_t rs1 = creg(bits(r, 9, 7)), rd = creg(bits(r, 4, 2));
  switch (bits(r, 15, 13)) {
    case 0: {  // C.ADDI4SPN -> addi rd', x2, nzuimm; nzuimm = 0 is reserved
      const uint32_t imm = (bits(r, 12, 11) << 4) | (bits(r, 10, 7) << 6) | (bits(r, 6, 6) << 2) |
                           (bits(r, 5, 5) << 3);
      return imm == 0 ? Insn{} : make(Op::Addi, rd, 2, 0, imm);
    }
    case 2: return make(Op::Lw, rd, rs1, 0, cl_w_imm(r));  // C.LW
    case 3: return make(Op::Ld, rd, rs1, 0, cl_d_imm(r));  // C.LD
    case 6: return make(Op::Sw, 0, rs1, rd, cl_w_imm(r));  // C.SW (rs2' in bits 4:2)
    case 7: return make(Op::Sd, 0, rs1, rd, cl_d_imm(r));  // C.SD
    default: return Insn{};  // C.FLD, C.FSD (no D), reserved
  }
}

Insn decode_q1(uint32_t r) {
  const uint32_t rd = bits(r, 11, 7);
  switch (bits(r, 15, 13)) {
    case 0: return make(Op::Addi, rd, rd, 0, ci_imm(r));  // C.ADDI / C.NOP (rd=0 or imm=0: HINT)
    case 1:  // C.ADDIW; rd = 0 is reserved
      return rd == 0 ? Insn{} : make(Op::Addiw, rd, rd, 0, ci_imm(r));
    case 2: return make(Op::Addi, rd, 0, 0, ci_imm(r));  // C.LI (rd=0: HINT)
    case 3: {
      if (rd == 2) {  // C.ADDI16SP; nzimm = 0 is reserved
        const int64_t imm = sext((bits(r, 12, 12) << 9) | (bits(r, 6, 6) << 4) | (bits(r, 5, 5) << 6) |
                                     (bits(r, 4, 3) << 7) | (bits(r, 2, 2) << 5),
                                 10);
        return imm == 0 ? Insn{} : make(Op::Addi, 2, 2, 0, imm);
      }
      // C.LUI; nzimm = 0 is reserved (rd=0 with nzimm != 0 is a HINT).
      const int64_t imm = sext((bits(r, 12, 12) << 17) | (bits(r, 6, 2) << 12), 18);
      return imm == 0 ? Insn{} : make(Op::Lui, rd, 0, 0, imm);
    }
    case 4: {
      const uint32_t rs1 = creg(bits(r, 9, 7)), rs2 = creg(bits(r, 4, 2));
      switch (bits(r, 11, 10)) {
        // RV64C: shamt[5] is part of the shift amount; shamt = 0 is a HINT.
        case 0: return make(Op::Srli, rs1, rs1, 0, ci_shamt(r));
        case 1: return make(Op::Srai, rs1, rs1, 0, ci_shamt(r));
        case 2: return make(Op::Andi, rs1, rs1, 0, ci_imm(r));
        default: {
          constexpr Op narrow[4] = {Op::Sub, Op::Xor, Op::Or, Op::And};
          constexpr Op wide[4] = {Op::Subw, Op::Addw, Op::Illegal, Op::Illegal};
          const Op o = bits(r, 12, 12) ? wide[bits(r, 6, 5)] : narrow[bits(r, 6, 5)];
          return o == Op::Illegal ? Insn{} : make(o, rs1, rs1, rs2, 0);
        }
      }
    }
    case 5: return make(Op::Jal, 0, 0, 0, cj_imm(r));                          // C.J
    case 6: return make(Op::Beq, 0, creg(bits(r, 9, 7)), 0, cb_imm(r));        // C.BEQZ
    default: return make(Op::Bne, 0, creg(bits(r, 9, 7)), 0, cb_imm(r));       // C.BNEZ
  }
}

Insn decode_q2(uint32_t r) {
  const uint32_t rd = bits(r, 11, 7), rs2 = bits(r, 6, 2);
  switch (bits(r, 15, 13)) {
    case 0: return make(Op::Slli, rd, rd, 0, ci_shamt(r));  // C.SLLI (rd=0 or shamt=0: HINT)
    case 2: {  // C.LWSP; rd = 0 is reserved
      const uint32_t imm = (bits(r, 12, 12) << 5) | (bits(r, 6, 4) << 2) | (bits(r, 3, 2) << 6);
      return rd == 0 ? Insn{} : make(Op::Lw, rd, 2, 0, imm);
    }
    case 3: {  // C.LDSP; rd = 0 is reserved
      const uint32_t imm = (bits(r, 12, 12) << 5) | (bits(r, 6, 5) << 3) | (bits(r, 4, 2) << 6);
      return rd == 0 ? Insn{} : make(Op::Ld, rd, 2, 0, imm);
    }
    case 4:
      if (bits(r, 12, 12) == 0) {
        if (rs2 == 0) return rd == 0 ? Insn{} : make(Op::Jalr, 0, rd, 0, 0);  // C.JR; rs1=0 reserved
        return make(Op::Add, rd, 0, rs2, 0);                                   // C.MV (rd=0: HINT)
      }
      if (rs2 == 0) {
        if (rd == 0) return make(Op::Ebreak, 0, 0, 0, 0);  // C.EBREAK
        return make(Op::Jalr, 1, rd, 0, 0);                // C.JALR
      }
      return make(Op::Add, rd, rd, rs2, 0);  // C.ADD (rd=0: HINT)
    case 6:  // C.SWSP
      return make(Op::Sw, 0, 2, rs2, (bits(r, 12, 9) << 2) | (bits(r, 8, 7) << 6));
    case 7:  // C.SDSP
      return make(Op::Sd, 0, 2, rs2, (bits(r, 12, 10) << 3) | (bits(r, 9, 7) << 6));
    default: return Insn{};  // C.FLDSP, C.FSDSP (no D)
  }
}

}  // namespace

Insn decode16(uint16_t raw) {
  const uint32_t r = raw;
  Insn d;
  switch (r & 3) {
    case 0: d = decode_q0(r); break;
    case 1: d = decode_q1(r); break;
    default: d = decode_q2(r); break;
  }
  d.len = 2;
  d.raw = r;
  return d;
}

}  // namespace rv
