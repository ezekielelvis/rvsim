package snapshot

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/devices/virtio/blk"
	. "github.com/ezekielelvis/rvsim/vmm/internal/rvasm"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

// echo polls the UART, echoes each byte and appends it to a RAM buffer.
var echo = Bytes(
	Lui(T0, 0x10000),
	Auipc(T2, 0x10),
	// loop:
	Lbu(T1, T0, 5),
	Andi(T1, T1, 1),
	Beq(T1, Zero, -8),
	Lbu(A0, T0, 0),
	Sb(A0, T0, 0),
	Sb(A0, T2, 0),
	Addi(T2, T2, 1),
	Jal(Zero, -28),
)

const (
	every = 10_000
	end   = 300_000
)

type hashLog struct {
	v      *vm.VM
	hashes map[uint64]vm.Hashes
}

func (h *hashLog) checkpoint(step uint64) error {
	hs, err := h.v.StateHashes()
	h.hashes[step] = hs
	return err
}

// newVM builds the test VM with a disk, the program and pending input.
func newVM(t *testing.T, disk string, log *hashLog, console *bytes.Buffer) *vm.VM {
	t.Helper()
	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	cfg := machine.DefaultConfig()
	cfg.RAMSize = 4 << 20
	v, err := vm.New(vm.Config{Machine: cfg, Console: console,
		Disk:            &blk.Config{Disk: f, Size: 64 << 10, Serial: "s"},
		CheckpointEvery: every, Checkpoint: log.checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	log.v = v
	if err := v.Machine().WritePhys(cfg.RAMBase, echo); err != nil {
		t.Fatal(err)
	}
	v.ScheduleInput(50_000, []byte("before "))
	v.ScheduleInput(200_000, []byte("after the snapshot"))
	return v
}

func diskImage(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	b := make([]byte, 64<<10)
	copy(b[8192:], "disk contents")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRestoredRunMatchesUninterruptedRun(t *testing.T) {
	// Uninterrupted.
	ref := &hashLog{hashes: map[uint64]vm.Hashes{}}
	var refOut bytes.Buffer
	v := newVM(t, diskImage(t), ref, &refOut)
	if _, err := v.Run(end); err != nil {
		t.Fatal(err)
	}
	refFinal, _ := v.StateHashes()
	v.Close()

	// Interrupted at a step that is not a checkpoint, saved and restored.
	got := &hashLog{hashes: map[uint64]vm.Hashes{}}
	var out bytes.Buffer
	v = newVM(t, diskImage(t), got, &out)
	const at = 123_457
	if _, err := v.Run(at); err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	if err := Save(&file, v); err != nil {
		t.Fatal(err)
	}
	v.Close()
	snap, err := Load(bytes.NewReader(file.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Steps() != at {
		t.Fatalf("snapshot at step %d", snap.Steps())
	}
	v, f, err := snap.Restore(vm.Config{Console: &out, CheckpointEvery: every, Checkpoint: got.checkpoint},
		filepath.Join(t.TempDir(), "restored.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got.v = v
	if _, err := v.Run(end - at); err != nil {
		t.Fatal(err)
	}
	final, _ := v.StateHashes()
	v.Close()

	if len(ref.hashes) != end/every {
		t.Fatalf("%d reference checkpoints", len(ref.hashes))
	}
	for step, want := range ref.hashes {
		if g, ok := got.hashes[step]; !ok || g != want {
			t.Errorf("checkpoint %d: %+v, want %+v", step, g, want)
		}
	}
	if final != refFinal {
		t.Errorf("final hashes %+v, want %+v", final, refFinal)
	}
	if out.String() != refOut.String() || out.String() != "before after the " { // input is paced: 10,000 steps a byte
		t.Errorf("console %q, uninterrupted %q", out.String(), refOut.String())
	}
}

func TestSnapshotOfRestoredVMIsIdentical(t *testing.T) {
	log := &hashLog{hashes: map[uint64]vm.Hashes{}}
	v := newVM(t, diskImage(t), log, &bytes.Buffer{})
	if _, err := v.Run(60_000); err != nil {
		t.Fatal(err)
	}
	var a bytes.Buffer
	if err := Save(&a, v); err != nil {
		t.Fatal(err)
	}
	v.Close()
	snap, err := Load(bytes.NewReader(a.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	dp := filepath.Join(t.TempDir(), "d.img")
	v2, f, err := snap.Restore(vm.Config{CheckpointEvery: every, Checkpoint: log.checkpoint}, dp)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer v2.Close()
	var b bytes.Buffer
	if err := Save(&b, v2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("saving a freshly restored VM gave different bytes")
	}
	disk, _ := os.ReadFile(dp)
	if !bytes.Contains(disk, []byte("disk contents")) || len(disk) != 64<<10 {
		t.Fatal("disk contents not restored")
	}
	if a.Len() > 64<<10 {
		t.Errorf("snapshot of a 4 MiB machine is %d bytes: zero pages are not being skipped", a.Len())
	}
}

func TestLoadRejectsDamage(t *testing.T) {
	log := &hashLog{hashes: map[uint64]vm.Hashes{}}
	v := newVM(t, diskImage(t), log, &bytes.Buffer{})
	defer v.Close()
	var file bytes.Buffer
	if err := Save(&file, v); err != nil {
		t.Fatal(err)
	}
	good := file.Bytes()
	for name, b := range map[string][]byte{
		"flipped bit": func() []byte { c := bytes.Clone(good); c[100] ^= 1; return c }(),
		"truncated":   good[:len(good)-1],
		"bad magic":   append([]byte("XXXXXXXX"), good[8:]...),
		"empty":       nil,
	} {
		if _, err := Load(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
