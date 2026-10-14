// Package plic implements a platform-level interrupt controller compatible
// with the SiFive PLIC ("riscv,plic0") as QEMU virt lays it out: one hart
// with two contexts, context 0 for M-mode (MEIP) and context 1 for S-mode
// (SEIP). See the RISC-V PLIC specification v1.0.0.
package plic

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/ezekielelvis/rvsim/vmm/devices"
)

const (
	// NumSources is the number of interrupt sources; ID 0 means "none".
	NumSources = 32
	// NumContexts: hart 0 M-mode, hart 0 S-mode.
	NumContexts = 2
	// Size is the PLIC's MMIO window.
	Size = 0x400_0000

	priorityMask = 0x7 // 7 priority levels, as QEMU virt

	offPending   = 0x1000
	offEnable    = 0x2000
	enableStride = 0x80
	offContext   = 0x20_0000
	ctxStride    = 0x1000
)

// Interrupt lines into the hart, as rv_set_irq_lines expects (mip bit numbers).
const (
	LineSEIP = 1 << 9
	LineMEIP = 1 << 11
)

var contextLine = [NumContexts]uint64{LineMEIP, LineSEIP}

// state is everything a snapshot must preserve. It is fixed-size so
// encoding/binary can serialize it.
type state struct {
	Priority  [NumSources]uint32
	Level     uint32 // input line levels
	Pending   uint32 // gateway output: interrupt pending, not yet claimed
	Claimed   uint32 // claimed and not yet completed
	Enable    [NumContexts]uint32
	Threshold [NumContexts]uint32
}

// PLIC is the interrupt controller. setLines receives the MEIP/SEIP levels
// whenever they change.
type PLIC struct {
	s        state
	setLines func(lines uint64)
	lines    uint64
}

// New returns a PLIC that reports its outputs through setLines.
func New(setLines func(lines uint64)) *PLIC {
	return &PLIC{setLines: setLines}
}

type line struct {
	p   *PLIC
	src uint32
}

func (l line) SetLevel(high bool) { l.p.setLevel(l.src, high) }

// Line returns interrupt source src (1..NumSources-1) as a device IRQ line.
func (p *PLIC) Line(src int) devices.IRQLine {
	if src <= 0 || src >= NumSources {
		panic(fmt.Sprintf("plic: invalid source %d", src))
	}
	return line{p, uint32(src)}
}

// setLevel models the level-triggered gateway (PLIC spec §4): while a source
// is claimed its level is ignored; otherwise pending follows the level.
func (p *PLIC) setLevel(src uint32, high bool) {
	bit := uint32(1) << src
	if high {
		p.s.Level |= bit
	} else {
		p.s.Level &^= bit
	}
	if p.s.Claimed&bit == 0 {
		if high {
			p.s.Pending |= bit
		} else {
			p.s.Pending &^= bit
		}
	}
	p.update()
}

// best returns the highest-priority pending, enabled source above the
// context's threshold, or 0. Ties go to the lowest ID (PLIC spec §7).
func (p *PLIC) best(ctx int) uint32 {
	cand := p.s.Pending & p.s.Enable[ctx]
	var id, prio uint32
	for src := uint32(1); src < NumSources; src++ {
		if cand&(1<<src) == 0 {
			continue
		}
		if pr := p.s.Priority[src]; pr > p.s.Threshold[ctx] && pr > prio {
			id, prio = src, pr
		}
	}
	return id
}

func (p *PLIC) update() {
	var lines uint64
	for ctx := range NumContexts {
		if p.best(ctx) != 0 {
			lines |= contextLine[ctx]
		}
	}
	if lines != p.lines {
		p.lines = lines
		p.setLines(lines)
	}
}

// Read implements devices.Device. Only 32-bit accesses are defined; other
// sizes read as zero.
func (p *PLIC) Read(off uint64, size int) uint64 {
	if size != 4 || off%4 != 0 {
		return 0
	}
	switch {
	case off < NumSources*4:
		return uint64(p.s.Priority[off/4])
	case off == offPending:
		return uint64(p.s.Pending)
	case off >= offEnable && off < offEnable+NumContexts*enableStride:
		ctx, word := (off-offEnable)/enableStride, (off-offEnable)%enableStride
		if word == 0 {
			return uint64(p.s.Enable[ctx])
		}
	case off >= offContext && off < offContext+NumContexts*ctxStride:
		ctx, reg := (off-offContext)/ctxStride, (off-offContext)%ctxStride
		switch reg {
		case 0:
			return uint64(p.s.Threshold[ctx])
		case 4:
			return uint64(p.claim(int(ctx)))
		}
	}
	return 0
}

// claim (PLIC spec §8): take the best source for ctx, clear its pending bit
// and hold its gateway until completion.
func (p *PLIC) claim(ctx int) uint32 {
	id := p.best(ctx)
	if id != 0 {
		p.s.Pending &^= 1 << id
		p.s.Claimed |= 1 << id
		p.update()
	}
	return id
}

// Write implements devices.Device. Only 32-bit accesses are defined.
func (p *PLIC) Write(off uint64, size int, v uint64) {
	if size != 4 || off%4 != 0 {
		return
	}
	switch {
	case off < NumSources*4:
		if off != 0 { // source 0 does not exist
			p.s.Priority[off/4] = uint32(v) & priorityMask
		}
	case off >= offEnable && off < offEnable+NumContexts*enableStride:
		ctx, word := (off-offEnable)/enableStride, (off-offEnable)%enableStride
		if word == 0 {
			p.s.Enable[ctx] = uint32(v) &^ 1 // bit 0 is source 0
		}
	case off >= offContext && off < offContext+NumContexts*ctxStride:
		ctx, reg := (off-offContext)/ctxStride, (off-offContext)%ctxStride
		switch reg {
		case 0:
			p.s.Threshold[ctx] = uint32(v) & priorityMask
		case 4:
			p.complete(int(ctx), uint32(v))
		}
	default:
		return // the pending array is read-only
	}
	p.update()
}

// complete (PLIC spec §9): release the gateway. A completion for a source
// that is not enabled for the context is ignored. If the line is still high,
// the source becomes pending again.
func (p *PLIC) complete(ctx int, id uint32) {
	if id == 0 || id >= NumSources {
		return
	}
	bit := uint32(1) << id
	if p.s.Enable[ctx]&bit == 0 || p.s.Claimed&bit == 0 {
		return
	}
	p.s.Claimed &^= bit
	if p.s.Level&bit != 0 {
		p.s.Pending |= bit
	}
}

// Reset clears all registers. Input levels are kept: they belong to the
// devices driving them.
func (p *PLIC) Reset() {
	level := p.s.Level
	p.s = state{Level: level, Pending: level}
	p.update()
}

// Snapshot implements devices.Device.
func (p *PLIC) Snapshot() []byte {
	var b bytes.Buffer
	b.WriteByte(1) // format version
	_ = binary.Write(&b, binary.LittleEndian, &p.s)
	return b.Bytes()
}

// Restore implements devices.Device.
func (p *PLIC) Restore(data []byte) error {
	if len(data) != 1+binary.Size(state{}) || data[0] != 1 {
		return fmt.Errorf("plic: bad snapshot (%d bytes)", len(data))
	}
	if err := binary.Read(bytes.NewReader(data[1:]), binary.LittleEndian, &p.s); err != nil {
		return err
	}
	p.lines = ^uint64(0) // force the outputs to be re-sent
	p.update()
	return nil
}
