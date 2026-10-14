package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ezekielelvis/rvsim/vmm/boot"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/snapshot"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

func snapshotCmd(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: rvsim snapshot [flags] -at N -o boot.rvsnap -firmware fw_jump.bin -kernel Image\n\n"+
			"Boots like \"rvsim boot\" (without input), runs to step N, and saves the\n"+
			"whole machine (core, RAM, devices, pending events, disk) to a snapshot\n"+
			"that \"rvsim restore\" resumes. The disk image itself is not modified.\n\nflags:\n")
		fs.PrintDefaults()
	}
	img := addImageFlags(fs)
	at := fs.Uint64("at", 0, "take the snapshot at step `n`")
	out := fs.String("o", "", "write the snapshot to `file`")
	quiet := fs.Bool("q", false, "do not print the guest's console output")
	fs.Parse(args)
	if fs.NArg() != 0 || *out == "" || *at == 0 || *img.firmware == "" || *img.kernel == "" {
		fs.Usage()
		return 2
	}
	images := boot.Images{Bootargs: img.bootargs()}
	for _, f := range []struct {
		path string
		dst  *[]byte
	}{{*img.firmware, &images.Firmware}, {*img.kernel, &images.Kernel}, {*img.initrd, &images.Initrd}} {
		if f.path == "" {
			continue
		}
		b, err := os.ReadFile(f.path)
		if err != nil {
			return fail(err)
		}
		*f.dst = b
	}
	cfg := machine.DefaultConfig()
	cfg.RAMSize = *img.memMiB << 20
	vcfg := vm.Config{Machine: cfg, Console: os.Stdout}
	if *quiet {
		vcfg.Console = io.Discard
	}
	if *img.disk != "" {
		// Work on a copy: the snapshot carries the disk as it is at step N.
		d, closeDisk, err := copyDisk(*img.disk, *img.diskRO)
		if err != nil {
			return fail(err)
		}
		defer closeDisk()
		vcfg.Disk = d
	}
	v, err := vm.New(vcfg)
	if err != nil {
		return fail(err)
	}
	defer v.Close()
	if err := boot.Load(v, cfg, images); err != nil {
		return fail(err)
	}
	res, err := v.Run(*at)
	if err != nil {
		return fail(err)
	}
	if res.Exited {
		return fail(fmt.Errorf("the guest exited at step %d, before step %d", res.Steps, *at))
	}
	if err := snapshot.SaveFile(*out, v); err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "rvsim: snapshot at step %d (%d instructions) saved to %s\n", res.Steps, res.Instret, *out)
	return 0
}

func restoreCmd(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: rvsim restore [flags] boot.rvsnap\n\n"+
			"Resumes a snapshot made by \"rvsim snapshot\" exactly where it stopped:\n"+
			"the console is stdout and standard input is fed to the UART, as with\n"+
			"\"rvsim boot\". A snapshot with a disk needs -disk-out for the disk's\n"+
			"contents, or uses a temporary file that is deleted at exit.\n\nflags:\n")
		fs.PrintDefaults()
	}
	diskOut := fs.String("disk-out", "", "write the snapshot's disk to `file` and use it")
	maxSteps := fs.Uint64("max-steps", 0, "stop after `n` more steps (0: no limit)")
	tracePath := fs.String("trace", "", traceUsage)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	snap, err := snapshot.LoadFile(fs.Arg(0))
	if err != nil {
		return fail(err)
	}
	trace, closeTrace, err := openTrace(*tracePath)
	if err != nil {
		return fail(err)
	}
	defer closeTrace()
	diskPath := *diskOut
	if snap.Config.Disk != nil && diskPath == "" {
		f, err := os.CreateTemp("", "rvsim-disk-*.img")
		if err != nil {
			return fail(err)
		}
		diskPath = f.Name()
		f.Close()
		defer os.Remove(diskPath)
	}
	v, disk, err := snap.Restore(vm.Config{Console: os.Stdout, Input: readInput(os.Stdin), Trace: trace}, diskPath)
	if err != nil {
		return fail(err)
	}
	if disk != nil {
		defer disk.Close()
	}
	defer v.Close()
	fmt.Fprintf(os.Stderr, "rvsim: restored at step %d\n", snap.Steps())
	limit := *maxSteps
	if limit == 0 {
		limit = ^uint64(0)
	}
	res, err := v.Run(limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rvsim: %v (after %d steps)\n", err, res.Steps)
		return 1
	}
	if !res.Exited {
		fmt.Fprintf(os.Stderr, "rvsim: stopped after %d steps\n", res.Steps)
	}
	return int(min(res.ExitCode, 255))
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "rvsim:", err)
	return 1
}
