// Package blk is a virtio block device (Virtual I/O Device Version 1.2,
// §5.2) backed by a host disk image.
//
// Requests complete in virtual time: a driver notification schedules one
// event Latency steps later, and that event performs every request then
// available and interrupts. No goroutine is involved, so completion timing
// depends only on guest behaviour (ADR-027).
package blk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ezekielelvis/rvsim/vmm/devices"
	"github.com/ezekielelvis/rvsim/vmm/devices/virtio"
	"github.com/ezekielelvis/rvsim/vmm/sched"
)

// SectorSize is the virtio-blk sector, independent of the image's own block
// size (§5.2.6).
const SectorSize = 512

// DeviceID is the virtio device ID for a block device (§5).
const DeviceID = 2

// QueueSize is the request queue's maximum size.
const QueueSize = 256

// DefaultLatency is the completion latency in steps: 50 µs at the nominal
// 100 M steps per second of the 10 MHz timebase (ADR-026).
const DefaultLatency = 5_000

// Feature bits (§5.2.3).
const (
	featSegMax = 1 << 2
	featRO     = 1 << 5
	featFlush  = 1 << 9
)

// Request types and status values (§5.2.6).
const (
	reqIn    = 0
	reqOut   = 1
	reqFlush = 4
	reqGetID = 8

	statusOK     = 0
	statusIOErr  = 1
	statusUnsupp = 2
)

const idLen = 20 // VIRTIO_BLK_ID_BYTES

// Disk is the backing store.
type Disk interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
}

// Config configures a block device.
type Config struct {
	Disk     Disk
	Size     uint64 // bytes; a multiple of SectorSize
	ReadOnly bool
	Latency  uint64 // steps from notification to completion; 0 means DefaultLatency
	Serial   string // returned by GET_ID, at most 20 bytes
}

// Block is a virtio-blk device on a virtio-mmio transport. It implements
// devices.Device.
type Block struct {
	*virtio.MMIO
	cfg   Config
	sched *sched.Scheduler
	self  sched.Target
	now   func() uint64

	pending bool   // a completion event is scheduled
	gen     uint64 // incremented on reset; stale events carry an old value

	chain virtio.Chain
	hdr   [16]byte
	buf   []byte
}

const chunk = 64 << 10 // bytes moved per disk access

// New returns a block device. Its completion events go through s at times
// relative to now().
func New(cfg Config, mem virtio.Memory, irq devices.IRQLine, s *sched.Scheduler, now func() uint64) (*Block, error) {
	if cfg.Disk == nil || cfg.Size == 0 || cfg.Size%SectorSize != 0 {
		return nil, fmt.Errorf("blk: disk size %d is not a positive multiple of %d", cfg.Size, SectorSize)
	}
	if len(cfg.Serial) > idLen {
		return nil, fmt.Errorf("blk: serial %q is longer than %d bytes", cfg.Serial, idLen)
	}
	if cfg.Latency == 0 {
		cfg.Latency = DefaultLatency
	}
	b := &Block{cfg: cfg, sched: s, now: now, buf: make([]byte, chunk)}
	b.MMIO = virtio.NewMMIO(backend{b}, mem, irq)
	b.self = s.Register("virtio-blk", b)
	return b, nil
}

// backend adapts Block to virtio.Backend without exposing the methods on
// Block, whose Reset is the device reset.
type backend struct{ b *Block }

func (backend) DeviceID() uint32 { return DeviceID }

func (be backend) Features() uint64 {
	f := uint64(featSegMax | featFlush)
	if be.b.cfg.ReadOnly {
		f |= featRO
	}
	return f
}

func (backend) QueueMax() []uint16 { return []uint16{QueueSize} }

// ReadConfig reads struct virtio_blk_config (§5.2.4): capacity in sectors
// and seg_max.
func (be backend) ReadConfig(off uint64, size int) uint64 {
	var cfg [16]byte
	binary.LittleEndian.PutUint64(cfg[0:], be.b.cfg.Size/SectorSize)
	binary.LittleEndian.PutUint32(cfg[12:], QueueSize-2) // seg_max
	if off+uint64(size) > uint64(len(cfg)) || size > 8 {
		return 0
	}
	var v [8]byte
	copy(v[:], cfg[off:off+uint64(size)])
	return binary.LittleEndian.Uint64(v[:])
}

func (be backend) Notify(int) {
	b := be.b
	if !b.pending {
		b.pending = true
		b.sched.Schedule(b.now()+b.cfg.Latency, b.self, 0, b.gen)
	}
}

func (be backend) Reset() {
	be.b.pending = false
	be.b.gen++
}

// HandleEvent completes every available request (sched.Handler).
func (b *Block) HandleEvent(_ uint32, gen uint64) {
	if gen != b.gen {
		return // the driver reset the device after scheduling
	}
	b.pending = false
	q := b.Queue(0)
	done := false
	for {
		ok, err := q.Pop(&b.chain)
		if err == nil && ok {
			var written uint32
			if written, err = b.handle(&b.chain); err == nil {
				err = q.Push(b.chain.Head, written)
			}
		}
		if err != nil {
			b.NeedsReset()
			return
		}
		if !ok {
			break
		}
		done = true
	}
	if done {
		if quiet, err := q.InterruptSuppressed(); err != nil {
			b.NeedsReset()
		} else if !quiet {
			b.Interrupt()
		}
	}
}

// handle performs one request and returns the bytes written to the chain.
func (b *Block) handle(c *virtio.Chain) (uint32, error) {
	if c.ReadLen < uint64(len(b.hdr)) || c.WriteLen < 1 {
		return 0, fmt.Errorf("%w: request without header or status", virtio.ErrMalformed)
	}
	if err := c.ReadAt(0, b.hdr[:]); err != nil {
		return 0, err
	}
	typ := binary.LittleEndian.Uint32(b.hdr[0:])
	sector := binary.LittleEndian.Uint64(b.hdr[8:])
	in := c.WriteLen - 1 // device-writable data bytes before the status byte
	out := c.ReadLen - uint64(len(b.hdr))

	var written uint64
	status := byte(statusOK)
	var err error
	switch typ {
	case reqIn:
		if err = b.checkRange(sector, in); err == nil {
			err = b.copyIn(c, sector*SectorSize, in)
			written = in
		}
	case reqOut:
		if b.cfg.ReadOnly {
			err = errors.New("read-only")
		} else if err = b.checkRange(sector, out); err == nil {
			err = b.copyOut(c, sector*SectorSize, out)
		}
	case reqFlush:
		err = b.cfg.Disk.Sync()
	case reqGetID:
		var id [idLen]byte
		copy(id[:], b.cfg.Serial)
		written = min(in, idLen)
		err = c.WriteAt(0, id[:written])
	default:
		status = statusUnsupp
	}
	if errors.Is(err, virtio.ErrMalformed) {
		return 0, err
	}
	if err != nil {
		status = statusIOErr
	}
	b.hdr[0] = status
	if err := c.WriteAt(in, b.hdr[:1]); err != nil {
		return 0, err
	}
	return uint32(written + 1), nil
}

func (b *Block) checkRange(sector, n uint64) error {
	if n%SectorSize != 0 || sector > b.cfg.Size/SectorSize || n > b.cfg.Size-sector*SectorSize {
		return fmt.Errorf("sectors [%d, +%d bytes) outside a %d-byte disk", sector, n, b.cfg.Size)
	}
	return nil
}

// copyIn reads n bytes of the disk at pos into the chain's writable part.
func (b *Block) copyIn(c *virtio.Chain, pos, n uint64) error {
	for off := uint64(0); off < n; {
		buf := b.buf[:min(n-off, chunk)]
		if _, err := b.cfg.Disk.ReadAt(buf, int64(pos+off)); err != nil {
			return err
		}
		if err := c.WriteAt(off, buf); err != nil {
			return err
		}
		off += uint64(len(buf))
	}
	return nil
}

// copyOut writes n bytes from the chain's readable data to the disk at pos.
func (b *Block) copyOut(c *virtio.Chain, pos, n uint64) error {
	for off := uint64(0); off < n; {
		buf := b.buf[:min(n-off, chunk)]
		if err := c.ReadAt(16+off, buf); err != nil {
			return err
		}
		if _, err := b.cfg.Disk.WriteAt(buf, int64(pos+off)); err != nil {
			return err
		}
		off += uint64(len(buf))
	}
	return nil
}

// Snapshot implements devices.Device: the transport plus whether a
// completion is pending. The pending event itself is scheduler state.
func (b *Block) Snapshot() []byte {
	var s bytes.Buffer
	s.WriteByte(1)
	_ = binary.Write(&s, binary.LittleEndian, struct {
		Pending bool
		Gen     uint64
	}{b.pending, b.gen})
	s.Write(b.MMIO.Snapshot())
	return s.Bytes()
}

// Restore implements devices.Device.
func (b *Block) Restore(data []byte) error {
	const n = 1 + 1 + 8
	if len(data) < n || data[0] != 1 {
		return errors.New("blk: unknown snapshot version")
	}
	pending, gen := data[1] != 0, binary.LittleEndian.Uint64(data[2:])
	if err := b.MMIO.Restore(data[n:]); err != nil {
		return err
	}
	b.pending, b.gen = pending, gen
	return nil
}
