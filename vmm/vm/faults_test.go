package vm

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/fault"
	. "github.com/ezekielelvis/rvsim/vmm/internal/rvasm"
	"github.com/ezekielelvis/rvsim/vmm/machine"
)

// faultVM builds a VM with a fault spec (YAML) and a program; it returns
// the VM, its console and the faults applied, in order.
func faultVM(t *testing.T, spec string, code []byte) (*VM, *bytes.Buffer, *[]fault.Applied) {
	t.Helper()
	s, err := fault.Parse([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	var applied []fault.Applied
	inj := fault.NewLive(s, func(a fault.Applied) error { applied = append(applied, a); return nil })
	cfg := machine.DefaultConfig()
	cfg.RAMSize = 1 << 20
	var out bytes.Buffer
	v, err := New(Config{Machine: cfg, Console: &out, Faults: inj})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	if err := v.Machine().WritePhys(ramBase, code); err != nil {
		t.Fatal(err)
	}
	return v, &out, &applied
}

// dataAddr is reached with auipc from the instruction at index 4 (pc
// ramBase+16): lui would sign-extend 0x8000_xxxx on RV64.
const dataAddr = ramBase + 16 + 0x4000

// spinThen spins ~2,000 steps, then runs tail, then halts (j .).
func spinThen(tail ...uint32) []byte {
	head := []uint32{
		Lui(T0, 0x10000), // UART
		Addi(S1, Zero, 1000),
		Addi(S1, S1, -1),
		Bne(S1, Zero, -4),
	}
	return Bytes(append(append(head, tail...), Self)...)
}

func TestFlipBitAtAStep(t *testing.T) {
	// After the spin, print the byte at dataAddr (initially 'A').
	v, out, applied := faultVM(t, fmt.Sprintf(`
faults:
  - name: cosmic-ray
    when: {step: 100}
    do: {flip_bit: {addr: %#x, bit: 5}}
`, dataAddr), spinThen(
		Auipc(T1, 4),
		Lbu(A0, T1, 0),
		Sb(A0, T0, 0),
	))
	v.Machine().WritePhys(dataAddr, []byte{'A'})
	if _, err := v.Run(5000); err != nil {
		t.Fatal(err)
	}
	if out.String() != "a" { // 'A' ^ 0x20
		t.Fatalf("console %q", out.String())
	}
	if len(*applied) != 1 || (*applied)[0].Step != 100 || (*applied)[0].Kind != "flip_bit" {
		t.Fatalf("applied %+v", *applied)
	}
}

func TestCorruptRegisterAtAPCAndAtAnInstructionCount(t *testing.T) {
	code := spinThen(
		Addi(A0, Zero, 'B'), // index 4: pc ramBase+16
		Sb(A0, T0, 0),       // index 5: pc ramBase+20
	)
	v, out, applied := faultVM(t, fmt.Sprintf(`
faults:
  - when: {pc: {addr: %#x}}
    do: {reg: {reg: a0, xor: 0x20}}
`, ramBase+20), code)
	if _, err := v.Run(5000); err != nil {
		t.Fatal(err)
	}
	if out.String() != "b" || len(*applied) != 1 {
		t.Fatalf("pc trigger: console %q, applied %+v", out.String(), *applied)
	}

	// The same program with a0 set when N instructions have retired: at 5
	// (inside the spin) the later li overwrites it; at 2003 (lui, addi,
	// 2,000 spin instructions, li) it shows.
	for _, tc := range []struct {
		instret uint64
		want    string
	}{{5, "B"}, {2003, "Z"}} {
		v, out, applied := faultVM(t, fmt.Sprintf(`
faults:
  - when: {instret: %d}
    do: {reg: {reg: a0, set: 0x5a}}
`, tc.instret), code)
		if _, err := v.Run(5000); err != nil {
			t.Fatal(err)
		}
		if out.String() != tc.want || len(*applied) != 1 {
			t.Fatalf("instret %d: console %q, applied %+v", tc.instret, out.String(), *applied)
		}
		if v.Instret() < tc.instret {
			t.Fatal("did not run far enough")
		}
	}
}

func TestMMIOReadOverride(t *testing.T) {
	// Write 'q' to the UART scratch register, read it back, print it: the
	// second UART read returns 'Q' instead.
	v, out, applied := faultVM(t, `
faults:
  - when: {access: {device: uart, kind: read, nth: 1}}
    do: {mmio_read: {value: 0x51}}
`, spinThen(
		Addi(A0, Zero, 'q'),
		Sb(A0, T0, 7),
		Lbu(A1, T0, 7),
		Sb(A1, T0, 0),
	))
	if _, err := v.Run(5000); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Q" || len(*applied) != 1 || (*applied)[0].Addr != UARTBase+7 {
		t.Fatalf("console %q, applied %+v", out.String(), *applied)
	}
}

func TestUARTTransmitDropAndGarble(t *testing.T) {
	prog := spinThen(
		Addi(A0, Zero, 'x'), Sb(A0, T0, 0),
		Addi(A0, Zero, 'y'), Sb(A0, T0, 0),
		Addi(A0, Zero, 'z'), Sb(A0, T0, 0),
	)
	for _, tc := range []struct{ spec, want string }{
		{"{when: {step: 1}, do: {uart: {drop: true}}}", "yz"},
		{"{when: {step: 1}, do: {uart: {garble: true, xor: 0x20}}, times: 2}", "XYz"},
		{"{when: {chance: {p: 1}}, do: {uart: {drop: true}}, times: 0}", ""},
	} {
		v, out, _ := faultVM(t, "faults:\n  - "+tc.spec+"\n", prog)
		if _, err := v.Run(5000); err != nil {
			t.Fatal(err)
		}
		if out.String() != tc.want {
			t.Errorf("%s: console %q, want %q", tc.spec, out.String(), tc.want)
		}
	}
}

func TestUARTReceiveGarble(t *testing.T) {
	// Echo two bytes of input.
	echo := Bytes(
		Lui(T0, 0x10000),
		Lbu(T1, T0, 5),
		Andi(T1, T1, 1),
		Beq(T1, Zero, -8),
		Lbu(A0, T0, 0),
		Sb(A0, T0, 0),
		Jal(Zero, -20),
	)
	v, out, applied := faultVM(t, `
faults:
  - {when: {step: 1}, do: {uart: {dir: rx, garble: true, xor: 0x01}}}
`, echo)
	v.ScheduleInput(100, []byte("ab"))
	if _, err := v.Run(50_000); err != nil {
		t.Fatal(err)
	}
	if out.String() != "`b" || (*applied)[0].Event != fault.EvUARTRx || (*applied)[0].Value != 'a' {
		t.Fatalf("console %q, applied %+v", out.String(), *applied)
	}
}

func TestInterruptDropAndDelay(t *testing.T) {
	for _, tc := range []struct {
		spec      string
		raisedBy  uint64 // MEIP must be set by this step (0: never)
		notBefore uint64
	}{
		{"", 20, 0},
		{"{when: {step: 1}, do: {irq: {source: 10, drop: true}}}", 0, 0},
		{"{when: {step: 1}, do: {irq: {source: 10, delay: 500}}}", 520, 500},
	} {
		spec := "faults: []\n"
		if tc.spec != "" {
			spec = "faults:\n  - " + tc.spec + "\n"
		}
		v, _, _ := faultVM(t, spec, Bytes(Self))
		v.uart.Write(1, 1, 1)
		v.plic.Write(UARTIRQ*4, 4, 1)
		v.plic.Write(0x2000, 4, 1<<UARTIRQ)
		v.ScheduleInput(10, []byte{'z'})
		meip := func() bool {
			mip, _ := v.Machine().Reg(machine.RegCSR(0x344))
			return mip&(1<<11) != 0
		}
		if tc.notBefore > 0 {
			v.Run(tc.notBefore - 1)
			if meip() {
				t.Fatalf("%s: MEIP before step %d", tc.spec, tc.notBefore)
			}
		}
		v.Run(2000 - v.Now())
		if got := meip(); got != (tc.raisedBy != 0) {
			t.Fatalf("%q: MEIP = %v at step %d", tc.spec, got, v.Now())
		}
	}
}

func TestFaultSpecValidation(t *testing.T) {
	for _, bad := range []string{
		"faults: [{when: {step: 1}, do: {}}]",
		"faults: [{when: {}, do: {flip_bit: {addr: 1, bit: 0}}}]",
		"faults: [{when: {step: 1, instret: 2}, do: {flip_bit: {addr: 1, bit: 0}}}]",
		"faults: [{when: {step: 1}, do: {flip_bit: {addr: 1, bit: 9}}}]",
		"faults: [{when: {step: 1}, do: {reg: {reg: q9, set: 1}}}]",
		"faults: [{when: {chance: {p: 2}}, do: {uart: {drop: true}}}]",
		"faults: [{when: {chance: {p: 0.5}}, do: {flip_bit: {addr: 1, bit: 0}}}]",
		"faults: [{when: {step: 1}, do: {irq: {source: 10}}}]",
		"faults: [{when: {step: 1}, do: {blk: {}}}]",
		"faults: [{when: {step: 1}, do: {uart: {dir: up, drop: true}}}]",
		"faults: [{when: {step: 1}, do: {flip_bit: {addr: 1, bit: 0}}, bogus: 1}]",
	} {
		if _, err := fault.Parse([]byte(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		} else if !strings.Contains(err.Error(), "fault spec") {
			t.Errorf("error lacks context: %v", err)
		}
	}
}
