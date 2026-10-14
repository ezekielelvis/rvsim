package linux

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/gdb"
	"github.com/ezekielelvis/rvsim/vmm/gdb/gdbtest"
)

// The Phase 10 acceptance test: GDB with vmlinux symbols attaches to the
// running kernel, stops at a breakpoint on a kernel function, and shows a
// backtrace; memory reads go through the kernel's page tables.
func TestGDBDebugsTheRunningKernel(t *testing.T) {
	gdbPath := gdbtest.Find(t)
	vmlinux := imagePath("vmlinux")
	readImage(t, "vmlinux")
	var console bytes.Buffer
	g := newGuestWithDisk(t, images(t), nil)
	g.v.SetConsole(&console)
	if _, err := g.v.Run(20_000_000); err != nil { // the kernel is booting
		t.Fatal(err)
	}
	out := gdbtest.Run(t, gdbPath, gdb.NewVMTarget(g.v), func(addr string) []string {
		return []string{
			"-ex", "file " + vmlinux,
			"-ex", "target remote " + addr,
			"-ex", "break kernel_execve",
			"-ex", "continue",
			"-ex", "bt",
			"-ex", "info registers priv",
			"-ex", "x/s $a0",
			"-ex", "kill",
		}
	})
	for _, want := range []string{
		"Breakpoint 1, kernel_execve",
		`kernel_filename=kernel_filename@entry=`,
		"in run_init_process",
		"in kernel_init",
		"prv:1 [Supervisor]",
		`"/init"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gdb output lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("gdb output:\n%s", out)
	}
}
