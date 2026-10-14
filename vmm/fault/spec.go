// Package fault describes faults to inject into a VM (ADR-031): a YAML spec
// of triggers and actions, and the concrete record of each fault applied,
// which recordings carry so that a faulty run replays exactly.
package fault

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Spec is a fault-injection plan.
type Spec struct {
	// Seed seeds the random number generator used by probability triggers
	// and random values. The same seed, guest and input give the same faults.
	Seed   uint64  `yaml:"seed"`
	Faults []Fault `yaml:"faults"`
}

// Fault is one rule: when the trigger fires, do the action, up to Times
// times (default 1; 0 with a probability trigger means unlimited).
type Fault struct {
	Name  string  `yaml:"name"`
	When  Trigger `yaml:"when"`
	Do    Action  `yaml:"do"`
	Times *int    `yaml:"times"`
}

// Trigger says when a fault fires. Exactly one field is set.
type Trigger struct {
	Step    *uint64     `yaml:"step"`    // at virtual time N (steps)
	Instret *uint64     `yaml:"instret"` // when N instructions have retired
	PC      *PCTrigger  `yaml:"pc"`      // the Nth time the hart is about to execute pc
	Access  *AccessTrig `yaml:"access"`  // the Nth MMIO access to a device or address
	Chance  *ChanceTrig `yaml:"chance"`  // each event of a kind, with probability P
}

// PCTrigger fires the Hit-th time (default 1) execution reaches Addr.
type PCTrigger struct {
	Addr uint64 `yaml:"addr"`
	Hit  uint64 `yaml:"hit"`
}

// AccessTrig fires on the Nth (default 1) MMIO access to Device (by name:
// uart, plic, finisher, virtio-blk) or to Addr, of Kind read, write or any.
type AccessTrig struct {
	Device string `yaml:"device"`
	Addr   uint64 `yaml:"addr"`
	Nth    uint64 `yaml:"nth"`
	Kind   string `yaml:"kind"`
}

// ChanceTrig fires on each event of the action's kind with probability P.
type ChanceTrig struct {
	P float64 `yaml:"p"`
}

// Action is what a fault does. Exactly one field is set.
type Action struct {
	FlipBit  *FlipBit  `yaml:"flip_bit"`  // flip bit Bit of the byte at physical Addr
	Reg      *RegFault `yaml:"reg"`       // corrupt a register
	MMIORead *MMIORead `yaml:"mmio_read"` // the next MMIO read returns Value (or random)
	IRQ      *IRQFault `yaml:"irq"`       // drop or delay the next interrupt from Source
	Blk      *BlkFault `yaml:"blk"`       // fail or corrupt the next virtio-blk request
	UART     *UARTFlt  `yaml:"uart"`      // drop or garble the next UART byte
}

// FlipBit flips one bit of RAM.
type FlipBit struct {
	Addr uint64 `yaml:"addr"`
	Bit  uint   `yaml:"bit"`
}

// RegFault sets a register (x0..x31 by number or ABI name, or pc) to Set,
// XORs it with Xor, or replaces it with a random value.
type RegFault struct {
	Reg    string  `yaml:"reg"`
	Set    *uint64 `yaml:"set"`
	Xor    uint64  `yaml:"xor"`
	Random bool    `yaml:"random"`
}

// MMIORead overrides the value of an MMIO read (the device is still read).
// With an access trigger it applies to that access; otherwise to the next
// read matching Device or Addr (any read if neither is set).
type MMIORead struct {
	Device string  `yaml:"device"`
	Addr   uint64  `yaml:"addr"`
	Value  *uint64 `yaml:"value"`
	Random bool    `yaml:"random"`
}

// IRQFault drops the next assertion of PLIC source Source (the device's
// interrupt is lost until it deasserts and asserts again), or delays it by
// Delay steps.
type IRQFault struct {
	Source uint32 `yaml:"source"`
	Drop   bool   `yaml:"drop"`
	Delay  uint64 `yaml:"delay"`
}

// BlkFault fails the next virtio-blk request with an I/O error, or XORs
// byte Offset of the next read's data with Xor (default 0xff).
type BlkFault struct {
	Fail    bool   `yaml:"fail"`
	Corrupt bool   `yaml:"corrupt"`
	Offset  uint64 `yaml:"offset"`
	Xor     uint8  `yaml:"xor"`
}

// UARTFlt drops or garbles (XOR with Xor, or random) the next UART byte in
// direction Dir: "tx" (guest output) or "rx" (input to the guest).
type UARTFlt struct {
	Dir    string `yaml:"dir"`
	Drop   bool   `yaml:"drop"`
	Garble bool   `yaml:"garble"`
	Xor    uint8  `yaml:"xor"`
}

// Event kinds: where an event-targeted action applies.
const (
	EvBoundary = "boundary" // immediate actions, at a run-loop boundary
	EvMMIORead = "mmio_read"
	EvIRQ      = "irq"
	EvBlk      = "blk"
	EvUARTTx   = "uart_tx"
	EvUARTRx   = "uart_rx"
)

// Event returns the kind of event the action applies to.
func (a *Action) Event() string {
	switch {
	case a.MMIORead != nil:
		return EvMMIORead
	case a.IRQ != nil:
		return EvIRQ
	case a.Blk != nil:
		return EvBlk
	case a.UART != nil:
		if a.UART.Dir == "rx" {
			return EvUARTRx
		}
		return EvUARTTx
	}
	return EvBoundary
}

// Load reads a spec file.
func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses and validates a YAML spec. Unknown fields are errors.
func Parse(b []byte) (*Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	s := new(Spec)
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("fault spec: %w", err)
	}
	for i := range s.Faults {
		f := &s.Faults[i]
		if f.Name == "" {
			f.Name = fmt.Sprintf("fault%d", i+1)
		}
		if err := f.validate(); err != nil {
			return nil, fmt.Errorf("fault spec: %s: %w", f.Name, err)
		}
	}
	return s, nil
}

func count(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func (f *Fault) validate() error {
	w, d := &f.When, &f.Do
	if count(w.Step != nil, w.Instret != nil, w.PC != nil, w.Access != nil, w.Chance != nil) != 1 {
		return errors.New("`when` needs exactly one of step, instret, pc, access, chance")
	}
	if count(d.FlipBit != nil, d.Reg != nil, d.MMIORead != nil, d.IRQ != nil, d.Blk != nil, d.UART != nil) != 1 {
		return errors.New("`do` needs exactly one of flip_bit, reg, mmio_read, irq, blk, uart")
	}
	if f.Times != nil && *f.Times < 0 {
		return errors.New("times must not be negative")
	}
	if w.PC != nil && w.PC.Hit == 0 {
		w.PC.Hit = 1
	}
	if a := w.Access; a != nil {
		if (a.Device == "") == (a.Addr == 0) {
			return errors.New("access needs exactly one of device, addr")
		}
		if a.Nth == 0 {
			a.Nth = 1
		}
		switch a.Kind {
		case "":
			a.Kind = "any"
		case "read", "write", "any":
		default:
			return fmt.Errorf("access kind %q (want read, write or any)", a.Kind)
		}
	}
	if c := w.Chance; c != nil {
		if c.P <= 0 || c.P > 1 {
			return fmt.Errorf("chance p=%v (want 0 < p <= 1)", c.P)
		}
		if d.Event() == EvBoundary {
			return errors.New("chance needs an event action (mmio_read, irq, blk, uart)")
		}
	}
	switch {
	case d.FlipBit != nil:
		if d.FlipBit.Bit > 7 {
			return fmt.Errorf("flip_bit bit %d (want 0-7)", d.FlipBit.Bit)
		}
	case d.Reg != nil:
		if _, err := RegNumber(d.Reg.Reg); err != nil {
			return err
		}
		if count(d.Reg.Set != nil, d.Reg.Xor != 0, d.Reg.Random) != 1 {
			return errors.New("reg needs exactly one of set, xor, random")
		}
	case d.MMIORead != nil:
		if (d.MMIORead.Value != nil) == d.MMIORead.Random {
			return errors.New("mmio_read needs exactly one of value, random")
		}
		if w.Access != nil && w.Access.Kind == "write" {
			return errors.New("mmio_read with a write access trigger")
		}
	case d.IRQ != nil:
		if d.IRQ.Source == 0 || d.IRQ.Source > 31 || (d.IRQ.Drop == (d.IRQ.Delay != 0)) {
			return errors.New("irq needs a source (1-31) and exactly one of drop, delay")
		}
	case d.Blk != nil:
		if d.Blk.Fail == d.Blk.Corrupt {
			return errors.New("blk needs exactly one of fail, corrupt")
		}
		if d.Blk.Corrupt && d.Blk.Xor == 0 {
			d.Blk.Xor = 0xff
		}
	case d.UART != nil:
		u := d.UART
		if u.Dir == "" {
			u.Dir = "tx"
		}
		if u.Dir != "tx" && u.Dir != "rx" {
			return fmt.Errorf("uart dir %q (want tx or rx)", u.Dir)
		}
		if u.Drop == u.Garble {
			return errors.New("uart needs exactly one of drop, garble")
		}
	}
	return nil
}

var abiNames = []string{"zero", "ra", "sp", "gp", "tp", "t0", "t1", "t2", "s0", "s1", "a0", "a1", "a2", "a3", "a4",
	"a5", "a6", "a7", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9", "s10", "s11", "t3", "t4", "t5", "t6"}

// RegNumber maps a register name (x5, t0, fp, pc) to 0-31, or 32 for pc.
func RegNumber(name string) (int, error) {
	n := strings.ToLower(name)
	switch n {
	case "pc":
		return 32, nil
	case "fp":
		return 8, nil
	}
	for i, a := range abiNames {
		if n == a || n == fmt.Sprintf("x%d", i) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown register %q", name)
}

// Applied is a fault as it was injected: the concrete action and where in
// the run it took effect. Recordings store these (tag 0x10), and replays
// apply them again without re-evaluating triggers or the random generator.
type Applied struct {
	Step  uint64 `json:"step"`
	Fault string `json:"fault"`
	Event string `json:"event"` // EvBoundary or an event kind
	Seq   int    `json:"seq"`   // which event of that kind at that step (0-based)
	// The concrete action (random values resolved):
	Kind   string `json:"kind"` // flip_bit, reg, mmio_read, irq_drop, irq_delay, blk_fail, blk_corrupt, uart_drop, uart_garble
	Addr   uint64 `json:"addr,omitempty"`
	Bit    uint   `json:"bit,omitempty"`
	Reg    int    `json:"reg,omitempty"`
	Value  uint64 `json:"value,omitempty"`
	Offset uint64 `json:"offset,omitempty"`
	Xor    uint8  `json:"xor,omitempty"`
	Source uint32 `json:"source,omitempty"`
	Delay  uint64 `json:"delay,omitempty"`
}

func (a Applied) String() string {
	var d string
	switch a.Kind {
	case "flip_bit":
		d = fmt.Sprintf("flipped bit %d of %#x", a.Bit, a.Addr)
	case "reg":
		d = fmt.Sprintf("set %s to %#x", regName(a.Reg), a.Value)
	case "mmio_read":
		d = fmt.Sprintf("MMIO read of %#x returned %#x", a.Addr, a.Value)
	case "irq_drop":
		d = fmt.Sprintf("dropped interrupt source %d", a.Source)
	case "irq_delay":
		d = fmt.Sprintf("delayed interrupt source %d by %d steps", a.Source, a.Delay)
	case "blk_fail":
		d = "failed a virtio-blk request with an I/O error"
	case "blk_corrupt":
		d = fmt.Sprintf("corrupted byte %d of a virtio-blk read (xor %#x)", a.Offset, a.Xor)
	case "uart_drop":
		d = fmt.Sprintf("dropped UART %s byte %#x", strings.TrimPrefix(a.Event, "uart_"), a.Value)
	case "uart_garble":
		d = fmt.Sprintf("garbled UART %s byte %#x (xor %#x)", strings.TrimPrefix(a.Event, "uart_"), a.Value, a.Xor)
	}
	return fmt.Sprintf("fault %q at step %d: %s", a.Fault, a.Step, d)
}

func regName(n int) string {
	if n == 32 {
		return "pc"
	}
	return abiNames[n]
}
