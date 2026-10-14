// Package vm assembles a machine (the C++ core) with its devices and runs
// it: the run loop owns all guest state and is the only goroutine that
// touches it (determinism rule 1).
package vm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ezekielelvis/rvsim/vmm/devices"
	"github.com/ezekielelvis/rvsim/vmm/devices/plic"
	"github.com/ezekielelvis/rvsim/vmm/devices/uart"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/sched"
	"github.com/ezekielelvis/rvsim/vmm/trace"
)

// Memory map (QEMU virt layout, ADR-002).
const (
	PLICBase = 0x0c00_0000
	UARTBase = 0x1000_0000
	UARTIRQ  = 10 // PLIC source
)

// InputSpacing is the virtual time, in steps, between successive host input
// bytes delivered to the UART, so a burst of input arrives at a pace the
// guest can drain instead of overrunning its 16-byte FIFO.
const InputSpacing = 10_000

// maxBatch bounds one rv_run call so host input is polled regularly.
const maxBatch = 1 << 20

// Config describes a VM.
type Config struct {
	Machine machine.Config
	// Console receives UART output and HTIF console output.
	Console io.Writer
	// Input supplies host bytes for the UART (e.g. stdin). Nil means none;
	// closing it signals end of input.
	Input <-chan byte
	// HTIF services tohost commands (Machine.HTIFToHost must be set).
	HTIF bool
	// Trace, if set, receives a Spike --log-commits style commit log.
	Trace io.Writer
}

// Result describes why Run returned.
type Result struct {
	Exited   bool   // the guest exited through HTIF
	ExitCode uint64 // HTIF exit code (tohost >> 1)
	Steps    uint64
	Instret  uint64
}

// ErrIdleForever is returned when the hart waits for an interrupt that
// nothing can raise.
var ErrIdleForever = errors.New("vm: hart is idle and nothing can wake it")

// Event kinds for the VM's own scheduler target.
const (
	evInputByte = iota // deliver the next queued input byte
	evInject           // queue an injected chunk (arg = index)
)

// VM is a machine with its devices, scheduler and run loop.
type VM struct {
	cfg   Config
	m     *machine.Machine
	bus   devices.Bus
	sched sched.Scheduler
	plic  *plic.PLIC
	uart  *uart.UART
	trace *trace.SpikeWriter

	now       uint64
	instret   uint64 // retired instructions at the last exit
	self      sched.Target
	input     []byte   // host bytes waiting for the UART
	inputBusy bool     // an evInputByte event is scheduled
	injected  [][]byte // chunks queued by ScheduleInput, by index
	inputOpen bool
}

// New creates a VM.
func New(cfg Config) (*VM, error) {
	m, err := machine.New(cfg.Machine)
	if err != nil {
		return nil, err
	}
	v := &VM{cfg: cfg, m: m, inputOpen: cfg.Input != nil}
	v.plic = plic.New(func(lines uint64) {
		// rv_set_irq_lines only fails on invalid bits, which the PLIC never sets.
		_ = m.SetIRQLines(lines)
	})
	v.uart = uart.New(cfg.Console, v.plic.Line(UARTIRQ))
	for _, d := range []struct {
		name       string
		base, size uint64
		dev        devices.Device
	}{
		{"plic", PLICBase, plic.Size, v.plic},
		{"uart", UARTBase, uart.Size, v.uart},
	} {
		if err := v.bus.Map(d.name, d.base, d.size, d.dev); err != nil {
			m.Close()
			return nil, err
		}
	}
	v.self = v.sched.Register("vm", v)
	if cfg.Trace != nil {
		if err := m.EnableTrace(1 << 20); err != nil {
			m.Close()
			return nil, err
		}
		v.trace = trace.NewSpikeWriter(cfg.Trace)
	}
	return v, nil
}

// Close releases the machine.
func (v *VM) Close() { v.m.Close() }

// Machine returns the core, for loading images and inspecting state.
func (v *VM) Machine() *machine.Machine { return v.m }

// Bus returns the device bus.
func (v *VM) Bus() *devices.Bus { return &v.bus }

// Scheduler returns the event scheduler, for devices that need timed events.
func (v *VM) Scheduler() *sched.Scheduler { return &v.sched }

// Now returns the current virtual time in steps.
func (v *VM) Now() uint64 { return v.now }

// ScheduleInput delivers data to the UART starting at virtual time at, as if
// typed by the host. Use it for deterministic tests and replays.
func (v *VM) ScheduleInput(at uint64, data []byte) {
	v.injected = append(v.injected, append([]byte(nil), data...))
	v.sched.Schedule(at, v.self, evInject, uint64(len(v.injected)-1))
}

// HandleEvent implements sched.Handler.
func (v *VM) HandleEvent(kind uint32, arg uint64) {
	switch kind {
	case evInject:
		v.queueInput(v.injected[arg])
		v.injected[arg] = nil
	case evInputByte:
		v.inputBusy = false
		if len(v.input) == 0 {
			return
		}
		// Hold input back while the guest's FIFO is full (flow control),
		// rather than losing it to an overrun.
		if v.uart.RxSpace() > 0 {
			v.uart.Receive(v.input[0])
			v.input = v.input[1:]
		}
		if len(v.input) > 0 {
			v.inputBusy = true
			v.sched.Schedule(v.now+InputSpacing, v.self, evInputByte, 0)
		}
	}
}

func (v *VM) queueInput(b []byte) {
	v.input = append(v.input, b...)
	if !v.inputBusy && len(v.input) > 0 {
		v.inputBusy = true
		v.sched.Schedule(v.now, v.self, evInputByte, 0)
	}
}

// pollInput moves host input that has already arrived into the queue,
// delivering it from the current virtual time. It never blocks.
func (v *VM) pollInput() {
	for v.inputOpen {
		select {
		case b, ok := <-v.cfg.Input:
			if !ok {
				v.inputOpen = false
				return
			}
			v.queueInput([]byte{b})
		default:
			return
		}
	}
}

// waitInput blocks until host input arrives or ends.
func (v *VM) waitInput() {
	b, ok := <-v.cfg.Input
	if !ok {
		v.inputOpen = false
		return
	}
	v.queueInput([]byte{b})
}

// Run executes until the guest exits through HTIF, maxSteps of virtual time
// have passed (Result.Exited false), or an error occurs.
func (v *VM) Run(maxSteps uint64) (Result, error) {
	limit := v.now + maxSteps
	if limit < v.now {
		limit = ^uint64(0)
	}
	for {
		v.sched.RunDue(v.now)
		v.pollInput()
		if v.now >= limit {
			return v.result(false, 0), nil
		}
		budget := min(uint64(maxBatch), limit-v.now)
		if next, ok := v.sched.Next(); ok {
			if next <= v.now {
				continue
			}
			budget = min(budget, next-v.now)
		}
		exit, err := v.m.Run(budget)
		if err != nil {
			return v.result(false, 0), err
		}
		v.now, v.instret = exit.Steps, exit.Instret
		if err := v.drainTrace(); err != nil {
			return v.result(false, 0), err
		}
		switch exit.Reason {
		case machine.ExitBudget, machine.ExitTraceFull:
		case machine.ExitMMIORead:
			var val uint64
			d, off, ok := v.bus.Lookup(exit.Addr, int(exit.Size))
			if ok {
				val = d.Read(off, int(exit.Size))
			}
			if err := v.m.CompleteMMIO(val, !ok); err != nil {
				return v.result(false, 0), err
			}
		case machine.ExitMMIOWrite:
			d, off, ok := v.bus.Lookup(exit.Addr, int(exit.Size))
			if ok {
				d.Write(off, int(exit.Size), exit.Value)
			}
			if err := v.m.CompleteMMIO(0, !ok); err != nil {
				return v.result(false, 0), err
			}
		case machine.ExitWFI:
			if err := v.idle(exit, limit); err != nil {
				return v.result(false, 0), err
			}
		case machine.ExitHalt:
			if !v.cfg.HTIF {
				return v.result(false, 0), fmt.Errorf("vm: HTIF command %#x but HTIF is not enabled", exit.Value)
			}
			done, code, err := v.handleHTIF(exit.Value)
			if err != nil || done {
				if ferr := v.flushTrace(); err == nil {
					err = ferr
				}
				return v.result(done, code), err
			}
		default:
			return v.result(false, 0), fmt.Errorf("vm: unexpected exit %v at pc %#x", exit.Reason, exit.PC)
		}
	}
}

// idle fast-forwards virtual time while the hart waits for an interrupt: to
// the next scheduled event or the CLINT timer deadline, whichever is first.
func (v *VM) idle(exit machine.Exit, limit uint64) error {
	target := ^uint64(0)
	if next, ok := v.sched.Next(); ok {
		target = next
	}
	if exit.TimerStep > v.now && exit.TimerStep < target {
		target = exit.TimerStep
	}
	if target == ^uint64(0) {
		if !v.inputOpen {
			return ErrIdleForever
		}
		// Only the host can wake the guest: wait for it. Virtual time does
		// not move, so the byte arrives at a deterministic step.
		v.waitInput()
		return nil
	}
	return v.m.AdvanceIdle(min(target, limit) - v.now)
}

func (v *VM) result(exited bool, code uint64) Result {
	return Result{Exited: exited, ExitCode: code, Steps: v.now, Instret: v.instret}
}

func (v *VM) drainTrace() error {
	if v.trace == nil {
		return nil
	}
	recs, err := v.m.TraceRecords()
	if err != nil {
		return err
	}
	if err := v.trace.Write(recs); err != nil {
		return err
	}
	return v.m.ConsumeTrace()
}

func (v *VM) flushTrace() error {
	if v.trace == nil {
		return nil
	}
	return v.trace.Flush()
}

// HTIF commands are (device << 56) | (command << 48) | payload, following
// riscv-isa-sim's fesvr (ADR-013).
const (
	htifDevSyscall = 0
	htifDevConsole = 1
	htifCmdPutchar = 1
)

// handleHTIF services one tohost command. It reports done with the exit
// code when the program has finished. Other commands are acknowledged by
// clearing tohost (and, for console output, writing fromhost).
func (v *VM) handleHTIF(cmd uint64) (done bool, code uint64, err error) {
	dev, op, payload := cmd>>56, (cmd>>48)&0xff, cmd&(1<<48-1)
	switch {
	case dev == htifDevSyscall && op == 0 && payload&1 == 1:
		return true, payload >> 1, nil
	case dev == htifDevConsole && op == htifCmdPutchar:
		if v.cfg.Console != nil {
			if _, err := v.cfg.Console.Write([]byte{byte(payload)}); err != nil {
				return false, 0, err
			}
		}
		if err := v.writeU64(v.cfg.Machine.HTIFToHost, 0); err != nil {
			return false, 0, err
		}
		if v.cfg.Machine.HTIFFromHost != 0 {
			// fesvr acknowledges putchar with 0x100 | char.
			return false, 0, v.writeU64(v.cfg.Machine.HTIFFromHost, dev<<56|op<<48|0x100|payload&0xff)
		}
		return false, 0, nil
	default:
		return false, 0, fmt.Errorf("vm: unsupported HTIF command %#x (device %d, command %d)", cmd, dev, op)
	}
}

func (v *VM) writeU64(addr, val uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], val)
	return v.m.WritePhys(addr, b[:])
}
