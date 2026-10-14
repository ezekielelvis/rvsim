package fdt

import (
	"bytes"
	"encoding/binary"
	"os/exec"
	"strings"
	"testing"
)

func sample() *Node {
	root := NewRoot()
	root.U32("#address-cells", 2).U32("#size-cells", 2).String("model", "test")
	cpus := root.Child("cpus")
	cpus.U32("timebase-frequency", 10_000_000)
	cpu := cpus.Child("cpu@0")
	cpu.String("device_type", "cpu").Strings("riscv,isa-extensions", "i", "m", "a")
	cpu.Child("interrupt-controller").Empty("interrupt-controller").U32("phandle", 1)
	root.Child("memory@80000000").String("device_type", "memory").U64("reg", 0x8000_0000, 0x1000_0000)
	return root
}

// decode walks a blob and renders "path prop=len" lines, checking the format.
func decode(t *testing.T, blob []byte) []string {
	t.Helper()
	be := binary.BigEndian
	if be.Uint32(blob[0:]) != magic || int(be.Uint32(blob[4:])) != len(blob) {
		t.Fatal("bad header")
	}
	structOff, stringsOff := be.Uint32(blob[8:]), be.Uint32(blob[12:])
	str := func(off uint32) string {
		s := blob[stringsOff+off:]
		return string(s[:bytes.IndexByte(s, 0)])
	}
	var out, path []string
	for p := structOff; ; {
		tok := be.Uint32(blob[p:])
		p += 4
		switch tok {
		case tokBeginNode:
			end := p + uint32(bytes.IndexByte(blob[p:], 0))
			path = append(path, string(blob[p:end]))
			p = (end + 4) &^ 3
		case tokEndNode:
			path = path[:len(path)-1]
		case tokProp:
			ln, nameOff := be.Uint32(blob[p:]), be.Uint32(blob[p+4:])
			out = append(out, strings.Join(path, "/")+" "+str(nameOff))
			p = (p + 8 + ln + 3) &^ 3
		case tokEnd:
			if len(path) != 0 {
				t.Fatal("unbalanced nodes")
			}
			return out
		default:
			t.Fatalf("bad token %d at %d", tok, p-4)
		}
	}
}

func TestStructure(t *testing.T) {
	blob, err := sample().Blob()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(decode(t, blob), "\n")
	want := strings.Join([]string{
		" #address-cells", " #size-cells", " model",
		"/cpus timebase-frequency",
		"/cpus/cpu@0 device_type", "/cpus/cpu@0 riscv,isa-extensions",
		"/cpus/cpu@0/interrupt-controller interrupt-controller", "/cpus/cpu@0/interrupt-controller phandle",
		"/memory@80000000 device_type", "/memory@80000000 reg",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestDeterministic(t *testing.T) {
	a, _ := sample().Blob()
	b, _ := sample().Blob()
	if !bytes.Equal(a, b) {
		t.Fatal("two identical trees serialized differently")
	}
}

func TestSettingAPropertyTwiceReplacesIt(t *testing.T) {
	n := NewRoot()
	n.U32("x", 1).U32("x", 2)
	if len(n.props) != 1 || binary.BigEndian.Uint32(n.props[0].value) != 2 {
		t.Fatalf("props = %+v", n.props)
	}
}

func TestUnnamedChildIsAnError(t *testing.T) {
	r := NewRoot()
	r.Child("")
	if _, err := r.Blob(); err == nil {
		t.Fatal("Blob accepted an unnamed node")
	}
}

// dtc is the reference decoder (Phase 6 requires validating with it).
func TestDtcAcceptsBlob(t *testing.T) {
	if _, err := exec.LookPath("dtc"); err != nil {
		t.Skip("dtc not installed")
	}
	blob, _ := sample().Blob()
	cmd := exec.Command("dtc", "-I", "dtb", "-O", "dts", "-")
	cmd.Stdin = bytes.NewReader(blob)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dtc: %v\n%s", err, out)
	}
	for _, want := range []string{
		`riscv,isa-extensions = "i", "m", "a";`,
		"reg = <0x00 0x80000000 0x00 0x10000000>;",
		"timebase-frequency = <0x989680>;",
		"interrupt-controller;",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("dtc output lacks %q:\n%s", want, out)
		}
	}
}
