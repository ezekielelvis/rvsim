package uart

import (
	"bytes"
	"testing"
)

type fakeLine struct {
	high    bool
	changes int
}

func (l *fakeLine) SetLevel(h bool) { l.high = h; l.changes++ }

func newUART(t *testing.T) (*UART, *bytes.Buffer, *fakeLine) {
	t.Helper()
	var out bytes.Buffer
	line := &fakeLine{}
	return New(&out, line), &out, line
}

// op: write reg, read reg (expect), receive byte, or check IRQ level.
type op struct {
	kind byte // 'w', 'r', 'x' (receive), 'i' (irq)
	reg  uint64
	val  uint64
}

func wr(reg, v uint64) op    { return op{'w', reg, v} }
func rd(reg, want uint64) op { return op{'r', reg, want} }
func rx(b byte) op           { return op{'x', 0, uint64(b)} }
func irq(high bool) op {
	if high {
		return op{'i', 0, 1}
	}
	return op{'i', 0, 0}
}

func TestScenarios(t *testing.T) {
	tests := []struct {
		name    string
		ops     []op
		wantOut string
	}{
		{"reset state", []op{
			rd(regLSR, lsrTHRE|lsrTEMT), rd(regIIR, iirNone), rd(regIER, 0), irq(false),
		}, ""},
		{"transmit", []op{
			wr(regRBR, 'h'), wr(regRBR, 'i'), rd(regLSR, lsrTHRE|lsrTEMT),
		}, "hi"},
		{"enabling THRE raises it; reading IIR acknowledges", []op{
			wr(regIER, ierTHRE), irq(true), rd(regIIR, iirTHRE), irq(false), rd(regIIR, iirNone),
		}, ""},
		{"each transmit re-raises THRE", []op{
			wr(regIER, ierTHRE), rd(regIIR, iirTHRE), wr(regRBR, 'a'), irq(true), rd(regIIR, iirTHRE),
		}, "a"},
		{"receive without FIFO", []op{
			wr(regIER, ierRDA), rx('x'), irq(true), rd(regLSR, lsrDR|lsrTHRE|lsrTEMT),
			rd(regIIR, iirRDA), rd(regRBR, 'x'), irq(false), rd(regLSR, lsrTHRE|lsrTEMT),
		}, ""},
		{"overrun without FIFO", []op{
			wr(regIER, ierRDA|ierRLS), rx('1'), rx('2'),
			rd(regIIR, iirRLS), rd(regLSR, lsrDR|lsrOE|lsrTHRE|lsrTEMT), // read clears OE
			rd(regIIR, iirRDA), rd(regRBR, '1'),
		}, ""},
		{"FIFO below trigger reports a character timeout", []op{
			wr(regIIR, fcrEnable|3<<6), wr(regIER, ierRDA), rx('a'),
			rd(regIIR, iirTimeout|iirFIFO), rd(regRBR, 'a'), rd(regIIR, iirNone|iirFIFO),
		}, ""},
		{"FIFO at trigger reports data available", []op{
			wr(regIIR, fcrEnable|1<<6), wr(regIER, ierRDA), // trigger 4
			rx('a'), rx('b'), rx('c'), rd(regIIR, iirTimeout|iirFIFO),
			rx('d'), rd(regIIR, iirRDA|iirFIFO),
			rd(regRBR, 'a'), rd(regRBR, 'b'), rd(regRBR, 'c'), rd(regRBR, 'd'), rd(regRBR, 0),
		}, ""},
		{"FCR clears the receive FIFO", []op{
			wr(regIIR, fcrEnable), rx('a'), wr(regIIR, fcrEnable|fcrClearRX), rd(regLSR, lsrTHRE|lsrTEMT),
		}, ""},
		{"receive priority over THRE", []op{
			wr(regIER, ierRDA|ierTHRE), rx('z'), rd(regIIR, iirRDA), rd(regRBR, 'z'), rd(regIIR, iirTHRE),
		}, ""},
		{"DLAB selects the divisor latch", []op{
			wr(regLCR, lcrDLAB|3), wr(regRBR, 0x12), wr(regIER, 0x34),
			rd(regRBR, 0x12), rd(regIER, 0x34),
			wr(regLCR, 3), rd(regIER, 0), rd(regLCR, 3),
		}, ""},
		{"scratch register", []op{wr(regSCR, 0x5a), rd(regSCR, 0x5a)}, ""},
		{"loopback sends output to the receiver", []op{
			wr(regMCR, mcrLoop|0x0f), wr(regRBR, 'L'), rd(regRBR, 'L'), rd(regMSR, 0xf0),
		}, ""},
		{"modem status reports a connected line", []op{rd(regMSR, msrConnected)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, out, line := newUART(t)
			for i, o := range tt.ops {
				switch o.kind {
				case 'w':
					u.Write(o.reg, 1, o.val)
				case 'r':
					if got := u.Read(o.reg, 1); got != o.val {
						t.Fatalf("op %d: read reg %d = %#x, want %#x", i, o.reg, got, o.val)
					}
				case 'x':
					u.Receive(byte(o.val))
				case 'i':
					if line.high != (o.val == 1) {
						t.Fatalf("op %d: irq = %v, want %v", i, line.high, o.val == 1)
					}
				}
			}
			if out.String() != tt.wantOut {
				t.Fatalf("output %q, want %q", out.String(), tt.wantOut)
			}
		})
	}
}

func TestFIFOOverrunAfterSixteenBytes(t *testing.T) {
	u, _, _ := newUART(t)
	u.Write(regIIR, 1, fcrEnable)
	for i := range fifoSize {
		u.Receive(byte(i))
	}
	if u.RxSpace() != 0 {
		t.Fatalf("RxSpace = %d", u.RxSpace())
	}
	u.Receive(0xff)
	if u.Read(regLSR, 1)&lsrOE == 0 {
		t.Fatal("no overrun")
	}
	for i := range fifoSize {
		if b := u.Read(regRBR, 1); b != uint64(i) {
			t.Fatalf("byte %d = %d", i, b)
		}
	}
}

func TestIRQLineOnlyDrivenOnChange(t *testing.T) {
	u, _, line := newUART(t)
	start := line.changes
	u.Write(regIER, 1, ierRDA)
	u.Receive('a')
	u.Receive('b') // still pending: no new edge
	if line.changes != start+1 {
		t.Fatalf("line changed %d times, want 1", line.changes-start)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	u, _, _ := newUART(t)
	u.Write(regIIR, 1, fcrEnable)
	u.Write(regIER, 1, ierRDA)
	u.Receive('q')
	u.Write(regSCR, 1, 9)
	snap := u.Snapshot()

	v, _, line := newUART(t)
	if err := v.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if !line.high || v.Read(regSCR, 1) != 9 || v.Read(regRBR, 1) != 'q' {
		t.Fatal("restored UART differs")
	}
	if err := v.Restore(snap[:3]); err == nil {
		t.Fatal("Restore accepted a truncated snapshot")
	}
}
