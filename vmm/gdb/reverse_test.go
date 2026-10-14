package gdb_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	. "github.com/ezekielelvis/rvsim/vmm/gdb"
	"github.com/ezekielelvis/rvsim/vmm/gdb/gdbtest"
	"github.com/ezekielelvis/rvsim/vmm/replay"
)

func TestReverseExecutionOnAReplay(t *testing.T) {
	path := gdbtest.RecordEcho(t, "xyzq")
	var console bytes.Buffer
	rt, err := NewReplayTarget(path, replay.Spec{}, 7_000, &console)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rt.Chunk = 3_000
	never := func() bool { return false }
	a0 := func() uint64 { v, _ := rt.ReadReg(10); return v }
	pc := func() uint64 { v, _ := rt.ReadReg(32); return v }

	if err := rt.AddBreakpoint(gdbtest.EchoPC); err != nil {
		t.Fatal(err)
	}
	if s, err := rt.Continue(never); err != nil || s.Reason != StopBreakpoint || a0() != 'x' {
		t.Fatalf("first continue: %+v %v a0=%#x", s, err, a0())
	}
	first := rt.Now()
	if s, err := rt.Continue(never); err != nil || s.Reason != StopBreakpoint || a0() != 'y' {
		t.Fatalf("second continue: %+v %v a0=%#x", s, err, a0())
	}

	// Back to the previous hit: the 'x' echo, at exactly the same step.
	if s, err := rt.ReverseContinue(never); err != nil || s.Reason != StopBreakpoint {
		t.Fatalf("reverse-continue: %+v %v", s, err)
	}
	if rt.Now() != first || a0() != 'x' || pc() != gdbtest.EchoPC {
		t.Fatalf("after reverse-continue: step %d (want %d), a0 %#x, pc %#x", rt.Now(), first, a0(), pc())
	}
	// One step back is the load of the byte; forward again returns here.
	if s, err := rt.ReverseStep(); err != nil || s.Reason != StopStep || rt.Now() != first-1 || pc() != gdbtest.EchoPC-4 {
		t.Fatalf("reverse-step: %+v %v at step %d pc %#x", s, err, rt.Now(), pc())
	}
	if s, err := rt.Step(); err != nil || rt.Now() != first || pc() != gdbtest.EchoPC {
		t.Fatalf("step: %+v %v at step %d pc %#x", s, err, rt.Now(), pc())
	}
	// Reverse-continue from the first hit finds nothing earlier.
	if s, err := rt.ReverseContinue(never); err != nil || s.Reason != StopHistory || rt.Now() != 0 {
		t.Fatalf("reverse-continue to the start: %+v %v at %d", s, err, rt.Now())
	}
	// Forward to the end of the recording.
	rt.RemoveBreakpoint(gdbtest.EchoPC)
	if s, err := rt.Continue(never); err != nil || (s.Reason != StopEnd && s.Reason != StopExited) {
		t.Fatalf("continue to the end: %+v %v", s, err)
	}
}

// Real GDB drives reverse execution on a replay.
func TestRealGDBReverseExecution(t *testing.T) {
	gdbPath := gdbtest.Find(t)
	path := gdbtest.RecordEcho(t, "xyzq")
	rt, err := NewReplayTarget(path, replay.Spec{}, 7_000, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	out := gdbtest.Run(t, gdbPath, rt, func(addr string) []string {
		return []string{
			"-ex", "set architecture riscv:rv64",
			"-ex", "target remote " + addr,
			"-ex", fmt.Sprintf("break *%#x", gdbtest.EchoPC),
			"-ex", "continue",
			"-ex", "continue",
			"-ex", "printf \"echoing %c\\n\", $a0",
			"-ex", "reverse-continue",
			"-ex", "printf \"echoing %c\\n\", $a0",
			"-ex", "reverse-stepi",
			"-ex", "printf \"pc %#x\\n\", $pc",
			"-ex", "kill",
		}
	})
	for _, want := range []string{"echoing y", "echoing x", fmt.Sprintf("pc %#x", gdbtest.EchoPC-4)} {
		if !strings.Contains(out, want) {
			t.Errorf("gdb output lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("gdb output:\n%s", out)
	}
}
