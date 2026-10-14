package gdb

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	. "github.com/ezekielelvis/rvsim/vmm/internal/rvasm"
	"github.com/ezekielelvis/rvsim/vmm/machine"
	"github.com/ezekielelvis/rvsim/vmm/vm"
)

const ramBase = 0x8000_0000

// client is a minimal RSP client.
type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (c *client) raw(b []byte) {
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) cmd(p string) string {
	c.t.Helper()
	c.raw([]byte(fmt.Sprintf("$%s#%02x", p, checksum(p))))
	return c.reply()
}

func (c *client) reply() string {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			c.t.Fatal(err)
		}
		if b == '+' {
			continue
		}
		if b != '$' {
			c.t.Fatalf("unexpected byte %q", b)
		}
		body, err := c.r.ReadString('#')
		if err != nil {
			c.t.Fatal(err)
		}
		c.r.Discard(2)
		c.raw([]byte("+"))
		return body[:len(body)-1]
	}
}

// counter: x5 counts up forever; x6 holds a marker; a data word at +0x1000.
var counter = Bytes(
	Addi(T1, Zero, 0x5a),
	// loop:
	Addi(T0, T0, 1),
	Addi(T0, T0, 1),
	Jal(Zero, -8),
)

func start(t *testing.T) (*client, *vm.VM, chan error) {
	t.Helper()
	cfg := machine.DefaultConfig()
	cfg.RAMSize = 1 << 20
	v, err := vm.New(vm.Config{Machine: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	if err := v.Machine().WritePhys(ramBase, counter); err != nil {
		t.Fatal(err)
	}
	if err := v.Machine().WritePhys(ramBase+0x1000, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	// A real TCP connection, as GDB uses: net.Pipe's unbuffered writes would
	// deadlock an ack against an interrupt.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	tgt := NewVMTarget(v)
	tgt.Chunk = 10_000
	go func() {
		b, err := ln.Accept()
		ln.Close()
		if err != nil {
			done <- err
			return
		}
		defer b.Close()
		_, err = Serve(b, tgt, nil)
		done <- err
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return &client{t: t, conn: a, r: bufio.NewReader(a)}, v, done
}

func TestHandshakeAndTargetDescription(t *testing.T) {
	c, _, _ := start(t)
	if r := c.cmd("qSupported:multiprocess+;swbreak+"); !strings.Contains(r, "qXfer:features:read+") ||
		!strings.Contains(r, "ReverseStep+") {
		t.Fatalf("qSupported: %q", r)
	}
	if r := c.cmd("?"); !strings.HasPrefix(r, "T05") {
		t.Fatalf("?: %q", r)
	}
	var xml strings.Builder
	for off := 0; ; {
		r := c.cmd(fmt.Sprintf("qXfer:features:read:target.xml:%x,200", off))
		xml.WriteString(r[1:])
		off += len(r) - 1
		if r[0] == 'l' {
			break
		}
	}
	for _, want := range []string{"riscv:rv64", `name="pc"`, `name="satp"`, `regnum="449"`, `name="priv"`} {
		if !strings.Contains(xml.String(), want) {
			t.Errorf("target.xml lacks %s", want)
		}
	}
	if r := c.cmd("vMustReplyEmpty"); r != "" {
		t.Errorf("unknown packet: %q", r)
	}
}

func TestRegistersAndMemory(t *testing.T) {
	c, v, _ := start(t)
	c.cmd("s")
	g := c.cmd("g")
	if len(g) != 33*16 {
		t.Fatalf("g: %d hex digits", len(g))
	}
	if g[6*16:6*16+2] != "5a" {
		t.Errorf("t1 (x6) = %s", g[6*16:7*16])
	}
	if pc := g[32*16:]; pc != "0400008000000000" {
		t.Errorf("pc = %s", pc)
	}
	// CSR (misa, 65+0x301) and priv (4161).
	if r := c.cmd(fmt.Sprintf("p%x", 65+0x301)); r[len(r)-2:] != "80" {
		t.Errorf("misa = %s (MXL=2 means 64-bit)", r)
	}
	if r := c.cmd(fmt.Sprintf("p%x", 4161)); r != "0300000000000000" {
		t.Errorf("priv = %s", r)
	}
	if r := c.cmd("P5=2a00000000000000"); r != "OK" {
		t.Fatalf("P: %s", r)
	}
	if x5, _ := v.Machine().Reg(5); x5 != 42 {
		t.Errorf("x5 = %d after P", x5)
	}
	if r := c.cmd(fmt.Sprintf("m%x,4", ramBase+0x1000)); r != "01020304" {
		t.Errorf("m: %s", r)
	}
	if r := c.cmd(fmt.Sprintf("M%x,2:abcd", ramBase+0x1001)); r != "OK" {
		t.Fatalf("M: %s", r)
	}
	if r := c.cmd(fmt.Sprintf("m%x,4", ramBase+0x1000)); r != "01abcd04" {
		t.Errorf("after M: %s", r)
	}
	if r := c.cmd("m100,4"); !strings.HasPrefix(r, "E") {
		t.Errorf("reading unmapped memory: %q", r)
	}
}

func TestBreakpointContinueStepAndInterrupt(t *testing.T) {
	c, v, done := start(t)
	if r := c.cmd(fmt.Sprintf("Z0,%x,4", ramBase+8)); r != "OK" {
		t.Fatalf("Z0: %s", r)
	}
	if r := c.cmd("c"); r != "T05swbreak:;thread:1;" {
		t.Fatalf("c: %q", r)
	}
	if pc, _ := v.Machine().Reg(machine.RegPC); pc != ramBase+8 {
		t.Fatalf("stopped at %#x", pc)
	}
	// Continuing runs the loop once more and stops at the same place.
	before, _ := v.Machine().Reg(5)
	c.cmd("vCont;c")
	after, _ := v.Machine().Reg(5)
	if after != before+2 {
		t.Errorf("x5 went from %d to %d around the loop", before, after)
	}
	if r := c.cmd("s"); r != "T05thread:1;" {
		t.Fatalf("s: %q", r)
	}
	if r := c.cmd(fmt.Sprintf("z0,%x,4", ramBase+8)); r != "OK" {
		t.Fatalf("z0: %s", r)
	}
	// Now the loop runs forever; Ctrl-C stops it.
	c.raw([]byte(fmt.Sprintf("$c#%02x", checksum("c"))))
	time.Sleep(50 * time.Millisecond)
	c.raw([]byte{0x03})
	if r := c.reply(); r != "T02thread:1;" {
		t.Fatalf("after Ctrl-C: %q", r)
	}
	if r := c.cmd("bs"); r != "E01" {
		t.Errorf("reverse step on a live VM: %q", r)
	}
	if r := c.cmd("D"); r != "OK" {
		t.Fatalf("D: %q", r)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
