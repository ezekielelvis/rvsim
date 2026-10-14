package finisher

import "testing"

func TestWrites(t *testing.T) {
	type call struct {
		a    Action
		code uint64
	}
	for _, tc := range []struct {
		name   string
		offset uint64
		size   int
		v      uint64
		want   []call
	}{
		{"pass", 0, 4, 0x5555, []call{{PowerOff, 0}}},
		{"fail with code", 0, 4, 0x002a_3333, []call{{PowerOff, 42}}},
		{"reset", 0, 4, 0x7777, []call{{Reset, 0}}},
		{"upper bits of pass are ignored", 0, 4, 0x1234_5555, []call{{PowerOff, 0}}},
		{"unknown status", 0, 4, 0x1111, nil},
		{"wrong offset", 4, 4, 0x5555, nil},
		{"byte write", 0, 1, 0x55, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []call
			f := New(func(a Action, code uint64) { got = append(got, call{a, code}) })
			f.Write(tc.offset, tc.size, tc.v)
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Fatalf("calls = %v, want %v", got, tc.want)
			}
			if r := f.Read(0, 4); r != 0 {
				t.Fatalf("read = %#x", r)
			}
		})
	}
}
