// Package perf measures the emulator (Phase 12): raw interpreter speed on a
// bare-metal loop, and Linux boot to the BusyBox shell.
//
//	go test -run XXX -bench . -benchtime 3x ./tests/perf
package perf

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

// cpuLoop is a mixed integer loop: arithmetic, a multiply, loads and stores
// to a small array, and branches; 12 instructions per iteration.
var cpuLoop = []uint32{
	0x00001537, // lui a0, 0x1        (a0 = 0x1000, array offset)
	0x00000297, // auipc t0, 0        (t0 = base)
	0x00a282b3, // add t0, t0, a0     (t0 = array)
	0x00000313, // li t1, 0           (i)
	0x00000393, // li t2, 0           (sum)
	// loop:
	0x00337e13, // andi t3, t1, 3
	0x003e1e13, // slli t3, t3, 3
	0x01c28e33, // add t3, t0, t3
	0x000e3e83, // ld t4, 0(t3)
	0x006e8eb3, // add t4, t4, t1
	0x01de3023, // sd t4, 0(t3)
	0x026e8f33, // mul t5, t4, t1
	0x01e383b3, // add t2, t2, t5
	0x00130313, // addi t1, t1, 1
	0x0013f393, // andi t2, t2, 1  (keep sum bounded)
	0x00539393, // slli t2, t2, 5
	0xfd5ff06f, // j loop (-44)
}

func BenchmarkInterpreterMIPS(b *testing.B) {
	cfg := machine.DefaultConfig()
	cfg.RAMSize = 1 << 20
	m, err := machine.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	var code []byte
	for _, w := range cpuLoop {
		code = append(code, byte(w), byte(w>>8), byte(w>>16), byte(w>>24))
	}
	if err := m.WritePhys(cfg.RAMBase, code); err != nil {
		b.Fatal(err)
	}
	const steps = 50_000_000
	b.ResetTimer()
	start := time.Now()
	for range b.N {
		if _, err := m.Run(steps); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(steps)*float64(b.N)/time.Since(start).Seconds()/1e6, "MIPS")
}

func imagePath(name string) string {
	dir := os.Getenv("RVSIM_IMAGES")
	if dir == "" {
		dir = filepath.Join("..", "..", "build", "images")
	}
	return filepath.Join(dir, name)
}

// BenchmarkLinuxBootToShell boots OpenSBI and Linux (initramfs) until the
// BusyBox prompt appears.
func BenchmarkLinuxBootToShell(b *testing.B) {
	var img boot.Images
	for _, f := range []struct {
		name string
		dst  *[]byte
	}{{"fw_jump.bin", &img.Firmware}, {"Image", &img.Kernel}, {"rootfs.cpio", &img.Initrd}} {
		data, err := os.ReadFile(imagePath(f.name))
		if err != nil {
			b.Skip(err)
		}
		*f.dst = data
	}
	img.Bootargs = "console=ttyS0 earlycon"
	var instret, steps uint64
	var elapsed time.Duration
	for range b.N {
		var out bytes.Buffer
		cfg := machine.DefaultConfig()
		v, err := vm.New(vm.Config{Machine: cfg, Console: &out})
		if err != nil {
			b.Fatal(err)
		}
		if err := boot.Load(v, cfg, img); err != nil {
			b.Fatal(err)
		}
		start := time.Now()
		for !strings.HasSuffix(out.String(), "# ") {
			if _, err := v.Run(1_000_000); err != nil {
				b.Fatal(err)
			}
			if v.Now() > 4_000_000_000 {
				b.Fatal("no shell prompt")
			}
		}
		elapsed += time.Since(start)
		instret += v.Instret()
		steps += v.Now()
		v.Close()
	}
	b.ReportMetric(elapsed.Seconds()/float64(b.N), "s/boot")
	b.ReportMetric(float64(instret)/elapsed.Seconds()/1e6, "MIPS")
	b.ReportMetric(float64(instret)/float64(b.N)/1e6, "Minsn/boot")
}
