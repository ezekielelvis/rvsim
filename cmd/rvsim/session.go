package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"

	"github.com/ezekielelvis/rvsim/vmm/replay"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

// imageFlags are the image and platform flags boot, record and replay share.
type imageFlags struct {
	firmware, kernel, initrd, disk *string
	diskRO                         *bool
	appendArgs                     *string
	memMiB                         *uint64
}

func addImageFlags(fs *flag.FlagSet) imageFlags {
	return imageFlags{
		firmware:   fs.String("firmware", "", "OpenSBI fw_jump `binary`, loaded at the start of RAM"),
		kernel:     fs.String("kernel", "", "Linux `Image`, loaded at RAM + 2 MiB"),
		initrd:     fs.String("initrd", "", "initramfs `file` (cpio)"),
		disk:       fs.String("disk", "", "disk `image` for a virtio block device (/dev/vda in Linux)"),
		diskRO:     fs.Bool("disk-ro", false, "make the disk read-only"),
		appendArgs: fs.String("append", "", "kernel command `line` (default \"console=ttyS0 earlycon\", plus\n\"root=/dev/vda rw\" with -disk and no -initrd)"),
		memMiB:     fs.Uint64("mem", 256, "RAM size in `MiB`"),
	}
}

func (f imageFlags) bootargs() string {
	if *f.appendArgs != "" {
		return *f.appendArgs
	}
	args := "console=ttyS0 earlycon"
	if *f.disk != "" && *f.initrd == "" {
		args += " root=/dev/vda rw"
	}
	return args
}

func (f imageFlags) spec() replay.Spec {
	return replay.Spec{
		Firmware: *f.firmware, Kernel: *f.kernel, Initrd: *f.initrd, Disk: *f.disk,
		DiskReadOnly: *f.diskRO, Bootargs: f.bootargs(), RAMSize: *f.memMiB << 20,
	}
}

// chunk is how many steps record runs between checks for Ctrl-C.
const chunk = 10_000_000

func recordCmd(args []string) int {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: rvsim record [flags] -o session.rvrec -firmware fw_jump.bin -kernel Image\n\n"+
			"Boots like \"rvsim boot\" and records the session: every byte of standard\n"+
			"input with the virtual-time step it reached the guest, and the machine's\n"+
			"state hashes every -checkpoint steps. \"rvsim replay\" reproduces it.\n"+
			"Ctrl-C ends the recording. The disk image is not modified: the session\n"+
			"runs on a copy (see -keep-disk).\n\nflags:\n")
		fs.PrintDefaults()
	}
	img := addImageFlags(fs)
	out := fs.String("o", "", "write the recording to `file`")
	every := fs.Uint64("checkpoint", replay.DefaultCheckpointEvery, "record state hashes every `n` steps")
	maxSteps := fs.Uint64("max-steps", 0, "stop after `n` steps (0: no limit)")
	keepDisk := fs.String("keep-disk", "", "save the disk as the session left it to `file`")
	fs.Parse(args)
	if fs.NArg() != 0 || *out == "" || *img.firmware == "" || *img.kernel == "" || *every == 0 {
		fs.Usage()
		return 2
	}
	spec := img.spec()
	spec.CheckpointEvery, spec.KeepDisk = *every, *keepDisk

	s, err := replay.StartRecording(spec, *out, os.Stdout, readInput(os.Stdin))
	if err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		return 1
	}
	defer func() {
		if err := s.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "rvsim:", err)
		}
	}()

	var stop atomic.Bool
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		<-sig
		stop.Store(true)
	}()

	limit := *maxSteps
	if limit == 0 {
		limit = ^uint64(0)
	}
	var res vm.Result
	for {
		res, err = s.Run(min(chunk, limit-s.VM.Now()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "rvsim: %v (after %d steps)\n", err, res.Steps)
			return 1
		}
		if res.Exited || res.Steps >= limit || stop.Load() {
			break
		}
	}
	if err := s.Finish(res, stop.Load()); err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "rvsim: recorded %d steps (%d instructions) to %s\n", res.Steps, res.Instret, *out)
	return 0
}

func replayCmd(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: rvsim replay [flags] session.rvrec\n\n"+
			"Replays a recording made by \"rvsim record\": feeds the recorded input at\n"+
			"the recorded steps and compares the machine's state hashes at every\n"+
			"checkpoint. Images are read from the paths in the recording unless\n"+
			"overridden; their contents must match. Exits 0 if the replay is identical,\n"+
			"1 if it diverges (reporting where).\n\nflags:\n")
		fs.PrintDefaults()
	}
	firmware := fs.String("firmware", "", "use `file` as the firmware image")
	kernel := fs.String("kernel", "", "use `file` as the kernel image")
	initrd := fs.String("initrd", "", "use `file` as the initramfs")
	disk := fs.String("disk", "", "use `file` as the disk image")
	keepDisk := fs.String("keep-disk", "", "save the disk as the replay left it to `file`")
	tracePath := fs.String("trace", "", traceUsage)
	traceFrom := fs.Uint64("trace-from", 0, "start tracing at step `n`")
	quiet := fs.Bool("q", false, "do not print the guest's console output")
	snapEvery := fs.Uint64("snapshot-every", 0, "save a snapshot every `n` steps into -snapshot-dir")
	snapDir := fs.String("snapshot-dir", "", "`directory` of snapshots for -snapshot-every and -jump")
	jump := fs.Uint64("jump", 0, "go to step `n` (restoring the nearest snapshot in -snapshot-dir),\nprint the state hashes there, and stop (or, with -trace, trace on from there)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	var console io.Writer = os.Stdout
	if *quiet {
		console = io.Discard
	}
	spec := replay.Spec{Firmware: *firmware, Kernel: *kernel, Initrd: *initrd, Disk: *disk, KeepDisk: *keepDisk}
	if *snapEvery != 0 && *snapDir == "" {
		fs.Usage()
		return 2
	}
	var s *replay.Session
	var err error
	if *jump != 0 {
		s, err = replay.JumpTo(fs.Arg(0), spec, *snapDir, *jump, console)
	} else {
		s, err = replay.StartReplay(fs.Arg(0), spec, console)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		return 1
	}
	defer s.Close()
	for _, w := range s.Warnings {
		fmt.Fprintln(os.Stderr, "rvsim: warning:", w)
	}
	if *jump != 0 {
		h, err := s.VM.StateHashes()
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(os.Stderr, "rvsim: at step %d (%d instructions): core %016x ram %016x vmm %016x\n",
			s.VM.Now(), s.VM.Instret(), h.Core, h.RAM, h.VMM)
		if *tracePath == "" {
			return 0
		}
	}
	if *snapEvery != 0 {
		if err := os.MkdirAll(*snapDir, 0o755); err != nil {
			return fail(err)
		}
		s.SnapshotEvery(*snapEvery, *snapDir)
	}
	if *tracePath != "" {
		trace, closeTrace, err := openTrace(*tracePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rvsim:", err)
			return 1
		}
		defer closeTrace()
		if *traceFrom > s.VM.Now() {
			if _, err := s.Run(*traceFrom - s.VM.Now()); err != nil {
				fmt.Fprintln(os.Stderr, "rvsim:", err)
				return 1
			}
		}
		if err := s.VM.EnableTrace(trace); err != nil {
			fmt.Fprintln(os.Stderr, "rvsim:", err)
			return 1
		}
	}
	rep, err := s.Replay()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		var d *replay.Divergence
		if errors.As(err, &d) {
			fmt.Fprintf(os.Stderr, "rvsim: to trace the window: rvsim replay -q -trace window.log -trace-from %d %s\n",
				d.LastGoodStep, fs.Arg(0))
		}
		return 1
	}
	end := rep.End
	if rep.Truncated {
		end = "truncated recording"
	}
	fmt.Fprintf(os.Stderr, "rvsim: replay identical: %d checkpoints, %d steps, %d instructions (%s)\n",
		rep.Checkpoints, rep.Steps, rep.Instret, end)
	return 0
}
