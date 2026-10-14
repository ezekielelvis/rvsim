// Package compliance runs the riscv-tests suites on rvsim.
package compliance

import (
	"testing"

	"github.com/ezekielelvis/rvsim/tests/riscvtests"
	"github.com/ezekielelvis/rvsim/vmm/baremetal"
)

// suites are the riscv-tests prefixes rvsim must pass in the current phase.
var suites = []string{
	"rv64ui-p-", "rv64um-p-", "rv64ua-p-", "rv64uc-p-", "rv64mi-p-", "rv64si-p-",
	"rv64ui-v-", "rv64um-v-", "rv64ua-v-", "rv64uc-v-",
}

func TestRiscvTests(t *testing.T) {
	for _, tc := range riscvtests.List(t, suites...) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			riscvtests.SkipIfUnsupported(t, tc)
			res, err := baremetal.RunFile(tc.Path, baremetal.Options{Flags: tc.Flags, MaxSteps: 10_000_000})
			if err != nil {
				t.Fatal(err)
			}
			// riscv-tests report the failing test number as the exit code.
			if res.ExitCode != 0 {
				t.Fatalf("failed test case %d (after %d instructions)", res.ExitCode, res.Instret)
			}
		})
	}
}
