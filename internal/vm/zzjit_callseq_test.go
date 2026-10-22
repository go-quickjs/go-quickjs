//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"sync/atomic"
	"testing"
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
