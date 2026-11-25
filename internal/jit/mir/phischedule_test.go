package mir

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// passSchedule is phiSchedule's order made as it had been, a pass over the
// pending moves at a time: the reference its heaps must follow.
func passSchedule(c *core, to *ssa.Block, idx int) []phiStep {
	type move struct{ dst, src *ssa.Value }
	var moves []move
	for _, phi := range to.Values {
		if phi.Op != ssa.OpPhi {
			break
		}
		if _, ok := c.loc(phi); !ok {
			continue
		}
		src := phi.Args[idx]
		if !c.isLazy(src) && c.locAt(src) == c.locAt(phi) {
			continue
		}
		moves = append(moves, move{phi, src})
	}
	same := func(a, b *ssa.Value) bool {
		if c.isLazy(a) || c.isLazy(b) || isFloat(a) != isFloat(b) {
			return false
		}
		return c.locAt(a) == c.locAt(b)
	}
	var steps []phiStep
	var parked []*ssa.Value
	isParked := func(v *ssa.Value) bool { return slices.Contains(parked, v) }
	readers := make([]int32, 2*(64+max(abi.SpillSlots, c.spills)))
	for _, m := range moves {
		if k := c.locKey(m.src); k >= 0 {
			readers[k]++
		}
	}
	for len(moves) > 0 {
		progress := false
		for i := 0; i < len(moves); i++ {
			m := moves[i]
			if k := c.locKey(m.dst); k >= 0 && readers[k] > 0 {
				continue
			}
			p := isParked(m.src)
			if k := c.locKey(m.src); k >= 0 && !p {
				readers[k]--
			}
			steps = append(steps, phiStep{dst: m.dst, src: m.src, parked: p})
			moves = append(moves[:i], moves[i+1:]...)
			i--
			progress = true
		}
		if !progress {
			var blocker *ssa.Value
			for _, o := range moves {
				if same(o.src, moves[0].dst) && !isParked(o.src) {
					blocker = o.src
					break
				}
			}
			if blocker == nil {
				panic("phi moves: blocked without a blocker")
			}
			for _, o := range moves {
				if isParked(o.src) && isFloat(o.src) == isFloat(blocker) {
					panic("phi moves: scratch already holds a parked value")
				}
			}
			steps = append(steps, phiStep{park: blocker})
			parked = append(parked, blocker)
			for _, o := range moves {
				if o.src == blocker {
					readers[c.locKey(blocker)]--
				}
			}
		}
	}
	return steps
}

// scheduleCase is an edge's phis and their arguments at random: the phis
// in locations of their own, each argument a phi (a variable the loop
// carries), a value of a location no phi has, which several may read, a
// value with no location of its own (a lazy one), or the phi itself.
func scheduleCase(rng *rand.Rand, n int) (*core, *ssa.Block) {
	c := &core{spills: 24}
	c.sched = &c.own
	var vals []*ssa.Value
	var locs []loc
	var lazy []bool
	add := func(t ssa.Type, l loc, isLazy bool) *ssa.Value {
		v := &ssa.Value{ID: len(vals), Op: ssa.OpPhi, Type: t}
		vals, locs, lazy = append(vals, v), append(locs, l), append(lazy, isLazy)
		return v
	}
	// Locations: 16 registers and 24 slots of each class.
	pick := func(taken map[[2]int]bool, float int) loc {
		for {
			var l loc
			if rng.IntN(3) == 0 {
				l = loc{reg: -1, spill: rng.IntN(24)}
			} else {
				l = loc{reg: rng.IntN(16), spill: -1}
			}
			k := [2]int{l.reg*100 + l.spill, float}
			if !taken[k] {
				taken[k] = true
				return l
			}
		}
	}
	taken := map[[2]int]bool{}
	floats := rng.IntN(3)
	var phis []*ssa.Value
	for range n {
		t := ssa.Tagged
		if floats == 2 || floats == 1 && rng.IntN(2) == 0 {
			t = ssa.Float64
		}
		phis = append(phis, add(t, pick(taken, b2i(t == ssa.Float64)), false))
	}
	others := map[[2]int]*ssa.Value{}
	for _, phi := range phis {
		var src *ssa.Value
		switch r := rng.IntN(10); {
		case r < 6:
			// A phi of the same class: chains and cycles.
			for src == nil || isFloat(src) != isFloat(phi) {
				src = phis[rng.IntN(len(phis))]
			}
		case r < 8:
			// A value of a location of its own, shared at times.
			f := b2i(isFloat(phi))
			if rng.IntN(2) == 0 {
				for k, v := range others {
					if k[1] == f {
						src = v
						break
					}
				}
			}
			if src == nil {
				l := pick(taken, f)
				src = add(phi.Type, l, false)
				others[[2]int{l.reg*100 + l.spill, f}] = src
			}
		case r < 9:
			src = add(phi.Type, loc{reg: -1, spill: -1}, true)
		default:
			src = phi
		}
		phi.Args = []*ssa.Value{src}
	}
	c.locs, c.lazy = locs, lazy
	c.hasLoc = make([]bool, len(vals))
	for i := range vals {
		c.hasLoc[i] = !lazy[i]
	}
	return c, &ssa.Block{Values: phis}
}

// showSteps is a schedule's steps by the values' IDs.
func showSteps(steps []phiStep) string {
	s := ""
	for _, st := range steps {
		switch {
		case st.park != nil:
			s += fmt.Sprintf(" park v%d", st.park.ID)
		case st.parked:
			s += fmt.Sprintf(" v%d<-scratch", st.dst.ID)
		default:
			s += fmt.Sprintf(" v%d<-v%d", st.dst.ID, st.src.ID)
		}
	}
	return s
}

// An edge's parallel move is made in the order a pass at a time had made
// it, so that the code is the same, and moves every phi's argument into
// it: what each location holds simulated through the steps, scratch
// included.
func TestPhiScheduleOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var parks, fromScratch, panics int
	for trial := range 20000 {
		c, to := scheduleCase(rng, 1+rng.IntN(24))
		desc := func() string {
			s := ""
			for _, phi := range to.Values {
				s += fmt.Sprintf(" v%d%v<-v%d%v", phi.ID, c.locs[phi.ID], phi.Args[0].ID, c.locs[phi.Args[0].ID])
			}
			return s
		}
		run := func(f func() []phiStep) (steps []phiStep, msg any) {
			defer func() { msg = recover() }()
			return f(), nil
		}
		want, wmsg := run(func() []phiStep { return passSchedule(c, to, 0) })
		got, gmsg := run(func() []phiStep { return slices.Clone(c.phiSchedule(to, 0)) })
		if wmsg != nil || gmsg != nil {
			if fmt.Sprint(wmsg) != fmt.Sprint(gmsg) {
				t.Fatalf("trial %d:%s: panicked %v, want %v", trial, desc(), gmsg, wmsg)
			}
			panics++
			continue
		}
		if !slices.Equal(got, want) {
			t.Fatalf("trial %d:%s:\n got %s\nwant %s", trial, desc(), showSteps(got), showSteps(want))
		}
		// What each location holds: at first its own value.
		holds := map[int]int{}
		key := func(v *ssa.Value) int { return c.locKey(v) }
		held := func(v *ssa.Value) int {
			if k := key(v); k >= 0 {
				if h, ok := holds[k]; ok {
					return h
				}
			}
			return v.ID
		}
		initial := map[int]int{}
		for _, phi := range to.Values {
			initial[phi.ID] = held(phi.Args[0])
		}
		var scratch [2]int
		for _, st := range got {
			if st.park != nil {
				parks++
				scratch[b2i(isFloat(st.park))] = held(st.park)
				continue
			}
			h := held(st.src)
			if st.parked {
				fromScratch++
				h = scratch[b2i(isFloat(st.src))]
			}
			holds[key(st.dst)] = h
		}
		for _, phi := range to.Values {
			if h := held(phi); h != initial[phi.ID] {
				t.Fatalf("trial %d:%s: v%d holds v%d, want v%d (%s)", trial, desc(), phi.ID, h, initial[phi.ID], showSteps(got))
			}
		}
		for i, r := range c.sched.readers {
			if r != 0 || c.sched.writers[i] != 0 || c.sched.reading[i] != 0 {
				t.Fatalf("trial %d: location %d's tables left %d %d %d", trial, i, r, c.sched.writers[i], c.sched.reading[i])
			}
		}
	}
	t.Logf("%d values parked, %d moves from scratch, %d edges refused", parks, fromScratch, panics)
	if parks == 0 || fromScratch == 0 {
		t.Error("no cycle met")
	}
}

// An edge of n moves, a chain listed against its order (each phi given
// the one before's value) or with it, schedules in steps linear in n:
// the passes had taken n²/2.
func TestPhiScheduleLinear(t *testing.T) {
	for _, back := range []bool{false, true} {
		var steps [2]int
		for k, n := range []int{1000, 8000} {
			c := &core{spills: n}
			c.sched = &c.own
			phis := make([]*ssa.Value, n)
			c.locs, c.lazy, c.hasLoc = make([]loc, n+1), make([]bool, n+1), make([]bool, n+1)
			for i := range phis {
				phis[i] = &ssa.Value{ID: i, Op: ssa.OpPhi, Type: ssa.Float64}
				c.locs[i], c.hasLoc[i] = loc{reg: -1, spill: i}, true
			}
			end := &ssa.Value{ID: n, Op: ssa.OpAddF64, Type: ssa.Float64}
			c.locs[n], c.hasLoc[n] = loc{reg: 3, spill: -1}, true
			for i, phi := range phis {
				src := end
				if back && i > 0 {
					src = phis[i-1]
				} else if !back && i < n-1 {
					src = phis[i+1]
				}
				phi.Args = []*ssa.Value{src}
			}
			if got := len(c.phiSchedule(&ssa.Block{Values: phis}, 0)); got != n {
				t.Fatalf("%d moves scheduled in %d steps", n, got)
			}
			steps[k] = c.steps
		}
		t.Logf("back %v: %d -> %d steps", back, steps[0], steps[1])
		if steps[1] > 24*max(steps[0], 1) {
			t.Errorf("back %v: eight times the moves took %.1f times the steps", back, float64(steps[1])/float64(max(steps[0], 1)))
		}
	}
}
