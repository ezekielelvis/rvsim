package vm

import (
	"encoding/binary"
	"fmt"

	"github.com/zeebo/xxh3"

	"github.com/ezekielelvis/rvsim/vmm/machine"
)

// Recorder receives the VM's external inputs as they enter it (record
// mode, ADR-028). Steps are virtual time; an error stops Run.
type Recorder interface {
	// Input reports host bytes queued for the UART at step.
	Input(step uint64, data []byte) error
	// InputEOF reports that host input ended at step.
	InputEOF(step uint64) error
}

// InputEvent is one recorded input: bytes, or the end of input.
type InputEvent struct {
	Step uint64
	Data []byte
	EOF  bool
}

// InputScript is recorded input to replay in place of Config.Input.
type InputScript struct {
	Open   bool         // host input was attached when recording began
	Events []InputEvent // in step order
}

// ErrReplayDiverged reports that replay could not deliver recorded input
// where it was recorded.
type ErrReplayDiverged struct {
	Step, Now uint64
}

func (e *ErrReplayDiverged) Error() string {
	return fmt.Sprintf("vm: replay missed input recorded at step %d (now %d)", e.Step, e.Now)
}

// VMMHash returns the XXH3-64 hash of the VMM's guest-affecting state:
// every mapped device's snapshot (by base address), the pending scheduler
// events with their targets' names, the scheduler's sequence counter, and
// the VM's own input state. Together with the core and RAM hashes it covers
// the whole machine.
func (v *VM) VMMHash() uint64 {
	h := xxh3.New()
	var b [8]byte
	u64 := func(x uint64) {
		binary.LittleEndian.PutUint64(b[:], x)
		_, _ = h.Write(b[:])
	}
	bytes := func(p []byte) {
		u64(uint64(len(p)))
		_, _ = h.Write(p)
	}
	flag := func(f bool) {
		if f {
			u64(1)
		} else {
			u64(0)
		}
	}
	u64(vmmHashFormat)
	for _, r := range v.bus.Regions() {
		bytes([]byte(r.Name))
		u64(r.Base)
		bytes(r.Device.Snapshot())
	}
	for _, e := range v.sched.Pending() {
		bytes([]byte(v.sched.Name(e.Target)))
		u64(e.At)
		u64(e.Seq)
		u64(uint64(e.Kind))
		u64(e.Arg)
	}
	u64(v.sched.Seq())
	bytes(v.input)
	flag(v.inputBusy)
	flag(v.inputOpen)
	u64(uint64(len(v.injected)))
	for _, c := range v.injected {
		bytes(c) // nil once delivered
	}
	return h.Sum64()
}

// vmmHashFormat versions VMMHash's serialization.
const vmmHashFormat = 1

// Hashes are the state hashes compared at replay checkpoints.
type Hashes struct {
	Core, RAM, VMM uint64
}

// StateHashes returns the core, RAM and VMM hashes.
func (v *VM) StateHashes() (Hashes, error) {
	core, err := v.m.StateHash(machine.HashCore)
	if err != nil {
		return Hashes{}, err
	}
	ram, err := v.m.StateHash(machine.HashRAM)
	if err != nil {
		return Hashes{}, err
	}
	return Hashes{Core: core, RAM: ram, VMM: v.VMMHash()}, nil
}

// Instret returns the instructions retired so far.
func (v *VM) Instret() uint64 { return v.instret }
