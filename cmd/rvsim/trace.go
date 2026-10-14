package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ezekielelvis/rvsim/vmm/replay"
)

func traceCmd(args []string) int {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: rvsim trace [flags] session.rvrec\n\n"+
			"Writes a Spike --log-commits style commit log of steps [-from, -to) of a\n"+
			"recorded session, by replaying it. With -snapshot-dir (snapshots saved by\n"+
			"\"rvsim replay -snapshot-every\"), the replay starts from the nearest\n"+
			"snapshot instead of the beginning. Use it to look inside the window a\n"+
			"divergence report names (docs/REPLAY.md).\n\nflags:\n")
		fs.PrintDefaults()
	}
	from := fs.Uint64("from", 0, "first step to trace")
	to := fs.Uint64("to", 0, "stop at step `n` (0: the end of the recording)")
	out := fs.String("o", "-", "write the trace to `file` (- for stdout)")
	snapDir := fs.String("snapshot-dir", "", "restore the nearest snapshot from `directory`")
	firmware := fs.String("firmware", "", "use `file` as the firmware image")
	kernel := fs.String("kernel", "", "use `file` as the kernel image")
	initrd := fs.String("initrd", "", "use `file` as the initramfs")
	disk := fs.String("disk", "", "use `file` as the disk image")
	fs.Parse(args)
	if fs.NArg() != 1 || (*to != 0 && *to <= *from) {
		fs.Usage()
		return 2
	}
	path := fs.Arg(0)
	end, err := replay.EndStep(path)
	if err != nil {
		return fail(err)
	}
	if *to == 0 || *to > end {
		*to = end
	}
	spec := replay.Spec{Firmware: *firmware, Kernel: *kernel, Initrd: *initrd, Disk: *disk}
	s, err := replay.JumpTo(path, spec, *snapDir, *from, io.Discard)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	w, closeTrace, err := openTrace(*out)
	if err != nil {
		return fail(err)
	}
	defer closeTrace()
	if err := s.VM.EnableTrace(w); err != nil {
		return fail(err)
	}
	res, err := s.VM.Run(*to - s.VM.Now())
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "rvsim: traced steps %d to %d (%d instructions retired by then)\n", *from, res.Steps, res.Instret)
	return 0
}
