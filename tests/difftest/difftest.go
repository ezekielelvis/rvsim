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

// Result is the outcome of Compare.
type Result struct {
	Divergence *Divergence // nil if the logs agree
	Commits    int         // rvsim commits compared
	// SpuriousSC counts store-conditionals that failed in Spike but
	// succeeded in rvsim and were resynchronized (see Compare).
	SpuriousSC int
}

// maxResync bounds how far ahead Compare looks in Spike's log after a
// spurious SC failure. Spike's retry is one LR/SC loop iteration.
const maxResync = 16

// Compare reads both logs and reports the first divergence. Spike lines
// before the first commit at or above startPC (its boot ROM) are skipped.
// Spike keeps running after the HTIF exit store until it polls tohost, so
// its extra trailing lines are ignored.
//
// One non-architectural difference is tolerated: Spike drops the LR
// reservation at the end of each scheduling quantum (sim_t::step, every
// 5000 steps or at a trap), so an SC can fail in Spike where rvsim's
// succeeds. Both are legal (Unpriv §14.2 lets an SC fail for
// implementation-specific reasons). When Spike's SC fails at the same pc
// where rvsim's succeeded, Compare skips Spike's retry lines until the logs
// agree again, and counts it in Result.SpuriousSC.
func Compare(ours, spike io.Reader, startPC uint64) (Result, error) {
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

	var res Result
	var ctx []string
	for oursSc.Scan() {
		res.Commits++
		our := oursSc.Text()
		ok := nextSpike()
		if ok && our != spikeLine && spuriousSCFailure(our, spikeLine) {
			for i := 0; i < maxResync && ok && our != spikeLine; i++ {
				ok = nextSpike()
			}
			if ok && our == spikeLine {
				res.SpuriousSC++
			}
		}
		if !ok || our != spikeLine {
			res.Divergence = &Divergence{Line: res.Commits, Ours: our, Spike: spikeLine, Context: ctx}
			return res, nil
		}
		ctx = append(ctx, our)
		if len(ctx) > ContextLines {
			ctx = ctx[1:]
		}
	}
	if err := oursSc.Err(); err != nil {
		return res, err
	}
	return res, spikeSc.Err()
}

// spuriousSCFailure reports whether spike's line is a failed SC (rd = 1, no
// store) at the same pc and encoding as our line, which is an SC.
func spuriousSCFailure(our, spike string) bool {
	o, s := strings.Fields(our), strings.Fields(spike)
	if len(o) < 5 || len(s) != 7 || o[3] != s[3] || o[4] != s[4] {
		return false
	}
	insn, err := strconv.ParseUint(strings.Trim(s[4], "()"), 0, 32)
	if err != nil || insn&0x7f != 0b0101111 || insn>>27 != 0b00011 {
		return false
	}
	return s[6] == "0x0000000000000001"
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
