package vm

import (
	"errors"
	"fmt"
	"io"

	"github.com/ezekielelvis/rvsim/vmm/devices/virtio/blk"
	"github.com/ezekielelvis/rvsim/vmm/sched"
)

// State is everything a VM holds besides RAM and disk contents: the core's
// snapshot blob, each device's state, the pending events, and the run
// loop's input state (ADR-029).
type State struct {
	Core        []byte
	Devices     []DeviceState
	Events      []sched.NamedEvent
	Seq         uint64
	Now         uint64
	Instret     uint64
	Input       []byte   // queued for the UART
	InputBusy   bool     // an input byte event is pending
	InputOpen   bool     // host input has not ended
	Injected    [][]byte // ScheduleInput chunks; nil once delivered
	Delivered   bool     // live input was delivered at step DeliveredAt
	DeliveredAt uint64
}

// DeviceState is one mapped device's snapshot.
type DeviceState struct {
	Name string
	Data []byte
}

// SaveState captures the VM between Run calls, where the run loop is at a
// clean instruction boundary.
func (v *VM) SaveState() (*State, error) {
	if v.mmioInFlight || len(v.held) > 0 || v.heldEOF || v.finished != nil {
		return nil, errors.New("vm: SaveState inside a run-loop step")
	}
	core, err := v.m.SnapshotCore()
	if err != nil {
		return nil, err
	}
	st := &State{
		Core: core, Events: v.sched.Named(), Seq: v.sched.Seq(), Now: v.now, Instret: v.instret,
		Input: append([]byte(nil), v.input...), InputBusy: v.inputBusy, InputOpen: v.inputOpen,
		Delivered: v.delivered, DeliveredAt: v.deliveredAt,
	}
	for _, c := range v.injected {
		st.Injected = append(st.Injected, append([]byte(nil), c...))
	}
	for _, r := range v.bus.Regions() {
		st.Devices = append(st.Devices, DeviceState{Name: r.Name, Data: r.Device.Snapshot()})
	}
	return st, nil
}

// RestoreState loads a State into a VM created with the same configuration
// as the one saved, whose RAM already holds the saved contents. Checkpoints
// resume at the next multiple of CheckpointEvery; replay resumes with the
// first recorded input after the saved step.
func (v *VM) RestoreState(st *State) error {
	if len(st.Devices) != len(v.bus.Regions()) {
		return fmt.Errorf("vm: restore: %d devices saved, %d present", len(st.Devices), len(v.bus.Regions()))
	}
	for i, r := range v.bus.Regions() {
		if st.Devices[i].Name != r.Name {
			return fmt.Errorf("vm: restore: device %q saved where %q is mapped", st.Devices[i].Name, r.Name)
		}
	}
	if err := v.m.RestoreCore(st.Core); err != nil {
		return err
	}
	for i, r := range v.bus.Regions() {
		if err := r.Device.Restore(st.Devices[i].Data); err != nil {
			return fmt.Errorf("vm: restore %s: %w", r.Name, err)
		}
	}
	if err := v.sched.Restore(st.Events, st.Seq); err != nil {
		return err
	}
	v.now, v.instret = st.Now, st.Instret
	v.input = append(v.input[:0], st.Input...)
	// Whether host input is open describes the session, not the guest: a
	// restored VM with live input attached starts with input open, and one
	// with neither input nor a replay script with it closed. A replay
	// follows the snapshot, which reflects the recording's end of input.
	v.inputBusy, v.inputOpen = st.InputBusy, st.InputOpen
	if v.cfg.Input != nil {
		v.inputOpen = true
	} else if v.cfg.Replay == nil {
		v.inputOpen = false
	}
	v.injected = nil
	for _, c := range st.Injected {
		v.injected = append(v.injected, append([]byte(nil), c...))
	}
	v.delivered, v.deliveredAt = st.Delivered, st.DeliveredAt
	v.mmioInFlight, v.held, v.heldEOF, v.finished = false, v.held[:0], false, nil
	if e := v.cfg.CheckpointEvery; e != 0 {
		v.nextCkpt = (v.now/e + 1) * e
	}
	if r := v.cfg.Replay; r != nil {
		v.scriptPos = 0
		for v.scriptPos < len(r.Events) && r.Events[v.scriptPos].Step <= v.now {
			v.scriptPos++
		}
	}
	return nil
}

// SetConsole redirects the UART's output.
func (v *VM) SetConsole(w io.Writer) {
	v.cfg.Console = w
	v.uart.SetOutput(w)
}

// Disk returns the VM's virtio disk configuration, or nil.
func (v *VM) Disk() *blk.Config { return v.cfg.Disk }

// Config returns the configuration the VM was created with.
func (v *VM) Config() Config { return v.cfg }
