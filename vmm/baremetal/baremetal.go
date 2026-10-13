// Package baremetal runs a bare-metal ELF that reports completion through
// HTIF tohost, as riscv-tests and riscv-arch-test programs do.
package baremetal

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ezekielelvis/rvsim/vmm/loader"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/trace"
)

// Options configure a run. Zero values select defaults.
type Options struct {
	MaxSteps uint64    // default 100M
	Flags    uint32    // machine.Flag*; default machine.DefaultConfig().Flags
	RAMSize  uint64    // default 256 MiB
	Trace    io.Writer // if set, receives a Spike --log-commits style trace
}

// Result is the outcome of a run that reached an HTIF exit.
type Result struct {
	ExitCode uint64 // tohost >> 1; 0 means pass for riscv-tests
	Steps    uint64
	Instret  uint64
}

// ErrStepLimit is returned when the program does not halt within MaxSteps.
var ErrStepLimit = errors.New("baremetal: step limit reached without HTIF exit")

const (
	defaultMaxSteps = 100_000_000
	batchSteps      = 1 << 20
	traceBufBytes   = 1 << 20
)

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

	m, err := machine.New(cfg)
	if err != nil {
		return Result{}, err
	}
	defer m.Close()
	if err := img.Load(m); err != nil {
		return Result{}, err
	}

	var tw *trace.SpikeWriter
	if opt.Trace != nil {
		if err := m.EnableTrace(traceBufBytes); err != nil {
			return Result{}, err
		}
		tw = trace.NewSpikeWriter(opt.Trace)
	}
	drain := func() error {
		if tw == nil {
			return nil
		}
		recs, err := m.TraceRecords()
		if err != nil {
			return err
		}
		if err := tw.Write(recs); err != nil {
			return err
		}
		return m.ConsumeTrace()
	}

	var steps uint64
	for {
		exit, err := m.Run(min(batchSteps, maxSteps-steps))
		if err != nil {
			return Result{}, err
		}
		if err := drain(); err != nil {
			return Result{}, err
		}
		steps = exit.Steps
		res := Result{Steps: exit.Steps, Instret: exit.Instret}
		switch exit.Reason {
		case machine.ExitHalt:
			if tw != nil {
				if err := tw.Flush(); err != nil {
					return Result{}, err
				}
			}
			// HTIF device 0, command 0: exit with code tohost>>1 when bit 0 is set.
			if exit.Value&1 == 0 {
				return res, fmt.Errorf("baremetal: unsupported HTIF request %#x", exit.Value)
			}
			res.ExitCode = exit.Value >> 1
			return res, nil
		case machine.ExitBudget, machine.ExitTraceFull:
			if exit.Steps >= maxSteps {
				if tw != nil {
					_ = tw.Flush()
				}
				return res, ErrStepLimit
			}
		default:
			return res, fmt.Errorf("baremetal: unexpected exit %v at pc %#x", exit.Reason, exit.PC)
		}
	}
}
