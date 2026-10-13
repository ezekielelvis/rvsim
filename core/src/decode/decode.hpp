// Instruction decoding: raw bits -> Insn. Pure functions, no machine state.
#pragma once

#include <cstdint>

namespace rv {

enum class Op : uint8_t {
  Illegal,
  // RV64I (Unpriv ch. 2 and 5)
  Lui, Auipc, Jal, Jalr,
  Beq, Bne, Blt, Bge, Bltu, Bgeu,
  Lb, Lh, Lw, Ld, Lbu, Lhu, Lwu,
  Sb, Sh, Sw, Sd,
  Addi, Slti, Sltiu, Xori, Ori, Andi, Slli, Srli, Srai,
  Add, Sub, Sll, Slt, Sltu, Xor, Srl, Sra, Or, And,
  Addiw, Slliw, Srliw, Sraiw,
  Addw, Subw, Sllw, Srlw, Sraw,
  Fence, Ecall, Ebreak,
  // Zifencei
  FenceI,
  // Zicsr
  Csrrw, Csrrs, Csrrc, Csrrwi, Csrrsi, Csrrci,
  // Privileged (Priv ch. 3 and 4)
  Mret, Sret, Wfi, SfenceVma,
};

struct Insn {
  Op op = Op::Illegal;
  uint8_t rd = 0;
  uint8_t rs1 = 0;  // also the zero-extended uimm for CSR*I
  uint8_t rs2 = 0;
  uint8_t len = 4;  // 2 for compressed (Phase 2)
  int64_t imm = 0;  // sign-extended immediate; CSR number for Zicsr
  uint32_t raw = 0;
};

// Decodes a 32-bit instruction (raw[1:0] == 0b11). Anything not in the
// implemented ISA decodes as Op::Illegal.
Insn decode32(uint32_t raw);

}  // namespace rv
