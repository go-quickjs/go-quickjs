//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

// jitCallsSource is AWFY's List and Towers, and calls like theirs: values
// read from cells kept across the calls after them (ssa's keep.go), call
// results used and dropped, numbers boxed as arguments, receivers, and
// objects made and popped while calls run.
const jitCallsSource = `
	class Element { constructor(v) { this.val = v; this.next = null } }
	class List {
		makeList(n) { if (n === 0) return null; const e = new Element(n); e.next = this.makeList(n - 1); return e }
		isShorterThan(x, y) {
			let xTail = x, yTail = y
			while (yTail !== null) {
				if (xTail === null) return true
				xTail = xTail.next
				yTail = yTail.next
			}
			return false
		}
		tail(x, y, z) {
			if (this.isShorterThan(y, x)) return this.tail(this.tail(x.next, y, z), this.tail(y.next, z, x), this.tail(z.next, x, y))
			return z
		}
		run() { const r = this.tail(this.makeList(13), this.makeList(7), this.makeList(5)); let n = 0; for (let e = r; e; e = e.next) n++; return n }
	}
	class Disk { constructor(size) { this.size = size; this.next = null } }
	class Towers {
		constructor() { this.piles = [null, null, null]; this.moves = 0 }
		pushDisk(disk, pile) { const top = this.piles[pile]; if (top && disk.size >= top.size) throw new Error("bad"); disk.next = top; this.piles[pile] = disk }
		popDiskFrom(pile) { const top = this.piles[pile]; if (top === null) throw new Error("empty"); this.piles[pile] = top.next; top.next = null; return top }
		moveTopDisk(fromPile, toPile) { this.pushDisk(this.popDiskFrom(fromPile), toPile); this.moves++ }
		moveDisks(disks, fromPile, toPile) {
			if (disks === 1) { this.moveTopDisk(fromPile, toPile) } else {
				const otherPile = (3 - fromPile) - toPile
				this.moveDisks(disks - 1, fromPile, otherPile)
				this.moveTopDisk(fromPile, toPile)
				this.moveDisks(disks - 1, otherPile, toPile)
			}
		}
		run(n) { for (let i = n - 1; i >= 0; i--) this.pushDisk(new Disk(i), 0); this.moveDisks(n, 0, 1); return this.moves }
	}
	function pair(a, b) { return { a, b } }
	function mix(o, n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			const p = o.p, q = o.q
			pair(p, i * 0.5)
			const r = pair(q, i + 0.25)
			o.p = q; o.q = p
			s += r.b + p.v - q.v
		}
		return s
	}
	function round() {
		const o = { p: { v: 1 }, q: { v: 2 } }
		return new List().run() + "/" + new Towers().run(9) + "/" + mix(o, 50) + "/" + o.p.v
	}`

// jitCallsCheck runs jitCallsSource's round in r and in the interpreter,
// the same answer each time, and has List's tail and Towers' moveDisks
// called natively.
func jitCallsCheck(t *testing.T, r *Runtime, rounds int) {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, jitCallsSource)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range rounds {
		wv, err := want.Run(compileForTest(t, `round()`))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, `round()`))
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	for _, m := range []struct{ class, method string }{{"List", "tail"}, {"Towers", "moveDisks"}} {
		class, err := r.Run(compileForTest(t, m.class+".prototype."+m.method))
		if err != nil || !class.IsObject() {
			t.Fatalf("%s.%s: %v, %v", m.class, m.method, class, err)
		}
		fn := class.Object().fn().closure.fn
		if e := r.jit.cache[weak.Make(fn)]; e == nil || e.ssa == nil || e.link.in == 0 {
			t.Fatalf("%s.%s is not called natively: %+v", m.class, m.method, e)
		}
	}
}

// Native calls, and the keeps around them, give the interpreter's answers
// with the collector running throughout, with its write-barrier flag set
// throughout (each keep and each call's result through Go's barrier), and
// with every loop leaving native code at each back edge.
func TestJITCallsUnderCollector(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, mode := range []string{"plain", "collecting", "marking", "budget"} {
		t.Run(mode, func(t *testing.T) {
			switch mode {
			case "collecting":
				var stop atomic.Bool
				done := make(chan struct{})
				go func() {
					defer close(done)
					for !stop.Load() {
						runtime.GC()
					}
				}()
				defer func() { stop.Store(true); <-done }()
			case "marking":
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			if mode == "budget" {
				r.jitStress = jitStressConfig{threshold: true, budget: 1}
			}
			jitCallsCheck(t, r, 30)
		})
	}
}

// A value a call reads back from the frame after it, rather than saving
// (mir's frameResident), is one its slot holds throughout: where the call's
// state has the slot hold something else -- b is a's value at entry, and a
// is assigned before the call -- Go writes that there should the callee
// leave native code, which, every back edge an exit, g does at each call,
// and the caller goes on natively after it: b is saved instead.
func TestJITCallsReadBackFromFrame(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function g(n) { let s = 0; for (let i = 0; i < n; i++) s += i; return s }
		class C {
			constructor(k) { this.k = k }
			h(a, n) { let b = a; a = n; const r = g(a); return b * 1000 + r + a + this.k }
			m(a, n) { const r = g(n); return a * 1000 + r + this.k }
		}
		function run() { const c = new C(7); let t = 0; for (let i = 0; i < 40; i++) t += c.h(i * 0.5, 3) + c.m(i, 4); return t }`
	for _, budget := range []uint16{0, 1} {
		t.Run(fmt.Sprintf("budget %d", budget), func(t *testing.T) {
			want := New(Config{})
			t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitStress = jitStressConfig{threshold: true, budget: budget}
			for _, rt := range []*Runtime{want, r} {
				if _, err := rt.Run(compileForTest(t, setup)); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 20 {
				wv, err := want.Run(compileForTest(t, `""+run()`))
				if err != nil {
					t.Fatal(err)
				}
				gv, err := r.Run(compileForTest(t, `""+run()`))
				if err != nil {
					t.Fatalf("round %d: %v", i, err)
				}
				if got, want := gv.String().Go(), wv.String().Go(); got != want {
					t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
				}
			}
			class, err := r.Run(compileForTest(t, `C.prototype.h`))
			if err != nil || !class.IsObject() {
				t.Fatal(class, err)
			}
			if e := r.jit.cache[weak.Make(class.Object().fn().closure.fn)]; e == nil || e.ssa == nil {
				t.Fatalf("C.prototype.h has no native code: %+v", e)
			}
		})
	}
}

// A call's state describes each value where Go finds it should the callee
// leave native code (mir's callState): a box live past the call, by the
// slot the call saves it in -- the box's own save, found by the value
// (callWord). A box the call does not read, which the state names but
// uses only through its operand, may have ended before the call, its
// register another value's that the call saves: found by that register,
// the loops' counters here, boxed in their headers, had Go make the frame
// with a keep cell's address for the counter, which ran on, allocating,
// until the OS ended it (darwin/arm64). Each callee throws at the last
// iteration, which Go unwinds through the caller's frame it makes.
func TestJITCallStateFindsBoxesSaved(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		class A { constructor(x) { this.x = x } }
		class NoSuper extends A { constructor(n) { if (!n) super(1) } }
		class RetPrim extends A { constructor(p) { super(1); if (p) return 5 } }
		class Twice extends A { constructor(t) { super(1); if (t) super(2) } }
		function boom(k) { if (k) throw new Error("boom"); return 1 }
		const o = { f: boom }
		function errs(C, n) {
			let last = ''
			for (let i = 0; i < n; i++) {
				try { last = JSON.stringify(new C(i === n - 1)) } catch (e) { last = e.name + ': ' + i }
			}
			return last
		}
		function calls(n) {
			let last = ''
			for (let i = 0; i < n; i++) {
				try { last = '' + o.f(i === n - 1) + i } catch (e) { last = e.message + ': ' + i }
			}
			return last
		}
		function run() { return [errs(NoSuper, 50), errs(RetPrim, 50), errs(Twice, 50), calls(50)].join() }`
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r.SetContext(ctx)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 6 {
		wv, err := want.Run(compileForTest(t, `run()`))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, `run()`))
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
}
