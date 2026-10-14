package linux

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ezekielelvis/rvsim/vmm/replay"
)

// The session typed at the shell: commands and a small program.
var sessionCommands = []string{
	"uname -a",
	"i=0; while [ $i -lt 300 ]; do i=$((i+1)); done; echo loop=$i",
	"echo replay-me > /root/r && md5sum /root/r",
	"cat /proc/interrupts | wc -l",
	"poweroff",
}

func diskSpec(t *testing.T) replay.Spec {
	t.Helper()
	readImage(t, "rootfs.ext4") // skip if the images are missing
	return replay.Spec{
		Firmware: imagePath("fw_jump.bin"), Kernel: imagePath("Image"), Disk: imagePath("rootfs.ext4"),
		Bootargs: "console=ttyS0 earlycon root=/dev/vda rw", CheckpointEvery: 5_000_000,
	}
}

// recordSession boots from the disk and types the session's commands, each
// after the previous prompt plus a random host-side delay, so the input's
// arrival in virtual time depends on host timing as it would for a person.
func recordSession(t *testing.T, path string) string {
	t.Helper()
	input := make(chan byte, 256)
	var console bytes.Buffer
	s, err := replay.StartRecording(diskSpec(t), path, &console, input)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	next, seen := 0, 0
	for {
		res, err := s.Run(5_000_000)
		if err != nil {
			t.Fatalf("%v; console:\n%s", err, console.String())
		}
		if res.Exited {
			if err := s.Finish(res, false); err != nil {
				t.Fatal(err)
			}
			return console.String()
		}
		out := console.String()
		if i := strings.LastIndex(out, "\n# "); i >= seen && next < len(sessionCommands) {
			seen = i + 1
			cmd := sessionCommands[next] + "\n"
			next++
			go func() {
				time.Sleep(time.Duration(rand.IntN(300)) * time.Millisecond)
				for _, b := range []byte(cmd) {
					input <- b
				}
			}()
		}
		if res.Steps > 40_000_000_000 {
			t.Fatalf("session did not finish; console:\n%s", out)
		}
	}
}

// replayTimes replays the recording n times; every replay must be identical.
func replayTimes(t *testing.T, path string, spec replay.Spec, n int, wantConsole string) {
	t.Helper()
	for i := range n {
		var console bytes.Buffer
		s, err := replay.StartReplay(path, spec, &console)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := s.Replay()
		s.Close()
		if err != nil {
			t.Fatalf("replay %d: %v", i+1, err)
		}
		if rep.End != replay.EndExit || rep.Checkpoints == 0 {
			t.Fatalf("replay %d: %+v", i+1, rep)
		}
		if wantConsole != "" && console.String() != wantConsole {
			t.Fatalf("replay %d printed different console output", i+1)
		}
		if i == 0 {
			t.Logf("replay: %d checkpoints, %d steps, %d instructions", rep.Checkpoints, rep.Steps, rep.Instret)
		}
	}
}

func TestRecordedInteractiveSessionReplaysIdentically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.rvrec")
	console := recordSession(t, path)
	for _, want := range []string{"Linux rvsim 6.12.112-rvsim", "loop=300", "/root/r", "reboot: Power down"} {
		if !strings.Contains(console, want) {
			t.Fatalf("session output lacks %q:\n%s", want, console)
		}
	}
	replayTimes(t, path, replay.Spec{}, 10, console)

	// Hand the recording to CI's replay-on-another-machine job.
	if out := os.Getenv("RVSIM_RECORDING_OUT"); out != "" {
		copyFile(t, path, out)
	}
}

// TestReplayRecordingFromAnotherMachine replays a recording made elsewhere
// (CI records on Linux and replays on macOS) with this machine's copies of
// the images, which must be byte-identical to the recorded ones.
func TestReplayRecordingFromAnotherMachine(t *testing.T) {
	path := os.Getenv("RVSIM_REPLAY")
	if path == "" {
		t.Skip("RVSIM_REPLAY not set")
	}
	spec := diskSpec(t)
	replayTimes(t, path, replay.Spec{Firmware: spec.Firmware, Kernel: spec.Kernel, Disk: spec.Disk}, 10, "")
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func sizeOf(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%.1f MiB", float64(st.Size())/(1<<20))
}
