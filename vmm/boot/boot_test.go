package boot

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

func platform() Platform {
	cfg := machine.DefaultConfig()
	return Platform{
		RAMBase: cfg.RAMBase, RAMSize: cfg.RAMSize, CLINTBase: cfg.CLINTBase, Svadu: true,
		Bootargs: "console=ttyS0 earlycon", InitrdStart: 0x8f00_0000, InitrdEnd: 0x8f00_1234,
	}
}

func dtc(t *testing.T, blob []byte) string {
	t.Helper()
	if _, err := exec.LookPath("dtc"); err != nil {
		t.Skip("dtc not installed")
	}
	cmd := exec.Command("dtc", "-I", "dtb", "-O", "dts", "-")
	cmd.Stdin = bytes.NewReader(blob)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("dtc: %v\n%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("dtc warnings:\n%s", stderr.String())
	}
	return stdout.String()
}

func TestDeviceTreeDescribesThePlatform(t *testing.T) {
	blob, err := DeviceTree(platform())
	if err != nil {
		t.Fatal(err)
	}
	dts := dtc(t, blob)
	for _, want := range []string{
		`bootargs = "console=ttyS0 earlycon";`,
		`stdout-path = "/soc/serial@10000000";`,
		"linux,initrd-start = <0x00 0x8f000000>;",
		"linux,initrd-end = <0x00 0x8f001234>;",
		"timebase-frequency = <0x989680>;",
		`riscv,isa = "rv64imac_zicsr_zifencei_zicntr_svade_svadu";`,
		`riscv,isa-extensions = "i", "m", "a", "c", "zicsr", "zifencei", "zicntr", "svade", "svadu";`,
		`mmu-type = "riscv,sv39";`,
		"memory@80000000 {",
		"reg = <0x00 0x80000000 0x00 0x10000000>;",
		"clint@2000000 {",
		"interrupts-extended = <0x01 0x03 0x01 0x07>;",
		"plic@c000000 {",
		"riscv,ndev = <0x1f>;",
		"interrupts-extended = <0x01 0x0b 0x01 0x09>;",
		"test@100000 {",
		`compatible = "sifive,test1", "sifive,test0", "syscon";`,
		"poweroff {",
		`compatible = "syscon-poweroff";`,
		"value = <0x5555>;",
		"reboot {",
		"value = <0x7777>;",
		"regmap = <0x03>;",
		"serial@10000000 {",
		`compatible = "ns16550a";`,
		"interrupts = <0x0a>;",
		"interrupt-parent = <0x02>;",
	} {
		if !strings.Contains(dts, want) {
			t.Errorf("device tree lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("device tree:\n%s", dts)
	}
}

func TestDeviceTreeWithoutSvaduOrInitrd(t *testing.T) {
	p := platform()
	p.Svadu, p.InitrdStart, p.InitrdEnd = false, 0, 0
	blob, err := DeviceTree(p)
	if err != nil {
		t.Fatal(err)
	}
	dts := dtc(t, blob)
	if !strings.Contains(dts, `riscv,isa = "rv64imac_zicsr_zifencei_zicntr_svade";`) {
		t.Error("expected Svade only")
	}
	if strings.Contains(dts, "svadu") || strings.Contains(dts, "linux,initrd") {
		t.Errorf("unexpected properties:\n%s", dts)
	}
}

// fakeKernel returns a file of n bytes with a RISC-V Image header claiming
// imageSize bytes in memory.
func fakeKernel(n int, imageSize uint64) []byte {
	k := make([]byte, n)
	binary.LittleEndian.PutUint64(k[16:], imageSize)
	copy(k[48:], "RISCV\x00\x00\x00")
	copy(k[56:], "RSC\x05")
	k[64] = 0xab
	return k
}

func TestKernelSize(t *testing.T) {
	if got := KernelSize(fakeKernel(4096, 1<<20)); got != 1<<20 {
		t.Errorf("with header: %d", got)
	}
	if got := KernelSize(make([]byte, 4096)); got != 4096 {
		t.Errorf("without header: %d", got)
	}
	if got := KernelSize(fakeKernel(4096, 16)); got != 4096 {
		t.Errorf("image_size smaller than the file: %d", got)
	}
}

func newVM(t *testing.T) (*vm.VM, machine.Config) {
	t.Helper()
	cfg := machine.DefaultConfig()
	v, err := vm.New(vm.Config{Machine: cfg, Console: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return v, cfg
}

func TestLoadPlacesImages(t *testing.T) {
	v, cfg := newVM(t)
	img := Images{
		Firmware: []byte{1, 2, 3, 4},
		Kernel:   fakeKernel(8192, 4<<20),
		Initrd:   bytes.Repeat([]byte{0x5a}, 10_000),
		Bootargs: "console=ttyS0",
	}
	if err := Load(v, cfg, img); err != nil {
		t.Fatal(err)
	}
	m := v.Machine()
	read := func(addr uint64, n int) []byte {
		b := make([]byte, n)
		if err := m.ReadPhys(addr, b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !bytes.Equal(read(cfg.RAMBase, 4), img.Firmware) {
		t.Error("firmware not at the start of RAM")
	}
	if !bytes.Equal(read(cfg.RAMBase+KernelOffset, len(img.Kernel)), img.Kernel) {
		t.Error("kernel not at RAM + 2 MiB")
	}
	top := cfg.RAMBase + cfg.RAMSize
	initrd := (top - uint64(len(img.Initrd))) &^ 0xfff
	if !bytes.Equal(read(initrd, len(img.Initrd)), img.Initrd) {
		t.Error("initramfs not at the top of RAM")
	}
	dtbAddr := cfg.RAMBase + FDTOffset
	if binary.BigEndian.Uint32(read(dtbAddr, 4)) != 0xd00dfeed {
		t.Fatal("no device tree at RAM + 34 MiB")
	}
	if a0, _ := m.Reg(10); a0 != cfg.HartID {
		t.Errorf("a0 = %#x", a0)
	}
	if a1, _ := m.Reg(11); a1 != dtbAddr {
		t.Errorf("a1 = %#x, want %#x", a1, dtbAddr)
	}
	if pc, _ := m.Reg(machine.RegPC); pc != cfg.RAMBase {
		t.Errorf("pc = %#x", pc)
	}

	size := binary.BigEndian.Uint32(read(dtbAddr+4, 4))
	dts := dtc(t, read(dtbAddr, int(size)))
	for _, want := range []string{
		`bootargs = "console=ttyS0";`,
		fmt.Sprintf("linux,initrd-start = <0x00 %#x>;", initrd),
		fmt.Sprintf("linux,initrd-end = <0x00 %#x>;", initrd+uint64(len(img.Initrd))),
	} {
		if !strings.Contains(dts, want) {
			t.Errorf("loaded device tree lacks %q:\n%s", want, dts)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*machine.Config, *Images)
		want string
	}{
		{"no firmware", func(_ *machine.Config, i *Images) { i.Firmware = nil }, "required"},
		{"firmware too large", func(_ *machine.Config, i *Images) { i.Firmware = make([]byte, KernelOffset+1) }, "overlaps the kernel"},
		{"kernel bss reaches the DTB", func(_ *machine.Config, i *Images) {
			i.Kernel = fakeKernel(4096, FDTOffset-KernelOffset+1)
		}, "overlaps the device tree"},
		{"initramfs too large", func(_ *machine.Config, i *Images) { i.Initrd = make([]byte, 250<<20) }, "too small"},
		{"reset PC elsewhere", func(c *machine.Config, _ *Images) { c.ResetPC += 4 }, "reset PC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, cfg := newVM(t)
			img := Images{Firmware: []byte{1}, Kernel: fakeKernel(4096, 4096)}
			tc.edit(&cfg, &img)
			err := Load(v, cfg, img)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
