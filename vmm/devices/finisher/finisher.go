// Package finisher models the SiFive test finisher ("sifive,test0") that
// QEMU's virt machine places at 0x10_0000: a 32-bit write of 0x5555 powers
// off, 0x3333 | code<<16 powers off with a failure code, and 0x7777 resets.
// OpenSBI drives it through its syscon-poweroff and syscon-reboot drivers,
// which is how a Linux guest's poweroff and reboot reach the VMM.
package finisher

// Size is the length of the register window.
const Size = 0x1000

const (
	statusFail  = 0x3333
	statusPass  = 0x5555
	statusReset = 0x7777
)

// Action is what the guest asked for.
type Action int

const (
	// PowerOff stops the machine; the exit code accompanies it.
	PowerOff Action = iota
	// Reset asks for a reboot.
	Reset
)

// Finisher reports guest power-off and reset requests to a callback, which
// runs on the run-loop goroutine during the store that made the request.
type Finisher struct {
	finish func(a Action, code uint64)
}

// New returns a finisher reporting to finish.
func New(finish func(a Action, code uint64)) *Finisher { return &Finisher{finish: finish} }

// Read returns 0: the register reads as zero (QEMU sifive_test).
func (f *Finisher) Read(offset uint64, size int) uint64 { return 0 }

// Write acts on a 32-bit write at offset 0; other writes are ignored.
func (f *Finisher) Write(offset uint64, size int, v uint64) {
	if offset != 0 || size != 4 {
		return
	}
	switch v & 0xffff {
	case statusPass:
		f.finish(PowerOff, 0)
	case statusFail:
		f.finish(PowerOff, (v>>16)&0xffff)
	case statusReset:
		f.finish(Reset, 0)
	}
}

// Reset does nothing: the finisher has no state.
func (f *Finisher) Reset() {}

// Snapshot returns no state.
func (f *Finisher) Snapshot() []byte { return nil }

// Restore accepts only the empty state.
func (f *Finisher) Restore(state []byte) error { return nil }
