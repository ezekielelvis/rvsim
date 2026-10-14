// Package riscvtests locates the riscv-tests binaries built by
// tests/compliance/build-riscv-tests.sh and records how each one must be run,
// both on rvsim and on Spike.
package riscvtests

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/machine"
)

// Test is one riscv-tests ELF.
type Test struct {
	Name  string // e.g. "rv64ui-p-add"
	Path  string
	Flags uint32 // machine config flags
	ISA   string // Spike --isa string mirroring Flags and the implemented ISA
	Priv  string // Spike --priv
	MMU   string // Spike device-tree mmu-type
}

// baseISA is what rvsim implements in the current phase.
const (
	baseISA  = "rv64imac_zicsr_zifencei_zicntr_svadu"
	basePriv = "MSU"
	baseMMU  = "riscv,sv39"
)

// needsMisaligned lists tests that require hardware misaligned access
// support. rvsim traps by default (ADR-004), so these run with
// RV_CFG_MISALIGNED_OK and Spike runs with Zicclsm.
var needsMisaligned = map[string]bool{
	"rv64ui-p-ma_data": true,
	"rv64ui-v-ma_data": true,
}

// unsupported lists tests that need features rvsim does not (yet) have.
// They are reported as skipped, with the reason, rather than hidden.
var unsupported = map[string]string{
	"rv64mi-p-breakpoint": "needs Sdtrig debug triggers, which rvsim does not implement",
	"rv64mi-p-pmpaddr":    "needs PMP; rvsim implements no PMP entries (ADR-007)",
}

// difftestUnsupported lists tests that pass on rvsim but cannot be compared
// with Spike because a legal implementation choice sends them down a
// different path.
var difftestUnsupported = map[string]string{
	"rv64mi-p-ma_fetch": "clears misa.C, which Spike allows and rvsim's read-only misa does not (ADR-016)",
}

// SkipIfDifftestUnsupported skips t for tests listed in difftestUnsupported.
func SkipIfDifftestUnsupported(t testing.TB, tc Test) {
	t.Helper()
	if reason, ok := difftestUnsupported[tc.Name]; ok {
		t.Skip(reason)
	}
}

// SkipIfUnsupported skips t if the test needs a feature rvsim lacks.
func SkipIfUnsupported(t testing.TB, tc Test) {
	t.Helper()
	if reason, ok := unsupported[tc.Name]; ok {
		t.Skip(reason)
	}
}

// Dir returns the directory holding the built tests: $RVSIM_RISCV_TESTS, or
// build/riscv-tests at the repository root.
func Dir() string {
	if d := os.Getenv("RVSIM_RISCV_TESTS"); d != "" {
		return d
	}
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "build", "riscv-tests")
}

// List returns the tests whose names start with any of prefixes (e.g.
// "rv64ui-p-"), sorted by name. If none are found the test is skipped, or
// failed when RVSIM_REQUIRE_TESTS=1 (as in CI).
func List(t testing.TB, prefixes ...string) []Test {
	t.Helper()
	dir := Dir()
	entries, err := os.ReadDir(dir)
	var tests []Test
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || strings.Contains(name, ".") || !hasAnyPrefix(name, prefixes) {
				continue
			}
			tests = append(tests, configure(name, filepath.Join(dir, name)))
		}
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].Name < tests[j].Name })
	if len(tests) == 0 {
		msg := "no riscv-tests in " + dir + "; run tests/compliance/build-riscv-tests.sh"
		if os.Getenv("RVSIM_REQUIRE_TESTS") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	return tests
}

func configure(name, path string) Test {
	tc := Test{Name: name, Path: path, Flags: machine.DefaultConfig().Flags, ISA: baseISA, Priv: basePriv, MMU: baseMMU}
	if needsMisaligned[name] {
		tc.Flags |= machine.FlagMisalignedOK
		tc.ISA += "_zicclsm"
	}
	return tc
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
