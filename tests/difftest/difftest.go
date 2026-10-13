// Package difftest compares rvsim's commit log against Spike's
// --log-commits output and reports the first divergence.
package difftest

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Divergence describes the first line where the two logs differ.
type Divergence struct {
	Line    int      // 1-based index into rvsim's log
	Ours    string   // "" if rvsim's log ended first
	Spike   string   // "" if Spike's log ended first
	Context []string // up to ContextLines matching lines before the divergence
}

// ContextLines is how many agreeing lines a Divergence carries.
const ContextLines = 8

func (d *Divergence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "first divergence at commit %d\n", d.Line)
	for _, l := range d.Context {
		fmt.Fprintf(&b, "         %s\n", l)
	}
	fmt.Fprintf(&b, "  rvsim: %s\n", orEOF(d.Ours))
	fmt.Fprintf(&b, "  spike: %s", orEOF(d.Spike))
	return b.String()
}

func orEOF(s string) string {
	if s == "" {
		return "<end of log>"
	}
	return s
}

// Compare reads both logs and returns the first divergence, or nil if every
// line of ours matches Spike. Spike lines before the first commit at or
// above startPC (its boot ROM) are skipped. Spike keeps running after the
// HTIF exit store until it polls tohost, so its extra trailing lines are
// ignored.
func Compare(ours, spike io.Reader, startPC uint64) (*Divergence, error) {
	oursSc := bufio.NewScanner(ours)
	spikeSc := bufio.NewScanner(spike)
	oursSc.Buffer(make([]byte, 64<<10), 1<<20)
	spikeSc.Buffer(make([]byte, 64<<10), 1<<20)

	var spikeLine string
	started := false
	nextSpike := func() bool {
		for spikeSc.Scan() {
			l := spikeSc.Text()
			if !strings.HasPrefix(l, "core ") {
				continue // e.g. HTIF or warning output
			}
			if !started {
				pc, ok := commitPC(l)
				if !ok || pc < startPC {
					continue
				}
				started = true
			}
			spikeLine = l
			return true
		}
		spikeLine = ""
		return false
	}

	var ctx []string
	line := 0
	for oursSc.Scan() {
		line++
		our := oursSc.Text()
		if !nextSpike() || our != spikeLine {
			return &Divergence{Line: line, Ours: our, Spike: spikeLine, Context: ctx}, nil
		}
		ctx = append(ctx, our)
		if len(ctx) > ContextLines {
			ctx = ctx[1:]
		}
	}
	if err := oursSc.Err(); err != nil {
		return nil, err
	}
	return nil, spikeSc.Err()
}

// commitPC extracts the pc from "core   0: 3 0x0000000080000000 (...".
func commitPC(line string) (uint64, bool) {
	f := strings.Fields(line)
	if len(f) < 4 {
		return 0, false
	}
	pc, err := strconv.ParseUint(strings.TrimPrefix(f[3], "0x"), 16, 64)
	return pc, err == nil
}

// SpikeConfig selects the reference model's configuration. It must mirror
// what rvsim implements, or the logs diverge for uninteresting reasons.
// Debug triggers (Sdtrig) are always disabled: rvsim does not implement them,
// and with triggers Spike's MRET also writes tcontrol.
type SpikeConfig struct {
	Path       string // spike binary
	ISA        string // e.g. "rv64i_zicsr_zifencei"
	Priv       string // e.g. "MU"
	PMPRegions int
}

// RunSpike runs elf under Spike and returns its commit log. Spike's exit
// status is ignored: a failing test is still a valid log to compare.
func RunSpike(cfg SpikeConfig, elf string) ([]byte, error) {
	args := []string{
		"--isa=" + cfg.ISA,
		"--priv=" + cfg.Priv,
		"--pmpregions=" + strconv.Itoa(cfg.PMPRegions),
		"--triggers=0",
		"--log-commits",
		elf,
	}
	var log bytes.Buffer
	cmd := exec.Command(cfg.Path, args...)
	cmd.Stderr = &log // Spike writes the commit log to stderr
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, fmt.Errorf("difftest: running spike: %w", err)
		}
	}
	return log.Bytes(), nil
}
