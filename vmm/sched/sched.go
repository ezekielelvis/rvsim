// Package sched is the VMM's discrete-event scheduler. Time is virtual: it
// is measured in the core's steps (ADR-001), never in wall-clock time.
//
// Events name a registered target, a kind and an argument rather than
// holding a closure, so the set of pending events is plain data that a
// snapshot can save and restore.
package sched

import (
	"fmt"
	"sort"
)

// Handler receives the events scheduled for it.
type Handler interface {
	HandleEvent(kind uint32, arg uint64)
}

// Target identifies a registered Handler.
type Target uint16

// Event is a pending event.
type Event struct {
	At     uint64 // step at which the event fires
	Seq    uint64 // tie-breaker: events at the same step fire in scheduling order
	Target Target
	Kind   uint32
	Arg    uint64
}

// Scheduler is a deterministic event queue. It is not safe for concurrent
// use: like the machine, it belongs to the run-loop goroutine.
type Scheduler struct {
	heap     []Event
	seq      uint64
	handlers []Handler
	names    []string
}

// Register adds a handler under a stable name and returns its Target.
func (s *Scheduler) Register(name string, h Handler) Target {
	for _, n := range s.names {
		if n == name {
			panic(fmt.Sprintf("sched: target %q registered twice", name))
		}
	}
	s.handlers = append(s.handlers, h)
	s.names = append(s.names, name)
	return Target(len(s.handlers) - 1)
}

// Schedule queues an event to fire when virtual time reaches at.
func (s *Scheduler) Schedule(at uint64, t Target, kind uint32, arg uint64) {
	if int(t) >= len(s.handlers) {
		panic(fmt.Sprintf("sched: unknown target %d", t))
	}
	s.heap = append(s.heap, Event{At: at, Seq: s.seq, Target: t, Kind: kind, Arg: arg})
	s.seq++
	s.up(len(s.heap) - 1)
}

// Next returns the time of the earliest pending event.
func (s *Scheduler) Next() (uint64, bool) {
	if len(s.heap) == 0 {
		return 0, false
	}
	return s.heap[0].At, true
}

// Pending returns the pending events in firing order, (At, Seq). It
// allocates; use it for hashing and snapshots, not on the hot path.
func (s *Scheduler) Pending() []Event {
	evs := append([]Event(nil), s.heap...)
	sort.Slice(evs, func(i, j int) bool { return less(&evs[i], &evs[j]) })
	return evs
}

// Name returns the name a target was registered under.
func (s *Scheduler) Name(t Target) string { return s.names[t] }

// Seq returns the sequence number the next scheduled event will get.
func (s *Scheduler) Seq() uint64 { return s.seq }

// Restore replaces the pending events and sequence counter, for example
// from a snapshot. Events name their target by registered name, so a
// restored machine may register its targets in any order.
func (s *Scheduler) Restore(events []NamedEvent, seq uint64) error {
	heap := make([]Event, 0, len(events))
	for _, e := range events {
		t := -1
		for i, n := range s.names {
			if n == e.Target {
				t = i
			}
		}
		if t < 0 {
			return fmt.Errorf("sched: restore: unknown target %q", e.Target)
		}
		if e.Seq >= seq {
			return fmt.Errorf("sched: restore: event sequence %d >= counter %d", e.Seq, seq)
		}
		heap = append(heap, Event{At: e.At, Seq: e.Seq, Target: Target(t), Kind: e.Kind, Arg: e.Arg})
	}
	s.heap, s.seq = heap[:0], seq
	for _, e := range heap {
		s.heap = append(s.heap, e)
		s.up(len(s.heap) - 1)
	}
	return nil
}

// NamedEvent is an Event whose target is identified by name.
type NamedEvent struct {
	At     uint64
	Seq    uint64
	Target string
	Kind   uint32
	Arg    uint64
}

// Named returns the pending events, in firing order, with target names.
func (s *Scheduler) Named() []NamedEvent {
	var out []NamedEvent
	for _, e := range s.Pending() {
		out = append(out, NamedEvent{At: e.At, Seq: e.Seq, Target: s.names[e.Target], Kind: e.Kind, Arg: e.Arg})
	}
	return out
}

// Len returns the number of pending events.
func (s *Scheduler) Len() int { return len(s.heap) }

// RunDue fires, in (At, Seq) order, every event due at or before now,
// including events that handlers schedule for times <= now while it runs.
// It returns the number of events fired.
func (s *Scheduler) RunDue(now uint64) int {
	n := 0
	for len(s.heap) > 0 && s.heap[0].At <= now {
		e := s.pop()
		s.handlers[e.Target].HandleEvent(e.Kind, e.Arg)
		n++
	}
	return n
}

func less(a, b *Event) bool {
	if a.At != b.At {
		return a.At < b.At
	}
	return a.Seq < b.Seq
}

func (s *Scheduler) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !less(&s.heap[i], &s.heap[p]) {
			return
		}
		s.heap[i], s.heap[p] = s.heap[p], s.heap[i]
		i = p
	}
}

func (s *Scheduler) pop() Event {
	top := s.heap[0]
	last := len(s.heap) - 1
	s.heap[0] = s.heap[last]
	s.heap = s.heap[:last]
	for i := 0; ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < len(s.heap) && less(&s.heap[l], &s.heap[m]) {
			m = l
		}
		if r < len(s.heap) && less(&s.heap[r], &s.heap[m]) {
			m = r
		}
		if m == i {
			break
		}
		s.heap[i], s.heap[m] = s.heap[m], s.heap[i]
		i = m
	}
	return top
}
