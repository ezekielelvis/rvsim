package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/ezekielelvis/rvsim/vmm/boot"
	"github.com/ezekielelvis/rvsim/vmm/gdb"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

const defaultListen = "localhost:1234"

// acceptOne listens on addr and returns the first connection.
func acceptOne(addr string) (net.Conn, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	fmt.Fprintf(os.Stderr, "rvsim: waiting for GDB on %s (target remote %s)\n", ln.Addr(), ln.Addr())
	return ln.Accept()
}

func protocolLog(verbose bool) func(string, ...any) {
	if !verbose {
		return nil
	}
	l := log.New(os.Stderr, "gdb: ", 0)
	return l.Printf
}

func gdbCmd(args []string) int {
	fs := flag.NewFlagSet("gdb", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: rvsim gdb [flags] -firmware fw_jump.bin -kernel Image\n\n"+
			"Boots like \"rvsim boot\", optionally runs to step -at, then waits for a\n"+
			"GDB connection (GDB Remote Serial Protocol) and runs under its control:\n"+
			"registers, memory through the MMU, breakpoints, step, continue, Ctrl-C.\n"+
			"Example: riscv64-unknown-elf-gdb vmlinux -ex 'target remote localhost:1234'\n"+
			"Reverse execution needs a recording: see \"rvsim replay -gdb\".\n\nflags:\n")
		fs.PrintDefaults()
	}
	img := addImageFlags(fs)
	listen := fs.String("listen", defaultListen, "TCP `address` to wait for GDB on")
	at := fs.Uint64("at", 0, "run to step `n` before waiting for GDB")
	verbose := fs.Bool("v", false, "log the protocol to stderr")
	fs.Parse(args)
	if fs.NArg() != 0 || *img.firmware == "" || *img.kernel == "" {
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
	vcfg := vm.Config{Machine: cfg, Console: os.Stdout, Input: readInput(os.Stdin)}
	if *img.disk != "" {
		d, closeDisk, err := openDisk(*img.disk, *img.diskRO)
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
	if *at > 0 {
		if res, err := v.Run(*at); err != nil || res.Exited {
			return fail(fmt.Errorf("before step %d: %+v %v", *at, res, err))
		}
	}
	conn, err := acceptOne(*listen)
	if err != nil {
		return fail(err)
	}
	end, err := gdb.Serve(conn, gdb.NewVMTarget(v), protocolLog(*verbose))
	conn.Close()
	if err != nil {
		return fail(err)
	}
	if end == gdb.Killed {
		fmt.Fprintln(os.Stderr, "rvsim: killed by GDB")
		return 0
	}
	// Detached or disconnected: let the guest run on.
	fmt.Fprintln(os.Stderr, "rvsim: GDB detached; running")
	res, err := v.Run(^uint64(0))
	if err != nil {
		return fail(err)
	}
	return int(min(res.ExitCode, 255))
}

// replayGDB serves a replay to GDB, with reverse execution.
func replayGDB(path, listen string, spec replaySpec, verbose bool) int {
	t, err := gdb.NewReplayTarget(path, spec, 50_000_000, os.Stdout)
	if err != nil {
		return fail(err)
	}
	defer t.Close()
	conn, err := acceptOne(listen)
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	if _, err := gdb.Serve(conn, t, protocolLog(verbose)); err != nil {
		return fail(err)
	}
	return 0
}
