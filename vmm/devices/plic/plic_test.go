package plic

import "testing"

const (
	ctx0Threshold = offContext
	ctx0Claim     = offContext + 4
	ctx1Threshold = offContext + ctxStride
	ctx1Claim     = offContext + ctxStride + 4
	ctx0Enable    = offEnable
	ctx1Enable    = offEnable + enableStride
)

// op is one step of a scenario: an MMIO access, a source level change, or
// a check of the output lines.
type op struct {
	write   bool
	read    bool
	off     uint64
	val     uint64 // value to write, or expected read value
	src     int    // > 0: set source level
	level   bool
	lines   uint64 // expected MEIP/SEIP after this op
	checkLn bool
}

func w(off, v uint64) op          { return op{write: true, off: off, val: v} }
func r(off, want uint64) op       { return op{read: true, off: off, val: want} }
func lvl(src int, high bool) op   { return op{src: src, level: high} }
func expectLines(lines uint64) op { return op{lines: lines, checkLn: true} }

func TestScenarios(t *testing.T) {
	tests := []struct {
		name string
		ops  []op
	}{
		{"priority zero never interrupts", []op{
			w(ctx0Enable, 1<<10), lvl(10, true), expectLines(0),
			w(10*4, 1), expectLines(LineMEIP),
		}},
		{"disabled source does not interrupt", []op{
			w(10*4, 1), lvl(10, true), expectLines(0),
			w(ctx0Enable, 1<<10), expectLines(LineMEIP),
		}},
		{"threshold masks priorities at or below it", []op{
			w(10*4, 3), w(ctx0Enable, 1<<10), w(ctx0Threshold, 3), lvl(10, true), expectLines(0),
			w(ctx0Threshold, 2), expectLines(LineMEIP),
		}},
		{"claim returns highest priority, ties to lowest ID", []op{
			w(3*4, 2), w(5*4, 5), w(7*4, 5), w(ctx0Enable, 1<<3|1<<5|1<<7),
			lvl(3, true), lvl(5, true), lvl(7, true),
			r(ctx0Claim, 5), r(ctx0Claim, 7), r(ctx0Claim, 3), r(ctx0Claim, 0),
			expectLines(0),
		}},
		{"claimed source stays quiet until completed", []op{
			w(10*4, 1), w(ctx0Enable, 1<<10), lvl(10, true),
			r(ctx0Claim, 10), expectLines(0),
			r(offPending, 0),
			w(ctx0Claim, 10), expectLines(LineMEIP), // line still high: pending again
			r(offPending, 1<<10),
		}},
		{"completion after the line drops leaves it idle", []op{
			w(10*4, 1), w(ctx0Enable, 1<<10), lvl(10, true),
			r(ctx0Claim, 10), lvl(10, false), w(ctx0Claim, 10),
			expectLines(0), r(offPending, 0),
		}},
		{"level drop clears an unclaimed pending source", []op{
			w(10*4, 1), w(ctx0Enable, 1<<10), lvl(10, true), expectLines(LineMEIP),
			lvl(10, false), expectLines(0),
		}},
		{"completion of a source not enabled for the context is ignored", []op{
			w(10*4, 1), w(ctx0Enable, 1<<10), lvl(10, true), r(ctx0Claim, 10),
			w(ctx1Claim, 10), // context 1 does not enable source 10
			r(offPending, 0), expectLines(0),
			w(ctx0Claim, 10), expectLines(LineMEIP),
		}},
		{"contexts drive MEIP and SEIP independently", []op{
			w(1*4, 1), w(2*4, 1), w(ctx0Enable, 1<<1), w(ctx1Enable, 1<<2),
			lvl(2, true), expectLines(LineSEIP),
			lvl(1, true), expectLines(LineMEIP | LineSEIP),
			r(ctx1Claim, 2), expectLines(LineMEIP),
		}},
		{"register masks", []op{
			w(4, 0xff), r(4, 7), // 3-bit priorities
			w(0, 7), r(0, 0), // source 0 has no priority
			w(ctx0Enable, 0xffff_ffff), r(ctx0Enable, 0xffff_fffe), // no source 0
			w(ctx0Threshold, 0xff), r(ctx0Threshold, 7),
			w(offPending, 0xffff_ffff), r(offPending, 0), // read-only
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lines uint64
			p := New(func(l uint64) { lines = l })
			for i, o := range tt.ops {
				switch {
				case o.src > 0:
					p.Line(o.src).SetLevel(o.level)
				case o.write:
					p.Write(o.off, 4, o.val)
				case o.read:
					if got := p.Read(o.off, 4); got != o.val {
						t.Fatalf("op %d: read %#x = %#x, want %#x", i, o.off, got, o.val)
					}
				case o.checkLn:
					if lines != o.lines {
						t.Fatalf("op %d: lines = %#x, want %#x", i, lines, o.lines)
					}
				}
			}
		})
	}
}

func TestNonWordAccessesAreIgnored(t *testing.T) {
	p := New(func(uint64) {})
	p.Write(4, 1, 5)
	p.Write(6, 4, 5)
	if v := p.Read(4, 4); v != 0 {
		t.Fatalf("priority = %d after byte/misaligned writes", v)
	}
	p.Write(4, 4, 5)
	if v := p.Read(4, 2); v != 0 {
		t.Fatalf("halfword read = %d, want 0", v)
	}
}

func TestOutputsOnlyReportChanges(t *testing.T) {
	calls := 0
	p := New(func(uint64) { calls++ })
	p.Write(4, 4, 1)
	p.Write(ctx0Enable, 4, 1<<1)
	p.Line(1).SetLevel(true)
	p.Line(1).SetLevel(true)
	p.Write(8, 4, 3) // unrelated change
	if calls != 1 {
		t.Fatalf("setLines called %d times, want 1", calls)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	var lines uint64
	p := New(func(l uint64) { lines = l })
	p.Write(10*4, 4, 2)
	p.Write(ctx0Enable, 4, 1<<10)
	p.Line(10).SetLevel(true)
	snap := p.Snapshot()

	var lines2 uint64
	q := New(func(l uint64) { lines2 = l })
	if err := q.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if lines2 != lines || q.Read(ctx0Claim, 4) != 10 {
		t.Fatalf("restored PLIC differs: lines %#x vs %#x", lines2, lines)
	}
	if err := q.Restore(snap[:5]); err == nil {
		t.Fatal("Restore accepted a truncated snapshot")
	}
}

func TestResetKeepsInputLevels(t *testing.T) {
	var lines uint64
	p := New(func(l uint64) { lines = l })
	p.Line(10).SetLevel(true)
	p.Write(10*4, 4, 1)
	p.Write(ctx0Enable, 4, 1<<10)
	p.Reset()
	if lines != 0 || p.Read(10*4, 4) != 0 {
		t.Fatal("Reset did not clear registers")
	}
	if p.Read(offPending, 4) != 1<<10 {
		t.Fatal("Reset lost the source's level")
	}
}
