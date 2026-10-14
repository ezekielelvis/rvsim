package vm

import (
	"bytes"
	"errors"
	"testing"

	. "github.com/ezekielelvis/rvsim/vmm/internal/rvasm"
	"github.com/ezekielelvis/rvsim/vmm/machine"
)

const ramBase = 0x8000_0000

func newVM(t *testing.T, console *bytes.Buffer, code []byte) *VM {
	t.Helper()
	cfg := machine.DefaultConfig()
	cfg.RAMSize = 1 << 20
	var w *bytes.Buffer
	if console != nil {
		w = console
	}
	v, err := New(Config{Machine: cfg, Console: w})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	if err := v.Machine().WritePhys(ramBase, code); err != nil {
		t.Fatal(err)
	}
	return v
}

func reg(t *testing.T, v *VM, r uint32) uint64 {
	t.Helper()
	x, err := v.Machine().Reg(r)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestUARTOutputThroughMMIO(t *testing.T) {
	var out bytes.Buffer
	v := newVM(t, &out, Bytes(
		Lui(T0, 0x10000), // t0 = UART
		Addi(T1, Zero, 'o'),
		Sb(T1, T0, 0),
		Addi(T1, Zero, 'k'),
		Sb(T1, T0, 0),
		Lbu(A0, T0, 5), // LSR
		Self,
	))
	if _, err := v.Run(100); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ok" {
		t.Fatalf("console %q", out.String())
	}
	if lsr := reg(t, v, A0); lsr != 0x60 {
		t.Fatalf("LSR = %#x, want THRE|TEMT", lsr)
	}
}

func TestUnmappedMMIOIsAccessFault(t *testing.T) {
	v := newVM(t, nil, Bytes(
		Lui(T0, 0x40000), // nothing mapped at 0x4000_0000
		Lw(A0, T0, 0),
		Self,
	))
	if _, err := v.Run(2); err != nil {
		t.Fatal(err)
	}
	if c, _ := v.Machine().Reg(machine.RegCSR(0x342)); c != 5 {
		t.Fatalf("mcause = %d, want load access fault", c)
	}
}

type stamp struct {
	v   *VM
	got []uint64
}

func (s *stamp) HandleEvent(uint32, uint64) { s.got = append(s.got, s.v.Now()) }

func TestEventsFireAtTheirExactStep(t *testing.T) {
	v := newVM(t, nil, Bytes(Self))
	st := &stamp{v: v}
	tg := v.Scheduler().Register("stamp", st)
	for _, at := range []uint64{1, 999, 1000, 123_457} {
		v.Scheduler().Schedule(at, tg, 0, 0)
	}
	if _, err := v.Run(200_000); err != nil {
		t.Fatal(err)
	}
	want := []uint64{1, 999, 1000, 123_457}
	for i := range want {
		if st.got[i] != want[i] {
			t.Fatalf("events fired at %v, want %v", st.got, want)
		}
	}
}

func TestWFIFastForwardsToNextEvent(t *testing.T) {
	v := newVM(t, nil, Bytes(
		Lui(T0, 0x2004),
		Addi(T1, Zero, -1),
		Sd(T1, T0, 0), // mtimecmp = never
		Wfi,
		Self,
	))
	st := &stamp{v: v}
	v.Scheduler().Schedule(50_000, v.Scheduler().Register("stamp", st), 0, 0)
	// The event does not wake the hart, so after it nothing can: the loop
	// must skip straight to it and then report the dead end.
	if _, err := v.Run(60_000); !errors.Is(err, ErrIdleForever) {
		t.Fatalf("err = %v, want ErrIdleForever", err)
	}
	if len(st.got) != 1 || st.got[0] != 50_000 {
		t.Fatalf("event fired at %v", st.got)
	}
	if e, _ := v.Machine().Run(0); e.Instret != 4 {
		t.Fatalf("instret = %d: idle time must not execute instructions", e.Instret)
	}
}

func TestIdleForeverIsAnError(t *testing.T) {
	v := newVM(t, nil, Bytes(Lui(T0, 0x2004), Addi(T1, Zero, -1), Sd(T1, T0, 0), Wfi))
	if _, err := v.Run(1_000_000); !errors.Is(err, ErrIdleForever) {
		t.Fatalf("err = %v, want ErrIdleForever", err)
	}
}

// echo is a guest that copies every received byte back to the UART.
var echo = Bytes(
	Lui(T0, 0x10000),
	Lbu(T1, T0, 5), // loop: LSR
	Andi(T1, T1, 1),
	Beq(T1, Zero, -8),
	Lbu(T2, T0, 0), // RBR
	Sb(T2, T0, 0),  // THR
	Jal(Zero, -20),
)

func TestInputIsPacedAndNotLost(t *testing.T) {
	var out bytes.Buffer
	v := newVM(t, &out, echo)
	msg := []byte("a burst longer than the sixteen-byte FIFO")
	v.ScheduleInput(100, msg)
	if _, err := v.Run(100 + uint64(len(msg)+1)*InputSpacing); err != nil {
		t.Fatal(err)
	}
	if out.String() != string(msg) {
		t.Fatalf("echoed %q, want %q", out.String(), msg)
	}
}

func TestFlowControlHoldsInputWhileFIFOIsFull(t *testing.T) {
	var out bytes.Buffer
	v := newVM(t, &out, Bytes(Self)) // never reads
	msg := bytes.Repeat([]byte{'x'}, 40)
	v.ScheduleInput(0, msg)
	if _, err := v.Run(40 * InputSpacing); err != nil {
		t.Fatal(err)
	}
	// FIFO disabled: one byte fits; the rest waits instead of overrunning.
	if got := v.uart.Read(5, 1); got&0x2 != 0 {
		t.Fatalf("LSR = %#x: overrun despite flow control", got)
	}
	if len(v.input) != 39 {
		t.Fatalf("%d bytes still queued, want 39", len(v.input))
	}
}

func TestHostInputChannel(t *testing.T) {
	var out bytes.Buffer
	in := make(chan byte, 8)
	cfg := machine.DefaultConfig()
	cfg.RAMSize = 1 << 20
	v, err := New(Config{Machine: cfg, Console: &out, Input: in})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := v.Machine().WritePhys(ramBase, echo); err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte("hi") {
		in <- b
	}
	close(in)
	if _, err := v.Run(10 * InputSpacing); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hi" {
		t.Fatalf("echoed %q", out.String())
	}
}

func TestUARTInterruptReachesTheHartThroughThePLIC(t *testing.T) {
	v := newVM(t, nil, Bytes(Self))
	m := v.Machine()
	// Guest-visible setup done from the host for brevity: UART RDA interrupt,
	// PLIC source priority and context 0 enable.
	v.uart.Write(1, 1, 1)
	v.plic.Write(UARTIRQ*4, 4, 1)
	v.plic.Write(0x2000, 4, 1<<UARTIRQ)
	if mip, _ := m.Reg(machine.RegCSR(0x344)); mip&(1<<11) != 0 {
		t.Fatal("MEIP set before any input")
	}
	v.ScheduleInput(10, []byte{'z'})
	if _, err := v.Run(20); err != nil {
		t.Fatal(err)
	}
	if mip, _ := m.Reg(machine.RegCSR(0x344)); mip&(1<<11) == 0 {
		t.Fatal("MEIP not raised by UART input")
	}
	if id := v.plic.Read(0x20_0004, 4); id != UARTIRQ {
		t.Fatalf("claim = %d, want %d", id, UARTIRQ)
	}
}

func TestFinisherEndsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hi, lo    int32 // value = hi<<12 + lo
		wantCode  uint64
		wantReset bool
	}{
		{"pass", 0x5, 0x555, 0, false},
		{"fail with code", 0x2a3, 0x333, 42, false},
		{"reset", 0x7, 0x777, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVM(t, nil, Bytes(
				Lui(T0, FinisherBase>>12),
				Lui(T1, uint32(tc.hi)),
				Addi(T1, T1, tc.lo),
				Sw(T1, T0, 0),
				Self,
			))
			res, err := v.Run(1000)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Exited || res.ExitCode != tc.wantCode || res.Reset != tc.wantReset {
				t.Fatalf("result %+v", res)
			}
			if res.Instret != 4 {
				t.Fatalf("instret %d: the finisher store must retire", res.Instret)
			}
		})
	}
}
