package ssa

import (
	"math/rand"
	"testing"
)

// A state's slots made from another's by setSlot are what a list of them
// says, set and cleared, past one node's 64 and past 64², and the state
// they were made from keeps its own: each state is checked against a
// dense list of its slots after every state made since.
func TestFrameStateSlots(t *testing.T) {
	f := &Func{}
	vals := make([]*Value, 50)
	for i := range vals {
		vals[i] = &Value{ID: i}
	}
	rng := rand.New(rand.NewSource(1))
	type made struct {
		s     *FrameState
		dense []*Value
	}
	var states []made
	for k := 0; k < 300; k++ {
		n := []int{5, 64, 65, 200, 4096, 4097, 9000}[rng.Intn(7)]
		s := &FrameState{n: n}
		dense := make([]*Value, n)
		if len(states) > 0 && rng.Intn(4) != 0 {
			from := states[rng.Intn(len(states))]
			s.shareSlots(from.s)
			copy(dense, from.dense)
			for i := n; i < from.s.n; i++ {
				// Past the new state's slots: cleared.
				if from.dense[i] != nil {
					f.openEdit()
					f.setSlot(s, i, nil)
				}
			}
		}
		f.openEdit()
		for range rng.Intn(40) {
			i := rng.Intn(n)
			if i%3 == 0 {
				// Some slots in a run, as a state of many live values has.
				i = rng.Intn(min(n, 130))
			}
			var v *Value
			if rng.Intn(3) != 0 {
				v = vals[rng.Intn(len(vals))]
			}
			f.setSlot(s, i, v)
			dense[i] = v
		}
		s.n = n
		states = append(states, made{s, dense})
		for _, m := range states {
			checkDense(t, m.s, m.dense)
		}
		if t.Failed() {
			return
		}
	}
}

// checkDense checks that s's slots are dense's.
func checkDense(t *testing.T, s *FrameState, dense []*Value) {
	t.Helper()
	if !s.checkSlots() {
		t.Fatalf("a state's slots are malformed")
	}
	count := 0
	for i, v := range dense {
		if got := s.Slot(i); got != v {
			t.Fatalf("slot %d is %v, want %v", i, got, v)
		}
		if v != nil {
			count++
		}
	}
	if s.Count() != count {
		t.Fatalf("%d slots set, want %d", s.Count(), count)
	}
	last := -1
	for i, v := range s.All() {
		if i <= last || i >= len(dense) || dense[i] != v {
			t.Fatalf("All gives slot %d as %v after slot %d", i, v, last)
		}
		last = i
	}
	if s.Len() != len(dense) {
		t.Fatalf("Len is %d, want %d", s.Len(), len(dense))
	}
}

// The uses the states' slots count (slotWeights) are each state's users
// times the slots it names a value in, as counted state by state; and a
// rewrite of shared nodes (rewriteSlots) replaces a value in every state
// that has it, each node once.
func TestFrameStateSlotUses(t *testing.T) {
	f := &Func{}
	vals := make([]*Value, 20)
	for i := range vals {
		vals[i] = &Value{ID: i}
	}
	rng := rand.New(rand.NewSource(2))
	var states []*FrameState
	for k := 0; k < 200; k++ {
		s := &FrameState{n: 5000, id: k}
		if k > 0 {
			s.shareSlots(states[rng.Intn(len(states))])
		}
		f.openEdit()
		for range 1 + rng.Intn(10) {
			f.setSlot(s, rng.Intn(5000), vals[rng.Intn(len(vals))])
		}
		states = append(states, s)
	}
	weights := make([]int, len(states))
	want := make([]int, len(vals))
	var live []*FrameState
	for k, s := range states {
		if k%3 == 0 {
			continue
		}
		weights[k] = 1 + k%4
		live = append(live, s)
		for _, v := range s.All() {
			want[v.ID] += weights[k]
		}
	}
	f.slotWeights(live, weights)
	for i, v := range vals {
		if v.Uses != want[i] {
			t.Errorf("v%d: %d uses, want %d", i, v.Uses, want[i])
		}
	}
	// Each value's ID even replaced with the next: wherever it is.
	dense := make([][]*Value, len(states))
	for k, s := range states {
		dense[k] = s.Dense()
		for i, v := range dense[k] {
			if v != nil && v.ID%2 == 0 {
				dense[k][i] = vals[v.ID+1]
			}
		}
	}
	seen := make([]bool, f.NumSlotNodes())
	for _, s := range states {
		rewriteSlots(s, seen, func(v *Value) *Value {
			if v.ID%2 == 0 {
				return vals[v.ID+1]
			}
			return v
		})
	}
	for k, s := range states {
		checkDense(t, s, dense[k])
	}
}
