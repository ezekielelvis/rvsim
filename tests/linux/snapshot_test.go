package linux

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/boot"
	"github.com/ezekielelvis/rvsim/vmm/devices/virtio/blk"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/snapshot"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

const snapEvery = 5_000_000

type checkpoints struct {
	v      *vm.VM
	hashes map[uint64]vm.Hashes
}

func (c *checkpoints) record(step uint64) error {
	h, err := c.v.StateHashes()
	c.hashes[step] = h
	return err
}

// bootWithCheckpoints boots img, with poweroff typed at a fixed step.
func bootWithCheckpoints(t *testing.T, img boot.Images, disk *blk.Config, ck *checkpoints, console *bytes.Buffer) *vm.VM {
	t.Helper()
	cfg := machine.DefaultConfig()
	v, err := vm.New(vm.Config{Machine: cfg, Console: console, Disk: disk,
		CheckpointEvery: snapEvery, Checkpoint: ck.record})
	if err != nil {
		t.Fatal(err)
	}
	ck.v = v
	if err := boot.Load(v, cfg, img); err != nil {
		t.Fatal(err)
	}
	v.ScheduleInput(1_500_000_000, []byte("cat /proc/uptime; poweroff -f\n"))
	return v
}

func runToExit(t *testing.T, v *vm.VM, console *bytes.Buffer) vm.Result {
	t.Helper()
	res, err := v.Run(4_000_000_000)
	if err != nil || !res.Exited {
		t.Fatalf("%+v %v; console:\n%s", res, err, console.String())
	}
	return res
}

// The Phase 9 acceptance test: snapshot mid-boot, restore into a new VM,
// and the continued boot matches an uninterrupted one at every checkpoint.
func TestSnapshotMidBootMatchesUninterruptedBoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		disk bool
	}{{"initramfs", false}, {"ext4 disk", true}} {
		t.Run(tc.name, func(t *testing.T) {
			img := images(t)
			var openDiskFn func() *blk.Config
			if tc.disk {
				img.Initrd, img.Bootargs = nil, "console=ttyS0 earlycon root=/dev/vda rw"
				openDiskFn = func() *blk.Config { d, _ := openDiskCopy(t); return d }
			} else {
				openDiskFn = func() *blk.Config { return nil }
			}

			ref := &checkpoints{hashes: map[uint64]vm.Hashes{}}
			var refOut bytes.Buffer
			v := bootWithCheckpoints(t, img, openDiskFn(), ref, &refOut)
			refRes := runToExit(t, v, &refOut)
			refFinal, _ := v.StateHashes()
			v.Close()

			got := &checkpoints{hashes: map[uint64]vm.Hashes{}}
			var out bytes.Buffer
			v = bootWithCheckpoints(t, img, openDiskFn(), got, &out)
			const at = 40_000_001 // mid-boot, between checkpoints
			if _, err := v.Run(at); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "boot.rvsnap")
			if err := snapshot.SaveFile(path, v); err != nil {
				t.Fatal(err)
			}
			v.Close()
			snap, err := snapshot.LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			v, disk, err := snap.Restore(vm.Config{Console: &out, CheckpointEvery: snapEvery, Checkpoint: got.record},
				filepath.Join(t.TempDir(), "disk.img"))
			if err != nil {
				t.Fatal(err)
			}
			if disk != nil {
				defer disk.Close()
			}
			got.v = v
			res := runToExit(t, v, &out)
			final, _ := v.StateHashes()
			v.Close()

			compared := 0
			for step, want := range ref.hashes {
				if step <= at {
					continue
				}
				compared++
				if g := got.hashes[step]; g != want {
					t.Fatalf("checkpoint at step %d differs: %+v, want %+v", step, g, want)
				}
			}
			if res != refRes || final != refFinal || out.String() != refOut.String() {
				t.Fatalf("end differs: %+v vs %+v", res, refRes)
			}
			t.Logf("snapshot at step %d (%s); %d checkpoints and the final state match", at, sizeOf(path), compared)
		})
	}
}
