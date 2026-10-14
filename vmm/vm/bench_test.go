package vm

import (
	"testing"

	. "github.com/ezekielelvis/rvsim/vmm/internal/rvasm"
	"github.com/ezekielelvis/rvsim/vmm/machine"
)

// BenchmarkMMIOExit measures a full MMIO round trip: rv_run exits, the bus
// dispatches to the UART, rv_complete_mmio, and the instruction retires.
// The guest polls the UART's LSR in a two-instruction loop, so every other
// step is one exit.
func BenchmarkMMIOExit(b *testing.B) {
	cfg := machine.DefaultConfig()
	cfg.RAMSize = 1 << 20
	v, err := New(Config{Machine: cfg})
	if err != nil {
		b.Fatal(err)
	}
	defer v.Close()
	loop := Bytes(Lui(T0, 0x10000), Lbu(T1, T0, 5), Jal(Zero, -4))
	if err := v.Machine().WritePhys(ramBase, loop); err != nil {
		b.Fatal(err)
	}
	if _, err := v.Run(1); err != nil { // lui
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := v.Run(2); err != nil { // one MMIO load + the jump
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "exits/s")
}

// BenchmarkMMIOExitBatched is the same loop run in long batches, which is
// how the run loop executes a guest: it isolates the per-exit cost from the
// cost of calling Run.
func BenchmarkMMIOExitBatched(b *testing.B) {
	cfg := machine.DefaultConfig()
	cfg.RAMSize = 1 << 20
	v, err := New(Config{Machine: cfg})
	if err != nil {
		b.Fatal(err)
	}
	defer v.Close()
	loop := Bytes(Lui(T0, 0x10000), Lbu(T1, T0, 5), Jal(Zero, -4))
	if err := v.Machine().WritePhys(ramBase, loop); err != nil {
		b.Fatal(err)
	}
	const steps = 2_000_000 // 1M exits per Run
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := v.Run(steps); err != nil {
			b.Fatal(err)
		}
	}
	exits := float64(b.N) * steps / 2
	b.ReportMetric(exits/b.Elapsed().Seconds(), "exits/s")
	b.ReportMetric(b.Elapsed().Seconds()*1e9/exits, "ns/exit")
}
