//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"slices"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitInlinedForTest records, for each function compiled while the test
// runs, the closures its last compile inlined (ssa.Func's Inlined).
func jitInlinedForTest(t *testing.T) map[string][]uintptr {
	t.Helper()
	got := map[string][]uintptr{}
	jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
		got[fn.Name] = slices.Clone(f.Inlined)
	}
	t.Cleanup(func() { jitSSAOptimized = nil })
	return got
}

// jitNoInlineForTest has the functions named name -- every function
// for "" -- not inlined while the test runs (jitNoInline).
func jitNoInlineForTest(t *testing.T, name string) {
	jitNoInline = func(fn *bytecode.Function) bool { return name == "" || fn.Name == name }
	t.Cleanup(func() { jitNoInline = nil })
}

// jitClosureForTest is the closure of the function the script's global
// expression src gives.
func jitClosureForTest(t *testing.T, r *Runtime, src string) uintptr {
	t.Helper()
	v, err := r.Run(compileForTest(t, src))
	if err != nil || !v.IsObject() || v.Object().fn() == nil {
		t.Fatalf("%s: %v %v", src, v, err)
	}
	return uintptr(unsafe.Pointer(v.Object().fn().closure))
}

const inlineThrowSetup = `
	function Pile() { this.items = []; this.n = 0 }
	Pile.prototype.push = function (v) {
		if (v < 0) throw new RangeError('negative ' + v + ' at ' + this.n)
		this.items[this.n] = v
		this.n = this.n + 1
	}
	Pile.prototype.pop = function () {
		if (this.n === 0) throw new Error('empty')
		this.n = this.n - 1
		return this.items[this.n]
	}
	Pile.prototype.move = function (o) { o.push(this.pop()) }
	Pile.prototype.check = function (v) { if (v > this.n) throw v; return v }
	function fill(p, n) { for (let i = 0; i < n; i++) p.push(i); return p.n }
	function shuffle(a, b, n) { let s = 0; for (let i = 0; i < n; i++) { a.move(b); b.move(a); s += b.n + a.n } return s }
	function pushAll(p, vs) {
		let out = ''
		for (let i = 0; i < vs.length; i++) {
			try { p.push(vs[i]); out += p.pop() } catch (e) { out += e.name + ':' + e.message + '|' + e.stack } finally { out += '|fin;' }
		}
		return out
	}
	function drain(p, k) { let s = ''; for (let i = 0; i < k; i++) { try { s += p.pop() + ',' } catch (e) { s += e.name + ':' + e.message + '|' + e.stack + ';' } } return s }
	function moves(a, b, k) { let s = 0; for (let i = 0; i < k; i++) { a.move(b); s += b.n } return s }
	function checks(p, k) { let s = 0; for (let i = 0; i < k; i++) { try { s += p.check(i) } catch (e) { s += 1000 * e } } return s }
	var a = new Pile(), b = new Pile()
	fill(a, 50); fill(b, 50)
`

// A callee whose throw, or whose call it has never been seen to make, is
// on a path seldom taken is inlined all the same, as V8 inlines one
// (ssa.InlineExitFeedback): native code leaves the inlined frame there, Go
// making its frame, and the interpreter throws -- the exception caught by
// the caller's try and its finally run, the stack trace the interpreter's,
// with the callee's frame and the caller's, this and the arguments as the
// callee had them. Before, a throw new Error(...) guard had kept each
// callee from being inlined, and the caller that called it with it. In the
// warm rounds, each callee inlined, no call leaves native code.
func TestJITInlinesColdThrows(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	var rounds []string
	for range 4 {
		rounds = append(rounds, `shuffle(a, b, 200)`, `pushAll(a, [1, 2, 3])`, `checks(a, 20)`)
	}
	rounds = append(rounds,
		// The throws: the callee inlined at each, the frame Go makes for
		// it, the catch and the finally the caller's.
		`pushAll(a, [4, -5, 6, -7])`,
		`drain(new Pile(), 3)`,
		`checks(a, 300)`,
		`moves(new Pile(), a, 3)`,
		// Uncaught, out of an inlined frame through the callers'.
		`moves(new Pile(), a, 2)`,
		`shuffle(a, b, 10)`)
	both := jitBothForTest(t, inlineThrowSetup)
	both.agree(t, rounds[:12]...)
	r := both.jit
	push := jitClosureForTest(t, r, `Pile.prototype.push`)
	pop := jitClosureForTest(t, r, `Pile.prototype.pop`)
	move := jitClosureForTest(t, r, `Pile.prototype.move`)
	check := jitClosureForTest(t, r, `Pile.prototype.check`)
	for _, c := range []struct {
		fn   string
		want []uintptr
	}{{"shuffle", []uintptr{pop, push, move}}, {"pushAll", []uintptr{push}}, {"checks", []uintptr{check}}} {
		for _, w := range c.want {
			if !slices.Contains(inlined[c.fn], w) {
				t.Errorf("%s inlines %x, not %x of %x, %x, %x, %x", c.fn, inlined[c.fn], w, push, pop, move, check)
			}
		}
	}
	hosts := r.jit.hosts
	both.agree(t, `shuffle(a, b, 1000)`, `pushAll(a, [1, 2, 3, 4, 5])`, `checks(a, 50)`)
	if n := r.jit.hosts - hosts; n != 0 {
		t.Errorf("the inlined callees left native code %d times", n)
	}
	// The throws, each answer the interpreter's.
	both.agree(t, rounds[12:]...)
}

// jitBoth is a runtime with the JIT and one without, which have run the
// same script.
type jitBoth struct{ jit, want *Runtime }

// jitBothForTest is a jitBoth that has run setup.
func jitBothForTest(t *testing.T, setup string) jitBoth {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	return jitBoth{jit: r, want: want}
}

// script runs src, a script, in both.
func (b jitBoth) script(t *testing.T, src string) {
	t.Helper()
	for _, rt := range []*Runtime{b.want, b.jit} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
}

// agree runs each of rounds in both, and fails unless each gives what the
// interpreter gives, or throws what it throws.
func (b jitBoth) agree(t *testing.T, rounds ...string) {
	t.Helper()
	for _, src := range rounds {
		src = "''+(" + src + ")"
		wv, werr := b.want.Run(compileForTest(t, src))
		gv, gerr := b.jit.Run(compileForTest(t, src))
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%s: error %v, interpreter's %v", src, gerr, werr)
		}
		if werr != nil {
			if got, want := gerr.Error(), werr.Error(); got != want {
				t.Fatalf("%s: error %q, interpreter's %q", src, got, want)
			}
			continue
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%s: got %s, interpreter %s", src, got, want)
		}
	}
}

// An inlined callee's speculations are those its own code learned: a
// failure its own code met is built generic where it is inlined too, as
// it is in its own code (jitFeedback's Generic, ownEntry). take's read of
// t.next, an object stored as an element, had been read for a number in
// every caller that inlined take, though take's own code had learned it
// was not one: each call left native code, until the call was no longer
// inlined -- AWFY Towers' popDiskFrom.
func TestJITInlinedCalleeKeepsItsFailures(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	both := jitBothForTest(t, `
		function D(v) { this.v = v; this.next = null }
		function Q() { this.slots = [null, null] }
		Q.prototype.take = function (i) { const t = this.slots[i]; this.slots[i] = t.next; t.next = null; return t }
		Q.prototype.put = function (i, d) { d.next = this.slots[i]; this.slots[i] = d }
		function own(q, n) { const take = Q.prototype.take; let s = 0; for (let k = 0; k < n; k++) { const d = take.call(q, 0); s += d.v; q.put(0, d) } return s }
		function cycle(q, n) { let s = 0; for (let k = 0; k < n; k++) { q.put(1, q.take(0)); q.put(0, q.take(1)); s += q.slots[0].v } return s }
		var q = new Q()
		for (let i = 0; i < 4; i++) q.put(0, new D(i))`)
	// take's own code first, called natively by own, never inlined there.
	both.agree(t, `own(q, 50)`, `own(q, 50)`)
	r := both.jit
	take := jitClosureForTest(t, r, `Q.prototype.take`)
	takeV, err := r.Run(compileForTest(t, `Q.prototype.take`))
	if err != nil {
		t.Fatal(err)
	}
	if e := r.jit.entryFor(takeV.Object().fn().closure.fn); e == nil || len(e.failed) == 0 {
		t.Fatalf("take's own code learned no failure: %+v", e)
	}
	both.agree(t, `cycle(q, 100)`, `cycle(q, 100)`)
	hosts := r.jit.hosts
	both.agree(t, `cycle(q, 1000)`)
	if n := r.jit.hosts - hosts; n != 0 {
		t.Errorf("cycle left native code %d times", n)
	}
	if !slices.Contains(inlined["cycle"], take) {
		t.Errorf("cycle inlines %x, not take %x", inlined["cycle"], take)
	}
	if e := jitNativeForTest(t, r, "cycle"); len(e.notInline) != 0 {
		t.Errorf("cycle's calls at %v left native code until they were not inlined", e.notInline)
	}
}

// A callee that reads globals is inlined with its reads, made as the
// caller's own are, from the callee's own sites (jitFeedback's Global, of
// the callee's closure, whose scope a native call's target shares
// (jitNativeTargetOf)): a var, a let, a const, Math.abs. What the script
// does to the bindings after is seen where they are read: a var assigned,
// a let given a string, a global property a later script's let shadows.
func TestJITInlinesGlobalReads(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	both := jitBothForTest(t, `
		var K = 3; let L = 4; const C = 5; globalThis.M = 1
		function g(x) { return x * K + L + C + M + Math.abs(x - 50) }
		function f(n) { let s = 0; for (let i = 0; i < n; i++) s += g(i); return s }`)
	both.agree(t, `f(100)`, `f(100)`, `f(100)`)
	r := both.jit
	if g := jitClosureForTest(t, r, `g`); !slices.Contains(inlined["f"], g) {
		t.Fatalf("f inlines %x, not g %x", inlined["f"], g)
	}
	hosts := r.jit.hosts
	both.agree(t, `f(1000)`)
	if n := r.jit.hosts - hosts; n != 0 {
		t.Errorf("f left native code %d times", n)
	}
	both.agree(t, `K = 7, f(10)`, `L = 'x', f(3)`, `L = 1, f(10)`, `f(100)`)
	both.script(t, `let M = 9`)
	both.agree(t, `f(10)`, `f(100)`, `M = 10, f(100)`)
}

const inlineUpvaluesSetup = `
	var mod = (function () {
		const NONE = null, LIMIT = 3
		let count = 0, last = 'start'
		class Node {
			constructor(v) { this.v = v; this.next = NONE }
			// Reads what the module's closure captured, assigns count.
			isEnd() { return this.next === NONE }
			bump() { count = count + 1; last = this.v; return count }
			check() { if (this.v > LIMIT * 100) throw new RangeError('past ' + LIMIT + ' at ' + count); return this.v }
		}
		function walk(n, k) {
			let s = 0
			for (let i = 0; i < k; i++) {
				// count assigned and read here, assigned by the inlined bump
				// between, and read again.
				count = count + 2
				const before = count
				s += n.isEnd() ? 1 : 2
				s += n.bump() - before
				s += n.check() + count
			}
			return s + ':' + count + ':' + last
		}
		function tdz(k) {
			let s = 0
			for (let i = 0; i < k; i++) s += late()
			return s
		}
		function late() { return later }
		function reset() { count = 0 }
		return { Node, walk, tdz, late, reset, get count() { return count } }
	})()
	var n1 = new mod.Node(5), n2 = new mod.Node(7)
	n1.next = n2
	function made(k) {
		let s = 0
		for (let i = 0; i < k; i++) {
			const j = i
			// A closure made anew at each turn: the inlined call's check of
			// its object fails, and it is called by its code from then on.
			const f = () => j * 2
			s += f()
		}
		return s
	}
	function walks(k) { try { return mod.walk(n1, k) + '|' + mod.walk(n2, k) } catch (e) { return e.name + ':' + e.message + '|' + e.stack } }
`

// A callee that reads and assigns bindings it captured -- a module's
// constants, a let it counts in -- is inlined, its reads and assignments
// made in the cells of its closure, which the call's check of its function
// object fixes (ssa.InlineSite's Upvalues): it had been called natively,
// every class method of a module's code with it. An assignment has the
// caller read its own captured bindings again, which may be the same
// cells. Each answer is the interpreter's: the count read across the
// inlined assignment, a throw from an inlined method with its stack, a
// global that is not there, a closure made anew at each call.
func TestJITInlinesCapturedBindings(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	both := jitBothForTest(t, inlineUpvaluesSetup)
	for range 3 {
		both.agree(t, `walks(50)`, `made(50)`)
	}
	r := both.jit
	for _, m := range []string{"isEnd", "bump", "check"} {
		if c := jitClosureForTest(t, r, `mod.Node.prototype.`+m); !slices.Contains(inlined["walk"], c) {
			t.Errorf("walk inlines %x, not %s %x", inlined["walk"], m, c)
		}
	}
	hosts := r.jit.hosts
	both.agree(t, `walks(1000)`)
	if n := r.jit.hosts - hosts; n != 0 {
		t.Errorf("walk left native code %d times", n)
	}
	both.agree(t, `mod.count`, `(n2.v = 500, walks(10))`, `(n2.v = 7, mod.reset(), walks(20))`, `made(100)`, `made(100)`,
		`(function () { try { return mod.tdz(3) } catch (e) { return e.name + ':' + e.message } })()`)
}

// A closure the code makes natively has a keep cell taken when the build
// is planned (ssa's closureKeep), as a literal's is: taken only when the
// closure was built, after the calls planned before it had taken theirs,
// it could be one too many in a build whose cells are limited -- a
// function whose cells live at once are more than the context has, built
// again within them (abi.MaxKeeps) -- the closure then left to an exit
// planning had made no block for, and made by no one: its local stayed
// undefined, and a call of it threw. Octane's Typescript, whose
// parseStatement inlines enough to need more cells than that, threw so in
// one run of three. Each answer is the interpreter's, every build limited
// to one cell, two, four: closures made at the start, a loop's closures
// of its iteration's binding, which a scope's end closes.
func TestJITClosuresWithKeepsLimited(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function P() { this.n = 1 }
		P.prototype.parse = function (k, parent) {
			var modifiers = 0
			function isA() { return (modifiers & 2) != 0 || (parent & 2) != 0 }
			function mark() { modifiers |= 1 }
			var acc = 0
			for (var i = 0; i < 4; i++) {
				if (isA()) acc++
				if ((k + i) % 3 == 0) mark()
				modifiers |= i
				acc += this.n + modifiers
			}
			return acc
		}
		function lets(n) { const fs = []; for (let i = 0; i < n; i++) { const j = i * 2; fs.push(() => i + j) } let s = 0; for (const f of fs) s += f(); return s }
		function drive(n) { const p = new P(); let s = 0; for (let i = 0; i < n; i++) s += p.parse(i, i & 2) + lets(i % 5); return s }`
	for _, limit := range []int{1, 2, 4} {
		jitKeepLimit = limit
		both := jitBothForTest(t, setup)
		r := both.jit
		r.jitCallThreshold, r.jitICThreshold = jitInvocationsForMaglev, jitInvocationsAfterICUpdate
		for range 6 {
			both.agree(t, `drive(200)`)
		}
		jitKeepLimit = 0
		v, err := r.Run(compileForTest(t, `P.prototype.parse`))
		if err != nil || !v.IsObject() || v.Object().fn() == nil {
			t.Fatalf("P.prototype.parse: %v %v", v, err)
		}
		if e := r.jit.hint(v.Object().fn().closure); e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
			t.Fatalf("keep cells %d: parse never ran natively: %+v", limit, e)
		}
	}
}

// A call is left to an exit in an inlined callee only where the callee's
// own code leaves it to Go -- at an exit, or by a call of Go -- and has
// never left there (ssa.Func's HostsLeft, jitInlineExit): one its own code
// makes natively, without leaving, which counts nothing -- a substring of
// one character, an array's push -- is no seldom one, and the callee is
// not inlined for it. Taken for one, AWFY Json's read, inlined with an exit
// at its substring, left native code at every call until it was no longer
// inlined, and taught its own code a polymorphic read it never met. Their
// own code runs first here, called natively, enough to tell (jitCodeRan).
func TestJITInlinedExitsOnlyWhereSeldom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	both := jitBothForTest(t, `
		class S {
			constructor(s) { this.s = s; this.i = 0; this.c = '' }
			next() { this.i = (this.i + 1) % this.s.length; this.c = this.s.substring(this.i, this.i + 1) }
			keep(a) { a.push(this.i) }
		}
		function scan(o, m) { let n = 0; for (let k = 0; k < m; k++) { o.next(); if (o.c === 'b') n++ } return n }
		function keeps(o, m) { const a = []; for (let k = 0; k < m; k++) o.keep(a); return a.length }
		function warm(o, m) { const a = []; for (let k = 0; k < m; k++) { o.next(); o.keep(a) } return a.length }
		var o = new S('abcabc')`)
	// Their own code run first, called natively by warm, which does not
	// inline them: run enough to tell (jitCodeRan) that it has never left
	// at its substring or its push.
	jitNoInline = func(fn *bytecode.Function) bool { return fn.Name == "next" || fn.Name == "keep" }
	for range 4 {
		both.agree(t, `warm(o, 100)`)
	}
	jitNoInline = nil
	for range 4 {
		both.agree(t, `scan(o, 100)`, `keeps(o, 100)`)
	}
	r := both.jit
	// Never inlined with an exit at the call, which would have left at
	// every call until the call was no longer inlined (notInline).
	for _, fn := range []string{"scan", "keeps"} {
		if e := jitNativeForTest(t, r, fn); len(e.notInline) != 0 {
			t.Errorf("%s's calls at %v left native code until they were not inlined", fn, e.notInline)
		}
	}
	for _, c := range []struct{ fn, m string }{{"scan", "next"}, {"keeps", "keep"}} {
		if m := jitClosureForTest(t, r, `S.prototype.`+c.m); slices.Contains(inlined[c.fn], m) {
			t.Errorf("%s inlines %s, whose call its own code makes natively, with an exit there", c.fn, c.m)
		}
	}
}

// A call a callee's own code leaves to Go and has not made is seldom only
// where that code has run enough to tell (jitCodeRan): a callee inlined
// wherever it is called -- a static factory, Item.make here, as AWFY
// DeltaBlue's Variable.value -- has code compiled for native callers that
// never runs, and its construction had been taken for a seldom one, the
// callee inlined with an exit there, which its callers' loops took at
// every turn until they were compiled again. Each answer is the
// interpreter's; in the warm rounds native code leaves at no call.
func TestJITInlinedExitsOnlyWhereCodeRan(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, `
		// A module's class, which its methods read as a captured binding.
		var Item = (function () {
			class Item {
				constructor(x) { this.x = x; this.y = x * 2; this.s = '' }
				static make(x) { const v = new Item(x); v.s = 'k'; return v }
			}
			return Item
		})()
		function drive(n) { let s = 0; for (let i = 0; i < n; i++) { const a = Item.make(i), b = Item.make(i + 1); s += a.x + b.y + a.s.length } return s }`)
	r := both.jit
	// The default budgets, which code waits to be compiled again for what
	// it learned (jitLearnedDue).
	r.jitCallThreshold, r.jitICThreshold = jitInvocationsForMaglev, jitInvocationsAfterICUpdate
	hosts := r.jit.hosts
	both.agree(t, `drive(3000)`, `drive(3000)`)
	if n := r.jit.hosts - hosts; n > 10 {
		t.Errorf("drive left native code %d times in two rounds of 3000", n)
	}
}

// Inlining's budget (ssa's maxInlineTotal) goes to a function's
// constructions first, of its calls equally often made: one not inlined is
// made by a call of Go wherever its constructor has no code native callers
// may call -- one forwarding its arguments, Prototype's Class.create's --
// where any other call not inlined is a native call. Inlined in the order
// of their PCs, the method calls before paint's construction took every
// bound, and each construction was a call of Go, as Octane's and the V8
// suite's RayTrace's colors and intersections were once their callers'
// callees reading globals could be inlined. Each answer is the
// interpreter's.
func TestJITInlinesConstructionsFirst(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	// Nine methods of 25 instructions each, more than the budget.
	both := jitBothForTest(t, `
		var Class = {create: function () { return function () { this.initialize.apply(this, arguments) } }}
		var C = Class.create()
		C.prototype = {initialize: function (r) { this.r = r * K }}
		var K = 3
		var M = {
			a: function (p) { return (p.x + K) * (p.y - K) + (p.x ^ p.y) - (p.y | K) * p.x },
			b: function (p) { return (p.y + K) * (p.x - K) + (p.y ^ p.x) - (p.x | K) * p.y },
			c: function (p) { return (p.x - K) * (p.y + K) + (p.x & p.y) - (p.y ^ K) * p.x },
			d: function (p) { return (p.y - K) * (p.x + K) + (p.y & p.x) - (p.x ^ K) * p.y },
			e: function (p) { return (p.x * K) + (p.y - K) * (p.x | p.y) - (p.y & K) + p.x },
			f: function (p) { return (p.y * K) + (p.x - K) * (p.y | p.x) - (p.x & K) + p.y },
			g: function (p) { return (p.x + K) - (p.y * K) + (p.x - p.y) * (p.y + K) ^ p.x },
			h: function (p) { return (p.y + K) - (p.x * K) + (p.y - p.x) * (p.x + K) ^ p.y },
			i: function (p) { return (p.x | K) + (p.y & K) * (p.x ^ p.y) - (p.y + K) * p.x },
		}
		function paint(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				const p = {x: i & 15, y: 2}
				s = (s + M.a(p) + M.b(p) + M.c(p) + M.d(p) + M.e(p) + M.f(p) + M.g(p) + M.h(p) + M.i(p)) | 0
				const c = new C(s & 7)
				s = (s + c.r) | 0
			}
			return s
		}`)
	for range 4 {
		both.agree(t, `paint(300)`)
	}
	r := both.jit
	if c := jitClosureForTest(t, r, `C`); !slices.Contains(inlined["paint"], c) {
		t.Errorf("paint inlines %x, not C's constructor %x", inlined["paint"], c)
	}
	goCalls := r.jit.goCalls
	both.agree(t, `paint(1000)`)
	if n := r.jit.goCalls - goCalls; n > 10 {
		t.Errorf("paint made %d calls of Go in 1,000 constructions", n)
	}
}

// jitCountForTest is how many times inlined holds c.
func jitCountForTest(inlined []uintptr, c uintptr) int {
	n := 0
	for _, x := range inlined {
		if x == c {
			n++
		}
	}
	return n
}

// Inlining is bounded by size alone, as V8's is (ssa's maxInlineTotal),
// not by how many calls a function inlines: near inlines delta twice, three
// frames each -- delta, minus, V's constructor -- and every minus and dot
// after them, twelve frames, 170 instructions, so that none of the objects
// minus makes is made. Bounded at eight inlines, as AWFY's CD's
// findIntersection was, near called its later minus natively and made its
// object, from a pool Go filled: 20,000 objects in drive(20000).
func TestJITInlinesBySize(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	inlined := jitInlinedForTest(t)
	both := jitBothForTest(t, `
		function V(x, y, z) { this.x = x; this.y = y; this.z = z }
		V.prototype.minus = function (o) { return new V(this.x - o.x, this.y - o.y, this.z - o.z) }
		V.prototype.dot = function (o) { return this.x * o.x + this.y * o.y + this.z * o.z }
		function M(a, b) { this.a = a; this.b = b }
		M.prototype.delta = function () { return this.b.minus(this.a) }
		var m = new M(new V(1, 2, 3), new V(4, 6, 8)), n = new M(new V(0, 1, 1), new V(5, 3, 2))
		function near(m, n) { const d1 = m.delta(), d2 = n.delta(), e = d2.minus(d1); return e.dot(e) + m.a.minus(n.a).dot(d1) }
		function drive(k) { let s = 0; for (let i = 0; i < k; i++) s += near(m, n) + i; return s }`)
	for range 6 {
		both.agree(t, `drive(300)`)
	}
	r := both.jit
	got := inlined["near"]
	for _, c := range []struct {
		src  string
		want int
	}{{`V`, 4}, {`V.prototype.minus`, 4}, {`M.prototype.delta`, 2}, {`V.prototype.dot`, 2}} {
		if n := jitCountForTest(got, jitClosureForTest(t, r, c.src)); n != c.want {
			t.Errorf("near inlines %s %d times, want %d (%x)", c.src, n, c.want, got)
		}
	}
	both.agree(t, `drive(20000)`)
	if n := jitCallbackMallocs(t, r, `drive(20000)`); n > 200 {
		t.Errorf("drive(20000) allocated %d times", n)
	}
}

// Past a function's budget a small callee (ssa's maxInlineSmall, its own
// callees counted in) is inlined still, as V8 inlines one
// (max_inlined_bytecode_size_small), and a larger one is called natively:
// spend's four bigs, 44 instructions each, and its first mid, 16, take the
// budget, 192; tiny, 5, is inlined past it, the second mid is not, nor
// wrap2, 9 instructions with two tinies inlined in it, 19 in all. Each
// answer is the interpreter's.
func TestJITInlinesSmallPastBudget(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	inlined := jitInlinedForTest(t)
	both := jitBothForTest(t, `
		function big(o) { return (o.a + o.b * 3 - o.c) * (o.b - o.a) + (o.c ^ o.a) - (o.a | o.c) * o.b + (o.c - o.b) * (o.a + o.c) }
		function mid(o) { return (o.a + o.b) * (o.c - o.a) + o.b }
		function tiny(o) { return o.a + 1 }
		function wrap2(o) { return tiny(o) + tiny(o) }
		function spend(o) { return big(o) + big(o) + big(o) + big(o) + mid(o) + tiny(o) + mid(o) + wrap2(o) }
		function drive(k) { let s = 0; for (let i = 0; i < k; i++) s = (s + spend({a: i, b: 2, c: 7})) | 0; return s }`)
	for range 6 {
		both.agree(t, `drive(300)`)
	}
	r := both.jit
	got := inlined["spend"]
	for _, c := range []struct {
		src  string
		want int
	}{{`big`, 4}, {`mid`, 1}, {`tiny`, 1}, {`wrap2`, 0}} {
		if n := jitCountForTest(got, jitClosureForTest(t, r, c.src)); n != c.want {
			t.Errorf("spend inlines %s %d times, want %d (%x)", c.src, n, c.want, got)
		}
	}
}

// Inlining's budget goes to the calls loops hold first, as V8's goes to the
// calls made most often (JSInliningHeuristic's candidates, by frequency):
// loopy's mid2, 31 instructions, in its loop, is inlined, and three of the
// four bigs before the loop, 44 each, with it. In the order of their PCs
// the bigs took the budget, and mid2 was called natively at every turn.
func TestJITInlinesLoopsFirst(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	inlined := jitInlinedForTest(t)
	both := jitBothForTest(t, `
		function big(o) { return (o.a + o.b * 3 - o.c) * (o.b - o.a) + (o.c ^ o.a) - (o.a | o.c) * o.b + (o.c - o.b) * (o.a + o.c) }
		function mid2(o) { return (o.a + o.b) * (o.c - o.a) + o.b * (o.a - o.c) + (o.b ^ o.c) - o.a }
		function loopy(o, n) { let s = big(o) + big(o) + big(o) + big(o); for (let i = 0; i < n; i++) s = (s + mid2(o)) | 0; return s }
		function drive(k) { let s = 0; for (let i = 0; i < k; i++) s = (s + loopy({a: i, b: 2, c: 7}, 20)) | 0; return s }`)
	for range 6 {
		both.agree(t, `drive(300)`)
	}
	r := both.jit
	got := inlined["loopy"]
	for _, c := range []struct {
		src  string
		want int
	}{{`mid2`, 1}, {`big`, 3}} {
		if n := jitCountForTest(got, jitClosureForTest(t, r, c.src)); n != c.want {
			t.Errorf("loopy inlines %s %d times, want %d (%x)", c.src, n, c.want, got)
		}
	}
}

// A megamorphic site's write inside an inlined callee -- an addition the
// megamorphic cache does not hold, an array's length -- is Go's from
// native code where Go needs no frame for it (abi.GoCallState's Inline,
// jitGoCallPlain), as the root's own is: Typescript's new ASTSpan(),
// whose this.minChar = -1 its subclasses' constructions, through
// _super.call(this), made megamorphic, and rd.actuals.length = 0, had left
// native code at each, the interpreter finishing the inlined frames, until
// the calls were inlined no more. A write Go does need a frame for -- a
// setter's, a length that may not be written -- leaves as before, the
// interpreter making it in the inlined frame: each answer, a stack trace
// through the inlined constructor included, is the interpreter's.
func TestJITInlinedMegamorphicWrites(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	inlined := jitInlinedForTest(t)
	setup := `
		'use strict'
		function Span() { this.min = -1; this.lim = -1 }
		var names = []
		for (let i = 0; i < 24; i++) names.push('p' + i)
		function viaCall(n) { let s = 0; for (let i = 0; i < n; i++) { const o = {}; o[names[i % 24]] = i; Span.call(o); s += o.min } return s }
		function RD() { this.a = []; this.b = [] }
		function reset(rd) { rd.a.length = 0; rd.b.length = 0 }
		function spans(n, rd) { let s = 0; for (let i = 0; i < n; i++) { const sp = new Span(); sp.lim = i; s += sp.min + sp.lim; rd.a.push(i); reset(rd); s += rd.a.length + rd.b.length } return s }
		var rd = new RD()
		function guarded(n, rd) { try { return spans(n, rd) } catch (e) { return e.name + ': ' + e.message + ' | ' + e.stack.split('\n').slice(0, 4).join(' | ') } }
	`
	both := jitBothForTest(t, setup)
	r := both.jit
	for range 4 {
		both.agree(t, `viaCall(400)`)
	}
	for range 6 {
		both.agree(t, `spans(400, rd)`)
	}
	span, reset := jitClosureForTest(t, r, `Span`), jitClosureForTest(t, r, `reset`)
	if !slices.Contains(inlined["spans"], span) || !slices.Contains(inlined["spans"], reset) {
		t.Fatalf("spans inlines %x, not Span %x and reset %x", inlined["spans"], span, reset)
	}
	sp := jitNativeForTest(t, r, "spans")
	hosts, entries := sp.ssaStats.hosts, sp.ssaStats.entries
	both.agree(t, `spans(400, rd)`)
	if sp.ssaStats.entries == entries {
		t.Fatalf("spans's code was not entered: slow %v", sp.entrySlow)
	}
	if n := sp.ssaStats.hosts - hosts; n != 0 {
		t.Errorf("spans left native code %d times", n)
	}
	// Writes Go needs a frame for: a setter up the chain, which throws at
	// the hundredth, and a length that may not be written.
	both.script(t, `var seen = 0; Object.defineProperty(Span.prototype, 'lim', { set(v) { if (++seen === 100) throw new RangeError('lim ' + v); this._lim = v }, get() { return this._lim | 0 }, configurable: true })`)
	both.agree(t, `guarded(400, rd)`, `seen`, `guarded(50, rd)`)
	both.script(t, `delete Span.prototype.lim; Object.defineProperty(rd.b, 'length', { writable: false })`)
	both.agree(t, `guarded(400, rd)`, `rd.a.length + ':' + rd.b.length`, `guarded(400, new RD())`)
}
