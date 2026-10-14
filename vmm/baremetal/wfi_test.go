package baremetal

import (
	"encoding/binary"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/loader"
)

// Minimal RV64 encoders for hand-written test programs.
func encI(imm int32, rs1, f3, rd, op uint32) uint32 {
	return uint32(imm&0xfff)<<20 | rs1<<15 | f3<<12 | rd<<7 | op
}
func encS(imm int32, rs2, rs1, f3 uint32) uint32 {
	u := uint32(imm)
	return (u>>5&0x7f)<<25 | rs2<<20 | rs1<<15 | f3<<12 | (u&0x1f)<<7 | 0x23
}
func encU(imm20 uint32, rd, op uint32) uint32 { return imm20<<12 | rd<<7 | op }

func program(insns ...uint32) []byte {
	b := make([]byte, 4*len(insns))
	for i, w := range insns {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	return b
}

// A program that waits in WFI for the CLINT timer: the runner must skip idle
// virtual time to the timer deadline rather than fail or spin.
func TestWFIWakesOnTimer(t *testing.T) {
	const (
		t0, t1, t2 = 5, 6, 7
		base       = 0x8000_0000
		tohost     = 0x8000_1000
	)
	code := program(
		encU(0x2004, t0, 0x37),      // lui  t0, 0x2004        -> CLINT mtimecmp
		encI(100, 0, 0, t1, 0x13),   // addi t1, zero, 100
		encS(0, t1, t0, 3),          // sd   t1, 0(t0)          mtimecmp = 100
		encI(0x80, 0, 0, t2, 0x13),  // addi t2, zero, 0x80     MTIE
		encI(0x304, t2, 1, 0, 0x73), // csrw mie, t2
		0x10500073,                  // wfi                     (MIE=0: wake, no trap)
		encU(0x1, t0, 0x17),         // auipc t0, 1             -> base + 0x18 + 0x1000
		encI(1, 0, 0, t1, 0x13),     // addi t1, zero, 1
		encS(-0x18, t1, t0, 3),      // sd   t1, -0x18(t0)      tohost = 1 (pass)
		0x0000006f,                  // j .
	)
	img := &loader.Image{
		Entry:    base,
		Segments: []loader.Segment{{Addr: base, Data: code, MemSize: uint64(len(code))}},
		Symbols:  map[string]uint64{"tohost": tohost},
	}
	res, err := Run(img, Options{MaxSteps: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d", res.ExitCode)
	}
	// mtime reaches 100 after 100 ticks of 10 steps (DefaultConfig).
	if res.Steps < 1000 {
		t.Fatalf("finished at step %d, before the timer deadline at 1000", res.Steps)
	}
	if res.Instret != 9 {
		t.Fatalf("retired %d instructions, want 9 (idle time retires nothing)", res.Instret)
	}
}

func TestWFIWithNoWakeSourceFails(t *testing.T) {
	const base = 0x8000_0000
	code := program(
		encU(0x2004, 5, 0x37), // lui t0, 0x2004
		encI(-1, 0, 0, 6, 0x13),
		encS(0, 6, 5, 3), // mtimecmp = ~0: the timer never fires
		0x10500073,       // wfi with mie = 0
	)
	img := &loader.Image{
		Entry:    base,
		Segments: []loader.Segment{{Addr: base, Data: code, MemSize: uint64(len(code))}},
		Symbols:  map[string]uint64{"tohost": 0x8000_1000},
	}
	if _, err := Run(img, Options{MaxSteps: 10_000}); err == nil {
		t.Fatal("Run succeeded, want an error for a hart that can never wake")
	}
}
