package linux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/boot"
	"github.com/ezekielelvis/rvsim/vmm/fault"
)

// diskBootImages boots from the ext4 disk.
func diskBootImages(t *testing.T) boot.Images {
	img := images(t)
	img.Initrd, img.Bootargs = nil, "console=ttyS0 earlycon root=/dev/vda rw"
	return img
}

// promptStep finds the step at which a disk boot's shell is ready. Boots
// are deterministic, so a second boot reaches it at the same step.
func promptStep(t *testing.T) uint64 {
	t.Helper()
	disk, _ := openDiskCopy(t)
	g := newGuestWithDisk(t, diskBootImages(t), disk)
	g.waitFor(prompt, 4_000_000_000)
	return g.v.Now()
}

// faultyDiskBoot boots from the disk with a fault spec applied, to the shell.
func faultyDiskBoot(t *testing.T, spec string) (*guest, *[]fault.Applied) {
	t.Helper()
	s, err := fault.Parse([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	var applied []fault.Applied
	inj := fault.NewLive(s, func(a fault.Applied) error { applied = append(applied, a); return nil })
	disk, _ := openDiskCopy(t)
	g := newGuestWithFaults(t, diskBootImages(t), disk, inj)
	g.waitFor(prompt, 4_000_000_000)
	return g, &applied
}

// readBlock reads one 4 KiB block of the raw disk well past the filesystem's
// data, so it comes from the device, not the page cache.
const readBlock = "dd if=/dev/vda of=/dev/null bs=4096 skip=6000 count=1; echo rc=$?"

func TestBlkFailIsAnIOErrorInTheGuest(t *testing.T) {
	p := promptStep(t)
	g, applied := faultyDiskBoot(t, fmt.Sprintf(`
faults:
  - name: dead-sector
    when: {step: %d}
    do: {blk: {fail: true}}
    times: 2 # the readahead, and the synchronous retry
`, p))
	g.command(readBlock)
	// The command's output, including the kernel's message, is everything
	// since it was typed.
	out := g.console.String()[strings.LastIndex(g.console.String(), "# dd "):]
	if !strings.Contains(out, "I/O error, dev vda") || !strings.Contains(out, "rc=1") {
		t.Fatalf("dd after an injected failure: %q\nconsole tail:\n%s", out, g.console.String()[max(0, g.console.Len()-1500):])
	}
	if len(*applied) != 2 || (*applied)[0].Kind != "blk_fail" {
		t.Fatalf("applied %+v", *applied)
	}
	// The fault was one request: the device works again.
	if out := g.command(readBlock); !strings.Contains(out, "rc=0") {
		t.Fatalf("second dd:\n%s", out)
	}
	t.Logf("guest saw:\n%s", out)
}

func TestBlkCorruptChangesTheDataTheGuestReads(t *testing.T) {
	p := promptStep(t)
	g, applied := faultyDiskBoot(t, fmt.Sprintf(`
faults:
  - name: bit-rot
    when: {step: %d}
    do: {blk: {corrupt: true, offset: 100, xor: 0x5a}}
`, p))
	const sum = "dd if=/dev/vda bs=4096 skip=6000 count=1 2>/dev/null | md5sum"
	corrupted := g.command(sum)
	g.command("echo 3 > /proc/sys/vm/drop_caches")
	clean := g.command(sum)
	const zeroBlock = "620f0b67a91f7f74151bc5be745b7110" // md5 of 4096 zero bytes
	if !strings.HasPrefix(clean, zeroBlock) || strings.HasPrefix(corrupted, zeroBlock) || len(corrupted) < 32 {
		t.Fatalf("corrupted read %q, clean read %q", corrupted, clean)
	}
	if len(*applied) != 1 || (*applied)[0].Kind != "blk_corrupt" || (*applied)[0].Offset != 100 {
		t.Fatalf("applied %+v", *applied)
	}
}
