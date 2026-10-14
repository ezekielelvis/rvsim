// Package rvasm encodes RV64 instructions for hand-written test programs,
// so Go tests need no cross toolchain. Register numbers are plain integers.
package rvasm

import "encoding/binary"

// Common registers.
const (
	Zero = 0
	RA   = 1
	SP   = 2
	T0   = 5
	T1   = 6
	T2   = 7
	S0   = 8
	S1   = 9
	A0   = 10
	A1   = 11
	A2   = 12
)

func i(imm int32, rs1, f3, rd, op uint32) uint32 {
	return uint32(imm&0xfff)<<20 | rs1<<15 | f3<<12 | rd<<7 | op
}

func s(imm int32, rs2, rs1, f3 uint32) uint32 {
	u := uint32(imm)
	return (u>>5&0x7f)<<25 | rs2<<20 | rs1<<15 | f3<<12 | (u&0x1f)<<7 | 0x23
}

func b(off int32, rs2, rs1, f3 uint32) uint32 {
	u := uint32(off)
	return (u>>12&1)<<31 | (u>>5&0x3f)<<25 | rs2<<20 | rs1<<15 | f3<<12 | (u>>1&0xf)<<8 | (u>>11&1)<<7 | 0x63
}

// Lui, Auipc, Addi and friends follow the assembler's operand order.
func Lui(rd, imm20 uint32) uint32           { return imm20<<12 | rd<<7 | 0x37 }
func Auipc(rd, imm20 uint32) uint32         { return imm20<<12 | rd<<7 | 0x17 }
func Addi(rd, rs1 uint32, imm int32) uint32 { return i(imm, rs1, 0, rd, 0x13) }
func Andi(rd, rs1 uint32, imm int32) uint32 { return i(imm, rs1, 7, rd, 0x13) }
func Lb(rd, rs1 uint32, off int32) uint32   { return i(off, rs1, 0, rd, 0x03) }
func Lbu(rd, rs1 uint32, off int32) uint32  { return i(off, rs1, 4, rd, 0x03) }
func Lw(rd, rs1 uint32, off int32) uint32   { return i(off, rs1, 2, rd, 0x03) }
func Ld(rd, rs1 uint32, off int32) uint32   { return i(off, rs1, 3, rd, 0x03) }
func Sb(rs2, rs1 uint32, off int32) uint32  { return s(off, rs2, rs1, 0) }
func Sw(rs2, rs1 uint32, off int32) uint32  { return s(off, rs2, rs1, 2) }
func Sd(rs2, rs1 uint32, off int32) uint32  { return s(off, rs2, rs1, 3) }
func Beq(rs1, rs2 uint32, off int32) uint32 { return b(off, rs2, rs1, 0) }
func Bne(rs1, rs2 uint32, off int32) uint32 { return b(off, rs2, rs1, 1) }
func Csrw(csr, rs1 uint32) uint32           { return i(int32(csr), rs1, 1, 0, 0x73) }
func Csrr(rd, csr uint32) uint32            { return i(int32(csr), 0, 2, rd, 0x73) }

// Jal encodes jal rd, off.
func Jal(rd uint32, off int32) uint32 {
	u := uint32(off)
	return (u>>20&1)<<31 | (u>>1&0x3ff)<<21 | (u>>11&1)<<20 | (u>>12&0xff)<<12 | rd<<7 | 0x6f
}

// Fixed encodings.
const (
	Nop  = 0x00000013
	Wfi  = 0x10500073
	Mret = 0x30200073
	Self = 0x0000006f // j .
)

// Bytes returns the program as little-endian bytes.
func Bytes(insns ...uint32) []byte {
	out := make([]byte, 4*len(insns))
	for k, w := range insns {
		binary.LittleEndian.PutUint32(out[4*k:], w)
	}
	return out
}
