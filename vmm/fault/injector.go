package fault

import (
	"math/rand/v2"
	"sort"
)

// Injector decides which faults to apply as a VM runs. Live, it evaluates
// a Spec's triggers (with a seeded generator); in replay, it applies a
// recording's Applied faults at the same steps and events. The VM calls it
// from the run-loop goroutine only.
type Injector struct {
	rules  []*rule
	rng    *rand.Rand
	replay []Applied // replay: faults still to apply, in step order
	log    func(Applied) error

	seqStep uint64
	seqs    map[string]int // events of each kind seen at seqStep

	pcStep, pcAddr uint64 // last pc-trigger arrival counted (dedupe)
	pcCounted      bool
}

type rule struct {
	Fault
	fired   int    // injections so far
	limit   int    // 0: unlimited
	hits    uint64 // pc arrivals / matching accesses so far
	armed   int    // event action: injections waiting for an event
	pending bool   // boundary action triggered by an access: apply at the next boundary
}

func (r *rule) exhausted() bool { return r.limit != 0 && r.fired+r.armed >= r.limit }

// NewLive returns an injector for spec. log receives each fault applied;
// an error from it stops the run.
func NewLive(spec *Spec, log func(Applied) error) *Injector {
	in := &Injector{rng: rand.New(rand.NewPCG(spec.Seed, spec.Seed^0x9e3779b97f4a7c15)), log: log, seqs: map[string]int{}}
	for _, f := range spec.Faults {
		r := &rule{Fault: f, limit: 1}
		if f.Times != nil {
			r.limit = *f.Times
		}
		in.rules = append(in.rules, r)
	}
	return in
}

// NewReplay returns an injector that applies recorded faults.
func NewReplay(applied []Applied, log func(Applied) error) *Injector {
	a := append([]Applied(nil), applied...)
	sort.SliceStable(a, func(i, j int) bool { return a[i].Step < a[j].Step })
	return &Injector{replay: a, log: log, seqs: map[string]int{}}
}

// Replaying reports whether the injector replays recorded faults.
func (in *Injector) Replaying() bool { return in.rules == nil && in.rng == nil }

// Deadline returns the earliest step at which the run loop must stop for
// an immediate fault, given the current step and instructions retired;
// ^0 if none.
func (in *Injector) Deadline(now, instret uint64) uint64 {
	d := ^uint64(0)
	if in.Replaying() {
		for _, a := range in.replay {
			if a.Event == EvBoundary {
				return max(a.Step, now)
			}
		}
		return d
	}
	for _, r := range in.rules {
		if r.exhausted() && !r.pending {
			continue
		}
		switch {
		case r.pending:
			return now // stop at the next boundary
		case r.When.Step != nil && *r.When.Step >= now:
			d = min(d, *r.When.Step)
		case r.When.Instret != nil && *r.When.Instret > instret:
			// Each step retires at most one instruction.
			d = min(d, now+(*r.When.Instret-instret))
		case r.When.Instret != nil:
			d = now
		}
	}
	return d
}

// PCs returns the addresses of pc triggers, for breakpoints.
func (in *Injector) PCs() []uint64 {
	var pcs []uint64
	for _, r := range in.rules {
		if r.When.PC != nil {
			pcs = append(pcs, r.When.PC.Addr)
		}
	}
	return pcs
}

// HasPCTriggers reports whether Boundary needs the pc.
func (in *Injector) HasPCTriggers() bool { return len(in.PCs()) > 0 }

// Boundary runs at a clean run-loop boundary (no MMIO access in flight):
// it applies the immediate faults due now with apply, and arms event faults
// whose triggers fired.
func (in *Injector) Boundary(now, instret, pc uint64, apply func(Applied) (Applied, error)) error {
	if in.Replaying() {
		for len(in.replay) > 0 && in.replay[0].Step <= now {
			a := in.replay[0]
			if a.Event != EvBoundary {
				break
			}
			in.replay = in.replay[1:]
			if _, err := in.apply(a, apply); err != nil {
				return err
			}
		}
		return nil
	}
	pcArrival := false
	for _, r := range in.rules {
		if r.When.PC != nil && pc == r.When.PC.Addr && !(in.pcCounted && in.pcStep == now && in.pcAddr == pc) {
			pcArrival = true
		}
	}
	for _, r := range in.rules {
		fire := r.pending
		switch {
		case r.exhausted() && !r.pending:
		case r.When.Step != nil:
			fire = fire || now >= *r.When.Step
		case r.When.Instret != nil:
			fire = fire || instret >= *r.When.Instret
		case r.When.PC != nil:
			if pcArrival && pc == r.When.PC.Addr {
				r.hits++
				fire = fire || r.hits == r.When.PC.Hit || (r.hits > r.When.PC.Hit && r.limit != 1)
			}
		}
		if !fire {
			continue
		}
		r.pending = false
		if r.Do.Event() != EvBoundary {
			r.armed++
			continue
		}
		a, err := in.concrete(r, now, EvBoundary, 0)
		if err != nil {
			return err
		}
		r.fired++
		if _, err := in.apply(a, apply); err != nil {
			return err
		}
	}
	if pcArrival {
		in.pcCounted, in.pcStep, in.pcAddr = true, now, pc
	}
	return nil
}

func (in *Injector) apply(a Applied, apply func(Applied) (Applied, error)) (Applied, error) {
	done, err := apply(a)
	if err != nil {
		return done, err
	}
	if in.log != nil {
		err = in.log(done)
	}
	return done, err
}

// seq returns the ordinal of this event among events of its kind at now.
func (in *Injector) seq(now uint64, kind string) int {
	if now != in.seqStep {
		in.seqStep = now
		clear(in.seqs)
	}
	n := in.seqs[kind]
	in.seqs[kind]++
	return n
}

// Event is called for each event of a kind (EvMMIORead, EvIRQ, EvBlk,
// EvUARTTx, EvUARTRx). match reports whether an armed or chance fault's
// action applies to this particular event (e.g. the right IRQ source). It
// returns the fault to apply, if any; the caller carries it out, completes
// the record (e.g. the byte affected) and passes it to Commit.
func (in *Injector) Event(now uint64, kind string, match func(Action) bool) (Applied, bool, error) {
	seq := in.seq(now, kind)
	if in.Replaying() {
		for i, a := range in.replay {
			if a.Step > now {
				break
			}
			if a.Step == now && a.Event == kind && a.Seq == seq {
				in.replay = append(in.replay[:i:i], in.replay[i+1:]...)
				return a, true, nil
			}
		}
		return Applied{}, false, nil
	}
	for _, r := range in.rules {
		if r.Do.Event() != kind || !match(r.Do) {
			continue
		}
		hit := false
		switch {
		case r.armed > 0:
			r.armed--
			hit = true
		case r.When.Chance != nil && !r.exhausted():
			hit = in.rng.Float64() < r.When.Chance.P
		}
		if !hit {
			continue
		}
		a, err := in.concrete(r, now, kind, seq)
		if err != nil {
			return Applied{}, false, err
		}
		r.fired++
		return a, true, nil
	}
	return Applied{}, false, nil
}

// Commit logs an event fault the caller has applied.
func (in *Injector) Commit(a Applied) error {
	if in.log != nil {
		return in.log(a)
	}
	return nil
}

// Access is called for each MMIO access (live only). It counts accesses
// for access triggers: a fault whose trigger fires either takes effect on
// this read (an mmio_read action, returned) or is applied at the next
// boundary.
func (in *Injector) Access(now uint64, device string, addr uint64, write bool) (Applied, bool, error) {
	if in.Replaying() {
		if write {
			return Applied{}, false, nil
		}
		return in.Event(now, EvMMIORead, func(Action) bool { return true })
	}
	var override *rule
	for _, r := range in.rules {
		t := r.When.Access
		if t == nil || r.exhausted() || (t.Device != "" && t.Device != device) || (t.Addr != 0 && t.Addr != addr) ||
			(t.Kind == "read" && write) || (t.Kind == "write" && !write) {
			continue
		}
		r.hits++
		if r.hits < t.Nth || (r.hits > t.Nth && r.limit == 1) {
			continue
		}
		switch {
		case r.Do.MMIORead != nil && !write && override == nil:
			override = r
		case r.Do.Event() == EvBoundary:
			r.pending = true
		default:
			r.armed++
		}
	}
	seq := in.seq(now, EvMMIORead)
	if write {
		in.seqs[EvMMIORead]-- // only reads count as mmio_read events
		return Applied{}, false, nil
	}
	if override != nil {
		a, err := in.concrete(override, now, EvMMIORead, seq)
		if err != nil {
			return Applied{}, false, err
		}
		a.Addr = addr
		override.fired++
		return a, true, nil
	}
	in.seqs[EvMMIORead]-- // Event counts it again
	return in.Event(now, EvMMIORead, func(a Action) bool {
		m := a.MMIORead
		return (m.Device == "" || m.Device == device) && (m.Addr == 0 || m.Addr == addr)
	})
}

// concrete resolves a rule's action into an Applied record.
func (in *Injector) concrete(r *rule, now uint64, event string, seq int) (Applied, error) {
	a := Applied{Step: now, Fault: r.Name, Event: event, Seq: seq}
	d := r.Do
	switch {
	case d.FlipBit != nil:
		a.Kind, a.Addr, a.Bit = "flip_bit", d.FlipBit.Addr, d.FlipBit.Bit
	case d.Reg != nil:
		a.Kind = "reg"
		a.Reg, _ = RegNumber(d.Reg.Reg)
		switch {
		case d.Reg.Set != nil:
			a.Value = *d.Reg.Set
		case d.Reg.Random:
			a.Value = in.rng.Uint64()
		default:
			a.Value, a.Xor = d.Reg.Xor, 1 // resolved against the register by the VM
		}
	case d.MMIORead != nil:
		a.Kind = "mmio_read"
		if d.MMIORead.Random {
			a.Value = in.rng.Uint64()
		} else {
			a.Value = *d.MMIORead.Value
		}
	case d.IRQ != nil:
		a.Source = d.IRQ.Source
		if d.IRQ.Drop {
			a.Kind = "irq_drop"
		} else {
			a.Kind, a.Delay = "irq_delay", d.IRQ.Delay
		}
	case d.Blk != nil:
		if d.Blk.Fail {
			a.Kind = "blk_fail"
		} else {
			a.Kind, a.Offset, a.Xor = "blk_corrupt", d.Blk.Offset, d.Blk.Xor
		}
	case d.UART != nil:
		if d.UART.Drop {
			a.Kind = "uart_drop"
		} else {
			a.Kind, a.Xor = "uart_garble", d.UART.Xor
			if a.Xor == 0 {
				a.Xor = uint8(in.rng.Uint32()) | 1
			}
		}
	}
	return a, nil
}
