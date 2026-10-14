// Package archtest runs the RISC-V Architectural Certification Tests (ACT4,
// riscv-arch-test) generated for rvsim's configuration in
// tests/archtest/rvsim-rv64imac by tests/archtest/build-archtest.sh.
package archtest

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/baremetal"
)

// elfDir returns $RVSIM_ARCHTEST_ELFS or build/archtest/rvsim-rv64imac/elfs.
func elfDir() string {
	if d := os.Getenv("RVSIM_ARCHTEST_ELFS"); d != "" {
		return d
	}
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "build", "archtest", "rvsim-rv64imac", "elfs")
}

func listELFs(t *testing.T) []string {
	t.Helper()
	dir := elfDir()
	var elfs []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".elf") {
			elfs = append(elfs, path)
		}
		return nil
	})
	sort.Strings(elfs)
	if len(elfs) == 0 {
		msg := "no ACT4 ELFs in " + dir + "; run tests/archtest/build-archtest.sh"
		if os.Getenv("RVSIM_REQUIRE_TESTS") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	return elfs
}

const passMarker = "RVCP-SUMMARY: TEST PASSED"

// TestArchTests requires each self-checking ELF to print ACT4's pass line
// and then exit with code 0, so a test that halts early cannot pass.
func TestArchTests(t *testing.T) {
	dir := elfDir()
	for _, elf := range listELFs(t) {
		name, _ := filepath.Rel(dir, elf)
		t.Run(strings.TrimSuffix(name, ".elf"), func(t *testing.T) {
			t.Parallel()
			var console bytes.Buffer
			res, err := baremetal.RunFile(elf, baremetal.Options{MaxSteps: 50_000_000, Console: &console})
			if err != nil {
				t.Fatalf("%v\nconsole:\n%s", err, console.String())
			}
			if res.ExitCode != 0 || !strings.Contains(console.String(), passMarker) {
				t.Fatalf("exit code %d after %d instructions\nconsole:\n%s", res.ExitCode, res.Instret, console.String())
			}
		})
	}
}
