//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
)

// escapeRandomSetup is what the programs escapeProgram writes use: objects
// of five constructions, one with no properties, whose prototypes hook
// replaces, one of them by a call of Go that native code goes on after, and
// two of them -- R's, and C, a class made anew -- with prototypes no object
// has had, whose first objects keep their prototype in their table; objects
// made on such a prototype by Object.create and a literal's __proto__; an
// accessor hook defines on a prototype for a property the programs write
// and another for one they read; and show, which says of an object all a
// program could tell: its prototype, whether it is frozen, its properties in
// their order.
const escapeRandomSetup = `
	function P(a, b) { this.a = a; this.b = b }
	P.prototype.m = function (o) { return this.a === o }
	P.prototype.add = function (o) { return new P(this.a + o.a, this.b) }
	function Q(a) { this.a = a; if (a === 'x') this.z = 1; this.b = 0 }
	Q.prototype.m = function (o) { return o === this }
	Q.prototype.add = function (o) { return new Q(this.a + 1) }
	function E() {}
	E.prototype.m = function (o) { return 1 }
	E.prototype.add = function (o) { return this }
	var odd = 'x', keep = {}, log = 0
	var altE = { m(o) { return 2 }, add(o) { return o } }
	var altP = { m(o) { return 3 }, add(o) { return this } }
	var altQ = { m(o) { return 4 }, add(o) { return o } }
	function R(a) { this.a = a; this.d = 0 }
	R.prototype.m = function (o) { return 6 }
	R.prototype.add = function (o) { return new R(this.a + 1) }
	function mkC() { return class { constructor(a) { this.a = a; this.b = 1 } m(o) { return 7 } add(o) { return this } } }
	var C = mkC(), fresh = { m(o) { return 8 }, add(o) { return o } }
	var protos = [P.prototype, Q.prototype, E.prototype, altE, altP, altQ], qs = [Q.prototype, altQ]
	protos.slice(3).forEach(p => Object.create(p))
	protos.push(R.prototype)
	function pid(p) {
		let k = protos.indexOf(p)
		if (k < 0) { protos.push(p); k = protos.length - 1 }
		return k
	}
	function hash(s) { for (let i = 0; i < s.length; i++) log = (log * 31 + s.charCodeAt(i)) | 0 }
	function show(v, d) {
		if (v === null || typeof v !== 'object') return typeof v === 'number' ? '' + v : typeof v + ':' + v
		if (d === 0) return '...'
		const p = Object.getPrototypeOf(v)
		let s = (p === Object.prototype ? 'O' : pid(p)) + (Object.isFrozen(v) ? 'f' : '') + '{'
		for (const k of Object.keys(v)) s += k + '=' + show(v[k], d - 1) + ','
		return s + '}'
	}
	function sink(o) { hash(show(o, 3)); if (o && typeof o === 'object' && !Object.isFrozen(o)) o.s = (o.s | 0) + 1 }
	function hook(i, at) {
		if (i === at) { hash('hook'); if (at % 2) E.prototype = altE; else P.prototype = altP }
		else if (i === at + 2) { E.prototype = protos[2]; P.prototype = protos[0] }
		Reflect.set(Q, 'prototype', qs[+(i === at - 1)])
		if (i === at + 1) {
			// Prototypes no object has had: R's next objects, C's, and
			// fresh's are each the first on theirs.
			R.prototype = { m(o) { return 9 }, add(o) { return new R(o.a) } }
			C = mkC()
			fresh = { m(o) { return 10 }, add(o) { return this } }
		} else if (i === at + 3) {
			Object.defineProperty(protos[0], 'd', { set(v) { hash('set:' + v) }, get() { return 5 }, configurable: true })
			Object.defineProperty(qs[0], 'c', { get() { hash('get'); return 11 }, configurable: true })
			// Which R's construction's own addition of d meets.
			Object.defineProperty(R.prototype, 'd', { set(v) { hash('setR:' + v) }, configurable: true })
		} else if (i === at + 5) {
			delete R.prototype.d
			delete protos[0].d
			delete qs[0].c
		}
	}
	function run(n, at, c) {
		let s = ''
		for (let i = 0; i < n; i++) {
			try { s = show(f(i, at, c), 3) } catch (e) { s = 'E:' + e.message }
			hash(s)
		}
		return s + '#' + log + '#' + show(keep, 3)
	}`

// escapeProgram is f(i, at, c), of the program seed gives: objects it makes,
// replaces and merges at joins and round loops, which the writes of it give
// other objects' properties, a value at at that exits where it is read as a
// number; and what needs one made, at some of the paths.
func escapeProgram(seed uint64) string {
	g := &escapeGen{r: rand.New(rand.NewPCG(seed, 17))}
	fmt.Fprintf(&g.b, "function f(i, at, c) {\nlet x0 = new P(i, 1), x1 = { a: 1, b: 2 }, x2 = new P(0, i), n0 = 0, n1 = 1, n2 = 2\n")
	g.block(6 + g.r.IntN(8))
	fmt.Fprintf(&g.b, "return %s\n}\n", g.obj())
	return g.b.String()
}

// escapeRandomRounds calls run: first with nothing out of the way, then with at
// past the warming, so that its exits meet code compiled for none.
func escapeRandomRounds() []string {
	var rounds []string
	for k := range 16 {
		rounds = append(rounds, `run(30, -5, 0)`, `run(30, -5, 1)`)
		if k > 6 {
			rounds = append(rounds, `run(30, `+strconv.Itoa(k*2)+`, 0)`)
		}
	}
	return rounds
}

type escapeGen struct {
	r            *rand.Rand
	b            strings.Builder
	depth, loops int
}

func (g *escapeGen) obj() string { return "x" + strconv.Itoa(g.r.IntN(3)) }
func (g *escapeGen) num() string { return "n" + strconv.Itoa(g.r.IntN(3)) }

func (g *escapeGen) pick(choices ...string) string { return choices[g.r.IntN(len(choices))] }

func (g *escapeGen) value() string {
	return g.pick(strconv.Itoa(g.r.IntN(9)), "i", g.obj()+".a", g.obj()+".b", g.obj()+".c", "(i === at ? odd : "+g.num()+")",
		g.num()+" + 1", g.num(), g.obj())
}

func (g *escapeGen) number() string {
	return g.pick(g.obj()+".a * 1", "+("+g.obj()+".b) | 0", g.num()+" + "+strconv.Itoa(g.r.IntN(5)),
		"("+g.obj()+" === "+g.obj()+") ? 1 : 2", "i")
}

func (g *escapeGen) object() string {
	return g.pick("new P("+g.value()+", "+g.value()+")", "new P("+g.value()+", "+g.value()+")", "new Q("+g.value()+")", "new E()",
		"{ a: "+g.value()+", b: "+g.value()+" }", "{ a: "+g.value()+" }", "new P("+g.num()+", "+g.num()+")",
		g.obj()+".add("+g.obj()+")", "new R("+g.value()+")", "new C("+g.value()+")", "Object.create(fresh)",
		"{ __proto__: fresh, a: "+g.value()+" }")
}

func (g *escapeGen) cond() string {
	return g.pick("i & 1", "i % 3 === 0", "i === at", g.num()+" & 1", g.obj()+" === "+g.obj(), "c")
}

func (g *escapeGen) block(n int) {
	for range n {
		g.statement()
	}
}

func (g *escapeGen) statement() {
	w := &g.b
	switch k := g.r.IntN(29); {
	case k < 4:
		fmt.Fprintf(w, "%s = %s\n", g.obj(), g.object())
	case k < 6:
		fmt.Fprintf(w, "%s = %s\n", g.obj(), g.obj())
	case k < 9:
		fmt.Fprintf(w, "%s.%s = %s\n", g.obj(), g.pick("a", "b", "c", "d"), g.value())
	case k < 12:
		fmt.Fprintf(w, "%s = %s\n", g.num(), g.number())
	case k < 13:
		fmt.Fprintf(w, "sink(%s)\n", g.obj())
	case k < 14:
		fmt.Fprintf(w, "hook(i, at)\n")
	case k < 15:
		fmt.Fprintf(w, "if (%s) return %s\n", g.cond(), g.obj())
	case k < 16:
		fmt.Fprintf(w, "%s = %s.b || %s\n", g.obj(), g.obj(), g.obj())
	case k < 17:
		x := g.obj()
		fmt.Fprintf(w, "%s.%s = %s\n", x, g.pick("a", "b"), x)
	case k < 19:
		// Merged, the name dead past what it does, and a way out that was
		// never compiled, whose state names what holds the objects.
		fmt.Fprintf(w, "{ const t = (%s) ? %s : %s; t.a = %s }\n", g.cond(), g.obj(), g.obj(), g.value())
		fmt.Fprintf(w, "if (i === at) { const t = { n: 1 }; hash(show(%s, 3)); return t.n }\n", g.obj())
	case k < 20:
		fmt.Fprintf(w, "%s = { a: %s, b: %s }\n", g.obj(), g.obj(), g.obj())
	case k < 21:
		w.WriteString(g.pick("Object.freeze("+g.obj()+")", "delete "+g.obj()+".a", "keep.x = "+g.obj(), g.obj()+".m("+g.obj()+")") + "\n")
	case k < 25 && g.depth < 3:
		g.depth++
		fmt.Fprintf(w, "if (%s) {\n", g.cond())
		g.block(1 + g.r.IntN(3))
		if g.r.IntN(2) == 0 {
			w.WriteString("} else {\n")
			g.block(1 + g.r.IntN(3))
		}
		w.WriteString("}\n")
		g.depth--
	case k < 28 && g.depth < 3 && g.loops < 3:
		g.depth++
		g.loops++
		j := "j" + strconv.Itoa(g.loops)
		fmt.Fprintf(w, "for (let %s = 0; %s < %d; %s++) {\n", j, j, 1+g.r.IntN(4), j)
		g.block(1 + g.r.IntN(4))
		if g.r.IntN(4) == 0 {
			fmt.Fprintf(w, "if (%s) break\n", g.cond())
		}
		w.WriteString("}\n")
		g.depth--
	default:
		g.depth++
		w.WriteString("try {\n")
		g.block(1 + g.r.IntN(2))
		fmt.Fprintf(w, "} catch (e) { %s = 3 }\n", g.num())
		g.depth--
	}
}

// Generated programs, each compiled under every setting of the boundary's
// stress: objects merged, carried, held by others, made late, and made by
// Go at an exit, every answer the interpreter's. QJS_ESCAPE_PROGRAMS says
// how many, QJS_ESCAPE_FIRST the first; a failure names its seed.
func TestJITEscapeRandom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	first, programs := uint64(1), uint64(60)
	if !allocsOrdinary() {
		// Under checkptr, CI's native boundary, whose budget the programs'
		// compiles would take: a few say what its checks can say.
		programs = 10
	}
	if s := os.Getenv("QJS_ESCAPE_FIRST"); s != "" {
		first, _ = strconv.ParseUint(s, 10, 64)
	}
	if s := os.Getenv("QJS_ESCAPE_PROGRAMS"); s != "" {
		programs, _ = strconv.ParseUint(s, 10, 64)
	}
	rounds := escapeRandomRounds()
	virtuals, materialized := 0, uint64(0)
	for seed := first; seed < first+programs; seed++ {
		prog := escapeProgram(seed)
		ok := t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1, deopt: 2}} {
				r := jitAgreesForTestWith(t, c, escapeRandomSetup+prog, rounds...)
				if e := jitCallbackEntry(r, "f"); e != nil && e.ssa != nil {
					virtuals += e.ssaVirtuals
				}
				materialized += r.jit.materialized
			}
			// Native code does the same work while the collector marks,
			// and compiled in the background.
			jitMarkingForTest(t)
			jitAgreesForTestWith(t, jitStressConfig{threshold: true, budget: 3, deopt: 7}, escapeRandomSetup+prog, rounds...)
			jitAgreesForTestWith(t, jitStressConfig{threshold: true, budget: 3, background: true, checks: 3}, escapeRandomSetup+prog, rounds...)
		})
		if !ok {
			t.Fatalf("seed %d:\n%s", seed, prog)
		}
	}
	t.Logf("%d objects left unmade, %d made by Go", virtuals, materialized)
	if programs >= 60 && (virtuals == 0 || materialized == 0) {
		t.Errorf("the programs no longer reach escape analysis: %d objects left unmade, %d made by Go", virtuals, materialized)
	}
}

// Programs that had crashed the compile, reduced. In the first, what
// follows x0, unbound, is reached only by the catch's entry, and dropping
// the entries that merge an object with a slot leaves an object's take in
// no block of the function; in the second, an object held in its own
// property, which merging it round a loop has escape while its other
// properties are merged. x0's and the other names' reads throw on purpose.
func TestJITEscapeReducedPrograms(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := escapeRandomRounds()
	for _, prog := range []string{`function f(i, at, c) {
		let x0 = new P(i, 1)
		for (let j1 = 0; j1 < 1; j1++) {
			if (c) {
			}
		}
		x0 = x2.b || x1
		x0 = new E()
		x2.c = n0 + 1
		x2 = new P((i === at ? odd : n2), (i === at ? odd : n2))
		if (c) return x0
		n0 = (x0 === x1) ? 1 : 2
		n2 = +(x2.b) | 0
		if (i & 1) {
			x1 = x0.b || x0
		} else {
			if (n2 & 1) {
				sink(x0)
			}
			for (let j2 = 0; j2 < 1; j2++) {
				x0 = new E()
			}
		}
		x0.b = i
	}`, `function f(i, at, c) {
		let x2 = new P(0, i)
		if (i & 1) {
			if (i === at) {
			}
		}
		x2.a = x2
		for (let j2 = 0; j2 < 4; j2++) {
			for (let j3 = 0; j3 < 1; j3++) {
				if (x0 === x0) {
				}
			}
			if (c) {
				if (i & 1) {
					try {
						try {
						} catch (e) { n2 = 3 }
					} catch (e) { n1 = 3 }
				}
				x0 = x2
			}
		}
		if (i & 1) {
		}
		try {
		} catch (e) { n2 = 3 }
	}`} {
		for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1, deopt: 2}} {
			jitAgreesForTestWith(t, c, escapeRandomSetup+prog, rounds...)
		}
	}
}

// An object merged at a join, its name dead past it, that a property of
// another not made still holds, is the merged object: a write through the
// one is seen through the other, where Go makes both at an exit no code was
// compiled for, which is the one object still.
func TestJITEscapeHeldMerged(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function P(v, w) { this.v = v; this.w = w }
		function field(i, at) {
			const h = { p: new P(1, 0), q: new P(2, 0) }
			const c = (i & 1) ? h.p : h.q
			c.v = 7
			if (i === at) { const t = { n: 1 }; return h.p.v + ':' + h.q.v + t.n }
			return c.v
		}
		function identity(i, at) {
			const a = new P(1, 0), b = new P(2, 0), h = { p: a }
			const c = (i & 1) ? a : b, k = { q: c }
			c.v = 3
			if (i === at) { const t = { n: 1 }; return (h.p === k.q) + ':' + h.p.v + t.n }
			return k.q.v
		}
		function deep(i, at) {
			const a = new P(1, 0), b = new P(2, 0), h = { p: a }, g = { h }
			const c = (i & 1) ? a : b
			c.w = 3
			if (i === at) { const t = { n: 1 }; return g.h.p.w + ':' + t.n }
			return c.w
		}
		function own(i, at) {
			const x = new P(1, 0), y = new P(2, 0)
			x.w = x
			y.w = x
			const c = (i & 1) ? x : y
			c.v = 9
			if (i === at) { const t = { n: 1 }; return (c.w === c) + ':' + c.w.v + t.n }
			return c.v
		}
		function loop(f, n, at) { let s = ''; for (let i = 0; i < n; i++) { const x = f(i, at); if (i === at) s = x } return s }`
	var rounds []string
	for range 14 {
		for _, f := range []string{"field", "identity", "deep", "own"} {
			rounds = append(rounds, `loop(`+f+`, 40, -1)`)
		}
	}
	for _, f := range []string{"field", "identity", "deep", "own"} {
		rounds = append(rounds, `loop(`+f+`, 40, 17)`, `loop(`+f+`, 40, 16)`)
	}
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}} {
		jitAgreesForTestWith(t, c, setup, rounds...)
	}
}

// A construction's take that is made has as operands what its constructor
// is given, which may be an object not made till there: the object made,
// not a value of what no longer is, the function compiled. Where the
// construction's own object is not made, its operands need nothing made:
// its constructor's code holds them.
func TestJITEscapeTakeOperand(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function Inner(v) { this.v = v }
		function Outer(x) {}
		function Holder(x) { this.x = x; this.k = 1 }
		function made(i) {
			const inner = new Inner(i)
			if (i & 1) return new Outer(inner) instanceof Outer
			return inner.v
		}
		function escapes(i) {
			const inner = new Inner(i)
			let o = new Holder(inner)
			if (i & 2) o = null
			return o ? o.x.v : inner.v
		}
		function held(n) {
			let s = 0
			for (let i = 0; i < n; i++) { const o = new Holder(new Inner(i)); s += o.x.v + o.k }
			return s
		}
		function loop(f, n) { let s = 0; for (let i = 0; i < n; i++) s += +f(i); return s }`
	var rounds []string
	for range 14 {
		rounds = append(rounds, `loop(made, 40)`, `loop(escapes, 40)`, `held(40)`)
	}
	for _, c := range []jitStressConfig{{}, {background: true}} {
		r := jitAgreesForTestWith(t, c, setup, rounds...)
		for _, name := range []string{"made", "escapes", "held"} {
			if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil {
				t.Errorf("%+v: %s was not compiled", c, name)
			}
		}
		if e := jitCallbackEntry(r, "held"); e != nil && e.ssaVirtuals != 2 {
			t.Errorf("%+v: held leaves %d objects unmade", c, e.ssaVirtuals)
		}
	}
}

// A property's phi that merges objects read from others is no object: the
// walk that made it has it merge what it only then knows are objects, and
// the next classification, which gave it a role it could not hold, had
// never ended.
func TestJITEscapeMergedReads(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function same(n, c) {
			let s = 0
			for (let i = 0; i < n; i++) {
				const a = { v: i }, b = { x: a }, w = { y: b }, z = { p: 0 }
				if (i & c) z.p = w.y.x; else z.p = b.x
				s += z.p.v
			}
			return s
		}
		function two(n, c) {
			let s = 0
			for (let i = 0; i < n; i++) {
				const a = { v: i }, d = { v: 2 * i }, b = { x: a, y: d }, w = { y: b }, z = { p: 0 }
				if (i & c) z.p = w.y.x; else z.p = w.y.y
				s += z.p.v
			}
			return s
		}`
	var rounds []string
	for range 20 {
		rounds = append(rounds, `same(40, 1)`, `two(40, 1)`)
	}
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}} {
		r := jitAgreesForTestWith(t, c, setup, rounds...)
		for _, name := range []string{"same", "two"} {
			if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil {
				t.Errorf("%+v: %s was not compiled", c, name)
			}
		}
	}
}

// A construction's receiver with no properties is made where it is taken,
// not where a use needs it: its constructor's pool, run out there, is filled
// for the prototype the constructor has by then, and nothing would compare
// the object's shape with the one the take checked. Here the prototype
// changes between the two, and the pool is empty, in a call of Go from
// which native code goes on.
func TestJITEscapeProtoAtMaking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	run := func(src string) Value {
		t.Helper()
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	global := func(name string) Value { return r.global.getOwn(r.atoms.intern(name)).value() }
	drained := 0
	r.global.setOwnRaw(r.atoms.intern("swap"), Obj(r.newNativeFunc("swap", 1, func(_ *Runtime, _ Value, args []Value) (Value, error) {
		if args[0].num != global("when").num {
			run("E.prototype = mine")
			return Undefined, nil
		}
		run("E.prototype = other")
		if e := jitCallbackEntry(r, "mk"); e != nil {
			for _, x := range jitTargets(e) {
				if x.obj == global("E").Object() && x.pool != nil {
					x.pool.Count = 0
					drained++
				}
			}
		}
		return Undefined, nil
	})), propDefault)
	run(`
		function E() {}
		var mine = E.prototype, other = { other: 1 }, when = -1
		Object.create(other)
		function mk(i, ret) { const o = new E(); swap(i); if (ret) return o; return null }
		function a1() { return 1 }
		function a2() { return 2 }
		function a3() { return 3 }
		function call(g, a, b) { return g(a, b) }
		var others = [a1, a2, a3]
		function loop(n, at) {
			let r = ''
			when = at
			for (let i = 0; i < n; i++) {
				call(others[i % 3], i, 0)
				const o = call(mk, i, i & 1)
				if (o && Object.getPrototypeOf(o) !== mine) r += i + ' '
			}
			when = -1
			return r
		}`)
	for range 40 {
		run(`loop(40, -1)`)
	}
	if got := run(`loop(40, 21)`).String().Go(); got != "" {
		t.Errorf("objects of another prototype, at %s", got)
	}
	if drained == 0 {
		t.Error("mk's pool was not the construction's")
	}
}
