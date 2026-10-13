package machine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func newTestMachine(t testing.TB) *Machine {
	t.Helper()
	cfg := DefaultConfig()
	cfg.RAMSize = 16 << 20
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// putInsns writes little-endian 32-bit instructions at addr.
func putInsns(t *testing.T, m *Machine, addr uint64, insns ...uint32) {
	t.Helper()
	buf := make([]byte, 4*len(insns))
	for i, w := range insns {
		binary.LittleEndian.PutUint32(buf[4*i:], w)
	}
	if err := m.WritePhys(addr, buf); err != nil {
		t.Fatal(err)
	}
}

func TestRunToHTIFHalt(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RAMSize = 16 << 20
	cfg.HTIFToHost = 0x8000_1000
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	putInsns(t, m, 0x8000_0000,
		0x02a00293, // addi t0, zero, 42
		0x00533023, // sd   t0, 0(t1)
	)
	if err := m.SetReg(6, 0x8000_1000); err != nil { // t1 = tohost
		t.Fatal(err)
	}
	exit, err := m.Run(1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := Exit{Reason: ExitHalt, Addr: 0x8000_1000, Value: 42, PC: 0x8000_0008, Steps: 2, Instret: 2, TimerStep: ^uint64(0)}
	if exit != want {
		t.Fatalf("Run exit = %+v, want %+v", exit, want)
	}
}

func TestBudgetFromEmptyRAM(t *testing.T) {
	m := newTestMachine(t)
	exit, err := m.Run(1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Zeroed RAM is illegal instructions: every step is a trap.
	if exit.Reason != ExitBudget || exit.Steps != 1000 || exit.Instret != 0 {
		t.Fatalf("Run exit = %+v, want budget exit after 1000 steps, 0 retired", exit)
	}
}

func TestZeroBudget(t *testing.T) {
	m := newTestMachine(t)
	exit, err := m.Run(0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit.Reason != ExitBudget {
		t.Fatalf("Reason = %v, want %v", exit.Reason, ExitBudget)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero RAM", func(c *Config) { c.RAMSize = 0 }},
		{"unaligned RAM size", func(c *Config) { c.RAMSize = 4097 }},
		{"unaligned RAM base", func(c *Config) { c.RAMBase = 0x8000_0010 }},
		{"CLINT overlaps RAM", func(c *Config) { c.CLINTBase = 0x8000_0000 }},
		{"zero steps per tick", func(c *Config) { c.StepsPerTick = 0 }},
		{"unknown flag", func(c *Config) { c.Flags = 1 << 31 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(&cfg)
			m, err := New(cfg)
			if !errors.Is(err, StatusInval) {
				t.Fatalf("New err = %v, want %v", err, StatusInval)
			}
			if m != nil {
				t.Fatal("New returned a machine on error")
			}
		})
	}
}

func TestRegisters(t *testing.T) {
	m := newTestMachine(t)
	tests := []struct {
		name  string
		reg   uint32
		write uint64
		want  uint64
	}{
		{"x0 hardwired", 0, 0xdead, 0},
		{"x1", 1, 0x1234, 0x1234},
		{"x31", 31, ^uint64(0), ^uint64(0)},
		{"pc", RegPC, 0x8000_1000, 0x8000_1000},
		{"priv U", RegPriv, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := m.SetReg(tt.reg, tt.write); err != nil {
				t.Fatalf("SetReg: %v", err)
			}
			got, err := m.Reg(tt.reg)
			if err != nil {
				t.Fatalf("Reg: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Reg = %#x, want %#x", got, tt.want)
			}
		})
	}
	if _, err := m.Reg(34); !errors.Is(err, StatusRange) {
		t.Fatalf("Reg(34) err = %v, want %v", err, StatusRange)
	}
	if v, err := m.Reg(RegCSR(0x300)); err != nil || v != 2<<32 {
		t.Fatalf("Reg(mstatus) = %#x, %v; want UXL=2 only", v, err)
	}
	if _, err := m.Reg(RegCSR(0x7c0)); !errors.Is(err, StatusRange) {
		t.Fatalf("Reg(unimplemented CSR) err = %v, want %v", err, StatusRange)
	}
}

func TestPhysMemory(t *testing.T) {
	m := newTestMachine(t)
	in := []byte("rvsim boundary")
	if err := m.WritePhys(0x8000_0100, in); err != nil {
		t.Fatalf("WritePhys: %v", err)
	}
	out := make([]byte, len(in))
	if err := m.ReadPhys(0x8000_0100, out); err != nil {
		t.Fatalf("ReadPhys: %v", err)
	}
	if !bytes.Equal(in, out) {
		t.Fatalf("ReadPhys = %q, want %q", out, in)
	}
	ram := m.RAM()
	if len(ram) != 16<<20 {
		t.Fatalf("len(RAM) = %d, want %d", len(ram), 16<<20)
	}
	if !bytes.Equal(ram[0x100:0x100+len(in)], in) {
		t.Fatal("RAM view does not reflect WritePhys")
	}
	if err := m.ReadPhys(0x7fff_ffff, out[:1]); !errors.Is(err, StatusRange) {
		t.Fatalf("ReadPhys below RAM err = %v, want %v", err, StatusRange)
	}
}

func TestIRQLines(t *testing.T) {
	m := newTestMachine(t)
	if err := m.SetIRQLines(IRQMEIP | IRQSEIP); err != nil {
		t.Fatalf("SetIRQLines: %v", err)
	}
	if err := m.SetIRQLines(1 << 7); !errors.Is(err, StatusInval) {
		t.Fatalf("SetIRQLines(MTIP) err = %v, want %v", err, StatusInval)
	}
}

func TestPendingStateErrors(t *testing.T) {
	m := newTestMachine(t)
	if err := m.CompleteMMIO(0, false); !errors.Is(err, StatusState) {
		t.Fatalf("CompleteMMIO err = %v, want %v", err, StatusState)
	}
	if err := m.AdvanceIdle(1); !errors.Is(err, StatusState) {
		t.Fatalf("AdvanceIdle err = %v, want %v", err, StatusState)
	}
}

func TestClose(t *testing.T) {
	m, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.Close()
	m.Close() // idempotent
	if _, err := m.Run(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run after Close err = %v, want %v", err, ErrClosed)
	}
}

// The run loop must not allocate per exit (engineering rule 3).
func TestRunDoesNotAllocate(t *testing.T) {
	m := newTestMachine(t)
	allocs := testing.AllocsPerRun(1000, func() {
		if _, err := m.Run(1); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("Run allocates %v times per call, want 0", allocs)
	}
}

func BenchmarkRunExit(b *testing.B) {
	m := newTestMachine(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.Run(1); err != nil {
			b.Fatal(err)
		}
	}
}
