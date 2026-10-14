// Package difftest compares rvsim's commit log against Spike's
// --log-commits output and reports the first divergence.
package difftest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	// IDReads counts reads of mvendorid/marchid/mimpid whose values
	// differed, as they must between two implementations.
	IDReads int
}

// maxResync bounds how far ahead Compare looks in Spike's log after a
// spurious SC failure. Spike's retry is one LR/SC loop iteration.
const maxResync = 16

// Compare reads both logs and reports the first divergence. Spike lines
// before the first commit at or above startPC (its boot ROM) are skipped.
// Spike keeps running after the HTIF exit store until it polls tohost, so
// its extra trailing lines are ignored.
//
// Reads of the implementation-ID CSRs (mvendorid, marchid, mimpid) are
// compared without their values: those identify the implementation and
// rightly differ (Spike's marchid is 5). A later use of the value would still
// show up as a divergence.
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
			l = normalizeSpike(l)
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
		if ok && our != spikeLine && idCSRRead(our, spikeLine) {
			res.IDReads++
			spikeLine = our
		}
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

// idCSRRead reports whether both lines are the same CSR-read instruction of
// mvendorid, marchid or mimpid at the same pc, differing only in the value.
func idCSRRead(our, spike string) bool {
	o, s := strings.Fields(our), strings.Fields(spike)
	if len(o) != 7 || len(s) != 7 || o[3] != s[3] || o[4] != s[4] || o[5] != s[5] {
		return false
	}
	insn, err := strconv.ParseUint(strings.Trim(o[4], "()"), 0, 32)
	if err != nil || insn&0x7f != 0b1110011 || (insn>>12)&7 == 0 {
		return false
	}
	switch insn >> 20 {
	case 0xf11, 0xf12, 0xf13:
		return true
	}
	return false
}

// mvipRe matches the mvip (AIA) entry Spike logs alongside every mip write:
// an artifact of Spike implementing mip through mvip. rvsim has no AIA.
var mvipRe = regexp.MustCompile(` c777_mvip 0x[0-9a-f]+`)

// normalizeSpike removes Spike log entries for state rvsim does not have.
func normalizeSpike(line string) string {
	return mvipRe.ReplaceAllString(line, "")
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
	ISA        string // e.g. "rv64imac_zicsr_zifencei_zicntr"
	Priv       string // e.g. "MSU"
	PMPRegions int
	// MMUType overrides the device tree's mmu-type (e.g. "riscv,sbare");
	// Spike's own default is Sv57. Empty keeps Spike's default.
	MMUType string
}

// RunSpike runs elf under Spike and returns its commit log. Spike's exit
// status is ignored: a failing test is still a valid log to compare.
func RunSpike(cfg SpikeConfig, elf string) ([]byte, error) {
	args := []string{
		"--isa=" + cfg.ISA,
		"--priv=" + cfg.Priv,
		"--pmpregions=" + strconv.Itoa(cfg.PMPRegions),
		"--triggers=0",
	}
	if cfg.MMUType != "" {
		dtb, err := spikeDTB(cfg, args, elf)
		if err != nil {
			return nil, err
		}
		args = append(args, "--dtb="+dtb)
	}
	args = append(args, "--log-commits", elf)
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

var (
	dtbMu    sync.Mutex
	dtbCache = map[SpikeConfig]string{}
)

var mmuTypeRe = regexp.MustCompile(`mmu-type = "[^"]*";`)

// spikeDTB builds (once per configuration) a device tree blob that matches
// Spike's generated one except for mmu-type. Spike derives the supported
// satp modes from the device tree, so this is how its MMU is configured.
func spikeDTB(cfg SpikeConfig, args []string, elf string) (string, error) {
	dtbMu.Lock()
	defer dtbMu.Unlock()
	if p, ok := dtbCache[cfg]; ok {
		return p, nil
	}
	out, err := exec.Command(cfg.Path, append(append([]string{}, args...), "--dump-dts", elf)...).Output()
	if err != nil {
		return "", fmt.Errorf("difftest: spike --dump-dts: %w", err)
	}
	if !mmuTypeRe.Match(out) {
		return "", errors.New("difftest: no mmu-type in Spike's device tree")
	}
	dts := mmuTypeRe.ReplaceAll(out, []byte(`mmu-type = "`+cfg.MMUType+`";`))
	dir, err := os.MkdirTemp("", "rvsim-difftest-dtb-")
	if err != nil {
		return "", err
	}
	dtb := filepath.Join(dir, "spike.dtb")
	cmd := exec.Command("dtc", "-I", "dts", "-O", "dtb", "-o", dtb)
	cmd.Stdin = bytes.NewReader(dts)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("difftest: dtc: %w: %s", err, out)
	}
	dtbCache[cfg] = dtb
	return dtb, nil
}
