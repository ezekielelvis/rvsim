// Package virtio implements the virtio-mmio transport, version 2 (Virtual
// I/O Device Version 1.2, §4.2), and split virtqueues (§2.7). Device types
// such as virtio-blk plug in as a Backend.
//
// Like every device, the transport is driven only from the run-loop
// goroutine. It touches guest memory only when a backend processes a queue,
// which a backend does from scheduled events (ADR-027).
package virtio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ezekielelvis/rvsim/vmm/devices"
)

// Size is the length of the register window; device configuration space
// starts at offset 0x100.
const Size = 0x1000

// Register offsets (§4.2.2).
const (
	regMagic          = 0x000
	regVersion        = 0x004
	regDeviceID       = 0x008
	regVendorID       = 0x00c
	regDeviceFeatures = 0x010
	regDeviceFeatSel  = 0x014
	regDriverFeatures = 0x020
	regDriverFeatSel  = 0x024
	regQueueSel       = 0x030
	regQueueNumMax    = 0x034
	regQueueNum       = 0x038
	regQueueReady     = 0x044
	regQueueNotify    = 0x050
	regIntStatus      = 0x060
	regIntACK         = 0x064
	regStatus         = 0x070
	regQueueDescLow   = 0x080
	regQueueDescHigh  = 0x084
	regQueueAvailLow  = 0x090
	regQueueAvailHigh = 0x094
	regQueueUsedLow   = 0x0a0
	regQueueUsedHigh  = 0x0a4
	regSHMLenLow      = 0x0b0
	regSHMLenHigh     = 0x0b4
	regConfigGen      = 0x0fc
	regConfig         = 0x100
)

const (
	magic    = 0x74726976 // "virt"
	version  = 2
	vendorID = 0x5256534d // "MSVR": rvsim
)

// Device status bits (§2.1).
const (
	StatusAcknowledge = 1
	StatusDriver      = 2
	StatusDriverOK    = 4
	StatusFeaturesOK  = 8
	StatusNeedsReset  = 64
	StatusFailed      = 128
)

// Interrupt status bits (§4.2.2).
const (
	IntUsedBuffer   = 1
	IntConfigChange = 2
)

// FeatureVersion1 (VIRTIO_F_VERSION_1) is required by the modern interface
// and always offered.
const FeatureVersion1 = 1 << 32

// Memory is guest physical memory as devices see it. Writes must go through
// WritePhys so the core can invalidate anything it caches.
type Memory interface {
	ReadPhys(addr uint64, b []byte) error
	WritePhys(addr uint64, b []byte) error
}

// Backend is a virtio device type behind the transport.
type Backend interface {
	DeviceID() uint32
	// Features returns the device-type feature bits offered to the driver.
	Features() uint64
	// QueueMax returns the maximum size of each of the device's queues.
	QueueMax() []uint16
	// ReadConfig reads size bytes of device configuration space.
	ReadConfig(offset uint64, size int) uint64
	// Notify reports that the driver made buffers available on queue q.
	Notify(q int)
	// Reset returns the backend to its initial state, abandoning any
	// requests in flight.
	Reset()
}

// mmioState is the transport's register state (fixed-size, for snapshots).
type mmioState struct {
	DeviceFeatSel uint32
	DriverFeatSel uint32
	DriverFeat    uint64
	QueueSel      uint32
	Status        uint32
	IntStatus     uint32
}

// MMIO is a virtio-mmio transport for one Backend.
type MMIO struct {
	be     Backend
	mem    Memory
	irq    devices.IRQLine
	s      mmioState
	queues []Queue
}

// NewMMIO returns a transport for be that reaches guest memory through mem
// and signals interrupts on irq.
func NewMMIO(be Backend, mem Memory, irq devices.IRQLine) *MMIO {
	t := &MMIO{be: be, mem: mem, irq: irq}
	for _, max := range be.QueueMax() {
		t.queues = append(t.queues, Queue{max: max, mem: mem})
	}
	return t
}

// Queue returns queue i.
func (t *MMIO) Queue(i int) *Queue { return &t.queues[i] }

// DriverOK reports whether the driver has finished initialization.
func (t *MMIO) DriverOK() bool {
	return t.s.Status&StatusDriverOK != 0 && t.s.Status&StatusNeedsReset == 0
}

// NegotiatedFeatures returns the features the driver accepted.
func (t *MMIO) NegotiatedFeatures() uint64 { return t.s.DriverFeat }

// Interrupt sends a used-buffer notification.
func (t *MMIO) Interrupt() { t.raise(IntUsedBuffer) }

// NeedsReset marks the device broken (§2.1.2), for example after a
// malformed descriptor chain, and notifies the driver.
func (t *MMIO) NeedsReset() {
	t.s.Status |= StatusNeedsReset
	if t.s.Status&StatusDriverOK != 0 {
		t.raise(IntConfigChange)
	}
}

func (t *MMIO) raise(bits uint32) {
	t.s.IntStatus |= bits
	t.irq.SetLevel(true)
}

func (t *MMIO) offered() uint64 { return FeatureVersion1 | t.be.Features() }

func (t *MMIO) selected() *Queue {
	if int(t.s.QueueSel) < len(t.queues) {
		return &t.queues[t.s.QueueSel]
	}
	return nil
}

// Read implements devices.Device. Registers are 32 bits wide; configuration
// space may be read at any width.
func (t *MMIO) Read(off uint64, size int) uint64 {
	if off >= regConfig {
		return t.be.ReadConfig(off-regConfig, size)
	}
	if size != 4 {
		return 0
	}
	q := t.selected()
	switch off {
	case regMagic:
		return magic
	case regVersion:
		return version
	case regDeviceID:
		return uint64(t.be.DeviceID())
	case regVendorID:
		return vendorID
	case regDeviceFeatures:
		if t.s.DeviceFeatSel < 2 {
			return (t.offered() >> (32 * t.s.DeviceFeatSel)) & 0xffff_ffff
		}
		return 0
	case regQueueNumMax:
		if q != nil {
			return uint64(q.max)
		}
	case regQueueNum:
		if q != nil {
			return uint64(q.s.Size)
		}
	case regQueueReady:
		if q != nil && q.s.Ready {
			return 1
		}
	case regIntStatus:
		return uint64(t.s.IntStatus)
	case regStatus:
		return uint64(t.s.Status)
	case regQueueDescLow, regQueueDescHigh, regQueueAvailLow, regQueueAvailHigh, regQueueUsedLow, regQueueUsedHigh:
		if q != nil {
			return (*q.addrReg(off) >> (32 * ((off >> 2) & 1))) & 0xffff_ffff
		}
	case regSHMLenLow, regSHMLenHigh:
		return 0xffff_ffff // no shared memory regions
	case regConfigGen:
		return 0 // configuration space never changes
	}
	return 0
}

// Write implements devices.Device.
func (t *MMIO) Write(off uint64, size int, v uint64) {
	if off >= regConfig || size != 4 {
		return // configuration space is read-only for the devices rvsim has
	}
	v32 := uint32(v)
	q := t.selected()
	switch off {
	case regDeviceFeatSel:
		t.s.DeviceFeatSel = v32
	case regDriverFeatSel:
		t.s.DriverFeatSel = v32
	case regDriverFeatures:
		if t.s.DriverFeatSel < 2 && t.s.Status&StatusFeaturesOK == 0 {
			shift := 32 * t.s.DriverFeatSel
			t.s.DriverFeat = t.s.DriverFeat&^(0xffff_ffff<<shift) | uint64(v32)<<shift
		}
	case regQueueSel:
		t.s.QueueSel = v32
	case regQueueNum:
		// The split ring size must be a power of two no larger than the
		// maximum (§2.7); anything else leaves the queue unusable.
		if q != nil && !q.s.Ready {
			q.s.Size = uint16(v32)
			if v32 == 0 || v32 > uint32(q.max) || v32&(v32-1) != 0 {
				q.s.Size = 0
			}
		}
	case regQueueReady:
		if q != nil {
			q.s.Ready = v32&1 != 0 && q.s.Size != 0
		}
	case regQueueNotify:
		if int(v32) < len(t.queues) && t.queues[v32].s.Ready && t.DriverOK() {
			t.be.Notify(int(v32))
		}
	case regIntACK:
		t.s.IntStatus &^= v32
		if t.s.IntStatus == 0 {
			t.irq.SetLevel(false)
		}
	case regStatus:
		t.writeStatus(v32)
	case regQueueDescLow, regQueueDescHigh, regQueueAvailLow, regQueueAvailHigh, regQueueUsedLow, regQueueUsedHigh:
		if q != nil && !q.s.Ready {
			reg, shift := q.addrReg(off), 32*((off>>2)&1)
			*reg = *reg&^(0xffff_ffff<<shift) | uint64(v32)<<shift
		}
	}
}

func (t *MMIO) writeStatus(v uint32) {
	if v == 0 {
		t.Reset()
		return
	}
	// FEATURES_OK sticks only if the driver accepted a subset of what was
	// offered, including VERSION_1 (§3.1.1, §6.1).
	if v&StatusFeaturesOK != 0 && t.s.Status&StatusFeaturesOK == 0 {
		if t.s.DriverFeat&^t.offered() != 0 || t.s.DriverFeat&FeatureVersion1 == 0 {
			v &^= StatusFeaturesOK
		}
	}
	t.s.Status = v | t.s.Status&StatusNeedsReset
}

// Reset implements devices.Device and the driver's status-0 reset (§2.4).
func (t *MMIO) Reset() {
	t.be.Reset()
	t.s = mmioState{}
	for i := range t.queues {
		t.queues[i].s = queueState{}
	}
	t.irq.SetLevel(false)
}

const snapshotVersion = 1

// Snapshot implements devices.Device: the transport and queue registers.
// Backend state (requests in flight) belongs to the backend.
func (t *MMIO) Snapshot() []byte {
	var b bytes.Buffer
	b.WriteByte(snapshotVersion)
	_ = binary.Write(&b, binary.LittleEndian, &t.s)
	for i := range t.queues {
		_ = binary.Write(&b, binary.LittleEndian, &t.queues[i].s)
	}
	return b.Bytes()
}

// Restore implements devices.Device.
func (t *MMIO) Restore(data []byte) error {
	if len(data) == 0 || data[0] != snapshotVersion {
		return errors.New("virtio: unknown snapshot version")
	}
	r := bytes.NewReader(data[1:])
	var s mmioState
	qs := make([]queueState, len(t.queues))
	if err := binary.Read(r, binary.LittleEndian, &s); err != nil {
		return fmt.Errorf("virtio: snapshot: %w", err)
	}
	for i := range qs {
		if err := binary.Read(r, binary.LittleEndian, &qs[i]); err != nil {
			return fmt.Errorf("virtio: snapshot: %w", err)
		}
	}
	if r.Len() != 0 {
		return errors.New("virtio: snapshot has trailing data")
	}
	t.s = s
	for i := range qs {
		t.queues[i].s = qs[i]
	}
	t.irq.SetLevel(t.s.IntStatus != 0)
	return nil
}
