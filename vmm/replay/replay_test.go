package replay

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/ezekielelvis/rvsim/vmm/internal/rvasm"
)

// echoProgram polls the UART, echoes each byte, appends it to a buffer in
// RAM, spins ~4,000 steps (so a session spans enough virtual time for
// several checkpoints however fast the host is), and powers off through the
// finisher after a 'q'.
func echoProgram() []byte {
	return Bytes(
		Lui(T0, 0x10000), // UART
		Auipc(T2, 0x10),  // buffer in RAM
		Lui(S0, 0x100),   // finisher
		// loop:
		Lbu(T1, T0, 5), // LSR
		Andi(T1, T1, 1),
		Beq(T1, Zero, -8),
		Lbu(A0, T0, 0), // RBR
		Sb(A0, T0, 0),  // echo
		Sb(A0, T2, 0),
		Addi(T2, T2, 1),
		Addi(S1, Zero, 2000),
		Addi(S1, S1, -1),
		Bne(S1, Zero, -4),
		Addi(T1, Zero, 'q'),
		Bne(A0, T1, -44),
		Lui(T1, 5),
		Addi(T1, T1, 0x555),
		Sw(T1, S0, 0), // power off
		Self,
	)
}

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func testSpec(t *testing.T) Spec {
	t.Helper()
	dir := t.TempDir()
	return Spec{
		Firmware:        writeFile(t, dir, "fw.bin", echoProgram()),
		Kernel:          writeFile(t, dir, "Image", make([]byte, 4096)),
		Disk:            writeFile(t, dir, "disk.img", bytes.Repeat([]byte{0xd5}, 8192)),
		Bootargs:        "test",
		RAMSize:         64 << 20,
		CheckpointEvery: 5_000,
	}
}

// typeSlowly sends text with random host-side delays, then closes.
func typeSlowly(text string) <-chan byte {
	ch := make(chan byte)
	go func() {
		defer close(ch)
		for i := range len(text) {
			time.Sleep(time.Duration(rand.IntN(1500)) * time.Microsecond)
			ch <- text[i]
		}
	}()
	return ch
}

func record(t *testing.T, spec Spec, out, text string) string {
	t.Helper()
	var console bytes.Buffer
	s, err := StartRecording(spec, out, &console, typeSlowly(text))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for {
		res, err := s.Run(1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if res.Exited {
			if err := s.Finish(res, false); err != nil {
				t.Fatal(err)
			}
			break
		}
		if res.Steps > 1_000_000_000 {
			t.Fatalf("no exit; console %q", console.String())
		}
	}
	return console.String()
}

func TestRecordThenReplayTenTimes(t *testing.T) {
	spec := testSpec(t)
	path := filepath.Join(t.TempDir(), "s.rvrec")
	const text = "hello, rvsim\nq"
	if got := record(t, spec, path, text); got != text {
		t.Fatalf("recorded console %q", got)
	}

	f, _ := os.Open(path)
	h, recs, err := Read(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var inputs, ckpts int
	for _, r := range recs {
		switch r.Tag {
		case TagInput:
			inputs++
		case TagCheckpoint:
			ckpts++
		}
	}
	if last := recs[len(recs)-1]; last.Tag != TagEnd || last.Reason != EndExit {
		t.Fatalf("last record %+v", last)
	}
	// 13 bytes before the q, at >= 4,000 steps each, span >= 10 checkpoints.
	if inputs == 0 || ckpts < 10 {
		t.Fatalf("%d input and %d checkpoint records", inputs, ckpts)
	}
	for _, role := range []string{"firmware", "kernel", "disk", "dtb"} {
		if _, ok := h.Image(role); !ok {
			t.Errorf("header lacks the %s image", role)
		}
	}

	for i := range 10 {
		var console bytes.Buffer
		s, err := StartReplay(path, Spec{}, &console)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := s.Replay()
		s.Close()
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if rep.Checkpoints != ckpts || rep.End != EndExit || console.String() != text {
			t.Fatalf("replay %d: %+v, console %q", i, rep, console.String())
		}
	}
}

// rewrite copies a recording, letting edit change each record.
func rewrite(t *testing.T, src, dst string, edit func(*Record)) {
	t.Helper()
	f, _ := os.Open(src)
	h, recs, err := Read(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	w, err := Create(dst, h)
	if err != nil {
		t.Fatal(err)
	}
	for i := range recs {
		r := &recs[i]
		edit(r)
		switch r.Tag {
		case TagInput:
			err = w.Input(r.Step, r.Data)
		case TagInputEOF:
			err = w.InputEOF(r.Step)
		case TagCheckpoint:
			err = w.Checkpoint(r.Step, r.Instret, r.Hashes)
		case TagEnd:
			err = w.End(r.Step, r.Instret, r.Hashes, r.Reason, r.Code)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayDetectsDivergence(t *testing.T) {
	spec := testSpec(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.rvrec")
	record(t, spec, path, "abcq")

	// Change one input byte: the guest stores it in RAM, so the next
	// checkpoint's RAM hash must differ.
	bad := filepath.Join(dir, "bad.rvrec")
	edited := false
	rewrite(t, path, bad, func(r *Record) {
		if r.Tag == TagInput && !edited && r.Data[0] == 'a' {
			r.Data = append([]byte{'x'}, r.Data[1:]...)
			edited = true
		}
	})
	s, err := StartReplay(bad, Spec{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Replay()
	s.Close()
	var d *Divergence
	if !errors.As(err, &d) {
		t.Fatalf("replay of altered input: %v", err)
	}
	// The byte lands in the RAM buffer (and in a0, until the next byte
	// replaces it, which depends on input timing).
	if d.Want.RAM == d.Got.RAM {
		t.Errorf("expected RAM to diverge: %v", d)
	}
	if !strings.Contains(d.Error(), "ram ") || d.Step <= d.LastGoodStep {
		t.Errorf("report: %v", d)
	}
}

func TestReplayRejectsDifferentImages(t *testing.T) {
	spec := testSpec(t)
	path := filepath.Join(t.TempDir(), "s.rvrec")
	record(t, spec, path, "q")
	other := writeFile(t, t.TempDir(), "disk.img", bytes.Repeat([]byte{1}, 8192))
	if _, err := StartReplay(path, Spec{Disk: other}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "disk") {
		t.Fatalf("different disk accepted: %v", err)
	}
}

func TestTruncatedRecordingReplaysToLastCheckpoint(t *testing.T) {
	spec := testSpec(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.rvrec")
	record(t, spec, path, "hello, rvsim\nq")
	b, _ := os.ReadFile(path)
	cut := writeFile(t, dir, "cut.rvrec", b[:len(b)-45]) // inside the END record
	s, err := StartReplay(cut, Spec{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Warnings) == 0 {
		t.Error("no truncation warning")
	}
	rep, err := s.Replay()
	if err != nil || !rep.Truncated || rep.Checkpoints == 0 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestReadRejectsBadFiles(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":     nil,
		"magic":     []byte("NOTRVSIM\x01\x00\x00\x00\x00\x00"),
		"version":   []byte("RVSIMREC\x09\x00\x02\x00\x00\x00{}"),
		"bad tag":   append([]byte("RVSIMREC\x01\x00\x02\x00\x00\x00{}"), 0x7f, 0, 0),
		"bad json":  []byte("RVSIMREC\x01\x00\x01\x00\x00\x00{"),
		"short end": append([]byte("RVSIMREC\x01\x00\x02\x00\x00\x00{}"), TagEnd, 0, 1, 0),
	} {
		if _, _, err := Read(bytes.NewReader(b)); err == nil || errors.Is(err, ErrTruncated) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestJumpToStepRestoresTheNearestSnapshot(t *testing.T) {
	spec := testSpec(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.rvrec")
	record(t, spec, path, "jump around, rvsim\nq")

	snaps := filepath.Join(dir, "snaps")
	if err := os.Mkdir(snaps, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := StartReplay(path, Spec{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	s.SnapshotEvery(15_000, snaps)
	rep, err := s.Replay()
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(snaps)
	if len(entries) == 0 {
		t.Fatal("no snapshots written")
	}

	for _, step := range []uint64{7_000, 31_234, 45_000, rep.Steps - 1} {
		// Reference: replay from the start to step.
		ref, err := StartReplay(path, Spec{}, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ref.VM.Run(step); err != nil {
			t.Fatal(err)
		}
		want, _ := ref.VM.StateHashes()
		ref.Close()

		j, err := JumpTo(path, Spec{}, snaps, step, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := j.VM.StateHashes()
		if j.VM.Now() != step || got != want {
			t.Errorf("jump to %d: at %d with %+v, want %+v", step, j.VM.Now(), got, want)
		}
		// The jumped-to session replays on to the end, still identical.
		if _, err := j.Replay(); err != nil {
			t.Errorf("continuing after jump to %d: %v", step, err)
		}
		j.Close()
	}
}
