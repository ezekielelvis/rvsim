package gdb

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/ezekielelvis/rvsim/vmm/replay"
	"github.com/ezekielelvis/rvsim/vmm/snapshot"
)

// ReplayTarget debugs a replay, which can also run backwards (ADR-030):
// forward execution saves snapshots every Every steps; going back restores
// the nearest snapshot before the target step and replays forward to it,
// which determinism makes exact.
type ReplayTarget struct {
	*VMTarget
	path    string
	spec    replay.Spec
	dir     string
	every   uint64
	console io.Writer
	sess    *replay.Session
	end     uint64 // last step of the recording
	snaps   map[uint64]bool
}

// StopEnd is a stop at the end of the recording.
const StopEnd StopReason = 100

// NewReplayTarget starts debugging the recording at path from its first
// step. Console receives the guest's output while going forward.
func NewReplayTarget(path string, spec replay.Spec, every uint64, console io.Writer) (*ReplayTarget, error) {
	dir, err := os.MkdirTemp("", "rvsim-gdb-snapshots-*")
	if err != nil {
		return nil, err
	}
	s, err := replay.StartReplay(path, spec, console)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	end, err := replay.EndStep(path)
	if err != nil {
		s.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	return &ReplayTarget{VMTarget: NewVMTarget(s.VM), path: path, spec: spec, dir: dir, every: every,
		console: console, sess: s, end: end, snaps: map[uint64]bool{}}, nil
}

// Close releases the session and the snapshots.
func (t *ReplayTarget) Close() {
	t.sess.Close()
	os.RemoveAll(t.dir)
}

// Now returns the current step.
func (t *ReplayTarget) Now() uint64 { return t.vm.Now() }

// forward runs up to n steps, stopping at snapshot boundaries to save one,
// at breakpoints, at the end of the recording, and when interrupted.
func (t *ReplayTarget) forward(n uint64, interrupted func() bool) (Stop, error) {
	target := t.end
	if now := t.vm.Now(); n < t.end-min(now, t.end) {
		target = now + n
	}
	for {
		now := t.vm.Now()
		if now%t.every == 0 && !t.snaps[now] {
			if err := t.saveSnapshot(now); err != nil {
				return Stop{}, err
			}
		}
		if now >= target {
			if now >= t.end {
				return Stop{Reason: StopEnd}, nil
			}
			return Stop{Reason: StopStep}, nil
		}
		stop := min(target, (now/t.every+1)*t.every, now+t.Chunk)
		res, err := t.vm.Run(stop - now)
		if err != nil {
			return Stop{}, err
		}
		if s, ok := stopFor(res); ok {
			return s, nil
		}
		if interrupted != nil && interrupted() {
			return Stop{Reason: StopInterrupt}, nil
		}
	}
}

func (t *ReplayTarget) saveSnapshot(step uint64) error {
	if err := snapshot.SaveFile(filepath.Join(t.dir, fmt.Sprintf("%d.rvsnap", step)), t.vm); err != nil {
		return err
	}
	t.snaps[step] = true
	return nil
}

// Step implements Target.
func (t *ReplayTarget) Step() (Stop, error) {
	s, err := t.forward(1, nil)
	if err == nil && s.Reason == StopStep && t.vm.Now() >= t.end {
		s.Reason = StopEnd
	}
	return s, err
}

// Continue implements Target.
func (t *ReplayTarget) Continue(interrupted func() bool) (Stop, error) {
	return t.forward(^uint64(0), interrupted)
}

// jump replaces the session with one stopped at step, keeping breakpoints.
func (t *ReplayTarget) jump(step uint64) error {
	s, err := replay.JumpTo(t.path, t.spec, t.dir, step, io.Discard)
	if err != nil {
		return err
	}
	t.sess.Close()
	t.sess = s
	s.VM.SetConsole(t.console)
	return t.SetVM(s.VM)
}

// ReverseStep implements Target: back one step.
func (t *ReplayTarget) ReverseStep() (Stop, error) {
	now := t.vm.Now()
	if now == 0 {
		return Stop{Reason: StopHistory}, nil
	}
	if err := t.jump(now - 1); err != nil {
		return Stop{}, err
	}
	return Stop{Reason: StopStep}, nil
}

// ReverseContinue implements Target: back to the most recent breakpoint
// hit before the current step, or to the start of the recording.
func (t *ReplayTarget) ReverseContinue(interrupted func() bool) (Stop, error) {
	now := t.vm.Now()
	starts := []uint64{0}
	for s := range t.snaps {
		if s > 0 && s < now {
			starts = append(starts, s)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] > starts[j] })
	for _, w := range starts {
		hits, err := t.hitsBetween(w, now)
		if err != nil {
			return Stop{}, err
		}
		if len(hits) > 0 {
			if err := t.jump(hits[len(hits)-1]); err != nil {
				return Stop{}, err
			}
			return Stop{Reason: StopBreakpoint}, nil
		}
		if interrupted() {
			// Leave the target where it was.
			return Stop{Reason: StopInterrupt}, t.jump(now)
		}
		now = w
	}
	if err := t.jump(0); err != nil {
		return Stop{}, err
	}
	return Stop{Reason: StopHistory}, nil
}

// hitsBetween replays [from, to) and returns the steps at which a
// breakpoint stopped it, in order.
func (t *ReplayTarget) hitsBetween(from, to uint64) ([]uint64, error) {
	s, err := replay.JumpTo(t.path, t.spec, t.dir, from, io.Discard)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	probe := NewVMTarget(s.VM)
	for addr := range t.bps {
		if err := probe.AddBreakpoint(addr); err != nil {
			return nil, err
		}
	}
	var hits []uint64
	// A breakpoint at the starting pc is not reported by the core; check it.
	if pc, _ := probe.ReadReg(regPC); t.hasBreakpoint(pc) {
		hits = append(hits, from)
	}
	for s.VM.Now() < to {
		res, err := s.VM.Run(to - s.VM.Now())
		if err != nil {
			return nil, err
		}
		if res.Breakpoint && res.Steps < to {
			hits = append(hits, res.Steps)
		}
		if res.Exited {
			break
		}
	}
	return hits, nil
}

func (t *ReplayTarget) hasBreakpoint(pc uint64) bool {
	_, ok := t.bps[pc]
	return ok
}
