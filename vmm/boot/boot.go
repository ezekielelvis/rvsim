// Package boot describes the rvsim platform in a device tree and loads
// OpenSBI, a Linux kernel and an initramfs following the RISC-V boot
// protocol: firmware at the start of RAM, a0 = hart ID, a1 = DTB address.
package boot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/ezekielelvis/rvsim/vmm/devices/finisher"
	"github.com/ezekielelvis/rvsim/vmm/devices/plic"
	"github.com/ezekielelvis/rvsim/vmm/devices/uart"
	"github.com/ezekielelvis/rvsim/vmm/devices/virtio"
	"github.com/ezekielelvis/rvsim/vmm/fdt"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

const (
	// KernelOffset is where OpenSBI fw_jump expects the next stage
	// (FW_JUMP_ADDR = RAM base + 2 MiB for RV64).
	KernelOffset = 0x20_0000
	// FDTOffset is where OpenSBI's generic fw_jump puts the device tree
	// (FW_JUMP_FDT_OFFSET). Loading it there makes the firmware's copy a
	// no-op; the kernel, including its bss, must end below it.
	FDTOffset = 0x220_0000
	fdtRoom   = 64 << 10
	// TimebaseFrequency is the guest-visible mtime rate: mtime advances
	// once per steps_per_tick (10) steps, so this assumes a nominal virtual
	// speed of 100 M steps per second.
	TimebaseFrequency = 10_000_000
	// UARTClock is the 16550's input clock (the PC standard, as QEMU).
	UARTClock = 3_686_400
	// isaBase is the implemented ISA without the A/D extensions.
	isaBase = "rv64imac_zicsr_zifencei_zicntr"
)

var baseExtensions = []string{"i", "m", "a", "c", "zicsr", "zifencei", "zicntr"}

// adExtensions returns what to advertise for PTE A/D handling. The hart
// always starts in Svade mode (menvcfg.ADUE resets to 0); advertising both
// says software may switch to Svadu (ADR-025, DT binding riscv,isa-extensions).
func adExtensions(svadu bool) []string {
	if svadu {
		return []string{"svade", "svadu"}
	}
	return []string{"svade"}
}

// Phandles.
const (
	phandleCPUIntc  = 1
	phandlePLIC     = 2
	phandleFinisher = 3
)

// Platform describes what the device tree must say.
type Platform struct {
	RAMBase, RAMSize       uint64
	CLINTBase              uint64
	Svadu                  bool // menvcfg.ADUE is writable
	VirtioBlk              bool // a virtio-mmio block device at vm.VirtioBase
	Bootargs               string
	InitrdStart, InitrdEnd uint64 // zero when there is no initramfs
}

// DeviceTree returns the DTB for p.
func DeviceTree(p Platform) ([]byte, error) {
	root := fdt.NewRoot()
	root.U32("#address-cells", 2).U32("#size-cells", 2)
	// OpenSBI 1.5 copies the model over its default platform name "Generic"
	// without a terminator, so the model must be at least 7 characters.
	root.String("compatible", "rvsim,virt").String("model", "rvsim-virt")

	chosen := root.Child("chosen")
	chosen.String("bootargs", p.Bootargs)
	chosen.String("stdout-path", fmt.Sprintf("/soc/serial@%x", vm.UARTBase))
	if p.InitrdEnd > p.InitrdStart {
		chosen.U64("linux,initrd-start", p.InitrdStart).U64("linux,initrd-end", p.InitrdEnd)
	}

	cpus := root.Child("cpus")
	cpus.U32("#address-cells", 1).U32("#size-cells", 0).U32("timebase-frequency", TimebaseFrequency)
	cpu := cpus.Child("cpu@0")
	cpu.String("device_type", "cpu").U32("reg", 0).String("status", "okay").String("compatible", "riscv")
	ad := adExtensions(p.Svadu)
	cpu.String("riscv,isa", isaBase+"_"+strings.Join(ad, "_")).String("riscv,isa-base", "rv64i")
	cpu.Strings("riscv,isa-extensions", append(append([]string{}, baseExtensions...), ad...)...)
	cpu.String("mmu-type", "riscv,sv39")
	intc := cpu.Child("interrupt-controller")
	intc.U32("#interrupt-cells", 1).Empty("interrupt-controller").String("compatible", "riscv,cpu-intc")
	intc.U32("phandle", phandleCPUIntc)

	root.Child(fmt.Sprintf("memory@%x", p.RAMBase)).String("device_type", "memory").U64("reg", p.RAMBase, p.RAMSize)

	soc := root.Child("soc")
	soc.U32("#address-cells", 2).U32("#size-cells", 2).String("compatible", "simple-bus").Empty("ranges")

	clint := soc.Child(fmt.Sprintf("clint@%x", p.CLINTBase))
	clint.Strings("compatible", "sifive,clint0", "riscv,clint0").U64("reg", p.CLINTBase, 0x10000)
	// Machine software (3) and machine timer (7) interrupts of hart 0.
	clint.U32("interrupts-extended", phandleCPUIntc, 3, phandleCPUIntc, 7)

	pl := soc.Child(fmt.Sprintf("plic@%x", vm.PLICBase))
	pl.Strings("compatible", "sifive,plic-1.0.0", "riscv,plic0").U64("reg", vm.PLICBase, plic.Size)
	pl.U32("#address-cells", 0).U32("#interrupt-cells", 1).Empty("interrupt-controller")
	pl.U32("riscv,ndev", plic.NumSources-1)
	// Context 0: M-mode external (11); context 1: S-mode external (9).
	pl.U32("interrupts-extended", phandleCPUIntc, 11, phandleCPUIntc, 9)
	pl.U32("phandle", phandlePLIC)

	// The test finisher, driven by OpenSBI's syscon reset drivers (SBI SRST).
	fin := soc.Child(fmt.Sprintf("test@%x", vm.FinisherBase))
	fin.Strings("compatible", "sifive,test1", "sifive,test0", "syscon").U64("reg", vm.FinisherBase, finisher.Size)
	fin.U32("phandle", phandleFinisher)
	for _, r := range []struct {
		name, compatible string
		value            uint32
	}{{"poweroff", "syscon-poweroff", 0x5555}, {"reboot", "syscon-reboot", 0x7777}} {
		root.Child(r.name).String("compatible", r.compatible).
			U32("regmap", phandleFinisher).U32("offset", 0).U32("value", r.value)
	}

	if p.VirtioBlk {
		vb := soc.Child(fmt.Sprintf("virtio_mmio@%x", vm.VirtioBase))
		vb.String("compatible", "virtio,mmio").U64("reg", vm.VirtioBase, virtio.Size)
		vb.U32("interrupts", vm.VirtioIRQ).U32("interrupt-parent", phandlePLIC)
	}

	ser := soc.Child(fmt.Sprintf("serial@%x", vm.UARTBase))
	ser.String("compatible", "ns16550a").U64("reg", vm.UARTBase, uart.Size)
	ser.U32("clock-frequency", UARTClock).U32("interrupts", vm.UARTIRQ).U32("interrupt-parent", phandlePLIC)

	return root.Blob()
}

// Images are the binaries to boot.
type Images struct {
	Firmware []byte // OpenSBI fw_jump.bin (flat binary)
	Kernel   []byte // Linux Image (flat binary)
	Initrd   []byte // initramfs (cpio); optional
	Bootargs string
}

// Load places the images in the RAM of v, created with cfg, and points the
// hart at the firmware (cfg.ResetPC must be cfg.RAMBase). The initramfs goes
// at the top of RAM, out of the kernel's way.
func Load(v *vm.VM, cfg machine.Config, img Images) error {
	if cfg.ResetPC != cfg.RAMBase {
		return fmt.Errorf("boot: reset PC %#x is not the start of RAM", cfg.ResetPC)
	}
	if len(img.Firmware) == 0 || len(img.Kernel) == 0 {
		return errors.New("boot: firmware and kernel are required")
	}
	if uint64(len(img.Firmware)) > KernelOffset {
		return fmt.Errorf("boot: firmware (%d bytes) overlaps the kernel at +%#x", len(img.Firmware), KernelOffset)
	}
	if size := KernelSize(img.Kernel); size > FDTOffset-KernelOffset {
		return fmt.Errorf("boot: kernel (%d bytes with bss) overlaps the device tree at +%#x", size, FDTOffset)
	}
	m := v.Machine()
	top := cfg.RAMBase + cfg.RAMSize
	dtbAddr := cfg.RAMBase + FDTOffset
	if top < dtbAddr+fdtRoom {
		return fmt.Errorf("boot: %d bytes of RAM is too small", cfg.RAMSize)
	}

	p := Platform{
		RAMBase: cfg.RAMBase, RAMSize: cfg.RAMSize, CLINTBase: cfg.CLINTBase,
		Svadu: cfg.Flags&machine.FlagHWADUpdate != 0, VirtioBlk: v.HasDisk(), Bootargs: img.Bootargs,
	}
	if len(img.Initrd) > 0 {
		start := (top - uint64(len(img.Initrd))) &^ 0xfff
		if uint64(len(img.Initrd)) > top || start < dtbAddr+fdtRoom {
			return fmt.Errorf("boot: RAM too small for a %d-byte initramfs", len(img.Initrd))
		}
		p.InitrdStart, p.InitrdEnd = start, start+uint64(len(img.Initrd))
		if err := m.WritePhys(start, img.Initrd); err != nil {
			return err
		}
	}
	dtb, err := DeviceTree(p)
	if err != nil {
		return err
	}
	if len(dtb) > fdtRoom {
		return fmt.Errorf("boot: device tree is %d bytes", len(dtb))
	}
	for _, w := range []struct {
		addr uint64
		data []byte
	}{{cfg.RAMBase, img.Firmware}, {cfg.RAMBase + KernelOffset, img.Kernel}, {dtbAddr, dtb}} {
		if err := m.WritePhys(w.addr, w.data); err != nil {
			return err
		}
	}
	// RISC-V boot protocol: a0 = hart ID, a1 = device tree.
	if err := m.SetReg(10, cfg.HartID); err != nil {
		return err
	}
	return m.SetReg(11, dtbAddr)
}

// KernelSize returns the memory a RISC-V Linux Image occupies once loaded,
// bss included: the header's image_size field (Documentation/arch/riscv/
// boot-image-header.rst), or the file size for anything without the header.
func KernelSize(image []byte) uint64 {
	const (
		sizeOff  = 16
		magicOff = 56 // magic2, "RSC\x05"
	)
	if len(image) >= magicOff+4 && string(image[magicOff:magicOff+4]) == "RSC\x05" {
		if n := binary.LittleEndian.Uint64(image[sizeOff:]); n > uint64(len(image)) {
			return n
		}
	}
	return uint64(len(image))
}
