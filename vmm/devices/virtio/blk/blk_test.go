package blk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/ezekielelvis/rvsim/vmm/devices/virtio"
	"github.com/ezekielelvis/rvsim/vmm/sched"
)

const (
	ramBase = 0x8000_0000
	qsize   = 8
	descAt  = ramBase + 0x1000
	availAt = ramBase + 0x2000
	usedAt  = ramBase + 0x3000
	bufAt   = ramBase + 0x10000 // request buffers from here
	latency = 100
)

type fakeMem struct{ b []byte }

func (m *fakeMem) span(addr uint64, n int) ([]byte, error) {
	if addr < ramBase || addr-ramBase+uint64(n) > uint64(len(m.b)) {
		return nil, fmt.Errorf("address %#x outside RAM", addr)
	}
	return m.b[addr-ramBase : addr-ramBase+uint64(n)], nil
}

func (m *fakeMem) ReadPhys(addr uint64, b []byte) error {
	s, err := m.span(addr, len(b))
	if err == nil {
		copy(b, s)
	}
	return err
}

func (m *fakeMem) WritePhys(addr uint64, b []byte) error {
	s, err := m.span(addr, len(b))
	if err == nil {
		copy(s, b)
	}
	return err
}

type memDisk struct {
	b     []byte
	syncs int
}

func (d *memDisk) ReadAt(p []byte, off int64) (int, error)  { return copy(p, d.b[off:]), nil }
func (d *memDisk) WriteAt(p []byte, off int64) (int, error) { return copy(d.b[off:], p), nil }
func (d *memDisk) Sync() error                              { d.syncs++; return nil }

type irqLine struct{ high bool }

func (l *irqLine) SetLevel(h bool) { l.high = h }

// rig is a block device with a test driver.
type rig struct {
	t     *testing.T
	b     *Block
	mem   *fakeMem
	disk  *memDisk
	irq   *irqLine
	sched *sched.Scheduler
	now   uint64
	avail uint16 // driver's avail idx
	next  uint16 // next free descriptor
	bufs  uint64 // next free buffer address
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	r := &rig{t: t, mem: &fakeMem{make([]byte, 1<<20)}, disk: &memDisk{b: make([]byte, 64*SectorSize)},
		irq: &irqLine{}, sched: &sched.Scheduler{}, bufs: bufAt}
	if cfg.Disk == nil {
		cfg.Disk, cfg.Size = r.disk, uint64(len(r.disk.b))
	}
	if cfg.Latency == 0 {
		cfg.Latency = latency
	}
	b, err := New(cfg, r.mem, r.irq, r.sched, func() uint64 { return r.now })
	if err != nil {
		t.Fatal(err)
	}
	r.b = b
	return r
}

func (r *rig) reg(off uint64) uint32       { return uint32(r.b.Read(off, 4)) }
func (r *rig) setReg(off uint64, v uint32) { r.b.Write(off, 4, uint64(v)) }

// initDriver performs the §3.1.1 initialization sequence.
func (r *rig) initDriver(features uint64) {
	r.setReg(0x070, virtio.StatusAcknowledge)
	r.setReg(0x070, virtio.StatusAcknowledge|virtio.StatusDriver)
	for sel := range uint32(2) {
		r.setReg(0x024, sel)
		r.setReg(0x020, uint32(features>>(32*sel)))
	}
	r.setReg(0x070, virtio.StatusAcknowledge|virtio.StatusDriver|virtio.StatusFeaturesOK)
	if r.reg(0x070)&virtio.StatusFeaturesOK == 0 {
		r.t.Fatal("device rejected the features")
	}
	r.setReg(0x030, 0)
	r.setReg(0x038, qsize)
	for _, a := range []struct {
		lo   uint64
		addr uint64
	}{{0x080, descAt}, {0x090, availAt}, {0x0a0, usedAt}} {
		r.setReg(a.lo, uint32(a.addr))
		r.setReg(a.lo+4, uint32(a.addr>>32))
	}
	r.setReg(0x044, 1)
	r.setReg(0x070, virtio.StatusAcknowledge|virtio.StatusDriver|virtio.StatusFeaturesOK|virtio.StatusDriverOK)
}

const allFeatures = virtio.FeatureVersion1 | featSegMax | featFlush

type desc struct {
	len   uint32
	write bool
	data  []byte // initial contents
}

// submit places a chain of buffers on the queue, notifies, and returns the
// buffers' addresses.
func (r *rig) submit(descs ...desc) []uint64 {
	r.t.Helper()
	head := r.next
	var addrs []uint64
	for i, d := range descs {
		idx := r.next
		r.next = (r.next + 1) % qsize
		addr := r.bufs
		r.bufs += uint64(d.len+15) &^ 15
		addrs = append(addrs, addr)
		if d.data != nil {
			_ = r.mem.WritePhys(addr, d.data)
		}
		var e [16]byte
		binary.LittleEndian.PutUint64(e[0:], addr)
		binary.LittleEndian.PutUint32(e[8:], d.len)
		var flags uint16
		if d.write {
			flags |= 2
		}
		if i < len(descs)-1 {
			flags |= 1
			binary.LittleEndian.PutUint16(e[14:], r.next)
		}
		binary.LittleEndian.PutUint16(e[12:], flags)
		_ = r.mem.WritePhys(descAt+16*uint64(idx), e[:])
	}
	r.put16(availAt+4+2*uint64(r.avail%qsize), head)
	r.avail++
	r.put16(availAt+2, r.avail)
	r.setReg(0x050, 0)
	return addrs
}

func (r *rig) put16(addr uint64, v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	_ = r.mem.WritePhys(addr, b[:])
}

func (r *rig) get(addr uint64, n int) []byte {
	b := make([]byte, n)
	if err := r.mem.ReadPhys(addr, b); err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *rig) usedIdx() uint16 { return binary.LittleEndian.Uint16(r.get(usedAt+2, 2)) }

func (r *rig) usedElem(i uint16) (id, length uint32) {
	e := r.get(usedAt+4+8*uint64(i%qsize), 8)
	return binary.LittleEndian.Uint32(e), binary.LittleEndian.Uint32(e[4:])
}

// advance moves virtual time forward and fires due events.
func (r *rig) advance(steps uint64) {
	r.now += steps
	r.sched.RunDue(r.now)
}

func header(typ uint32, sector uint64) []byte {
	h := make([]byte, 16)
	binary.LittleEndian.PutUint32(h, typ)
	binary.LittleEndian.PutUint64(h[8:], sector)
	return h
}

// request submits a header, data and status chain and completes it.
func (r *rig) request(typ uint32, sector uint64, data desc) (status byte, used uint32, dataAt uint64) {
	r.t.Helper()
	chain := []desc{{len: 16, data: header(typ, sector)}}
	if data.len > 0 {
		chain = append(chain, data)
	}
	chain = append(chain, desc{len: 1, write: true, data: []byte{0xff}})
	addrs := r.submit(chain...)
	before := r.usedIdx()
	r.advance(latency)
	if r.usedIdx() != before+1 {
		r.t.Fatalf("request not completed (used idx %d)", r.usedIdx())
	}
	_, used = r.usedElem(before)
	if data.len > 0 {
		dataAt = addrs[1]
	}
	return r.get(addrs[len(addrs)-1], 1)[0], used, dataAt
}

func TestIdentityAndConfig(t *testing.T) {
	r := newRig(t, Config{Serial: "x"})
	for _, c := range []struct {
		off  uint64
		want uint32
	}{{0x000, 0x74726976}, {0x004, 2}, {0x008, DeviceID}, {0x034, QueueSize}} {
		if got := r.reg(c.off); got != c.want {
			t.Errorf("reg %#x = %#x, want %#x", c.off, got, c.want)
		}
	}
	r.setReg(0x014, 0)
	if got := r.reg(0x010); got != featSegMax|featFlush {
		t.Errorf("features[0] = %#x", got)
	}
	r.setReg(0x014, 1)
	if got := r.reg(0x010); got != 1 {
		t.Errorf("features[1] = %#x (VERSION_1)", got)
	}
	if got := r.b.Read(0x100, 8); got != 64 {
		t.Errorf("capacity = %d sectors", got)
	}
	if got := r.b.Read(0x100, 4); got != 64 {
		t.Errorf("capacity (low word) = %d", got)
	}
	if got := r.b.Read(0x10c, 4); got != QueueSize-2 {
		t.Errorf("seg_max = %d", got)
	}
	r.setReg(0x030, 1)
	if got := r.reg(0x034); got != 0 {
		t.Errorf("queue 1 max = %d, want 0 (no such queue)", got)
	}
}

func TestFeatureNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features uint64
		ok       bool
	}{
		{"offered subset", virtio.FeatureVersion1 | featFlush, true},
		{"not offered", virtio.FeatureVersion1 | featRO, false},
		{"legacy driver", featFlush, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Config{})
			r.setReg(0x070, virtio.StatusAcknowledge|virtio.StatusDriver)
			r.setReg(0x024, 0)
			r.setReg(0x020, uint32(tc.features))
			r.setReg(0x024, 1)
			r.setReg(0x020, uint32(tc.features>>32))
			r.setReg(0x070, virtio.StatusAcknowledge|virtio.StatusDriver|virtio.StatusFeaturesOK)
			if got := r.reg(0x070)&virtio.StatusFeaturesOK != 0; got != tc.ok {
				t.Fatalf("FEATURES_OK = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestWriteThenReadCompletesAfterLatency(t *testing.T) {
	r := newRig(t, Config{})
	r.initDriver(allFeatures)
	payload := bytes.Repeat([]byte{0xa5, 0x5a}, SectorSize) // two sectors

	addrs := r.submit(desc{len: 16, data: header(reqOut, 3)}, desc{len: 2 * SectorSize, data: payload},
		desc{len: 1, write: true, data: []byte{0xff}})
	r.advance(latency - 1)
	if r.usedIdx() != 0 || r.irq.high {
		t.Fatal("completed before the latency elapsed")
	}
	r.advance(1)
	if r.usedIdx() != 1 || !r.irq.high {
		t.Fatalf("not completed at the latency: used %d, irq %v", r.usedIdx(), r.irq.high)
	}
	if id, n := r.usedElem(0); id != 0 || n != 1 {
		t.Errorf("used elem = (%d, %d), want (0, 1)", id, n)
	}
	if s := r.get(addrs[2], 1)[0]; s != statusOK {
		t.Errorf("status %d", s)
	}
	if !bytes.Equal(r.disk.b[3*SectorSize:5*SectorSize], payload) {
		t.Error("disk does not hold the written data")
	}
	if r.reg(0x060) != virtio.IntUsedBuffer {
		t.Errorf("interrupt status %#x", r.reg(0x060))
	}
	r.setReg(0x064, virtio.IntUsedBuffer)
	if r.irq.high || r.reg(0x060) != 0 {
		t.Error("ACK did not lower the interrupt")
	}

	status, used, at := r.request(reqIn, 3, desc{len: 2 * SectorSize, write: true})
	if status != statusOK || used != 2*SectorSize+1 {
		t.Fatalf("read: status %d, used %d", status, used)
	}
	if !bytes.Equal(r.get(at, 2*SectorSize), payload) {
		t.Error("read returned different data")
	}
}

// The device must not assume descriptor boundaries (§2.6.4): here the
// header and data share one descriptor, and data and status share another.
func TestAnyLayout(t *testing.T) {
	r := newRig(t, Config{})
	r.initDriver(allFeatures)
	copy(r.disk.b[SectorSize:], bytes.Repeat([]byte{7}, SectorSize))
	out := append(header(reqOut, 2), bytes.Repeat([]byte{9}, SectorSize)...)
	r.submit(desc{len: uint32(len(out)), data: out}, desc{len: 1, write: true})
	r.advance(latency)
	if r.disk.b[2*SectorSize] != 9 || r.disk.b[3*SectorSize-1] != 9 {
		t.Fatal("combined header+data write failed")
	}
	addrs := r.submit(desc{len: 16, data: header(reqIn, 1)}, desc{len: SectorSize + 1, write: true})
	r.advance(latency)
	got := r.get(addrs[1], SectorSize+1)
	if got[0] != 7 || got[SectorSize-1] != 7 || got[SectorSize] != statusOK {
		t.Fatalf("combined data+status read: % x ... % x", got[:4], got[SectorSize-2:])
	}
}

func TestRequestErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      Config
		typ      uint32
		sector   uint64
		data     desc
		want     byte
		wantUsed uint32
	}{
		{"read past the end", Config{}, reqIn, 63, desc{len: 2 * SectorSize, write: true}, statusIOErr, 1},
		{"huge sector", Config{}, reqIn, 1 << 60, desc{len: SectorSize, write: true}, statusIOErr, 1},
		{"partial sector", Config{}, reqOut, 0, desc{len: 100}, statusIOErr, 1},
		{"write to read-only disk", Config{ReadOnly: true}, reqOut, 0, desc{len: SectorSize}, statusIOErr, 1},
		{"unknown type", Config{}, 99, 0, desc{}, statusUnsupp, 1},
		{"get id", Config{Serial: "rvsim-disk"}, reqGetID, 0, desc{len: idLen, write: true}, statusOK, idLen + 1},
		{"flush", Config{}, reqFlush, 0, desc{}, statusOK, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.cfg)
			features := uint64(allFeatures)
			if tc.cfg.ReadOnly {
				features |= featRO
			}
			r.initDriver(features)
			status, used, at := r.request(tc.typ, tc.sector, tc.data)
			if status != tc.want || used != tc.wantUsed {
				t.Fatalf("status %d used %d, want %d and %d", status, used, tc.want, tc.wantUsed)
			}
			switch tc.typ {
			case reqGetID:
				if got := string(bytes.TrimRight(r.get(at, idLen), "\x00")); got != "rvsim-disk" {
					t.Errorf("id %q", got)
				}
			case reqFlush:
				if r.disk.syncs != 1 {
					t.Errorf("%d syncs", r.disk.syncs)
				}
			}
		})
	}
}

func TestOneEventCompletesEverythingAvailable(t *testing.T) {
	r := newRig(t, Config{})
	r.initDriver(allFeatures)
	for i := range 3 {
		r.submit(desc{len: 16, data: header(reqIn, uint64(i))}, desc{len: SectorSize + 1, write: true})
		r.advance(10)
	}
	if r.sched.Len() != 1 {
		t.Fatalf("%d events pending, want 1", r.sched.Len())
	}
	r.advance(latency)
	if r.usedIdx() != 3 {
		t.Fatalf("used idx %d, want 3", r.usedIdx())
	}
}

func TestNoInterruptFlagSuppressesInterrupt(t *testing.T) {
	r := newRig(t, Config{})
	r.initDriver(allFeatures)
	r.put16(availAt, 1) // VIRTQ_AVAIL_F_NO_INTERRUPT
	r.request(reqFlush, 0, desc{})
	if r.irq.high || r.reg(0x060) != 0 {
		t.Fatal("interrupted despite NO_INTERRUPT")
	}
}

func TestMalformedChainNeedsReset(t *testing.T) {
	r := newRig(t, Config{})
	r.initDriver(allFeatures)
	// A descriptor that chains to itself.
	var e [16]byte
	binary.LittleEndian.PutUint64(e[0:], bufAt)
	binary.LittleEndian.PutUint32(e[8:], 16)
	binary.LittleEndian.PutUint16(e[12:], 1) // NEXT
	binary.LittleEndian.PutUint16(e[14:], 0)
	_ = r.mem.WritePhys(descAt, e[:])
	r.put16(availAt+4, 0)
	r.put16(availAt+2, 1)
	r.setReg(0x050, 0)
	r.advance(latency)
	if r.reg(0x070)&virtio.StatusNeedsReset == 0 || r.reg(0x060) != virtio.IntConfigChange || !r.irq.high {
		t.Fatalf("status %#x, interrupt status %#x", r.reg(0x070), r.reg(0x060))
	}
	r.setReg(0x050, 0)
	if r.sched.Len() != 0 {
		t.Fatal("a broken device accepted a notification")
	}
	r.setReg(0x070, 0)
	if r.reg(0x070) != 0 || r.irq.high {
		t.Fatal("reset did not clear the device")
	}
}

func TestResetAbandonsPendingCompletion(t *testing.T) {
	r := newRig(t, Config{})
	r.initDriver(allFeatures)
	r.submit(desc{len: 16, data: header(reqFlush, 0)}, desc{len: 1, write: true})
	r.setReg(0x070, 0)
	r.advance(latency)
	if r.usedIdx() != 0 || r.irq.high || r.disk.syncs != 0 {
		t.Fatal("a request completed after the device was reset")
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	r := newRig(t, Config{})
	r.initDriver(allFeatures)
	r.request(reqFlush, 0, desc{})
	r.submit(desc{len: 16, data: header(reqFlush, 0)}, desc{len: 1, write: true}) // in flight
	snap := r.b.Snapshot()

	irq2 := &irqLine{}
	b2, err := New(Config{Disk: r.disk, Size: uint64(len(r.disk.b)), Latency: latency}, r.mem, irq2, &sched.Scheduler{},
		func() uint64 { return r.now })
	if err != nil {
		t.Fatal(err)
	}
	if err := b2.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b2.Snapshot(), snap) {
		t.Fatal("restored state serializes differently")
	}
	if !irq2.high {
		t.Error("restore did not reassert the pending interrupt")
	}
	if err := b2.Restore(snap[:len(snap)-1]); err == nil {
		t.Error("truncated snapshot accepted")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for _, cfg := range []Config{
		{Disk: &memDisk{}, Size: 0},
		{Disk: &memDisk{}, Size: 1000},
		{Disk: &memDisk{}, Size: 512, Serial: "a serial longer than twenty bytes"},
		{Size: 512},
	} {
		if _, err := New(cfg, &fakeMem{}, &irqLine{}, &sched.Scheduler{}, func() uint64 { return 0 }); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
}
