package virtio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Descriptor flags (§2.7.5).
const (
	descNext     = 1
	descWrite    = 2
	descIndirect = 4
)

// availNoInterrupt is VIRTQ_AVAIL_F_NO_INTERRUPT (§2.7.7).
const availNoInterrupt = 1

// queueState is a queue's registers and progress (fixed-size, for
// snapshots).
type queueState struct {
	Size      uint16
	Ready     bool
	Desc      uint64
	Avail     uint64
	Used      uint64
	LastAvail uint16 // next available-ring entry to consume
	UsedIdx   uint16 // next used-ring entry to fill
}

// Queue is a split virtqueue.
type Queue struct {
	max uint16
	mem Memory
	s   queueState
	buf [16]byte
}

func (q *Queue) addrReg(off uint64) *uint64 {
	switch off &^ 4 {
	case regQueueDescLow:
		return &q.s.Desc
	case regQueueAvailLow:
		return &q.s.Avail
	default:
		return &q.s.Used
	}
}

// Ready reports whether the driver has enabled the queue.
func (q *Queue) Ready() bool { return q.s.Ready }

func (q *Queue) read16(addr uint64) (uint16, error) {
	if err := q.mem.ReadPhys(addr, q.buf[:2]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(q.buf[:2]), nil
}

func (q *Queue) write16(addr uint64, v uint16) error {
	binary.LittleEndian.PutUint16(q.buf[:2], v)
	return q.mem.WritePhys(addr, q.buf[:2])
}

// Segment is one guest buffer of a descriptor chain.
type Segment struct {
	Addr uint64
	Len  uint32
}

// Chain is a descriptor chain popped from a queue: device-readable segments
// followed by device-writable ones. Reuse a Chain across Pops to avoid
// allocation.
type Chain struct {
	Head     uint16
	Readable []Segment
	Writable []Segment
	ReadLen  uint64 // total bytes in Readable
	WriteLen uint64 // total bytes in Writable
	mem      Memory
}

// ErrMalformed reports a descriptor chain or ring that breaks the spec; the
// device should then need a reset.
var ErrMalformed = errors.New("virtio: malformed virtqueue")

// Pop takes the next available descriptor chain into c. It returns false
// when the driver has made nothing more available.
func (q *Queue) Pop(c *Chain) (bool, error) {
	if !q.s.Ready {
		return false, nil
	}
	idx, err := q.read16(q.s.Avail + 2)
	if err != nil {
		return false, err
	}
	if idx == q.s.LastAvail {
		return false, nil
	}
	if idx-q.s.LastAvail > q.s.Size {
		return false, fmt.Errorf("%w: %d buffers available on a queue of %d", ErrMalformed, idx-q.s.LastAvail, q.s.Size)
	}
	head, err := q.read16(q.s.Avail + 4 + 2*uint64(q.s.LastAvail%q.s.Size))
	if err != nil {
		return false, err
	}
	q.s.LastAvail++

	*c = Chain{Head: head, Readable: c.Readable[:0], Writable: c.Writable[:0], mem: q.mem}
	for i, n := head, uint16(0); ; n++ {
		if i >= q.s.Size || n >= q.s.Size {
			return false, fmt.Errorf("%w: descriptor %d (chain of %d) on a queue of %d", ErrMalformed, i, n, q.s.Size)
		}
		if err := q.mem.ReadPhys(q.s.Desc+16*uint64(i), q.buf[:]); err != nil {
			return false, err
		}
		seg := Segment{Addr: binary.LittleEndian.Uint64(q.buf[0:]), Len: binary.LittleEndian.Uint32(q.buf[8:])}
		flags := binary.LittleEndian.Uint16(q.buf[12:])
		next := binary.LittleEndian.Uint16(q.buf[14:])
		switch {
		case flags&descIndirect != 0:
			return false, fmt.Errorf("%w: indirect descriptor (not negotiated)", ErrMalformed)
		case flags&descWrite != 0:
			c.Writable = append(c.Writable, seg)
			c.WriteLen += uint64(seg.Len)
		case len(c.Writable) > 0:
			return false, fmt.Errorf("%w: device-readable descriptor after a writable one", ErrMalformed)
		default:
			c.Readable = append(c.Readable, seg)
			c.ReadLen += uint64(seg.Len)
		}
		if flags&descNext == 0 {
			return true, nil
		}
		i = next
	}
}

// Push returns a chain to the driver through the used ring, recording that
// the device wrote written bytes into it.
func (q *Queue) Push(head uint16, written uint32) error {
	var elem [8]byte
	binary.LittleEndian.PutUint32(elem[0:], uint32(head))
	binary.LittleEndian.PutUint32(elem[4:], written)
	if err := q.mem.WritePhys(q.s.Used+4+8*uint64(q.s.UsedIdx%q.s.Size), elem[:]); err != nil {
		return err
	}
	q.s.UsedIdx++
	return q.write16(q.s.Used+2, q.s.UsedIdx)
}

// InterruptSuppressed reports whether the driver asked not to be
// interrupted for used buffers on this queue.
func (q *Queue) InterruptSuppressed() (bool, error) {
	flags, err := q.read16(q.s.Avail)
	return flags&availNoInterrupt != 0, err
}

// ReadAt copies device-readable bytes starting at byte off of the chain.
func (c *Chain) ReadAt(off uint64, b []byte) error {
	return c.span(c.Readable, off, b, c.mem.ReadPhys)
}

// WriteAt copies b into the device-writable part of the chain at off.
func (c *Chain) WriteAt(off uint64, b []byte) error {
	return c.span(c.Writable, off, b, c.mem.WritePhys)
}

func (c *Chain) span(segs []Segment, off uint64, b []byte, f func(uint64, []byte) error) error {
	for _, s := range segs {
		if len(b) == 0 {
			return nil
		}
		if off >= uint64(s.Len) {
			off -= uint64(s.Len)
			continue
		}
		n := min(uint64(len(b)), uint64(s.Len)-off)
		if err := f(s.Addr+off, b[:n]); err != nil {
			return err
		}
		b, off = b[n:], 0
	}
	if len(b) != 0 {
		return fmt.Errorf("%w: access beyond the chain", ErrMalformed)
	}
	return nil
}
