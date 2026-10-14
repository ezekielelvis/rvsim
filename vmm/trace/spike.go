// Package trace formats core trace records.
package trace

import (
	"bufio"
	"io"
	"strconv"

	"github.com/ezekielelvis/rvsim/vmm/machine"
)

// SpikeWriter writes commit records in the format of Spike's --log-commits
// output, so logs from both simulators can be compared line by line:
//
//	core   0: 3 0x0000000080000000 (0x00000297) x5  0x0000000080000000
//	core   0: 3 0x0000000080000004 (0x0182b283) x5  0x...  mem 0x0000000080001018
//	core   0: 3 0x0000000080000008 (0x00628023) mem 0x0000000080001000 0x01
//	core   0: 3 0x000000008000000c (0x30529073) c773_mtvec 0x0000000080000098
//	core   0: 0 0x00000000800001a8 (0x00b6a72f) x14 0x...  mem 0x...2000 mem 0x...2000 0x7ffff800
//
// Traps are not written: Spike does not log them either.
type SpikeWriter struct {
	w    *bufio.Writer
	hart int
	buf  []byte
}

// NewSpikeWriter returns a writer for hart 0.
func NewSpikeWriter(w io.Writer) *SpikeWriter {
	return &SpikeWriter{w: bufio.NewWriterSize(w, 1<<16), buf: make([]byte, 0, 160)}
}

// Write formats each commit record as one line.
func (s *SpikeWriter) Write(records []machine.TraceRecord) error {
	for i := range records {
		r := &records[i]
		if r.Kind != machine.TraceCommit {
			continue
		}
		if _, err := s.w.Write(s.format(r)); err != nil {
			return err
		}
	}
	return nil
}

// Flush flushes buffered output.
func (s *SpikeWriter) Flush() error { return s.w.Flush() }

func (s *SpikeWriter) format(r *machine.TraceRecord) []byte {
	b := s.buf[:0]
	b = append(b, "core "...)
	b = appendPadded(b, strconv.Itoa(s.hart), 3)
	b = append(b, ": "...)
	b = strconv.AppendUint(b, uint64(r.Priv), 10)
	b = append(b, ' ')
	b = appendHex(b, r.PC, 16)
	b = append(b, " ("...)
	if r.InsnLen == 2 {
		b = appendHex(b, uint64(r.Insn), 4)
	} else {
		b = appendHex(b, uint64(r.Insn), 8)
	}
	b = append(b, ')')
	if r.Flags&machine.TraceFlagRd != 0 {
		b = append(b, " x"...)
		reg := strconv.Itoa(int(r.Rd))
		b = append(b, reg...)
		if len(reg) == 1 {
			b = append(b, ' ')
		}
		b = append(b, ' ')
		b = appendHex(b, r.RdValue, 16)
	}
	if r.Flags&machine.TraceFlagCSR != 0 {
		b = appendCSR(b, r.CSR, r.CSRValue)
	}
	if r.Flags&machine.TraceFlagCSR2 != 0 {
		b = appendCSR(b, r.CSR2, r.CSR2Value)
	}
	// An AMO sets both flags; Spike logs its read and then its write.
	if r.Flags&machine.TraceFlagLoad != 0 {
		b = append(b, " mem "...)
		b = appendHex(b, r.MemAddr, 16)
	}
	if r.Flags&machine.TraceFlagStore != 0 {
		b = append(b, " mem "...)
		b = appendHex(b, r.MemAddr, 16)
		b = append(b, ' ')
		b = appendHex(b, r.MemValue, 2*int(r.MemSize))
	}
	b = append(b, '\n')
	s.buf = b
	return b
}

func appendCSR(b []byte, csr uint16, v uint64) []byte {
	b = append(b, " c"...)
	b = strconv.AppendUint(b, uint64(csr), 10)
	b = append(b, '_')
	b = append(b, CSRName(csr)...)
	b = append(b, ' ')
	return appendHex(b, v, 16)
}

// appendHex appends "0x" and v as lowercase hex, zero-padded to digits.
func appendHex(b []byte, v uint64, digits int) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '0', 'x')
	for i := digits - 1; i >= 0; i-- {
		b = append(b, hex[(v>>(4*uint(i)))&0xf])
	}
	return b
}

func appendPadded(b []byte, s string, width int) []byte {
	for i := len(s); i < width; i++ {
		b = append(b, ' ')
	}
	return append(b, s...)
}
