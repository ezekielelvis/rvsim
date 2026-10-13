package loader

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"testing"
)

type seg struct {
	vaddr, paddr uint64
	data         []byte
	memsz        uint64
}

// buildELF assembles a minimal ELF64 executable with the given segments and
// no section headers (so no symbols).
func buildELF(class elf.Class, machine elf.Machine, typ elf.Type, entry uint64, segs []seg) []byte {
	const ehsize, phentsize = 64, 56
	var b bytes.Buffer
	le := binary.LittleEndian
	ident := [16]byte{0x7f, 'E', 'L', 'F', byte(class), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)}
	b.Write(ident[:])
	binary.Write(&b, le, uint16(typ))
	binary.Write(&b, le, uint16(machine))
	binary.Write(&b, le, uint32(elf.EV_CURRENT))
	binary.Write(&b, le, entry)
	binary.Write(&b, le, uint64(ehsize)) // phoff
	binary.Write(&b, le, uint64(0))      // shoff
	binary.Write(&b, le, uint32(0))      // flags
	binary.Write(&b, le, uint16(ehsize))
	binary.Write(&b, le, uint16(phentsize))
	binary.Write(&b, le, uint16(len(segs)))
	binary.Write(&b, le, uint16(64)) // shentsize
	binary.Write(&b, le, uint16(0))  // shnum
	binary.Write(&b, le, uint16(0))  // shstrndx
	off := uint64(ehsize + phentsize*len(segs))
	for _, s := range segs {
		binary.Write(&b, le, uint32(elf.PT_LOAD))
		binary.Write(&b, le, uint32(elf.PF_R|elf.PF_X))
		binary.Write(&b, le, off)
		binary.Write(&b, le, s.vaddr)
		binary.Write(&b, le, s.paddr)
		binary.Write(&b, le, uint64(len(s.data)))
		binary.Write(&b, le, s.memsz)
		binary.Write(&b, le, uint64(8))
		off += uint64(len(s.data))
	}
	for _, s := range segs {
		b.Write(s.data)
	}
	return b.Bytes()
}

type fakeMem map[uint64]byte

func (f fakeMem) WritePhys(addr uint64, buf []byte) error {
	for i, v := range buf {
		f[addr+uint64(i)] = v
	}
	return nil
}

func TestParseAndLoad(t *testing.T) {
	raw := buildELF(elf.ELFCLASS64, elf.EM_RISCV, elf.ET_EXEC, 0x8000_0000, []seg{
		{vaddr: 0xffff_ffff_8000_0000, paddr: 0x8000_0000, data: []byte{1, 2, 3, 4}, memsz: 8},
		{vaddr: 0x8000_1000, paddr: 0x8000_1000, data: []byte{9}, memsz: 1},
	})
	img, err := ParseELF(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if img.Entry != 0x8000_0000 || len(img.Segments) != 2 {
		t.Fatalf("image = %+v", img)
	}
	mem := fakeMem{}
	for i := range uint64(8) {
		mem[0x8000_0000+i] = 0xee // bss must be cleared, not left as is
	}
	if err := img.Load(mem); err != nil {
		t.Fatal(err)
	}
	want := map[uint64]byte{
		0x8000_0000: 1, 0x8000_0001: 2, 0x8000_0002: 3, 0x8000_0003: 4,
		0x8000_0004: 0, 0x8000_0005: 0, 0x8000_0006: 0, 0x8000_0007: 0,
		0x8000_1000: 9,
	}
	for a, v := range want {
		if mem[a] != v {
			t.Errorf("mem[%#x] = %d, want %d", a, mem[a], v)
		}
	}
	if _, ok := mem[0xffff_ffff_8000_0000]; ok {
		t.Error("segment loaded at its virtual address instead of p_paddr")
	}
}

func TestParseRejects(t *testing.T) {
	one := []seg{{paddr: 0x8000_0000, data: []byte{0}, memsz: 1}}
	tests := []struct {
		name string
		raw  []byte
	}{
		{"not an ELF", []byte("hello, world, this is not an ELF file at all..........................")},
		{"x86-64", buildELF(elf.ELFCLASS64, elf.EM_X86_64, elf.ET_EXEC, 0, one)},
		{"shared object", buildELF(elf.ELFCLASS64, elf.EM_RISCV, elf.ET_DYN, 0, one)},
		{"filesz > memsz", buildELF(elf.ELFCLASS64, elf.EM_RISCV, elf.ET_EXEC, 0, []seg{{paddr: 0x8000_0000, data: []byte{1, 2}, memsz: 1}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseELF(bytes.NewReader(tt.raw)); err == nil {
				t.Fatal("ParseELF succeeded, want error")
			}
		})
	}
}

type failingMem struct{}

func (failingMem) WritePhys(uint64, []byte) error { return errors.New("out of range") }

func TestLoadPropagatesWriteErrors(t *testing.T) {
	raw := buildELF(elf.ELFCLASS64, elf.EM_RISCV, elf.ET_EXEC, 0, []seg{{paddr: 0x1000, data: []byte{1}, memsz: 1}})
	img, err := ParseELF(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := img.Load(failingMem{}); err == nil {
		t.Fatal("Load succeeded, want error")
	}
}
