package difftest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ezekielelvis/rvsim/tests/riscvtests"
	"github.com/ezekielelvis/rvsim/vmm/baremetal"
)

const ramBase = 0x8000_0000

// spikePath finds the pinned Spike from scripts/toolchain.sh, then PATH.
func spikePath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("RVSIM_SPIKE"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".cache", "rvsim-tools", "bin", "spike")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("spike"); err == nil {
		return p
	}
	if os.Getenv("RVSIM_REQUIRE_TESTS") == "1" {
		t.Fatal("spike not found; run scripts/toolchain.sh --install")
	}
	t.Skip("spike not found; run scripts/toolchain.sh --install")
	return ""
}

func TestAgainstSpike(t *testing.T) {
	spike := spikePath(t)
	for _, tc := range riscvtests.List(t, "rv64ui-p-", "rv64um-p-", "rv64ua-p-", "rv64uc-p-", "rv64mi-p-", "rv64si-p-") {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			riscvtests.SkipIfUnsupported(t, tc)
			riscvtests.SkipIfDifftestUnsupported(t, tc)
			var ours bytes.Buffer
			if _, err := baremetal.RunFile(tc.Path, baremetal.Options{
				Flags: tc.Flags, MaxSteps: 10_000_000, Trace: &ours,
			}); err != nil {
				t.Fatal(err)
			}
			if ours.Len() == 0 {
				t.Fatal("rvsim produced an empty commit log")
			}
			commits := bytes.Count(ours.Bytes(), []byte("\n"))
			ref, err := RunSpike(SpikeConfig{Path: spike, ISA: tc.ISA, Priv: tc.Priv, MMUType: tc.MMU}, tc.Path)
			if err != nil {
				t.Fatal(err)
			}
			res, err := Compare(&ours, bytes.NewReader(ref), ramBase)
			if err != nil {
				t.Fatal(err)
			}
			if res.Divergence != nil {
				t.Fatal(res.Divergence)
			}
			if res.Commits != commits {
				t.Fatalf("compared %d commits, rvsim logged %d", res.Commits, commits)
			}
			t.Logf("%d commits match Spike (%d spurious Spike SC failures resynchronized, %d implementation-ID reads)",
				commits, res.SpuriousSC, res.IDReads)
		})
	}
}

func TestCompareReportsFirstDivergence(t *testing.T) {
	boot := "core   0: 3 0x0000000000001000 (0x00000297) x5  0x0000000000001000\n"
	a := "core   0: 3 0x0000000080000000 (0x00000297) x5  0x0000000080000000\n"
	b := "core   0: 3 0x0000000080000004 (0x00000013)\n"
	bad := "core   0: 3 0x0000000080000004 (0x00000093) x1  0x0000000000000000\n"
	// An LR/SC loop where Spike's first SC fails and it retries.
	lr := "core   0: 0 0x00000000800001fc (0x1005272f) x14 0x0000000000000005 mem 0x0000000080002008\n"
	add := "core   0: 0 0x0000000080000200 (0x00c70733) x14 0x0000000000000006\n"
	scOK := "core   0: 0 0x0000000080000204 (0x18e5272f) x14 0x0000000000000000 mem 0x0000000080002008 0x00000006\n"
	scFail := "core   0: 0 0x0000000080000204 (0x18e5272f) x14 0x0000000000000001\n"
	bnezTaken := "core   0: 0 0x0000000080000208 (0xfe071ae3)\n"
	markOurs := "core   0: 3 0x000000008000015c (0xf1202573) x10 0x0000000000000000\n"
	markSpike := "core   0: 3 0x000000008000015c (0xf1202573) x10 0x0000000000000005\n"
	mipOurs := "core   0: 1 0x0000000080000022 (0x14416073) c836_mip 0x0000000000000082\n"
	mipSpike := "core   0: 1 0x0000000080000022 (0x14416073) c777_mvip 0x0000000000000000 c836_mip 0x0000000000000082\n"
	mtvecOurs := "core   0: 3 0x0000000080000010 (0x30502573) x10 0x0000000000000000\n"
	mtvecSpike := "core   0: 3 0x0000000080000010 (0x30502573) x10 0x0000000000000004\n"
	tests := []struct {
		name       string
		ours       string
		spike      string
		wantLine   int // 0 = no divergence
		wantSpike  string
		wantResync int
	}{
		{"identical after boot ROM", a + b, boot + a + b, 0, "", 0},
		{"spike runs longer", a, boot + a + b + b, 0, "", 0},
		{"differs on second commit", a + bad, boot + a + b, 2, strings.TrimSpace(b), 0},
		{"spike ends first", a + b, boot + a, 2, "", 0},
		{"spurious spike SC failure", lr + add + scOK + b, lr + add + scFail + bnezTaken + lr + add + scOK + b, 0, "", 1},
		{"SC failure in rvsim is a divergence", lr + add + scFail, lr + add + scOK, 3, strings.TrimSpace(scOK), 0},
		{"implementation ID read differs", markOurs, markSpike, 0, "", 0},
		{"mvip entry is ignored", mipOurs, mipSpike, 0, "", 0},
		{"other CSR read value differs", mtvecOurs, mtvecSpike, 1, strings.TrimSpace(mtvecSpike), 0},
		{"no resync after unrelated difference", lr + add + scOK + bad, lr + add + scFail + bnezTaken + lr + add + scOK + b, 4, strings.TrimSpace(b), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Compare(strings.NewReader(tt.ours), strings.NewReader(tt.spike), ramBase)
			if err != nil {
				t.Fatal(err)
			}
			if res.SpuriousSC != tt.wantResync {
				t.Errorf("SpuriousSC = %d, want %d", res.SpuriousSC, tt.wantResync)
			}
			d := res.Divergence
			if tt.wantLine == 0 {
				if d != nil {
					t.Fatalf("unexpected divergence:\n%v", d)
				}
				return
			}
			if d == nil || d.Line != tt.wantLine || d.Spike != tt.wantSpike {
				t.Fatalf("got %+v, want line %d spike %q", d, tt.wantLine, tt.wantSpike)
			}
		})
	}
}
