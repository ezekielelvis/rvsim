package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ezekielelvis/rvsim/vmm/fault"
	"github.com/ezekielelvis/rvsim/vmm/replay"
)

func faultCmd(args []string) int {
	fs := flag.NewFlagSet("fault", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: rvsim fault check spec.yaml\n"+
			"       rvsim fault list session.rvrec\n\n"+
			"check validates a fault-injection spec (docs/FAULTS.md) and explains each\n"+
			"rule; list prints the faults a recording made with -faults applied. To\n"+
			"inject faults, pass -faults spec.yaml to boot, record, snapshot or gdb.\n")
	}
	fs.Parse(args)
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	switch fs.Arg(0) {
	case "check":
		s, err := fault.Load(fs.Arg(1))
		if err != nil {
			return fail(err)
		}
		fmt.Printf("seed %d, %d fault(s):\n", s.Seed, len(s.Faults))
		for _, f := range s.Faults {
			fmt.Printf("  %s: %s\n", f.Name, describe(f))
		}
		return 0
	case "list":
		f, err := os.Open(fs.Arg(1))
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		_, recs, err := replay.Read(f)
		if err != nil {
			return fail(err)
		}
		n := 0
		for _, r := range recs {
			if r.Tag == replay.TagFault {
				fmt.Println(r.Fault)
				n++
			}
		}
		fmt.Fprintf(os.Stderr, "rvsim: %d fault(s) applied\n", n)
		return 0
	}
	fs.Usage()
	return 2
}

// describe explains a fault rule in words.
func describe(f fault.Fault) string {
	var when string
	w := f.When
	switch {
	case w.Step != nil:
		when = fmt.Sprintf("at step %d", *w.Step)
	case w.Instret != nil:
		when = fmt.Sprintf("when %d instructions have retired", *w.Instret)
	case w.PC != nil:
		when = fmt.Sprintf("the %s time pc reaches %#x", ordinal(w.PC.Hit), w.PC.Addr)
	case w.Access != nil:
		target := w.Access.Device
		if target == "" {
			target = fmt.Sprintf("%#x", w.Access.Addr)
		}
		when = fmt.Sprintf("on the %s MMIO %s access to %s", ordinal(w.Access.Nth), w.Access.Kind, target)
	case w.Chance != nil:
		when = fmt.Sprintf("on each %s event with probability %g", f.Do.Event(), w.Chance.P)
	}
	var do string
	d := f.Do
	switch {
	case d.FlipBit != nil:
		do = fmt.Sprintf("flip bit %d of %#x", d.FlipBit.Bit, d.FlipBit.Addr)
	case d.Reg != nil:
		switch {
		case d.Reg.Set != nil:
			do = fmt.Sprintf("set %s to %#x", d.Reg.Reg, *d.Reg.Set)
		case d.Reg.Random:
			do = fmt.Sprintf("set %s to a random value", d.Reg.Reg)
		default:
			do = fmt.Sprintf("xor %s with %#x", d.Reg.Reg, d.Reg.Xor)
		}
	case d.MMIORead != nil:
		v := "a random value"
		if d.MMIORead.Value != nil {
			v = fmt.Sprintf("%#x", *d.MMIORead.Value)
		}
		do = "make the next MMIO read return " + v
	case d.IRQ != nil:
		if d.IRQ.Drop {
			do = fmt.Sprintf("drop the next interrupt from source %d", d.IRQ.Source)
		} else {
			do = fmt.Sprintf("delay the next interrupt from source %d by %d steps", d.IRQ.Source, d.IRQ.Delay)
		}
	case d.Blk != nil:
		if d.Blk.Fail {
			do = "fail the next virtio-blk request"
		} else {
			do = fmt.Sprintf("xor byte %d of the next virtio-blk read with %#x", d.Blk.Offset, d.Blk.Xor)
		}
	case d.UART != nil:
		verb := "drop"
		if d.UART.Garble {
			verb = "garble"
		}
		do = fmt.Sprintf("%s the next UART %s byte", verb, d.UART.Dir)
	}
	times := "once"
	switch {
	case f.Times == nil:
	case *f.Times == 0:
		times = "with no limit"
	case *f.Times > 1:
		times = fmt.Sprintf("up to %d times", *f.Times)
	}
	return strings.Join([]string{when, do, times}, ", ")
}

func ordinal(n uint64) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}
