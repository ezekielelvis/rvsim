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
	"github.com/ezekielelvis/rvsim/vmm/machine"
)

const usage = `usage: rvsim <command> [flags]

commands:
  run       run a bare-metal ELF that exits through HTIF tohost
  version   print the rvsim and core ABI versions

Run "rvsim <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "version":
		fmt.Printf("rvsim (core ABI v%d)\n", machine.ABIVersion)
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
			"Runs a bare-metal RV64 ELF that communicates through HTIF tohost: console\n"+
			"output goes to stdout, and the exit status is the program's exit code\n"+
			"(tohost>>1), or 1 on an emulator error.\n\nflags:\n")
		fs.PrintDefaults()
	}
	tracePath := fs.String("trace", "", "write a Spike --log-commits style commit log to `file` (- for stdout)")
	maxSteps := fs.Uint64("max-steps", 100_000_000, "stop with an error after `n` steps")
	misaligned := fs.Bool("misaligned", false, "perform misaligned loads/stores instead of trapping")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	opt := baremetal.Options{MaxSteps: *maxSteps, Flags: machine.DefaultConfig().Flags, Console: os.Stdout}
	if *misaligned {
		opt.Flags |= machine.FlagMisalignedOK
	}
	if *tracePath != "" {
		var w io.Writer = os.Stdout
		if *tracePath != "-" {
			f, err := os.Create(*tracePath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "rvsim:", err)
				return 1
			}
			defer f.Close()
			bw := bufio.NewWriter(f)
			defer bw.Flush()
			w = bw
		}
		opt.Trace = w
	}

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
