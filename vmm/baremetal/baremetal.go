// Package baremetal runs a bare-metal ELF that reports completion through
// HTIF tohost, as riscv-tests and riscv-arch-test programs do.
package baremetal

import (
	"encoding/binary"
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
	Console  io.Writer // receives HTIF console output; discarded if nil
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
			done, code, err := handleHTIF(m, cfg, exit.Value, opt.Console)
			if err != nil {
				return res, err
			}
			if !done {
				continue
			}
			if tw != nil {
				if err := tw.Flush(); err != nil {
					return Result{}, err
				}
			}
			res.ExitCode = code
			return res, nil
		case machine.ExitWFI:
			// Idle until the CLINT timer fires, the only wake source a bare
			// program has here. (The Phase 4 scheduler generalizes this.)
			if exit.TimerStep == ^uint64(0) || exit.TimerStep <= exit.Steps || exit.TimerStep > maxSteps {
				return res, fmt.Errorf("baremetal: WFI at pc %#x with no interrupt that can wake the hart", exit.PC)
			}
			if err := m.AdvanceIdle(exit.TimerStep - exit.Steps); err != nil {
				return res, err
			}
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

// HTIF commands are (device << 56) | (command << 48) | payload, following
// riscv-isa-sim's fesvr.
const (
	htifDevSyscall = 0
	htifDevConsole = 1
	htifCmdPutchar = 1
)

// handleHTIF services one tohost command. It reports done with the exit code
// when the program has finished. Every other command is acknowledged by
// clearing tohost (and, for console output, writing fromhost) so the guest
// can issue the next one.
func handleHTIF(m *machine.Machine, cfg machine.Config, v uint64, console io.Writer) (done bool, code uint64, err error) {
	dev, cmd, payload := v>>56, (v>>48)&0xff, v&(1<<48-1)
	switch {
	case dev == htifDevSyscall && cmd == 0 && payload&1 == 1:
		// Exit with code payload >> 1.
		return true, payload >> 1, nil
	case dev == htifDevConsole && cmd == htifCmdPutchar:
		if console != nil {
			if _, err := console.Write([]byte{byte(payload)}); err != nil {
				return false, 0, err
			}
		}
		if err := writeU64(m, cfg.HTIFToHost, 0); err != nil {
			return false, 0, err
		}
		if cfg.HTIFFromHost != 0 {
			// fesvr acknowledges putchar with 0x100 | char.
			if err := writeU64(m, cfg.HTIFFromHost, dev<<56|cmd<<48|0x100|payload&0xff); err != nil {
				return false, 0, err
			}
		}
		return false, 0, nil
	default:
		return false, 0, fmt.Errorf("baremetal: unsupported HTIF command %#x (device %d, command %d)", v, dev, cmd)
	}
}

func writeU64(m *machine.Machine, addr, v uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return m.WritePhys(addr, b[:])
}
