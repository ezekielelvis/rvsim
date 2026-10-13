// Package loader loads guest images into physical memory.
package loader

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
)

// Segment is one PT_LOAD segment: Data is copied to Addr and followed by
// MemSize-len(Data) zero bytes.
type Segment struct {
	Addr    uint64
	Data    []byte
	MemSize uint64
}

// Image is a parsed RV64 ELF executable.
type Image struct {
	Entry    uint64
	Segments []Segment
	Symbols  map[string]uint64
}

// PhysWriter is the subset of machine.Machine used to load an image.
type PhysWriter interface {
	WritePhys(paddr uint64, buf []byte) error
}

// ParseELF reads a little-endian RV64 ELF executable. Segment physical
// addresses (p_paddr) are used, so images linked for a VMA != LMA load where
// the hardware sees them.
func ParseELF(r io.ReaderAt) (*Image, error) {
	f, err := elf.NewFile(r)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != elf.EM_RISCV {
		return nil, fmt.Errorf("loader: not a little-endian RV64 ELF (class %v, data %v, machine %v)",
			f.Class, f.Data, f.Machine)
	}
	if f.Type != elf.ET_EXEC {
		return nil, fmt.Errorf("loader: ELF type %v, want ET_EXEC", f.Type)
	}
	img := &Image{Entry: f.Entry, Symbols: map[string]uint64{}}
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Memsz == 0 {
			continue
		}
		if p.Filesz > p.Memsz {
			return nil, fmt.Errorf("loader: segment at %#x has filesz %d > memsz %d", p.Paddr, p.Filesz, p.Memsz)
		}
		data := make([]byte, p.Filesz)
		if _, err := p.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("loader: reading segment at %#x: %w", p.Paddr, err)
		}
		img.Segments = append(img.Segments, Segment{Addr: p.Paddr, Data: data, MemSize: p.Memsz})
	}
	syms, err := f.Symbols()
	if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
		return nil, err
	}
	for _, s := range syms {
		if s.Name != "" && elf.ST_TYPE(s.Info) != elf.STT_SECTION {
			img.Symbols[s.Name] = s.Value
		}
	}
	return img, nil
}

// Load writes every segment into guest physical memory, zero-filling the
// part of each segment beyond its file contents (.bss).
func (img *Image) Load(m PhysWriter) error {
	for _, s := range img.Segments {
		if err := m.WritePhys(s.Addr, s.Data); err != nil {
			return fmt.Errorf("loader: segment at %#x: %w", s.Addr, err)
		}
		if bss := s.MemSize - uint64(len(s.Data)); bss > 0 {
			if err := m.WritePhys(s.Addr+uint64(len(s.Data)), make([]byte, bss)); err != nil {
				return fmt.Errorf("loader: bss at %#x: %w", s.Addr+uint64(len(s.Data)), err)
			}
		}
	}
	return nil
}
