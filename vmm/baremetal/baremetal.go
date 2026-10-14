// Package baremetal runs a bare-metal ELF that reports completion through
// HTIF tohost, as riscv-tests and riscv-arch-test programs do.
package baremetal

import (
	"errors"
	"io"
	"os"

	"github.com/ezekielelvis/rvsim/vmm/loader"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

// Options configure a run. Zero values select defaults.
type Options struct {
	MaxSteps uint64    // default 100M
	Flags    uint32    // machine.Flag*; default machine.DefaultConfig().Flags
	RAMSize  uint64    // default 256 MiB
	Trace    io.Writer // if set, receives a Spike --log-commits style trace
	Console  io.Writer // receives UART and HTIF console output; discarded if nil
	Input    <-chan byte
}

// Result is the outcome of a run that reached an HTIF exit.
type Result struct {
	ExitCode uint64 // tohost >> 1; 0 means pass for riscv-tests
	Steps    uint64
	Instret  uint64
}

// ErrStepLimit is returned when the program does not halt within MaxSteps.
var ErrStepLimit = errors.New("baremetal: step limit reached without HTIF exit")

const defaultMaxSteps = 100_000_000

// RunFile loads and runs the ELF at path.
func RunFile(path string, opt Options) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	img, err := loader.ParseELF(f)
	if err != nil {
		return Result{}, err
	}
	return Run(img, opt)
}

// Run runs a parsed image. The image must define the tohost symbol.
func Run(img *loader.Image, opt Options) (Result, error) {
	tohost, ok := img.Symbols["tohost"]
	if !ok {
		return Result{}, errors.New("baremetal: ELF has no tohost symbol")
	}
	cfg := machine.DefaultConfig()
	cfg.ResetPC = img.Entry
	cfg.HTIFToHost = tohost
	cfg.HTIFFromHost = img.Symbols["fromhost"]
	if opt.Flags != 0 {
		cfg.Flags = opt.Flags
	}
	if opt.RAMSize != 0 {
		cfg.RAMSize = opt.RAMSize
	}
	maxSteps := opt.MaxSteps
	if maxSteps == 0 {
		maxSteps = defaultMaxSteps
	}

	v, err := vm.New(vm.Config{Machine: cfg, Console: opt.Console, Input: opt.Input, HTIF: true, Trace: opt.Trace})
	if err != nil {
		return Result{}, err
	}
	defer v.Close()
	if err := img.Load(v.Machine()); err != nil {
		return Result{}, err
	}
	r, err := v.Run(maxSteps)
	res := Result{ExitCode: r.ExitCode, Steps: r.Steps, Instret: r.Instret}
	if err != nil {
		return res, err
	}
	if !r.Exited {
		return res, ErrStepLimit
	}
	return res, nil
}
