// Package devices defines the interface memory-mapped devices implement and
// the bus that maps them into the guest physical address space.
//
// Devices are driven only from the run-loop goroutine (determinism rule 1),
// so they need no locking.
package devices

import (
	"fmt"
	"sort"
)

// Device is a memory-mapped device. Offsets are relative to its base
// address; size is the access width in bytes (1, 2, 4 or 8).
type Device interface {
	Read(offset uint64, size int) uint64
	Write(offset uint64, size int, value uint64)
	// Reset returns the device to its power-on state.
	Reset()
	// Snapshot serializes the device's state; Restore loads it back.
	Snapshot() []byte
	Restore(state []byte) error
}

// IRQLine is an interrupt input, typically one source of the PLIC.
// Lines are level-triggered.
type IRQLine interface {
	SetLevel(high bool)
}

// Region is one mapping on the bus.
type Region struct {
	Name   string
	Base   uint64
	Size   uint64
	Device Device
}

// Bus maps devices into the physical address space.
type Bus struct {
	regions []Region // sorted by Base, non-overlapping
}

// Map adds a device. Regions must be non-empty and must not overlap.
func (b *Bus) Map(name string, base, size uint64, d Device) error {
	if size == 0 || base+size < base {
		return fmt.Errorf("devices: %s: invalid region [%#x, +%#x)", name, base, size)
	}
	for _, r := range b.regions {
		if base < r.Base+r.Size && r.Base < base+size {
			return fmt.Errorf("devices: %s [%#x, %#x) overlaps %s [%#x, %#x)",
				name, base, base+size, r.Name, r.Base, r.Base+r.Size)
		}
	}
	b.regions = append(b.regions, Region{Name: name, Base: base, Size: size, Device: d})
	sort.Slice(b.regions, func(i, j int) bool { return b.regions[i].Base < b.regions[j].Base })
	return nil
}

// Lookup finds the device for an access of size bytes at addr. The whole
// access must fall inside one region.
func (b *Bus) Lookup(addr uint64, size int) (Device, uint64, bool) {
	r := b.Region(addr, size)
	if r == nil {
		return nil, 0, false
	}
	return r.Device, addr - r.Base, true
}

// Region returns the region an access of size bytes at addr falls in, or nil.
func (b *Bus) Region(addr uint64, size int) *Region {
	i := sort.Search(len(b.regions), func(i int) bool { return b.regions[i].Base+b.regions[i].Size > addr })
	if i == len(b.regions) {
		return nil
	}
	r := &b.regions[i]
	if addr < r.Base || addr-r.Base > r.Size-uint64(size) || uint64(size) > r.Size {
		return nil
	}
	return r
}

// Regions returns the mappings, sorted by base address.
func (b *Bus) Regions() []Region { return b.regions }

// Reset resets every mapped device, in address order.
func (b *Bus) Reset() {
	for _, r := range b.regions {
		r.Device.Reset()
	}
}
