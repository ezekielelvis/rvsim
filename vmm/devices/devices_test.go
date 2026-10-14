package devices

import "testing"

type nopDevice struct{ id int }

func (nopDevice) Read(uint64, int) uint64   { return 0 }
func (nopDevice) Write(uint64, int, uint64) {}
func (nopDevice) Reset()                    {}
func (nopDevice) Snapshot() []byte          { return nil }
func (nopDevice) Restore([]byte) error      { return nil }

func TestBusMapRejectsBadRegions(t *testing.T) {
	var b Bus
	if err := b.Map("a", 0x1000, 0x100, nopDevice{}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		base, size uint64
	}{
		{"empty", 0x5000, 0},
		{"wraps", ^uint64(0) - 10, 0x100},
		{"overlaps start", 0x0f80, 0x100},
		{"overlaps end", 0x10ff, 0x10},
		{"inside", 0x1010, 0x10},
		{"covers", 0x0, 0x10000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := b.Map(tt.name, tt.base, tt.size, nopDevice{}); err == nil {
				t.Fatalf("Map(%#x, %#x) succeeded", tt.base, tt.size)
			}
		})
	}
}

func TestBusLookup(t *testing.T) {
	var b Bus
	uart, plic := nopDevice{1}, nopDevice{2}
	mustMap := func(name string, base, size uint64, d Device) {
		if err := b.Map(name, base, size, d); err != nil {
			t.Fatal(err)
		}
	}
	mustMap("plic", 0x0c00_0000, 0x400_0000, plic)
	mustMap("uart", 0x1000_0000, 0x100, uart)
	tests := []struct {
		name    string
		addr    uint64
		size    int
		want    Device
		wantOff uint64
		ok      bool
	}{
		{"uart first byte", 0x1000_0000, 1, uart, 0, true},
		{"uart last byte", 0x1000_00ff, 1, uart, 0xff, true},
		{"uart straddles end", 0x1000_00fe, 4, nil, 0, false},
		{"after uart", 0x1000_0100, 1, nil, 0, false},
		{"plic context", 0x0c20_0004, 4, plic, 0x20_0004, true},
		{"before everything", 0x100, 4, nil, 0, false},
		{"plic last byte", 0x0fff_ffff, 1, plic, 0x3ff_ffff, true},
		{"gap", 0x0800_0000, 1, nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, off, ok := b.Lookup(tt.addr, tt.size)
			if ok != tt.ok || d != tt.want || off != tt.wantOff {
				t.Fatalf("Lookup(%#x, %d) = %v, %#x, %v; want %v, %#x, %v", tt.addr, tt.size, d, off, ok, tt.want, tt.wantOff, tt.ok)
			}
		})
	}
	if r := b.Regions(); len(r) != 2 || r[0].Name != "plic" {
		t.Fatalf("Regions not sorted: %+v", r)
	}
}
