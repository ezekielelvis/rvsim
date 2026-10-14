// Package linux boots OpenSBI, Linux and a BusyBox initramfs (built by
// tests/images/build-images.sh) and drives the shell over the UART.
package linux

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ezekielelvis/rvsim/vmm/boot"
	"github.com/ezekielelvis/rvsim/vmm/devices/virtio/blk"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

const (
	kernelVersion = "6.12.112-rvsim"
	prompt        = "# "
	// chunk is how many steps run between checks of the console.
	chunk = 20_000_000
)

func imagePath(name string) string {
	dir := os.Getenv("RVSIM_IMAGES")
	if dir == "" {
		dir = filepath.Join("..", "..", "build", "images")
	}
	return filepath.Join(dir, name)
}

func readImage(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(imagePath(name))
	if err != nil {
		if os.Getenv("RVSIM_REQUIRE_LINUX") == "1" {
			t.Fatal(err)
		}
		t.Skipf("%v; run tests/images/build-images.sh", err)
	}
	return b
}

// images returns firmware, kernel and initramfs, booting from the latter.
func images(t *testing.T) boot.Images {
	t.Helper()
	return boot.Images{
		Firmware: readImage(t, "fw_jump.bin"),
		Kernel:   readImage(t, "Image"),
		Initrd:   readImage(t, "rootfs.cpio"),
		Bootargs: "console=ttyS0 earlycon",
	}
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
	return newGuestWithDisk(t, img, nil)
}

func newGuestWithDisk(t *testing.T, img boot.Images, disk *blk.Config) *guest {
	t.Helper()
	cfg := machine.DefaultConfig()
	g := &guest{t: t}
	v, err := vm.New(vm.Config{Machine: cfg, Console: &g.console, Disk: disk})
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

// openDiskCopy copies the ext4 root image to a temporary file the guest may
// modify, and opens it as a virtio disk.
func openDiskCopy(t *testing.T) (*blk.Config, string) {
	t.Helper()
	src, err := os.Open(imagePath("rootfs.ext4"))
	if err != nil {
		if os.Getenv("RVSIM_REQUIRE_LINUX") == "1" {
			t.Fatal(err)
		}
		t.Skipf("%v; run tests/images/build-images.sh", err)
	}
	defer src.Close()
	path := filepath.Join(t.TempDir(), "disk.ext4")
	dst, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return openDisk(t, path), path
}

func openDisk(t *testing.T, path string) *blk.Config {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return &blk.Config{Disk: f, Size: uint64(st.Size()), Serial: "rvsim-disk0"}
}

// bootFromDisk boots with the root filesystem on /dev/vda, to the shell.
func bootFromDisk(t *testing.T, disk *blk.Config) *guest {
	t.Helper()
	img := boot.Images{
		Firmware: readImage(t, "fw_jump.bin"),
		Kernel:   readImage(t, "Image"),
		Bootargs: "console=ttyS0 earlycon root=/dev/vda rw",
	}
	g := newGuestWithDisk(t, img, disk)
	g.waitFor("virtio_blk virtio0: [vda]", 4_000_000_000)
	g.waitFor("EXT4-fs (vda): mounted filesystem", 4_000_000_000)
	g.waitFor(prompt, 4_000_000_000)
	return g
}

// powerOff shuts the guest down cleanly (init unmounts the disk) and waits
// for the finisher.
func (g *guest) powerOff() {
	g.t.Helper()
	g.v.ScheduleInput(g.v.Now(), []byte("poweroff\n"))
	res, err := g.v.Run(4_000_000_000)
	if err != nil || !res.Exited || res.ExitCode != 0 {
		g.t.Fatalf("poweroff: %+v %v; console:\n%s", res, err, g.console.String())
	}
}

func TestExt4RootOnVirtioDiskPersistsAcrossReboot(t *testing.T) {
	disk, path := openDiskCopy(t)
	g := bootFromDisk(t, disk)
	if m := g.command("grep ' / ' /proc/mounts"); !strings.HasPrefix(m, "/dev/root / ext4 rw") {
		t.Errorf("root mount: %q", m)
	}
	// sysfs prints the serial without a newline; echo adds one before the prompt.
	if got := g.command("echo $(cat /sys/block/vda/serial)"); got != "rvsim-disk0" {
		t.Errorf("disk serial %q", got)
	}
	g.command("echo written-by-the-first-boot > /root/note && sync")
	g.powerOff()
	t.Logf("first boot: %d steps", g.v.Now())

	// Boot again from the same image file.
	g = bootFromDisk(t, openDisk(t, path))
	if strings.Contains(g.console.String(), "recovery") {
		t.Errorf("ext4 needed journal recovery: the first shutdown was not clean:\n%s", g.console.String())
	}
	if got := g.command("cat /root/note"); got != "written-by-the-first-boot" {
		t.Errorf("after reboot /root/note = %q", got)
	}
	g.powerOff()
	t.Logf("second boot: %d steps", g.v.Now())
}

// Two disk boots from identical images, given the same input at the same
// steps, must match exactly, down to the bytes they leave on the disk.
func TestDiskBootIsDeterministic(t *testing.T) {
	run := func() (string, vm.Result, []byte) {
		disk, path := openDiskCopy(t)
		g := bootFromDisk(t, disk)
		g.v.ScheduleInput(g.v.Now(), []byte("echo deterministic > /root/d; cat /proc/interrupts; poweroff\n"))
		res, err := g.v.Run(4_000_000_000)
		if err != nil || !res.Exited {
			t.Fatalf("%+v %v; console:\n%s", res, err, g.console.String())
		}
		image, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return g.console.String(), res, image
	}
	out1, res1, img1 := run()
	out2, res2, img2 := run()
	if res1 != res2 {
		t.Errorf("results differ: %+v vs %+v", res1, res2)
	}
	if out1 != out2 {
		t.Errorf("console output differs:\n%s\n---\n%s", out1, out2)
	}
	if !bytes.Equal(img1, img2) {
		t.Error("the runs left different bytes on the disk")
	}
}
