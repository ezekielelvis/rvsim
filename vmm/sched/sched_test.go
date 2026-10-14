package sched

import (
	"math/rand"
	"sort"
	"testing"
)

type recorder struct {
	got []Event
	s   *Scheduler
	now uint64
	// When chainOn, a kind-1 event schedules a kind-2 event at now+chain.
	chainOn bool
	chain   uint64
	self    Target
}

func (r *recorder) HandleEvent(kind uint32, arg uint64) {
	r.got = append(r.got, Event{At: r.now, Kind: kind, Arg: arg})
	if kind == 1 && r.chainOn {
		r.s.Schedule(r.now+r.chain, r.self, 2, arg)
	}
}

func TestOrderingByTimeThenSchedulingOrder(t *testing.T) {
	var s Scheduler
	r := &recorder{s: &s}
	tg := s.Register("r", r)
	s.Schedule(30, tg, 0, 3)
	s.Schedule(10, tg, 0, 1)
	s.Schedule(20, tg, 0, 2)
	s.Schedule(10, tg, 0, 11) // same time as arg 1, scheduled later
	if at, ok := s.Next(); !ok || at != 10 {
		t.Fatalf("Next = %d, %v; want 10, true", at, ok)
	}
	r.now = 25
	if n := s.RunDue(25); n != 3 {
		t.Fatalf("RunDue fired %d, want 3", n)
	}
	want := []uint64{1, 11, 2}
	for i, e := range r.got {
		if e.Arg != want[i] {
			t.Fatalf("event %d arg %d, want %d (order %v)", i, e.Arg, want[i], r.got)
		}
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
}

func TestEventsScheduledWhileFiringRunIfDue(t *testing.T) {
	var s Scheduler
	r := &recorder{s: &s, chainOn: true, chain: 0}
	r.self = s.Register("r", r)
	s.Schedule(5, r.self, 1, 7) // kind 1 schedules kind 2 at the same time
	r.now = 5
	if n := s.RunDue(5); n != 2 {
		t.Fatalf("RunDue fired %d, want 2", n)
	}
	if r.got[1].Kind != 2 {
		t.Fatalf("follow-up event not fired: %v", r.got)
	}
}

func TestFutureEventsWait(t *testing.T) {
	var s Scheduler
	r := &recorder{s: &s}
	tg := s.Register("r", r)
	s.Schedule(100, tg, 0, 0)
	if n := s.RunDue(99); n != 0 {
		t.Fatalf("RunDue(99) fired %d", n)
	}
	if n := s.RunDue(100); n != 1 {
		t.Fatalf("RunDue(100) fired %d", n)
	}
	if _, ok := s.Next(); ok {
		t.Fatal("queue not empty")
	}
}

// The heap must agree with a sort by (At, Seq) for arbitrary input.
func TestHeapMatchesSort(t *testing.T) {
	var s Scheduler
	r := &recorder{s: &s}
	tg := s.Register("r", r)
	rng := rand.New(rand.NewSource(1))
	var want []Event
	for i := range 1000 {
		at := uint64(rng.Intn(50))
		s.Schedule(at, tg, 0, uint64(i))
		want = append(want, Event{At: at, Seq: uint64(i), Arg: uint64(i)})
	}
	sort.Slice(want, func(i, j int) bool { return less(&want[i], &want[j]) })
	var got []uint64
	for s.Len() > 0 {
		got = append(got, s.pop().Arg)
	}
	for i := range want {
		if got[i] != want[i].Arg {
			t.Fatalf("position %d: got %d, want %d", i, got[i], want[i].Arg)
		}
	}
}

func TestDuplicateTargetNamePanics(t *testing.T) {
	var s Scheduler
	s.Register("uart", &recorder{})
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	s.Register("uart", &recorder{})
}

func TestScheduleDoesNotAllocateOnceGrown(t *testing.T) {
	var s Scheduler
	tg := s.Register("r", &recorder{s: &s})
	for i := range 64 {
		s.Schedule(uint64(i), tg, 0, 0)
	}
	s.RunDue(1 << 20)
	allocs := testing.AllocsPerRun(1000, func() {
		s.Schedule(1, tg, 0, 0)
		s.pop()
	})
	if allocs != 0 {
		t.Fatalf("Schedule/pop allocate %v times", allocs)
	}
}
