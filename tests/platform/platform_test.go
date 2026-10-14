// Package platform tests the whole VM — core, run loop, scheduler, CLINT,
// PLIC and UART — with bare-metal programs assembled at test time.
package platform

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/loader"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

// build assembles src (in this directory) with the bare-metal toolchain.
func build(t *testing.T, src string) *loader.Image {
	t.Helper()
	prefix := os.Getenv("RISCV_PREFIX")
	if prefix == "" {
		prefix = "riscv64-unknown-elf-"
	}
	cc := prefix + "gcc"
	if _, err := exec.LookPath(cc); err != nil {
		if os.Getenv("RVSIM_REQUIRE_TESTS") == "1" {
			t.Fatalf("%s not found", cc)
		}
		t.Skipf("%s not found; run scripts/toolchain.sh --install", cc)
	}
	out := filepath.Join(t.TempDir(), "prog.elf")
	cmd := exec.Command(cc, "-march=rv64imac_zicsr", "-mabi=lp64", "-nostdlib", "-nostartfiles",
		"-Wl,--no-warn-rwx-segments", "-T", "link.ld", src, "-o", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("assembling %s: %v\n%s", src, err, msg)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := loader.ParseELF(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func newVM(t *testing.T, img *loader.Image, console *bytes.Buffer) *vm.VM {
	t.Helper()
	cfg := machine.DefaultConfig()
	cfg.ResetPC = img.Entry
	cfg.HTIFToHost = img.Symbols["tohost"]
	cfg.HTIFFromHost = img.Symbols["fromhost"]
	v, err := vm.New(vm.Config{Machine: cfg, Console: console, HTIF: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	if err := img.Load(v.Machine()); err != nil {
		t.Fatal(err)
	}
	return v
}

// Phase 4 done criterion: print over the UART, take a CLINT timer
// interrupt, then take a UART receive interrupt through the PLIC.
func TestInterrupts(t *testing.T) {
	img := build(t, "interrupts.S")
	var console bytes.Buffer
	v := newVM(t, img, &console)
	const typedAt = 500_000
	v.ScheduleInput(typedAt, []byte{'x'})
	res, err := v.Run(10_000_000)
	if err != nil {
		t.Fatalf("%v\nconsole: %q", err, console.String())
	}
	if !res.Exited || res.ExitCode != 0 {
		t.Fatalf("result %+v\nconsole: %q", res, console.String())
	}
	if want := "hello\ntimer\nrx:x\ndone\n"; console.String() != want {
		t.Fatalf("console %q, want %q", console.String(), want)
	}
	// The hart idled until the byte arrived: it cannot have finished earlier.
	if res.Steps < typedAt {
		t.Fatalf("finished at step %d, before the input at %d", res.Steps, typedAt)
	}
}

// The same inputs give the same execution, step for step (determinism rule 1).
func TestInterruptsAreDeterministic(t *testing.T) {
	img := build(t, "interrupts.S")
	var first vm.Result
	for i := range 3 {
		var console bytes.Buffer
		v := newVM(t, img, &console)
		v.ScheduleInput(500_000, []byte{'x'})
		res, err := v.Run(10_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = res
		} else if res != first {
			t.Fatalf("run %d: %+v, first run %+v", i, res, first)
		}
	}
}
