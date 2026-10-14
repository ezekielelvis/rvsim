// Package snapshot saves a whole VM to a file and restores it (ADR-029).
//
// File format (.rvsnap), little-endian:
//
//	"RVSIMSNP" | version u16 | reserved u16
//	section*:  tag [4]byte | length u64 | data
//	trailer:   XXH3-64 of every byte before it
//
// Sections, in order: CONF (JSON: machine and disk configuration), CORE (the
// core's rv_core_snapshot blob), RAM (paged), VMST (JSON: devices, events,
// run-loop input state) and, with a disk, DISK (paged). A paged section is
// size u64 | page size u32 | a bitmap of non-zero pages | the non-zero
// pages, flate-compressed: zero pages cost one bit.
package snapshot

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/zeebo/xxh3"

	"github.com/ezekielelvis/rvsim/vmm/devices/virtio/blk"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

const (
	magic    = "RVSIMSNP"
	version  = 1
	pageSize = 4096
)

// Config is the configuration a snapshot was taken with: restoring needs an
// identical machine.
type Config struct {
	Machine machine.Config `json:"machine"`
	Disk    *DiskConfig    `json:"disk,omitempty"`
}

// DiskConfig describes the virtio disk; its contents are the DISK section.
type DiskConfig struct {
	Size     uint64 `json:"size"`
	ReadOnly bool   `json:"read_only"`
	Latency  uint64 `json:"latency"`
	Serial   string `json:"serial"`
}

// Snapshot is a loaded snapshot file.
type Snapshot struct {
	Config Config
	core   []byte
	ram    paged
	state  *vm.State
	disk   *paged
}

// Steps returns the virtual time at which the snapshot was taken.
func (s *Snapshot) Steps() uint64 { return s.state.Now }

// paged is a sparse image: which pages are non-zero, and their contents.
type paged struct {
	size  uint64
	bits  []byte
	pages []byte // the non-zero pages, concatenated
}

func (p *paged) nonzero(i uint64) bool { return p.bits[i/8]&(1<<(i%8)) != 0 }

func makePaged(size uint64, readPage func(i uint64, buf []byte) error) (*paged, error) {
	n := (size + pageSize - 1) / pageSize
	p := &paged{size: size, bits: make([]byte, (n+7)/8)}
	buf := make([]byte, pageSize)
	for i := range n {
		page := buf[:min(pageSize, size-i*pageSize)]
		if err := readPage(i, page); err != nil {
			return nil, err
		}
		if !allZero(page) {
			p.bits[i/8] |= 1 << (i % 8)
			p.pages = append(p.pages, page...)
		}
	}
	return p, nil
}

func allZero(b []byte) bool {
	for len(b) >= 8 {
		if binary.LittleEndian.Uint64(b) != 0 {
			return false
		}
		b = b[8:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// each calls f with every non-zero page and its index.
func (p *paged) each(f func(i uint64, page []byte) error) error {
	off := uint64(0)
	for i := range (p.size + pageSize - 1) / pageSize {
		if !p.nonzero(i) {
			continue
		}
		n := min(pageSize, p.size-i*pageSize)
		if err := f(i, p.pages[off:off+n]); err != nil {
			return err
		}
		off += n
	}
	return nil
}

func (p *paged) encode() ([]byte, error) {
	var b bytes.Buffer
	var hdr [12]byte
	binary.LittleEndian.PutUint64(hdr[0:], p.size)
	binary.LittleEndian.PutUint32(hdr[8:], pageSize)
	b.Write(hdr[:])
	b.Write(p.bits)
	zw, err := flate.NewWriter(&b, flate.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(p.pages); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func decodePaged(data []byte) (*paged, error) {
	if len(data) < 12 || binary.LittleEndian.Uint32(data[8:]) != pageSize {
		return nil, errors.New("snapshot: bad paged section")
	}
	p := &paged{size: binary.LittleEndian.Uint64(data)}
	n := (p.size + pageSize - 1) / pageSize
	nb := (n + 7) / 8
	if uint64(len(data)-12) < nb {
		return nil, errors.New("snapshot: truncated page bitmap")
	}
	p.bits = data[12 : 12+nb]
	var want uint64
	for i := range n {
		if p.nonzero(i) {
			want += min(pageSize, p.size-i*pageSize)
		}
	}
	pages, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(data[12+nb:])), int64(want)+1))
	if err != nil {
		return nil, fmt.Errorf("snapshot: pages: %w", err)
	}
	if uint64(len(pages)) != want {
		return nil, fmt.Errorf("snapshot: %d bytes of pages, want %d", len(pages), want)
	}
	p.pages = pages
	return p, nil
}

// Save writes v to w. Call it between Run calls.
func Save(w io.Writer, v *vm.VM) error {
	st, err := v.SaveState()
	if err != nil {
		return err
	}
	vcfg := v.Config()
	cfg := Config{Machine: vcfg.Machine}
	m := v.Machine()
	ram, err := makePaged(vcfg.Machine.RAMSize, func(i uint64, page []byte) error {
		return m.ReadPhys(vcfg.Machine.RAMBase+i*pageSize, page)
	})
	if err != nil {
		return err
	}
	var disk *paged
	if d := v.Disk(); d != nil {
		cfg.Disk = &DiskConfig{Size: d.Size, ReadOnly: d.ReadOnly, Latency: d.Latency, Serial: d.Serial}
		disk, err = makePaged(d.Size, func(i uint64, page []byte) error {
			_, err := d.Disk.ReadAt(page, int64(i*pageSize))
			return err
		})
		if err != nil {
			return fmt.Errorf("snapshot: reading the disk: %w", err)
		}
	}
	core := st.Core
	st.Core = nil
	defer func() { st.Core = core }()

	h := xxh3.New()
	bw := bufio.NewWriter(io.MultiWriter(w, h))
	var pre [12]byte
	copy(pre[:], magic)
	binary.LittleEndian.PutUint16(pre[8:], version)
	bw.Write(pre[:])
	section := func(tag string, data []byte) {
		var hdr [12]byte
		copy(hdr[:4], tag)
		binary.LittleEndian.PutUint64(hdr[4:], uint64(len(data)))
		bw.Write(hdr[:])
		bw.Write(data)
	}
	cj, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	section("CONF", cj)
	section("CORE", core)
	rb, err := ram.encode()
	if err != nil {
		return err
	}
	section("RAM ", rb)
	sj, err := json.Marshal(st)
	if err != nil {
		return err
	}
	section("VMST", sj)
	if disk != nil {
		db, err := disk.encode()
		if err != nil {
			return err
		}
		section("DISK", db)
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	var sum [8]byte
	binary.LittleEndian.PutUint64(sum[:], h.Sum64())
	_, err = w.Write(sum[:])
	return err
}

// Load reads and verifies a snapshot.
func Load(r io.Reader) (*Snapshot, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(data) < 12+8 || string(data[:8]) != magic {
		return nil, errors.New("snapshot: not an rvsim snapshot")
	}
	if v := binary.LittleEndian.Uint16(data[8:]); v != version {
		return nil, fmt.Errorf("snapshot: format version %d, this rvsim reads %d", v, version)
	}
	body, sum := data[:len(data)-8], binary.LittleEndian.Uint64(data[len(data)-8:])
	if xxh3.Hash(body) != sum {
		return nil, errors.New("snapshot: checksum mismatch (the file is damaged)")
	}
	sections := map[string][]byte{}
	for rest := body[12:]; len(rest) > 0; {
		if len(rest) < 12 {
			return nil, errors.New("snapshot: truncated section header")
		}
		tag, n := string(rest[:4]), binary.LittleEndian.Uint64(rest[4:12])
		if n > uint64(len(rest)-12) {
			return nil, fmt.Errorf("snapshot: section %q overruns the file", tag)
		}
		sections[tag] = rest[12 : 12+n]
		rest = rest[12+n:]
	}
	s := &Snapshot{core: sections["CORE"], state: new(vm.State)}
	for _, tag := range []string{"CONF", "CORE", "RAM ", "VMST"} {
		if sections[tag] == nil {
			return nil, fmt.Errorf("snapshot: missing section %q", tag)
		}
	}
	if err := json.Unmarshal(sections["CONF"], &s.Config); err != nil {
		return nil, fmt.Errorf("snapshot: config: %w", err)
	}
	if err := json.Unmarshal(sections["VMST"], s.state); err != nil {
		return nil, fmt.Errorf("snapshot: VM state: %w", err)
	}
	ram, err := decodePaged(sections["RAM "])
	if err != nil {
		return nil, err
	}
	if ram.size != s.Config.Machine.RAMSize {
		return nil, errors.New("snapshot: RAM section does not match the configuration")
	}
	s.ram = *ram
	if s.Config.Disk != nil {
		if sections["DISK"] == nil {
			return nil, errors.New("snapshot: missing section \"DISK\"")
		}
		if s.disk, err = decodePaged(sections["DISK"]); err != nil {
			return nil, err
		}
		if s.disk.size != s.Config.Disk.Size {
			return nil, errors.New("snapshot: DISK section does not match the configuration")
		}
	}
	return s, nil
}

// LoadFile reads a snapshot file.
func LoadFile(path string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(bufio.NewReader(f))
}

// SaveFile writes v to path.
func SaveFile(path string, v *vm.VM) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := Save(f, v); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Restore creates a VM in the snapshot's state. rc supplies what a snapshot
// does not hold (console, input or replay script, checkpoints, trace); its
// Machine and Disk are ignored. With a disk, the saved contents are written
// to diskPath, which the restored VM then uses.
func (s *Snapshot) Restore(rc vm.Config, diskPath string) (*vm.VM, *os.File, error) {
	rc.Machine = s.Config.Machine
	rc.Disk = nil
	var f *os.File
	if d := s.Config.Disk; d != nil {
		if diskPath == "" {
			return nil, nil, errors.New("snapshot: restoring a disk needs a path for it")
		}
		var err error
		if f, err = os.OpenFile(diskPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644); err != nil {
			return nil, nil, err
		}
		if err := f.Truncate(int64(d.Size)); err != nil {
			f.Close()
			return nil, nil, err
		}
		if err := s.disk.each(func(i uint64, page []byte) error {
			_, err := f.WriteAt(page, int64(i*pageSize))
			return err
		}); err != nil {
			f.Close()
			return nil, nil, err
		}
		rc.Disk = &blk.Config{Disk: f, Size: d.Size, ReadOnly: d.ReadOnly, Latency: d.Latency, Serial: d.Serial}
	}
	fail := func(err error) (*vm.VM, *os.File, error) {
		if f != nil {
			f.Close()
		}
		return nil, nil, err
	}
	v, err := vm.New(rc)
	if err != nil {
		return fail(err)
	}
	// A new machine's RAM is zero: only non-zero pages need writing.
	base := s.Config.Machine.RAMBase
	if err := s.ram.each(func(i uint64, page []byte) error {
		return v.Machine().WritePhys(base+i*pageSize, page)
	}); err != nil {
		v.Close()
		return fail(err)
	}
	st := *s.state
	st.Core = s.core
	if err := v.RestoreState(&st); err != nil {
		v.Close()
		return fail(err)
	}
	return v, f, nil
}
