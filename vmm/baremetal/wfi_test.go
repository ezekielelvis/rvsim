package baremetal

import (
	"testing"

	. "github.com/ezekielelvis/rvsim/vmm/internal/rvasm"
	"github.com/ezekielelvis/rvsim/vmm/loader"
)

const base = 0x8000_0000

func image(code []byte) *loader.Image {
	return &loader.Image{
		Entry:    base,
		Segments: []loader.Segment{{Addr: base, Data: code, MemSize: uint64(len(code))}},
		Symbols:  map[string]uint64{"tohost": 0x8000_1000},
	}
}

// A program that waits in WFI for the CLINT timer: the run loop must skip
// idle virtual time to the timer deadline rather than fail or spin.
func TestWFIWakesOnTimer(t *testing.T) {
	code := Bytes(
		Lui(T0, 0x2004),     // t0 = CLINT mtimecmp
		Addi(T1, Zero, 100), //
		Sd(T1, T0, 0),       // mtimecmp = 100
		Addi(T2, Zero, 0x80),
		Csrw(0x304, T2), // mie = MTIE
		Wfi,             // MIE = 0: the timer wakes WFI without trapping
		Auipc(T0, 1),    // t0 = base + 0x18 + 0x1000
		Addi(T1, Zero, 1),
		Sd(T1, T0, -0x18), // tohost = 1 (pass)
		Self,
	)
	res, err := Run(image(code), Options{MaxSteps: 100_000})
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
	code := Bytes(
		Lui(T0, 0x2004),
		Addi(T1, Zero, -1),
		Sd(T1, T0, 0), // mtimecmp = ~0: the timer never fires
		Wfi,           // with mie = 0
	)
	if _, err := Run(image(code), Options{MaxSteps: 10_000}); err == nil {
		t.Fatal("Run succeeded, want an error for a hart that can never wake")
	}
}
