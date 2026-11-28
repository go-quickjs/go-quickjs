//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// linearBlocksSource is a loop of n values, each written in a block of
// its own, an if's, and all of them live across every block: n values
// live across 2n ends of branches.
func linearBlocksSource(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "function %s(a, rounds) {\nvar s = 0;\n", name)
	for i := range n {
		fmt.Fprintf(&b, "var v%d = a + %d;\n", i, i)
	}
	b.WriteString("for (var r = 0; r < rounds; r++) {\n")
	for i := range n {
		fmt.Fprintf(&b, "if (a > %d) v%d = v%d * 0.5 + r;\n", i%7, i, i)
	}
	b.WriteString("}\n")
	for i := range n {
		fmt.Fprintf(&b, "s = s + v%d;\n", i)
	}
	b.WriteString("return s;\n}\n")
	return b.String()
}

// linearStoresSource is n property reads, each kept tagged, each stored
// into an array on a branch of its own and into a property after it:
// values read from cells live across a store or two, as TestJITLongCode's
// reads of o.v are, in a function of n stores and 2n blocks. What is live
// across the stores (ssa's liveAcross) is linear in it; the check of it
// against its reference, which the quickjs_verify build makes, had taken
// a table of every candidate by every block, quadratic.
func linearStoresSource(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "function %s(a, rounds) {\nvar o = {p: 1, q: 0}, arr = [0, 0, 0, 0, 0, 0, 0, 0], s = 0;\n", name)
	b.WriteString("for (var r = 0; r < rounds; r++) {\n")
	for i := range n {
		fmt.Fprintf(&b, "var v%d = o.p;\nif (a > %d) { arr[%d] = v%d; }\no.q = v%d;\n", i, i%7, i%8, i, i)
	}
	b.WriteString("s = s + arr[r % 8] + o.q;\n}\nreturn s;\n}\n")
	return b.String()
}

// linearRotateSource is n variables a loop carries, each given the
// next's value every turn (back, the one before's): the loop's back edge
// moves every phi's location into another's.
func linearRotateSource(back bool) func(string, int) string {
	return func(name string, n int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "function %s(a, rounds) {\n", name)
		for i := range n {
			fmt.Fprintf(&b, "var v%d = a + %d;\n", i, i)
		}
		b.WriteString("for (var r = 0; r < rounds; r++) {\n")
		if back {
			fmt.Fprintf(&b, "var t = v%d;\n", n-1)
			for i := n - 1; i > 0; i-- {
				fmt.Fprintf(&b, "v%d = v%d;\n", i, i-1)
			}
			b.WriteString("v0 = t + r;\n")
		} else {
			b.WriteString("var t = v0;\n")
			for i := 0; i < n-1; i++ {
				fmt.Fprintf(&b, "v%d = v%d;\n", i, i+1)
			}
			fmt.Fprintf(&b, "v%d = t + r;\n", n-1)
		}
		b.WriteString("}\nvar s = 0;\n")
		for i := range n {
			fmt.Fprintf(&b, "s = s + v%d;\n", i)
		}
		b.WriteString("return s;\n}\n")
		return b.String()
	}
}

// linearLoopsSource is n loops one after another, each its own budget
// loop: what is found for each loop must take the loop's own blocks, not
// the function's.
func linearLoopsSource(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "function %s(a, rounds) {\nvar s = 0;\n", name)
	for i := range n {
		fmt.Fprintf(&b, "for (var i%d = 0; i%d < rounds; i%d++) s = s + a * %d;\n", i, i, i, i)
	}
	b.WriteString("return s;\n}\n")
	return b.String()
}

// linearDeepSource is loops nested n/10 deep (the parser's bound of a
// nest is some 2,000 statements, a loop and its block two), each of one
// turn, adding an invariant product before the loop inside it and halving
// a sum after it; doWhile makes each a do-while loop, whose test, and so
// its way out, is at its end, after the loops inside it. One counter
// steers them all, so that what is live at each level is the same few
// values: a counter of each loop's own would be live through every loop
// inside it, and every exit there would name them all, the depth's
// square, as a program has it. What is found of each loop -- licm's walk,
// the peeling's choice of a loop with none inside it, mir's budget loops
// (findBudgets) -- must not take each loop's nest, the function's depth
// for each of its loops.
func linearDeepSource(doWhile bool) func(name string, n int) string {
	return func(name string, n int) string {
		var b strings.Builder
		fmt.Fprintf(&b, "function %s(a, rounds) {\nvar s = 0, c = 0;\n", name)
		d := n / 10
		for i := range d {
			if doWhile {
				fmt.Fprintf(&b, "do {\nc = c + 1;\ns = s + a * %d;\n", i)
			} else {
				fmt.Fprintf(&b, "while (c < %d) {\nc = c + 1;\ns = s + a * %d;\n", i+1, i)
			}
		}
		for i := range d {
			if doWhile {
				fmt.Fprintf(&b, "s = s * 0.5 + %d;\n} while (c < %d);\n", d-i, d-i)
			} else {
				fmt.Fprintf(&b, "s = s * 0.5 + %d;\n}\n", d-i)
			}
		}
		b.WriteString("return s;\n}\n")
		return b.String()
	}
}

// linearBreaksSource is linearDeepSource's nest of while loops whose
// innermost body leaves each of them by a labeled break of its own, a
// test of each level's, which the loops around it walk from their own
// back edges: each break's way out lies in every loop around its target.
func linearBreaksSource(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "function %s(a, rounds) {\nvar s = 0, c = 0;\n", name)
	d := n / 20
	for i := range d {
		fmt.Fprintf(&b, "l%d: while (c < %d) {\nc = c + 1;\ns = s + a * %d;\n", i, i+1, i)
	}
	for i := range d {
		fmt.Fprintf(&b, "if (s < %d - rounds * 1000) break l%d;\n", -i, i)
	}
	for i := range d {
		fmt.Fprintf(&b, "s = s * 0.5 + %d;\n}\n", d-i)
	}
	b.WriteString("return s;\n}\n")
	return b.String()
}

// linearGroupsSource is n variables a loop carries, each a phi group of
// its header's phi and its new value, past every register: each turn reads
// them all into n temporaries, live till the second half of the turn
// gives each variable its new value. A temporary spilled finds the slots
// of the groups whose phis have ended held for their new values, which
// begin before it ends: none may hold it.
func linearGroupsSource(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "function %s(a, rounds) {\n", name)
	for i := range n {
		fmt.Fprintf(&b, "var v%d = a + %d;\n", i, i)
	}
	b.WriteString("for (var r = 0; r < rounds; r++) {\n")
	for i := range n {
		fmt.Fprintf(&b, "var t%d = v%d * 0.5;\n", i, i)
	}
	for i := range n {
		fmt.Fprintf(&b, "v%d = t%d + t%d;\n", i, i, (i*7+3)%n)
	}
	b.WriteString("}\nvar s = 0;\n")
	for i := range n {
		fmt.Fprintf(&b, "s = s + v%d;\n", i)
	}
	b.WriteString("return s;\n}\n")
	return b.String()
}

// linearGuardsSource is n doubles live at once, each multiplied by an
// element of an array whose read a guard checks: n exits that survive
// Optimize, each naming the doubles still to be read.
func linearGuardsSource(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "var ARR = []; for (var k = 0; k < %d; k++) ARR.push(k * 0.5);\n", n)
	fmt.Fprintf(&b, "function %s(a, rounds) {\nvar t = 0, arr = ARR;\nfor (var r = 0; r < rounds; r++) {\n", name)
	for i := range n {
		fmt.Fprintf(&b, "var d%d = a * %d + r;\n", i, i+3)
	}
	for i := range n {
		fmt.Fprintf(&b, "t = t + arr[%d] * d%d;\n", i, i)
	}
	b.WriteString("}\nreturn t;\n}\n")
	return b.String()
}

// linearCompile is what compiling function name of src, from its register
// code, takes: the bytes allocated and the time of its lowering, its build
// and Optimize, and its compile for the host, the allocation check off, as
// production's compiles are, and that compile's time alone, the least of
// three (backend); with what the program keeps of its liveness and the
// code of its exits' descriptions.
type linearCompile struct {
	lower, build, code uint64
	took, backend      time.Duration
	live, exits        int
	// steps and bytes are each phase's of the compile for the host
	// (mir.Times' Steps, and its Ticks read from the heap's allocation).
	steps, bytes [mir.NumPhases]int64
}

func linearCompileOf(t *testing.T, src, name string, args func(*Runtime) []Value) linearCompile {
	t.Helper()
	rts := regNativeRuntimes(t, src)
	r := rts[0]
	r.jitMemoryLimit = 1 << 36
	spillMoreCollect(r)
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
	r.RegNativeCall(fn, args(r), false)
	cl := fn.Object().fn().closure
	e := r.jit.regCache[r.jitRegKeyOf(cl)]
	if e == nil || e.p == nil {
		t.Fatalf("%s: not lowered", name)
	}
	var ms runtime.MemStats
	alloc := func() uint64 {
		runtime.ReadMemStats(&ms)
		return ms.TotalAlloc
	}
	// The least of three lowerings, each with a scratch of its own
	// (LowerRegCold), as the build and the backend below each have a
	// workspace of their own. The scratch LowerReg takes from the
	// process's pool, kept in a slot of the processor the last lowering
	// ran on, is there or not as the goroutine moves between processors
	// and collections run: with it the loops' lowering took 3.4 MB for
	// 1,000 values and 30.8 MB for 8,000, without it 7.6 and 72.7 MB, and
	// on a loaded runner the 1,000 had it and the 8,000 did not, 21.3
	// times the memory (spill's 16.8 times, 5.7 and 95.3 MB). Nor does a
	// collection run while they do, nor the finalizers and cleanups it
	// sets off, whose allocations the process's count (TotalAlloc) adds
	// to the lowering's.
	var c linearCompile
	var p *ir.Program
	runtime.GC()
	start := time.Now()
	func() {
		defer holdCollectorNow(t)()
		for k := range 3 {
			a0 := alloc()
			q, err := jitcompile.LowerRegCold(cl.reg.rc, cl.fn)
			if err != nil {
				t.Fatal(err)
			}
			if a := alloc() - a0; k == 0 || a < c.lower {
				c.lower = a
			}
			p = q
		}
	}()
	a1 := alloc()
	f, err := ssa.BuildWith(e.p, r.jitRegFeedbackFor(cl, e.p))
	if err != nil {
		t.Fatal(err)
	}
	jitRegShape(f, e.p, cl, cl.reg)
	ssa.Optimize(f)
	a2 := alloc()
	opts := mir.Settings()
	opts.CheckAllocations = false
	mc, err := mir.CompileWith(new(mir.Workspace), f, jitEncoding, opts)
	if err != nil {
		t.Fatal(err)
	}
	a3 := alloc()
	c.took = time.Since(start)
	c.build, c.code = a2-a1, a3-a2
	// The compiles for the host timed with no collection running either,
	// as the lowerings are: a collection marks the whole heap, the
	// program's n values' code among it, and the compile's allocation,
	// n's too, sets collections off, so what they took grew as n² and
	// was the collector's time, not the compiler's.
	runtime.GC()
	func() {
		defer holdCollectorNow(t)()
		for k := range 3 {
			t0 := time.Now()
			if _, err := mir.CompileWith(new(mir.Workspace), f, jitEncoding, opts); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(t0); k == 0 || d < c.backend {
				c.backend = d
			}
		}
		// Its work: each phase's steps and what it allocated.
		tm := &mir.Times{Clock: func() int64 { return int64(alloc()) }}
		wopts := opts
		wopts.Timing = tm
		tm.Start()
		if _, err := mir.CompileWith(new(mir.Workspace), f, jitEncoding, wopts); err != nil {
			t.Fatal(err)
		}
		c.steps, c.bytes = tm.Steps, tm.Ticks
	}()
	c.live = p.Live.Size()
	if len(mc.Exits) > 0 && mc.Exits[0].Frame != nil {
		x := mc.Exits[0].Frame
		c.exits = len(x.Tree)*4 + len(x.Slots)*28
	}
	return c
}

// A function's compile takes memory linear in its size, however its values
// are live: n values live across n ends of branches (blocks), whose first
// states had read every live value and made a phi of each at every one;
// n values live at once with an exit after each of n reads that survives
// Optimize (guards), whose exits' descriptions had each listed every
// value still live; and n values live at once past every register
// (spill). Each of the lowering -- its liveness had been a row of every
// register at every instruction -- the build and Optimize, and the
// compile for the host takes some eight times the memory for eight times
// the values, not sixty-four, and what the program keeps of its liveness
// and the code keeps of its exits' descriptions grows as much: up to
// twice that, where the shared sets' trees take a level more, for each
// sixteen times the slots (ir.Liveness, ssa's slots.go).
//
// And each phase of the compile for the host does some eight times the
// work for eight times the values, not sixty-four: its steps in the loops
// whose length is not the item's own (mir.Times' Steps) and what it
// allocates, at most three and two times eight. The allocator's liveness
// of values over blocks had walked each value's blocks back from its
// uses, as many steps as values are live in each block summed over the
// blocks -- n values live across 2n blocks 2n² steps, 40 s of a compile
// of 16,000 -- where its sets now take what changes from block to block
// (mir's blockLiveness); and n loops one after another (loops) had each
// cleared a table of every block and looked at every block, to find its
// own and to give its budget register an interval, n² steps (findBudgets);
// and loops nested n/10 deep (deep, deepdo, breaks) had each loop's walk
// take the loops inside it, d²/2 loops' blocks for a nest of d, to find
// that only the innermost could count its budget in a register.
// n variables a loop carries, each given the next's value every turn or
// the one before's (rotate, rotateback), had their back edge's parallel
// move made a pass over the pending moves at a time, a pass for each move
// of the chain listed against its order and a removal from the list for
// each move made, n² steps (mir's phiSchedule); and the one before's had
// the phis' spill classes walk each member's way to its component's root
// again, a chain the pairs' links make in their order, n²/2 steps
// (spillClasses.prepare). The work, not the time: the work is linear, and its time is not where
// the larger function leaves the caches -- a pass over every value of the
// blocks' shape took 0.66 ns a value at 1,000 values and 4.78 at 8,000 --
// nor where the machine is busy, where the ratio had been 29 to 39 times.
// The allocation check is off (mir.CheckAllocations), whose liveness
// computed again is the dense sets' still.
func TestJITCompileLinear(t *testing.T) {
	if !jitSSABackend || testing.Short() {
		t.Skip("no SSA backend on this architecture, or short")
	}
	plain := func(*Runtime) []Value { return []Value{Float(3), Float(2)} }
	shapes := []struct {
		name string
		src  func(string, int) string
		args func(r *Runtime) []Value
	}{
		{"blocks", linearBlocksSource, plain},
		{"guards", linearGuardsSource, plain},
		{"spill", func(name string, n int) string { return spillMoreHelpers + spillMoreSource(name, n, 150) }, func(r *Runtime) []Value {
			return []Value{Float(1.25), Float(0.5), Float(7), spillMoreObject(r, 0), Str(NewString("pp")), Float(2)}
		}},
		{"loops", linearLoopsSource, plain},
		{"deep", linearDeepSource(false), plain},
		{"deepdo", linearDeepSource(true), plain},
		{"breaks", linearBreaksSource, plain},
		{"groups", linearGroupsSource, plain},
		{"stores", linearStoresSource, plain},
		{"rotate", linearRotateSource(false), plain},
		{"rotateback", linearRotateSource(true), plain},
	}
	const small, big = 1000, 8000
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			var cs [2]linearCompile
			for k, n := range []int{small, big} {
				name := fmt.Sprintf("%s%d", sh.name, n)
				cs[k] = linearCompileOf(t, sh.src(name, n), name, sh.args)
			}
			s, b := cs[0], cs[1]
			t.Logf("%d values: lower %.1f MB, build %.1f MB, code %.1f MB, %v (backend %v), liveness %d bytes, exits %d bytes",
				small, float64(s.lower)/(1<<20), float64(s.build)/(1<<20), float64(s.code)/(1<<20), s.took.Round(time.Millisecond), s.backend.Round(time.Millisecond), s.live, s.exits)
			for p := mir.Phase(0); p < mir.NumPhases; p++ {
				t.Logf("%-9s steps %9d -> %9d (%5.2fx), bytes %10d -> %10d (%5.2fx)", p, s.steps[p], b.steps[p], float64(b.steps[p])/float64(max(s.steps[p], 1)), s.bytes[p], b.bytes[p], float64(b.bytes[p])/float64(max(s.bytes[p], 1)))
			}
			t.Logf("%d values: lower %.1f MB, build %.1f MB, code %.1f MB, %v (backend %v), liveness %d bytes, exits %d bytes",
				big, float64(b.lower)/(1<<20), float64(b.build)/(1<<20), float64(b.code)/(1<<20), b.took.Round(time.Millisecond), b.backend.Round(time.Millisecond), b.live, b.exits)
			for _, c := range []struct {
				what       string
				small, big uint64
			}{
				{"the lowering", s.lower, b.lower},
				{"the build", s.build, b.build},
				{"the compile", s.code, b.code},
				{"the liveness", uint64(s.live), uint64(b.live)},
				{"the exits' descriptions", uint64(s.exits), uint64(b.exits)},
			} {
				if c.big > 16*c.small {
					t.Errorf("%s: eight times the values took %.1f times the memory", c.what, float64(c.big)/float64(c.small))
				}
			}
			for p := mir.Phase(0); p < mir.NumPhases; p++ {
				if b.steps[p] > 24*max(s.steps[p], 1) {
					t.Errorf("the compile's %s: eight times the values took %.1f times the steps", p, float64(b.steps[p])/float64(max(s.steps[p], 1)))
				}
				if b.bytes[p] > 16*max(s.bytes[p], 1<<10) {
					t.Errorf("the compile's %s: eight times the values allocated %.1f times the bytes", p, float64(b.bytes[p])/float64(max(s.bytes[p], 1)))
				}
			}
		})
	}
}

// A function is compiled at the default budget whatever its size, up to
// what its code and its program hold: the estimate of what a compile will
// hold (jitCompileBytes) is linear in the function's instructions and
// slots, and no less than what the compile of each shape below holds,
// nor more than eight times it. It had been a word for each slot at each
// instruction, of 256 slots at least, and refused these at 8 MiB, the
// default budget then: 3,000 locals of a few instructions each, and a loop of 1,000
// values in blocks of their own, some 10,000 instructions. Each answer is
// the interpreter's.
func TestJITCompileEstimateLinear(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	shapes := []struct {
		name, src string
		args      func(r *Runtime) []Value
	}{
		{"many3000", manySlotsHelpers + manySlotsSource("many3000", 3000), func(*Runtime) []Value { return []Value{Float(1), Float(1)} }},
		{"blocks1000", linearBlocksSource("blocks1000", 1000), func(*Runtime) []Value { return []Value{Float(3), Float(2)} }},
	}
	// The old estimate, of a slot of each instruction's for each of the
	// function's, at least 256.
	old := func(fn *bytecode.Function) int64 {
		rc := firstReg(fn).rc
		return int64(len(rc.Code))*int64(max(rc.NumRegs+len(fn.Upvalues), 256))*8 + 32<<10
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			rts := regNativeRuntimes(t, sh.src)
			r := rts[0]
			if r.jitMemoryLimit != 0 || r.meter != nil || r.jitBudget() != jitCacheBytes {
				t.Fatalf("not the default budget: %d", r.jitBudget())
			}
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern(sh.name))
			cl := fn.Object().fn().closure
			est := jitCompileBytes(cl.fn)
			if o := old(cl.fn); o <= 8<<20 {
				t.Fatalf("%d instructions of %d slots: the old estimate, %d, compiled it already", len(firstReg(cl.fn).rc.Code), firstReg(cl.fn).rc.NumRegs, o)
			}
			res := r.RegNativeCall(fn, sh.args(r), false)
			f1, _ := rts[1].GetProp(Obj(rts[1].Global()), rts[1].Intern(sh.name))
			v, err := rts[1].Call(f1, Undefined, sh.args(rts[1]))
			if got, want := regNativeShow(r, res.Value, res.Err), regNativeShow(rts[1], v, err); res.Skipped != "" || got != want {
				t.Fatalf("natively %q (%s), called %q", got, res.Skipped, want)
			}
			e := r.jit.regCache[r.jitRegKeyOf(cl)]
			if e == nil || e.code == nil {
				t.Fatal("not compiled")
			}
			held := int64(e.bytes)
			if l := r.jit.regLowered[e.key]; l != nil {
				held += int64(l.bytes)
			}
			t.Logf("%d instructions, %d slots: estimate %d KB, held %d KB; the old estimate %d KB", len(firstReg(cl.fn).rc.Code), firstReg(cl.fn).rc.NumRegs, est>>10, held>>10, old(cl.fn)>>10)
			if est < held || est > 8*held {
				t.Errorf("estimate %d bytes for a compile that holds %d", est, held)
			}
		})
	}
}
