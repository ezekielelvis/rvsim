// Command rvsim is the rvsim CLI. Subcommands (run, record, replay, snapshot,
// restore, gdb, trace, fault) arrive with their phases.
package main

import (
	"fmt"
	"os"

	"github.com/ezekielelvis/rvsim/vmm/machine"
)

const usage = `usage: rvsim <command>

commands:
  version   print the rvsim and core ABI versions
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Printf("rvsim (core ABI v%d)\n", machine.ABIVersion)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "rvsim: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
