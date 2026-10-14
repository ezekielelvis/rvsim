// Package linux boots OpenSBI, Linux and a BusyBox initramfs (built by
// tests/images/build-images.sh) and drives the shell over the UART.
package linux

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ezekielelvis/rvsim/vmm/boot"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

const (
	kernelVersion = "6.12.112-rvsim"
	prompt        = "# "
	// chunk is how many steps run between checks of the console.
	chunk = 20_000_000
)

func images(t *testing.T) boot.Images {
	t.Helper()
	dir := os.Getenv("RVSIM_IMAGES")
	if dir == "" {
		dir = filepath.Join("..", "..", "build", "images")
	}
	var img boot.Images
	for _, f := range []struct {
		name string
		dst  *[]byte
	}{{"fw_jump.bin", &img.Firmware}, {"Image", &img.Kernel}, {"rootfs.cpio", &img.Initrd}} {
		b, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			if os.Getenv("RVSIM_REQUIRE_LINUX") == "1" {
				t.Fatal(err)
			}
			t.Skipf("%v; run tests/images/build-images.sh", err)
		}
		*f.dst = b
	}
	img.Bootargs = "console=ttyS0 earlycon"
	return img
}

// guest is a booted VM with its console output.
type guest struct {
	t       *testing.T
	v       *vm.VM
	console bytes.Buffer
	seen    int // console offset already matched by waitFor
}

// waitFor runs until the console shows want after the previous match, and
// returns the output up to and including it.
func (g *guest) waitFor(want string, maxSteps uint64) string {
	g.t.Helper()
	start := g.v.Now()
	for g.v.Now()-start < maxSteps {
		if i := strings.Index(g.console.String()[g.seen:], want); i >= 0 {
			out := g.console.String()[g.seen : g.seen+i+len(want)]
			g.seen += i + len(want)
			return out
		}
		res, err := g.v.Run(chunk)
		if err != nil {
			g.t.Fatalf("%v at step %d; console:\n%s", err, g.v.Now(), g.console.String())
		}
		if res.Exited {
			g.t.Fatalf("guest exited (code %d) waiting for %q; console:\n%s", res.ExitCode, want, g.console.String())
		}
	}
	g.t.Fatalf("no %q within %d steps; console:\n%s", want, maxSteps, g.console.String())
	return ""
}

// command types line at the shell and returns its output.
func (g *guest) command(line string) string {
	g.t.Helper()
	g.v.ScheduleInput(g.v.Now(), []byte(line+"\n"))
	out := g.waitFor("\n"+prompt, 2_000_000_000)
	out = strings.TrimSuffix(out, "\n"+prompt)
	// The UART echo (from the shell) comes first.
	if _, rest, ok := strings.Cut(out, "\n"); ok {
		return strings.TrimRight(rest, "\r\n")
	}
	return ""
}

func newGuest(t *testing.T, img boot.Images) *guest {
	t.Helper()
	cfg := machine.DefaultConfig()
	g := &guest{t: t}
	v, err := vm.New(vm.Config{Machine: cfg, Console: &g.console})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	g.v = v
	if err := boot.Load(v, cfg, img); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestBootToShell(t *testing.T) {
	g := newGuest(t, images(t))
	v := g.v

	began := time.Now()
	g.waitFor("OpenSBI v1.5", 50_000_000)                               // release tarballs print major.minor
	g.waitFor("Platform Name             : rvsim-virt\r\n", 50_000_000) // OpenSBI writes CRLF
	g.waitFor("Linux version "+kernelVersion, 200_000_000)
	g.waitFor("Run /init as init process", 4_000_000_000)
	g.waitFor(prompt, 4_000_000_000)
	t.Logf("shell prompt after %d steps (%.1fs)", v.Now(), time.Since(began).Seconds())

	uname := g.command("uname -a")
	t.Logf("uname -a: %s", uname)
	if !strings.HasPrefix(uname, "Linux rvsim "+kernelVersion+" ") || !strings.HasSuffix(uname, " riscv64 GNU/Linux") {
		t.Errorf("uname -a = %q", uname)
	}
	if got := g.command("echo $((6 * 7))"); got != "42" {
		t.Errorf("arithmetic = %q", got)
	}

	// poweroff -f reaches the test finisher through SBI SRST and OpenSBI.
	v.ScheduleInput(v.Now(), []byte("poweroff -f\n"))
	res, err := v.Run(2_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Exited || res.ExitCode != 0 || res.Reset {
		t.Fatalf("poweroff: %+v; console:\n%s", res, g.console.String())
	}
	t.Logf("powered off after %d steps, %d instructions (%.1fs)", res.Steps, res.Instret, time.Since(began).Seconds())
}

// Two boots with the same input at the same steps must match exactly.
func TestBootIsDeterministic(t *testing.T) {
	img := images(t)
	run := func() (string, vm.Result) {
		g := newGuest(t, img)
		g.waitFor(prompt, 4_000_000_000)
		g.v.ScheduleInput(g.v.Now(), []byte("cat /proc/interrupts; poweroff -f\n"))
		res, err := g.v.Run(2_000_000_000)
		if err != nil || !res.Exited {
			t.Fatalf("%+v %v; console:\n%s", res, err, g.console.String())
		}
		return g.console.String(), res
	}
	out1, res1 := run()
	out2, res2 := run()
	if res1 != res2 {
		t.Errorf("results differ: %+v vs %+v", res1, res2)
	}
	if out1 != out2 {
		t.Errorf("console output differs:\n%s\n---\n%s", out1, out2)
	}
}
