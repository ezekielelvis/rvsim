package vm

import (
	"github.com/ezekielelvis/rvsim/vmm/devices"
	"github.com/ezekielelvis/rvsim/vmm/devices/virtio/blk"
	"github.com/ezekielelvis/rvsim/vmm/fault"
	"github.com/ezekielelvis/rvsim/vmm/machine"
)

// Fault injection (ADR-031): the VM consults Config.Faults at run-loop
// boundaries and at each device event, and carries out what it returns.

// irqLine is a PLIC source line through which interrupt faults (drop,
// delay) act. Lines are level-triggered: a fault takes the next assertion.
type irqLine struct {
	v       *VM
	src     uint32
	inner   devices.IRQLine
	level   bool   // the device's level
	blocked bool   // the current assertion is dropped or delayed
	gen     uint64 // increments on each deassertion; stale delays ignore
}

func (l *irqLine) SetLevel(high bool) {
	if high == l.level {
		return
	}
	l.level = high
	if !high {
		l.blocked = false
		l.gen++
		l.inner.SetLevel(false)
		return
	}
	a, ok, err := l.v.cfg.Faults.Event(l.v.now, fault.EvIRQ, func(act fault.Action) bool {
		return act.IRQ.Source == l.src
	})
	if err == nil && ok {
		err = l.v.cfg.Faults.Commit(a)
		l.blocked = true
		if a.Kind == "irq_delay" {
			l.v.sched.Schedule(l.v.now+a.Delay, l.v.self, evIRQRaise, uint64(l.src)|l.gen<<8)
		}
	}
	l.v.noteFault(err)
	if !l.blocked {
		l.inner.SetLevel(true)
	}
}

// raiseDelayed asserts a delayed interrupt if its line is still asserted.
func (v *VM) raiseDelayed(arg uint64) {
	l := v.irqLines[uint32(arg&0xff)]
	if l != nil && l.level && l.blocked && l.gen == arg>>8 {
		l.blocked = false
		l.inner.SetLevel(true)
	}
}

// line returns PLIC source src, through the fault layer when injecting.
func (v *VM) line(src uint32) devices.IRQLine {
	inner := v.plic.Line(int(src))
	if v.cfg.Faults == nil {
		return inner
	}
	l := &irqLine{v: v, src: src, inner: inner}
	if v.irqLines == nil {
		v.irqLines = map[uint32]*irqLine{}
	}
	v.irqLines[src] = l
	return l
}

// noteFault keeps the first error from a device-side fault hook; Run
// returns it.
func (v *VM) noteFault(err error) {
	if err != nil && v.faultErr == nil {
		v.faultErr = err
	}
}

// installFaults hooks the injector into the devices and sets breakpoints
// for its pc triggers.
func (v *VM) installFaults() error {
	inj := v.cfg.Faults
	if inj == nil {
		return nil
	}
	v.uart.SetTxHook(func(b byte) (byte, bool) {
		return v.uartFault(fault.EvUARTTx, b)
	})
	for _, pc := range inj.PCs() {
		id, err := v.m.AddBreakpoint(pc, 2, machine.BreakExec)
		if err != nil {
			return err
		}
		if v.faultBPs == nil {
			v.faultBPs = map[uint32]bool{}
		}
		v.faultBPs[id] = true
	}
	return nil
}

// blkFault is the virtio-blk device's fault hook.
func (v *VM) blkFault(typ uint32) (fail, corrupt bool, off uint64, xor uint8) {
	a, ok, err := v.cfg.Faults.Event(v.now, fault.EvBlk, func(act fault.Action) bool {
		return act.Blk.Fail || typ == blk.ReqIn // corruption needs a read
	})
	if err == nil && ok {
		err = v.cfg.Faults.Commit(a)
	}
	v.noteFault(err)
	if !ok {
		return false, false, 0, 0
	}
	return a.Kind == "blk_fail", a.Kind == "blk_corrupt", a.Offset, a.Xor
}

// uartFault applies a UART fault to byte b, if one is due; it returns the
// byte to use and whether to keep it.
func (v *VM) uartFault(kind string, b byte) (byte, bool) {
	if v.cfg.Faults == nil {
		return b, true
	}
	a, ok, err := v.cfg.Faults.Event(v.now, kind, func(fault.Action) bool { return true })
	if err != nil || !ok {
		v.noteFault(err)
		return b, true
	}
	a.Value = uint64(b)
	v.noteFault(v.cfg.Faults.Commit(a))
	if a.Kind == "uart_drop" {
		return b, false
	}
	return b ^ a.Xor, true
}

// mmioFault lets the injector count an MMIO access and override a read.
func (v *VM) mmioFault(addr uint64, size int, write bool, val *uint64) {
	inj := v.cfg.Faults
	if inj == nil {
		return
	}
	name := ""
	if r := v.bus.Region(addr, size); r != nil {
		name = r.Name
	}
	a, ok, err := inj.Access(v.now, name, addr, write)
	if err == nil && ok && !write {
		a.Addr = addr
		*val = a.Value
		err = inj.Commit(a)
	}
	v.noteFault(err)
}

// boundaryFaults applies the immediate faults due at a clean boundary.
func (v *VM) boundaryFaults() error {
	inj := v.cfg.Faults
	if inj == nil || v.mmioInFlight {
		return nil
	}
	var pc uint64
	if inj.HasPCTriggers() {
		var err error
		if pc, err = v.m.Reg(machine.RegPC); err != nil {
			return err
		}
	}
	return inj.Boundary(v.now, v.instret, pc, v.applyFault)
}

// applyFault carries out an immediate fault and returns its concrete record.
func (v *VM) applyFault(a fault.Applied) (fault.Applied, error) {
	switch a.Kind {
	case "flip_bit":
		var b [1]byte
		if err := v.m.ReadPhys(a.Addr, b[:]); err != nil {
			return a, err
		}
		b[0] ^= 1 << a.Bit
		return a, v.m.WritePhys(a.Addr, b[:])
	case "reg":
		reg := uint32(a.Reg)
		if a.Reg == 32 {
			reg = machine.RegPC
		}
		if a.Xor == 1 { // "xor" from a spec: resolve against the register now
			cur, err := v.m.Reg(reg)
			if err != nil {
				return a, err
			}
			a.Value, a.Xor = cur^a.Value, 0
		}
		if a.Reg == 0 {
			return a, nil // x0 stays zero
		}
		return a, v.m.SetReg(reg, a.Value)
	}
	return a, nil
}

// faultDeadline is the injector's next stop, ignored while an MMIO access
// is in flight (the boundary that applies it comes after).
func (v *VM) faultDeadline() uint64 {
	if v.cfg.Faults == nil || v.mmioInFlight {
		return ^uint64(0)
	}
	return max(v.cfg.Faults.Deadline(v.now, v.instret), v.now+1)
}
