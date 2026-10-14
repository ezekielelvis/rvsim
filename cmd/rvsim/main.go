// Command rvsim is the rvsim CLI. Subcommands arrive with their phases:
// record, replay, snapshot, restore, gdb, trace and fault are still to come.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ezekielelvis/rvsim/vmm/baremetal"
	"github.com/ezekielelvis/rvsim/vmm/boot"
	"github.com/ezekielelvis/rvsim/vmm/devices/virtio/blk"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

const usage = `usage: rvsim <command> [flags]

commands:
  boot      boot OpenSBI and a Linux kernel
  gdb       boot under a GDB remote stub
  record    boot and record the session for replay
  replay    replay a recorded session and check it is identical
  restore   resume a snapshot
  run       run a bare-metal ELF that exits through HTIF tohost
  snapshot  boot to a given step and save the whole machine
  version   print the rvsim and core ABI versions

Run "rvsim <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "boot":
		os.Exit(bootCmd(os.Args[2:]))
	case "gdb":
		os.Exit(gdbCmd(os.Args[2:]))
	case "record":
		os.Exit(recordCmd(os.Args[2:]))
	case "replay":
		os.Exit(replayCmd(os.Args[2:]))
	case "restore":
		os.Exit(restoreCmd(os.Args[2:]))
	case "snapshot":
		os.Exit(snapshotCmd(os.Args[2:]))
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "version":
		build := machine.CoreBuild()
		if len(build) > 12 {
			build = build[:12]
		}
		fmt.Printf("rvsim (core ABI v%d, core build %s)\n", machine.ABIVersion, build)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "rvsim: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: rvsim run [flags] program.elf\n\n"+
			"Runs a bare-metal RV64 ELF on the rvsim platform (CLINT, PLIC, UART at\n"+
			"0x1000_0000). UART and HTIF console output go to stdout; the program\n"+
			"exits through HTIF tohost, and the exit status is its exit code\n"+
			"(tohost>>1), or 1 on an emulator error.\n\nflags:\n")
		fs.PrintDefaults()
	}
	tracePath := fs.String("trace", "", traceUsage)
	maxSteps := fs.Uint64("max-steps", 100_000_000, "stop with an error after `n` steps")
	misaligned := fs.Bool("misaligned", false, "perform misaligned loads/stores instead of trapping")
	stdin := fs.Bool("stdin", false, "feed standard input to the guest's UART")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	opt := baremetal.Options{MaxSteps: *maxSteps, Flags: machine.DefaultConfig().Flags, Console: os.Stdout}
	if *stdin {
		opt.Input = readInput(os.Stdin)
	}
	if *misaligned {
		opt.Flags |= machine.FlagMisalignedOK
	}
	trace, closeTrace, err := openTrace(*tracePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		return 1
	}
	defer closeTrace()
	opt.Trace = trace

	res, err := baremetal.RunFile(fs.Arg(0), opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		return 1
	}
	if res.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "rvsim: program exited with code %d after %d instructions\n", res.ExitCode, res.Instret)
	}
	return int(min(res.ExitCode, 255))
}

const traceUsage = "write a Spike --log-commits style commit log to `file` (- for stdout)"

// openTrace opens the -trace destination; a nil writer means no trace.
func openTrace(path string) (io.Writer, func(), error) {
	switch path {
	case "":
		return nil, func() {}, nil
	case "-":
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	bw := bufio.NewWriter(f)
	return bw, func() { bw.Flush(); f.Close() }, nil
}

func bootCmd(args []string) int {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: rvsim boot [flags] -firmware fw_jump.bin -kernel Image\n\n"+
			"Boots OpenSBI fw_jump and a Linux Image on the rvsim platform, with an\n"+
			"optional initramfs and virtio disk. The UART is the console: its output\n"+
			"goes to stdout and standard input is fed to it. Guest writes to the disk\n"+
			"go straight to the image file.\n\nflags:\n")
		fs.PrintDefaults()
	}
	flags := addImageFlags(fs)
	maxSteps := fs.Uint64("max-steps", 0, "stop after `n` steps (0: no limit)")
	tracePath := fs.String("trace", "", traceUsage)
	fs.Parse(args)
	if fs.NArg() != 0 || *flags.firmware == "" || *flags.kernel == "" {
		fs.Usage()
		return 2
	}

	img := boot.Images{Bootargs: flags.bootargs()}
	for _, f := range []struct {
		path string
		dst  *[]byte
	}{{*flags.firmware, &img.Firmware}, {*flags.kernel, &img.Kernel}, {*flags.initrd, &img.Initrd}} {
		if f.path == "" {
			continue
		}
		b, err := os.ReadFile(f.path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rvsim:", err)
			return 1
		}
		*f.dst = b
	}
	trace, closeTrace, err := openTrace(*tracePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		return 1
	}
	defer closeTrace()

	cfg := machine.DefaultConfig()
	cfg.RAMSize = *flags.memMiB << 20
	vcfg := vm.Config{Machine: cfg, Console: os.Stdout, Input: readInput(os.Stdin), Trace: trace}
	if vcfg.Faults, err = flags.liveFaults(); err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		return 1
	}
	if *flags.disk != "" {
		d, closeDisk, err := openDisk(*flags.disk, *flags.diskRO)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rvsim:", err)
			return 1
		}
		defer closeDisk()
		vcfg.Disk = d
	}
	v, err := vm.New(vcfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		return 1
	}
	defer v.Close()
	if err := boot.Load(v, cfg, img); err != nil {
		fmt.Fprintln(os.Stderr, "rvsim:", err)
		return 1
	}
	limit := *maxSteps
	if limit == 0 {
		limit = ^uint64(0)
	}
	res, err := v.Run(limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rvsim: %v (after %d steps)\n", err, res.Steps)
		return 1
	}
	switch {
	case !res.Exited:
		fmt.Fprintf(os.Stderr, "rvsim: stopped after %d steps\n", res.Steps)
	case res.Reset:
		fmt.Fprintf(os.Stderr, "rvsim: guest requested a reboot after %d steps; stopping\n", res.Steps)
	}
	return int(min(res.ExitCode, 255))
}

// copyDisk opens a private, temporary copy of a disk image.
func copyDisk(path string, readOnly bool) (*blk.Config, func(), error) {
	src, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer src.Close()
	f, err := os.CreateTemp("", "rvsim-disk-*.img")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { f.Close(); os.Remove(f.Name()) }
	n, err := io.Copy(f, src)
	if err == nil && n%blk.SectorSize != 0 {
		err = fmt.Errorf("%s: size %d is not a multiple of %d bytes", path, n, blk.SectorSize)
	}
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return &blk.Config{Disk: f, Size: uint64(n), ReadOnly: readOnly, Serial: "rvsim-disk0"}, cleanup, nil
}

// openDisk opens a disk image for a virtio block device.
func openDisk(path string, readOnly bool) (*blk.Config, func(), error) {
	mode := os.O_RDWR
	if readOnly {
		mode = os.O_RDONLY
	}
	f, err := os.OpenFile(path, mode, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if st.Size()%blk.SectorSize != 0 {
		f.Close()
		return nil, nil, fmt.Errorf("%s: size %d is not a multiple of %d bytes", path, st.Size(), blk.SectorSize)
	}
	cfg := &blk.Config{Disk: f, Size: uint64(st.Size()), ReadOnly: readOnly, Serial: "rvsim-disk0"}
	return cfg, func() { f.Close() }, nil
}

// readInput copies r into a channel byte by byte. The run loop alone decides
// when each byte reaches the guest; this goroutine never touches the VM.
func readInput(r io.Reader) <-chan byte {
	ch := make(chan byte, 4096)
	go func() {
		defer close(ch)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			for _, b := range buf[:n] {
				ch <- b
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}
