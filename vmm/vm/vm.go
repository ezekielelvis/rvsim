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
	"github.com/ezekielelvis/rvsim/vmm/devices/finisher"
	"github.com/ezekielelvis/rvsim/vmm/devices/plic"
	"github.com/ezekielelvis/rvsim/vmm/devices/uart"
	"github.com/ezekielelvis/rvsim/vmm/devices/virtio"
	"github.com/ezekielelvis/rvsim/vmm/devices/virtio/blk"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/sched"
	"github.com/ezekielelvis/rvsim/vmm/trace"
)

// Memory map (QEMU virt layout, ADR-002).
const (
	FinisherBase = 0x0010_0000 // SiFive test finisher (power-off/reset)
	PLICBase     = 0x0c00_0000
	UARTBase     = 0x1000_0000
	UARTIRQ      = 10 // PLIC source
	// VirtioBase is the virtio-mmio block device (present with Config.Disk)
	// at PLIC source VirtioIRQ: QEMU virt's first virtio slot.
	VirtioBase = 0x1000_1000
	VirtioIRQ  = 1
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
	// Disk, if set, adds a virtio block device backed by Disk.Disk.
	Disk *blk.Config

	// Record, if set, receives every external input as it enters the VM.
	Record Recorder
	// Replay, if set, supplies recorded input; Input must then be nil.
	Replay *InputScript
	// Checkpoint, if set, is called at every multiple of CheckpointEvery
	// steps, at a run-loop boundary; an error stops Run.
	CheckpointEvery uint64
	Checkpoint      func(step uint64) error
}

// Result describes why Run returned.
type Result struct {
	Exited   bool   // the guest exited through HTIF or the test finisher
	ExitCode uint64 // HTIF exit code (tohost >> 1) or finisher failure code
	Reset    bool   // the guest asked the finisher for a reset (with Exited)
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

	finished *Result // set by the finisher during an MMIO store

	now       uint64
	instret   uint64 // retired instructions at the last exit
	self      sched.Target
	input     []byte   // host bytes waiting for the UART
	inputBusy bool     // an evInputByte event is scheduled
	injected  [][]byte // chunks queued by ScheduleInput, by index
	inputOpen bool

	// An MMIO access was completed but its instruction has not retired yet:
	// the next rv_run retires it. Host input is accepted only when this is
	// false, at instruction boundaries replay can reproduce (ADR-028).
	mmioInFlight bool
	pollBuf      []byte
	held         []byte // live: host bytes taken while idle, delivered at the next poll
	heldEOF      bool
	delivered    bool // live: input was delivered at step deliveredAt
	deliveredAt  uint64
	scriptPos    int    // next InputScript event to deliver
	nextCkpt     uint64 // step of the next checkpoint
}

// New creates a VM.
func New(cfg Config) (*VM, error) {
	m, err := machine.New(cfg.Machine)
	if err != nil {
		return nil, err
	}
	if cfg.Replay != nil && cfg.Input != nil {
		m.Close()
		return nil, errors.New("vm: Replay and Input are mutually exclusive")
	}
	if (cfg.CheckpointEvery == 0) != (cfg.Checkpoint == nil) {
		m.Close()
		return nil, errors.New("vm: CheckpointEvery and Checkpoint go together")
	}
	v := &VM{cfg: cfg, m: m, inputOpen: cfg.Input != nil || (cfg.Replay != nil && cfg.Replay.Open)}
	v.nextCkpt = cfg.CheckpointEvery
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
		{"finisher", FinisherBase, finisher.Size, finisher.New(v.finish)},
		{"plic", PLICBase, plic.Size, v.plic},
		{"uart", UARTBase, uart.Size, v.uart},
	} {
		if err := v.bus.Map(d.name, d.base, d.size, d.dev); err != nil {
			m.Close()
			return nil, err
		}
	}
	if cfg.Disk != nil {
		d, err := blk.New(*cfg.Disk, m, v.plic.Line(VirtioIRQ), &v.sched, v.Now)
		if err == nil {
			err = v.bus.Map("virtio-blk", VirtioBase, virtio.Size, d)
		}
		if err != nil {
			m.Close()
			return nil, err
		}
	}
	v.self = v.sched.Register("vm", v)
	if cfg.Trace != nil {
		if err := v.EnableTrace(cfg.Trace); err != nil {
			m.Close()
			return nil, err
		}
	}
	return v, nil
}

// EnableTrace starts writing a Spike --log-commits style commit log of every
// instruction retired from now on to w. Tracing does not affect the guest.
func (v *VM) EnableTrace(w io.Writer) error {
	if err := v.m.EnableTrace(1 << 20); err != nil {
		return err
	}
	v.trace = trace.NewSpikeWriter(w)
	return nil
}

// HasDisk reports whether the VM has a virtio block device.
func (v *VM) HasDisk() bool { return v.cfg.Disk != nil }

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
// delivering it from the current virtual time. It never blocks. In replay
// it delivers the recorded input due now instead.
func (v *VM) pollInput() error {
	if v.cfg.Replay != nil {
		return v.pollScript()
	}
	// Host input enters at most once per step, at a run-loop boundary with
	// no MMIO access in flight: points replay reproduces exactly (ADR-028).
	if v.mmioInFlight || !v.inputOpen || (v.delivered && v.deliveredAt == v.now) {
		return nil
	}
	buf, eof := append(v.pollBuf[:0], v.held...), v.heldEOF
	v.held, v.heldEOF = v.held[:0], false
poll:
	for !eof {
		select {
		case b, ok := <-v.cfg.Input:
			if !ok {
				eof = true
				break poll
			}
			buf = append(buf, b)
		default:
			break poll
		}
	}
	v.pollBuf = buf
	if len(buf) > 0 || eof {
		v.delivered, v.deliveredAt = true, v.now
	}
	return v.hostInput(buf, eof)
}

// waitInput blocks until host input arrives or ends, and holds it for the
// next poll: input is delivered only at run-loop boundaries.
func (v *VM) waitInput() error {
	if len(v.held) > 0 || v.heldEOF {
		return fmt.Errorf("vm: internal error: idle at step %d with host input held", v.now)
	}
	b, ok := <-v.cfg.Input
	if ok {
		v.held = append(v.held, b)
	} else {
		v.heldEOF = true
	}
	return nil
}

// hostInput records and queues host bytes and the end of input.
func (v *VM) hostInput(data []byte, eof bool) error {
	if len(data) > 0 {
		if v.cfg.Record != nil {
			if err := v.cfg.Record.Input(v.now, data); err != nil {
				return err
			}
		}
		v.queueInput(data)
	}
	if eof {
		v.inputOpen = false
		if v.cfg.Record != nil {
			return v.cfg.Record.InputEOF(v.now)
		}
	}
	return nil
}

// pollScript delivers the recorded input due at the current step.
func (v *VM) pollScript() error {
	evs := v.cfg.Replay.Events
	for v.scriptPos < len(evs) && evs[v.scriptPos].Step <= v.now {
		e := evs[v.scriptPos]
		if e.Step < v.now || v.mmioInFlight {
			return &ErrReplayDiverged{Step: e.Step, Now: v.now}
		}
		v.scriptPos++
		if e.EOF {
			v.inputOpen = false
		} else {
			v.queueInput(e.Data)
		}
	}
	return nil
}

// deadline is the step the run loop must stop at next: the run limit, the
// next checkpoint, or the next recorded input, whichever comes first.
func (v *VM) deadline(limit uint64) uint64 {
	d := limit
	if v.cfg.CheckpointEvery != 0 {
		d = min(d, v.nextCkpt)
	}
	if r := v.cfg.Replay; r != nil && v.scriptPos < len(r.Events) {
		d = min(d, max(r.Events[v.scriptPos].Step, v.now))
	}
	return d
}

// checkpoint calls Config.Checkpoint when the run loop reaches its step.
func (v *VM) checkpoint() error {
	if v.cfg.CheckpointEvery == 0 || v.now < v.nextCkpt {
		return nil
	}
	step := v.nextCkpt
	v.nextCkpt += v.cfg.CheckpointEvery
	if v.now != step {
		return fmt.Errorf("vm: internal error: run loop passed checkpoint step %d (now %d)", step, v.now)
	}
	return v.cfg.Checkpoint(step)
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
		// Checkpoint before input: a step's checkpoint never includes input
		// delivered at that step, however the loop arrived there.
		if err := v.checkpoint(); err != nil {
			return v.result(false, 0), err
		}
		if err := v.pollInput(); err != nil {
			return v.result(false, 0), err
		}
		if v.now >= limit {
			return v.result(false, 0), nil
		}
		budget := min(uint64(maxBatch), v.deadline(limit)-v.now)
		if v.mmioInFlight && v.inputOpen && v.cfg.Replay == nil {
			// Retire just the completed MMIO instruction, so host input
			// finds a clean boundary even in a guest that polls a device in
			// a tight loop (ADR-028).
			budget = 1
		}
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
		v.mmioInFlight = false // the completed access, if any, has retired
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
			v.mmioInFlight = true
		case machine.ExitMMIOWrite:
			d, off, ok := v.bus.Lookup(exit.Addr, int(exit.Size))
			if ok {
				d.Write(off, int(exit.Size), exit.Value)
			}
			if err := v.m.CompleteMMIO(0, !ok); err != nil {
				return v.result(false, 0), err
			}
			v.mmioInFlight = true
			if f := v.finished; f != nil {
				return v.finishRun(*f)
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
		switch {
		case !v.inputOpen:
			return ErrIdleForever
		case v.cfg.Replay != nil:
			// Recorded input will wake the guest; deadline stops there.
			if v.scriptPos == len(v.cfg.Replay.Events) {
				return errors.New("vm: replay: the guest waits for input the recording does not have")
			}
		default:
			// Only the host can wake the guest: wait for it. Virtual time
			// does not move, so the byte arrives at a deterministic step.
			return v.waitInput()
		}
	}
	n := min(target, v.deadline(limit)) - v.now
	if err := v.m.AdvanceIdle(n); err != nil {
		return err
	}
	v.now += n // the loop's view of time must follow the core's
	return nil
}

// finish records a finisher request; Run returns once the store completes.
func (v *VM) finish(a finisher.Action, code uint64) {
	v.finished = &Result{ExitCode: code, Reset: a == finisher.Reset}
}

// finishRun retires the store that reached the finisher (MMIO accesses
// complete by re-execution, ADR-022) and ends the run.
func (v *VM) finishRun(f Result) (Result, error) {
	v.finished = nil
	exit, err := v.m.Run(1)
	if err != nil {
		return v.result(false, 0), err
	}
	v.now, v.instret = exit.Steps, exit.Instret
	if err := v.drainTrace(); err != nil {
		return v.result(false, 0), err
	}
	res := v.result(true, f.ExitCode)
	res.Reset = f.Reset
	return res, v.flushTrace()
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
