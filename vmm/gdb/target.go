package gdb

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

// GDB's RISC-V register numbers (gdb/riscv-tdep.h).
const (
	regPC       = 32
	regFirstCSR = 65
	regLastCSR  = regFirstCSR + 4095
	regPriv     = regLastCSR + 1 // 4161
)

// StopReason is why the target stopped.
type StopReason int

const (
	StopStep       StopReason = iota // a single step finished
	StopBreakpoint                   // an execution breakpoint
	StopInterrupt                    // the debugger asked (Ctrl-C)
	StopExited                       // the guest powered off; Code is its exit code
	StopHistory                      // reverse execution reached the start of history
)

// Stop describes a stop.
type Stop struct {
	Reason StopReason
	Code   uint64
}

// ErrNoReverse reports a target that cannot execute backwards.
var ErrNoReverse = errors.New("gdb: this target cannot execute in reverse")

// Target is what the stub debugs.
type Target interface {
	ReadReg(n int) (uint64, error)
	WriteReg(n int, v uint64) error
	ReadMem(addr uint64, b []byte) error
	WriteMem(addr uint64, b []byte) error
	AddBreakpoint(addr uint64) error
	RemoveBreakpoint(addr uint64) error
	// Step executes one step; Continue runs until a stop. Continue polls
	// interrupted between bounded chunks of work.
	Step() (Stop, error)
	Continue(interrupted func() bool) (Stop, error)
	ReverseStep() (Stop, error)
	ReverseContinue(interrupted func() bool) (Stop, error)
}

// VMTarget debugs a live VM. It runs the VM on the caller's goroutine, which
// must be the only one touching it.
type VMTarget struct {
	vm  *vm.VM
	bps map[uint64]uint32 // address -> core breakpoint id
	// Chunk bounds each Run call while continuing, so an interrupt is noticed
	// promptly (default 1 M steps).
	Chunk uint64
}

// NewVMTarget returns a target for v.
func NewVMTarget(v *vm.VM) *VMTarget {
	return &VMTarget{vm: v, bps: map[uint64]uint32{}, Chunk: 1 << 20}
}

// SetVM switches the target to another VM (after a reverse jump), moving
// the breakpoints over.
func (t *VMTarget) SetVM(v *vm.VM) error {
	t.vm = v
	for addr := range t.bps {
		id, err := v.Machine().AddBreakpoint(addr, 2, machine.BreakExec)
		if err != nil {
			return err
		}
		t.bps[addr] = id
	}
	return nil
}

// VM returns the VM being debugged.
func (t *VMTarget) VM() *vm.VM { return t.vm }

func (t *VMTarget) m() *machine.Machine { return t.vm.Machine() }

// ReadReg implements Target.
func (t *VMTarget) ReadReg(n int) (uint64, error) {
	switch {
	case n >= 0 && n < 32:
		return t.m().Reg(uint32(n))
	case n == regPC:
		return t.m().Reg(machine.RegPC)
	case n >= regFirstCSR && n <= regLastCSR:
		return t.m().Reg(machine.RegCSR(uint16(n - regFirstCSR)))
	case n == regPriv:
		return t.m().Reg(machine.RegPriv)
	}
	return 0, fmt.Errorf("gdb: no register %d", n)
}

// WriteReg implements Target.
func (t *VMTarget) WriteReg(n int, v uint64) error {
	switch {
	case n == 0:
		return nil // x0 is hardwired
	case n > 0 && n < 32:
		return t.m().SetReg(uint32(n), v)
	case n == regPC:
		return t.m().SetReg(machine.RegPC, v)
	case n >= regFirstCSR && n <= regLastCSR:
		return t.m().SetReg(machine.RegCSR(uint16(n-regFirstCSR)), v)
	case n == regPriv:
		return t.m().SetReg(machine.RegPriv, v)
	}
	return fmt.Errorf("gdb: no register %d", n)
}

// translate maps a debugger address to a physical one. With Sv39 enabled it
// walks the page table ignoring permissions (U, SUM, R/W/X), so kernel
// addresses resolve whatever mode the hart is in; if the walk fails while
// the hart is in M-mode, which runs untranslated, the address is physical.
func (t *VMTarget) translate(va uint64) (uint64, error) {
	satp, err := t.m().Reg(machine.RegCSR(0x180))
	if err != nil {
		return 0, err
	}
	if satp>>60 != 8 { // Bare
		return va, nil
	}
	if pa, ok := t.walk(satp, va); ok {
		return pa, nil
	}
	if priv, _ := t.m().Reg(machine.RegPriv); priv == 3 {
		return va, nil
	}
	return 0, fmt.Errorf("gdb: %#x is not mapped", va)
}

// walk performs an Sv39 walk (Priv §4.3.2) without permission checks or
// side effects.
func (t *VMTarget) walk(satp, va uint64) (uint64, bool) {
	if uint64(int64(va<<25)>>25) != va { // bits 63:39 must equal bit 38
		return 0, false
	}
	table := (satp & (1<<44 - 1)) << 12
	var b [8]byte
	for level := 2; level >= 0; level-- {
		vpn := (va >> (12 + 9*level)) & 0x1ff
		if err := t.m().ReadPhys(table+vpn*8, b[:]); err != nil {
			return 0, false
		}
		pte := binary.LittleEndian.Uint64(b[:])
		if pte&1 == 0 {
			return 0, false
		}
		ppn := (pte >> 10) & (1<<44 - 1)
		if pte&0xe != 0 { // leaf: R, W or X
			offMask := uint64(1)<<(12+9*level) - 1
			return (ppn<<12)&^offMask | va&offMask, true
		}
		table = ppn << 12
	}
	return 0, false
}

// eachPage applies f to the page-sized pieces of [addr, addr+len(b)).
func (t *VMTarget) eachPage(addr uint64, b []byte, f func(pa uint64, b []byte) error) error {
	for len(b) > 0 {
		n := min(uint64(len(b)), 4096-addr%4096)
		pa, err := t.translate(addr)
		if err != nil {
			return err
		}
		if err := f(pa, b[:n]); err != nil {
			return err
		}
		addr, b = addr+n, b[n:]
	}
	return nil
}

// ReadMem implements Target (RAM only: device registers are not read).
func (t *VMTarget) ReadMem(addr uint64, b []byte) error {
	return t.eachPage(addr, b, t.m().ReadPhys)
}

// WriteMem implements Target.
func (t *VMTarget) WriteMem(addr uint64, b []byte) error {
	return t.eachPage(addr, b, t.m().WritePhys)
}

// AddBreakpoint implements Target. Breakpoints live in the core, not in
// guest memory, so they never change what the guest sees.
func (t *VMTarget) AddBreakpoint(addr uint64) error {
	if _, ok := t.bps[addr]; ok {
		return nil
	}
	id, err := t.m().AddBreakpoint(addr, 2, machine.BreakExec)
	if err != nil {
		return err
	}
	t.bps[addr] = id
	return nil
}

// RemoveBreakpoint implements Target.
func (t *VMTarget) RemoveBreakpoint(addr uint64) error {
	id, ok := t.bps[addr]
	if !ok {
		return nil
	}
	delete(t.bps, addr)
	return t.m().RemoveBreakpoint(id)
}

func stopFor(res vm.Result) (Stop, bool) {
	switch {
	case res.Exited:
		return Stop{Reason: StopExited, Code: res.ExitCode}, true
	case res.Breakpoint:
		return Stop{Reason: StopBreakpoint}, true
	}
	return Stop{}, false
}

// Step implements Target.
func (t *VMTarget) Step() (Stop, error) {
	res, err := t.vm.Run(1)
	if err != nil {
		return Stop{}, err
	}
	if s, ok := stopFor(res); ok {
		return s, nil
	}
	return Stop{Reason: StopStep}, nil
}

// Continue implements Target.
func (t *VMTarget) Continue(interrupted func() bool) (Stop, error) {
	for {
		res, err := t.vm.Run(t.Chunk)
		if err != nil {
			return Stop{}, err
		}
		if s, ok := stopFor(res); ok {
			return s, nil
		}
		if interrupted() {
			return Stop{Reason: StopInterrupt}, nil
		}
	}
}

// ReverseStep implements Target: a live VM has no history.
func (t *VMTarget) ReverseStep() (Stop, error) { return Stop{}, ErrNoReverse }

// ReverseContinue implements Target.
func (t *VMTarget) ReverseContinue(func() bool) (Stop, error) { return Stop{}, ErrNoReverse }
