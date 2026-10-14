package replay

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ezekielelvis/rvsim/vmm/boot"
	"github.com/ezekielelvis/rvsim/vmm/devices/virtio/blk"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/snapshot"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

// DefaultCheckpointEvery is the default checkpoint interval in steps.
const DefaultCheckpointEvery = 10_000_000

// Spec names a session's images and settings.
type Spec struct {
	Firmware, Kernel, Initrd, Disk string // paths; Initrd and Disk are optional
	DiskReadOnly                   bool
	Bootargs                       string
	RAMSize                        uint64 // bytes; 0 means the default
	CheckpointEvery                uint64 // steps; 0 means DefaultCheckpointEvery
	// KeepDisk, if set, receives the disk image as the session left it.
	// Otherwise the session's writes are discarded: both recording and replay
	// run on a private copy, so the original stays a pristine input.
	KeepDisk string
}

// Session is a VM being recorded or replayed.
type Session struct {
	VM     *vm.VM
	Header *Header

	// Warnings are non-fatal replay findings (e.g. a different core build).
	Warnings []string

	replay   bool
	disk     *os.File
	diskPath string // private copy
	keepDisk string
	w        *Writer
	ckpts    []Record // replay: recorded checkpoints
	next     int      // replay: next checkpoint to compare
	end      *Record  // replay: the END record, if the recording has one

	snapEvery uint64 // replay: save a snapshot every snapEvery steps
	snapDir   string
}

// Divergence reports a replay whose state differs from the recording.
type Divergence struct {
	Checkpoint   int    // index of the first mismatching checkpoint (-1: the end)
	Step         uint64 // where it was detected
	LastGoodStep uint64 // the previous matching checkpoint (0: the start)
	Want, Got    vm.Hashes
}

func (d *Divergence) Error() string {
	var parts []string
	for _, c := range []struct {
		name      string
		want, got uint64
	}{{"core", d.Want.Core, d.Got.Core}, {"ram", d.Want.RAM, d.Got.RAM}, {"vmm", d.Want.VMM, d.Got.VMM}} {
		if c.want != c.got {
			parts = append(parts, fmt.Sprintf("%s %016x != recorded %016x", c.name, c.got, c.want))
		}
	}
	where := fmt.Sprintf("checkpoint %d", d.Checkpoint)
	if d.Checkpoint < 0 {
		where = "the end of the recording"
	}
	return fmt.Sprintf("replay diverged at %s (step %d): %v; state last matched at step %d, so the divergence is in steps (%d, %d]",
		where, d.Step, parts, d.LastGoodStep, d.LastGoodStep, d.Step)
}

// loaded is a session's images in memory, plus the disk copy.
type loaded struct {
	img    boot.Images
	images []Image
	disk   *os.File
	path   string
	size   int64
}

func hashFile(role, path string) ([]byte, Image, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, Image{}, err
	}
	sum := sha256.Sum256(b)
	return b, Image{Role: role, Path: path, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}, nil
}

// load reads and hashes the images and copies the disk to a private file.
func load(spec Spec) (*loaded, error) {
	l := &loaded{img: boot.Images{Bootargs: spec.Bootargs}}
	for _, f := range []struct {
		role, path string
		dst        *[]byte
	}{{"firmware", spec.Firmware, &l.img.Firmware}, {"kernel", spec.Kernel, &l.img.Kernel}, {"initrd", spec.Initrd, &l.img.Initrd}} {
		if f.path == "" {
			continue
		}
		b, im, err := hashFile(f.role, f.path)
		if err != nil {
			return nil, err
		}
		*f.dst = b
		l.images = append(l.images, im)
	}
	if spec.Disk != "" {
		if err := l.copyDisk(spec.Disk); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *loaded) copyDisk(src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp("", "rvsim-disk-*.img")
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		out.Close()
		os.Remove(out.Name())
		return err
	}
	l.disk, l.path, l.size = out, out.Name(), n
	l.images = append(l.images, Image{Role: "disk", Path: src, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
	return nil
}

func (l *loaded) release() {
	if l.disk != nil {
		l.disk.Close()
		os.Remove(l.path)
	}
}

// dtbImage reads the device tree boot.Load placed in guest memory.
func dtbImage(v *vm.VM, cfg machine.Config) (Image, error) {
	addr := cfg.RAMBase + boot.FDTOffset
	var hdr [8]byte
	if err := v.Machine().ReadPhys(addr, hdr[:]); err != nil {
		return Image{}, err
	}
	b := make([]byte, binary.BigEndian.Uint32(hdr[4:]))
	if err := v.Machine().ReadPhys(addr, b); err != nil {
		return Image{}, err
	}
	sum := sha256.Sum256(b)
	return Image{Role: "dtb", Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}, nil
}

// StartRecording boots spec and records the session to out. Host input
// arrives on input (nil for none); console receives guest output.
func StartRecording(spec Spec, out string, console io.Writer, input <-chan byte) (*Session, error) {
	l, err := load(spec)
	if err != nil {
		return nil, err
	}
	cfg := machine.DefaultConfig()
	if spec.RAMSize != 0 {
		cfg.RAMSize = spec.RAMSize
	}
	every := spec.CheckpointEvery
	if every == 0 {
		every = DefaultCheckpointEvery
	}
	h := &Header{
		ABIVersion: machine.ABIVersion, CoreBuild: machine.CoreBuild(), Machine: cfg,
		Bootargs: spec.Bootargs, Input: input != nil, CheckpointEvery: every, Hash: "xxh3-64",
	}
	s := &Session{Header: h, keepDisk: spec.KeepDisk}
	vcfg := vm.Config{Machine: cfg, Console: console, Input: input, CheckpointEvery: every}
	if l.disk != nil {
		h.Disk = &DiskConfig{ReadOnly: spec.DiskReadOnly, Latency: blk.DefaultLatency, Serial: "rvsim-disk0"}
		vcfg.Disk = diskConfig(h.Disk, l)
	}
	vcfg.Checkpoint = func(step uint64) error {
		hs, err := s.VM.StateHashes()
		if err != nil {
			return err
		}
		return s.w.Checkpoint(step, s.VM.Instret(), hs)
	}
	if err := s.start(vcfg, l, func() error {
		var err error
		s.w, err = Create(out, h)
		return err
	}); err != nil {
		return nil, err
	}
	return s, nil
}

func diskConfig(d *DiskConfig, l *loaded) *blk.Config {
	return &blk.Config{Disk: l.disk, Size: uint64(l.size), ReadOnly: d.ReadOnly, Latency: d.Latency, Serial: d.Serial}
}

// start loads the images into a new VM. beforeVM runs once the header's
// images are complete except the DTB, and may adjust vcfg.
func (s *Session) start(vcfg vm.Config, l *loaded, beforeVM func() error) error {
	s.disk, s.diskPath = l.disk, l.path
	if !s.replay {
		s.Header.Images = l.images
	}
	// The DTB depends only on the configuration; load it into a VM to learn
	// its hash, which the header must carry before any record.
	probe, err := vm.New(vm.Config{Machine: vcfg.Machine, Disk: vcfg.Disk})
	if err != nil {
		l.release()
		return err
	}
	if err := boot.Load(probe, vcfg.Machine, l.img); err != nil {
		probe.Close()
		l.release()
		return err
	}
	dtb, err := dtbImage(probe, vcfg.Machine)
	probe.Close()
	if err != nil {
		l.release()
		return err
	}
	if err := s.checkDTB(dtb); err != nil {
		l.release()
		return err
	}
	if err := beforeVM(); err != nil {
		l.release()
		return err
	}
	if s.w != nil {
		vcfg.Record = s.w
	}
	v, err := vm.New(vcfg)
	if err == nil {
		err = boot.Load(v, vcfg.Machine, l.img)
		if err != nil {
			v.Close()
		}
	}
	if err != nil {
		if s.w != nil {
			s.w.Close()
		}
		l.release()
		return err
	}
	s.VM = v
	return nil
}

// checkDTB adds the DTB to a new recording's header, or checks it against a
// recorded one.
func (s *Session) checkDTB(dtb Image) error {
	if s.replay {
		want, ok := s.Header.Image("dtb")
		if !ok || want.SHA256 != dtb.SHA256 {
			return fmt.Errorf("replay: this rvsim generates a different device tree than the recording's (sha256 %s, recorded %s)", dtb.SHA256, want.SHA256)
		}
		return nil
	}
	s.Header.Images = append(s.Header.Images, dtb)
	return nil
}

// Run runs the VM (see vm.VM.Run).
func (s *Session) Run(maxSteps uint64) (vm.Result, error) { return s.VM.Run(maxSteps) }

// Finish records how the session ended. Call it once, after the last Run.
func (s *Session) Finish(res vm.Result, interrupted bool) error {
	hs, err := s.VM.StateHashes()
	if err != nil {
		return err
	}
	reason := EndLimit
	switch {
	case res.Exited && res.Reset:
		reason = EndReset
	case res.Exited:
		reason = EndExit
	case interrupted:
		reason = EndInterrupted
	}
	return s.w.End(res.Steps, res.Instret, hs, reason, res.ExitCode)
}

// Close releases the VM and the disk copy (saving it to KeepDisk if set),
// and closes the recording.
func (s *Session) Close() error {
	var err error
	if s.w != nil {
		err = s.w.Close()
	}
	if s.VM != nil {
		s.VM.Close()
	}
	if s.disk != nil {
		if s.keepDisk != "" {
			if kerr := keep(s.disk, s.keepDisk); err == nil {
				err = kerr
			}
		}
		s.disk.Close()
		os.Remove(s.diskPath)
	}
	return err
}

func keep(src *os.File, dst string) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Report summarizes a successful replay.
type Report struct {
	Checkpoints int
	Steps       uint64
	Instret     uint64
	End         string // END reason, or "" for a truncated recording
	Truncated   bool
}

// openRecording reads a recording into a replay session (no VM yet) and
// the VM configuration to replay it with.
func openRecording(path string, spec Spec, console io.Writer) (*Session, vm.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, vm.Config{}, err
	}
	h, recs, rerr := Read(f)
	f.Close()
	if rerr != nil && !errors.Is(rerr, ErrTruncated) {
		return nil, vm.Config{}, rerr
	}
	if h.ABIVersion != machine.ABIVersion {
		return nil, vm.Config{}, fmt.Errorf("replay: recorded with core ABI v%d, this rvsim has v%d", h.ABIVersion, machine.ABIVersion)
	}
	s := &Session{Header: h, replay: true, keepDisk: spec.KeepDisk}
	if h.CoreBuild != machine.CoreBuild() {
		s.Warnings = append(s.Warnings, fmt.Sprintf("recorded with core build %.12s, replaying with %.12s", h.CoreBuild, machine.CoreBuild()))
	}
	if rerr != nil {
		s.Warnings = append(s.Warnings, "the recording is truncated; replaying up to its last complete record")
	}
	script := &vm.InputScript{Open: h.Input}
	for i := range recs {
		r := &recs[i]
		switch r.Tag {
		case TagInput:
			script.Events = append(script.Events, vm.InputEvent{Step: r.Step, Data: r.Data})
		case TagInputEOF:
			script.Events = append(script.Events, vm.InputEvent{Step: r.Step, EOF: true})
		case TagCheckpoint:
			s.ckpts = append(s.ckpts, *r)
		case TagEnd:
			s.end = r
		}
	}
	vcfg := vm.Config{Machine: h.Machine, Console: console, Replay: script, CheckpointEvery: h.CheckpointEvery,
		Checkpoint: s.verify}
	return s, vcfg, nil
}

// StartReplay prepares to replay the recording at path. Image paths come
// from the recording unless spec overrides them; the images must match the
// recorded hashes.
func StartReplay(path string, spec Spec, console io.Writer) (*Session, error) {
	s, vcfg, err := openRecording(path, spec, console)
	if err != nil {
		return nil, err
	}
	h := s.Header
	rspec := Spec{Bootargs: h.Bootargs, DiskReadOnly: h.Disk != nil && h.Disk.ReadOnly}
	for _, role := range []struct {
		name     string
		override string
		dst      *string
	}{{"firmware", spec.Firmware, &rspec.Firmware}, {"kernel", spec.Kernel, &rspec.Kernel},
		{"initrd", spec.Initrd, &rspec.Initrd}, {"disk", spec.Disk, &rspec.Disk}} {
		im, ok := h.Image(role.name)
		if !ok {
			if role.override != "" {
				return nil, fmt.Errorf("replay: the recording has no %s image", role.name)
			}
			continue
		}
		*role.dst = im.Path
		if role.override != "" {
			*role.dst = role.override
		}
	}
	l, err := load(rspec)
	if err != nil {
		return nil, err
	}
	for _, im := range l.images {
		want, _ := h.Image(im.Role)
		if im.SHA256 != want.SHA256 {
			l.release()
			return nil, fmt.Errorf("replay: %s %s has sha256 %s; the recording used %s", im.Role,
				filepath.Base(im.Path), im.SHA256, want.SHA256)
		}
	}
	if h.Disk != nil {
		vcfg.Disk = diskConfig(h.Disk, l)
	}
	if err := s.start(vcfg, l, func() error { return nil }); err != nil {
		return nil, err
	}
	return s, nil
}

// JumpTo returns a replay of the recording at path stopped at step: it
// restores the latest snapshot at or before step from snapDir (written by
// SnapshotEvery) and replays forward, checking checkpoints on the way.
// Without a usable snapshot it replays from the start.
func JumpTo(path string, spec Spec, snapDir string, step uint64, console io.Writer) (*Session, error) {
	best := uint64(0)
	if entries, err := os.ReadDir(snapDir); err == nil {
		for _, e := range entries {
			var n uint64
			if _, err := fmt.Sscanf(e.Name(), "%d.rvsnap", &n); err == nil && n <= step && n > best {
				best = n
			}
		}
	}
	var s *Session
	if best == 0 {
		var err error
		if s, err = StartReplay(path, spec, console); err != nil {
			return nil, err
		}
	} else {
		var vcfg vm.Config
		var err error
		if s, vcfg, err = openRecording(path, spec, console); err != nil {
			return nil, err
		}
		snap, err := snapshot.LoadFile(filepath.Join(snapDir, fmt.Sprintf("%d.rvsnap", best)))
		if err != nil {
			return nil, err
		}
		diskPath := ""
		if snap.Config.Disk != nil {
			f, err := os.CreateTemp("", "rvsim-disk-*.img")
			if err != nil {
				return nil, err
			}
			diskPath = f.Name()
			f.Close()
		}
		v, disk, err := snap.Restore(vcfg, diskPath)
		if err != nil {
			if diskPath != "" {
				os.Remove(diskPath)
			}
			return nil, err
		}
		s.VM, s.disk, s.diskPath = v, disk, diskPath
		for s.next < len(s.ckpts) && s.ckpts[s.next].Step <= best {
			s.next++
		}
	}
	if step > s.VM.Now() {
		if _, err := s.VM.Run(step - s.VM.Now()); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// verify compares a replay checkpoint with the recorded one.
func (s *Session) verify(step uint64) error {
	if s.next >= len(s.ckpts) {
		if s.end != nil {
			return fmt.Errorf("replay: checkpoint at step %d, but the recording has no more", step)
		}
		return nil // truncated recording: nothing left to compare
	}
	want := s.ckpts[s.next]
	if want.Step != step {
		return fmt.Errorf("replay: checkpoint at step %d, recorded at step %d", step, want.Step)
	}
	got, err := s.VM.StateHashes()
	if err != nil {
		return err
	}
	last := uint64(0)
	if s.next > 0 {
		last = s.ckpts[s.next-1].Step
	}
	if got != want.Hashes || s.VM.Instret() != want.Instret {
		return &Divergence{Checkpoint: s.next, Step: step, LastGoodStep: last, Want: want.Hashes, Got: got}
	}
	s.next++
	return nil
}

// SnapshotEvery makes Replay save a snapshot of the VM every n steps into
// dir, as <step>.rvsnap, for JumpTo.
func (s *Session) SnapshotEvery(n uint64, dir string) {
	s.snapEvery, s.snapDir = n, dir
}

// run runs to step target, saving snapshots on the way if asked to.
func (s *Session) run(target uint64) (vm.Result, error) {
	for {
		stop := target
		if s.snapEvery != 0 {
			stop = min(stop, (s.VM.Now()/s.snapEvery+1)*s.snapEvery)
		}
		res, err := s.VM.Run(stop - s.VM.Now())
		if err != nil || res.Exited || res.Steps >= target {
			return res, err
		}
		if s.snapEvery != 0 && res.Steps%s.snapEvery == 0 {
			p := filepath.Join(s.snapDir, fmt.Sprintf("%d.rvsnap", res.Steps))
			if err := snapshot.SaveFile(p, s.VM); err != nil {
				return res, err
			}
		}
	}
}

// Replay runs the whole recording, checking every checkpoint and the end.
func (s *Session) Replay() (Report, error) {
	var rep Report
	target := uint64(0)
	switch {
	case s.end != nil:
		target = s.end.Step
		if s.end.Reason == EndExit || s.end.Reason == EndReset {
			target++ // let the guest's exit end the run
		}
	case len(s.ckpts) > 0:
		target = s.ckpts[len(s.ckpts)-1].Step
		rep.Truncated = true
	default:
		return rep, errors.New("replay: the recording has no checkpoints and no end")
	}
	res, err := s.run(target)
	if err != nil {
		return rep, err
	}
	rep.Checkpoints, rep.Steps, rep.Instret = s.next, res.Steps, res.Instret
	if s.end == nil {
		return rep, nil
	}
	rep.End = s.end.Reason
	exited := s.end.Reason == EndExit || s.end.Reason == EndReset
	if res.Exited != exited || res.Steps != s.end.Step || (exited && res.ExitCode != s.end.Code) ||
		res.Reset != (s.end.Reason == EndReset) {
		return rep, fmt.Errorf("replay: ended at step %d (exited %v, code %d), recording ended at step %d (%s, code %d)",
			res.Steps, res.Exited, res.ExitCode, s.end.Step, s.end.Reason, s.end.Code)
	}
	if s.next != len(s.ckpts) {
		return rep, fmt.Errorf("replay: checked %d of %d checkpoints", s.next, len(s.ckpts))
	}
	got, err := s.VM.StateHashes()
	if err != nil {
		return rep, err
	}
	if got != s.end.Hashes || res.Instret != s.end.Instret {
		last := uint64(0)
		if len(s.ckpts) > 0 {
			last = s.ckpts[len(s.ckpts)-1].Step
		}
		return rep, &Divergence{Checkpoint: -1, Step: res.Steps, LastGoodStep: last, Want: s.end.Hashes, Got: got}
	}
	return rep, nil
}

// EndStep returns the step at which the recording at path ends: its END
// record, or for a truncated recording its last checkpoint.
func EndStep(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	_, recs, err := Read(f)
	if err != nil && !errors.Is(err, ErrTruncated) {
		return 0, err
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Tag == TagEnd || recs[i].Tag == TagCheckpoint {
			return recs[i].Step, nil
		}
	}
	return 0, errors.New("replay: the recording has no checkpoints and no end")
}
