//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A construction with a class whose constructor is the one a class without
// one has -- super(...args), no fields -- is made natively as a
// construction with the class it extends (jitDerivedBase), as V8 skips a
// default derived constructor: its object made as the derived class makes
// one, its prototype the derived class's, the parent's body run on it.
// The call checks the class still extends that parent: made to extend
// another, it constructs with the other's body, as super() would. Each
// answer is the interpreter's -- the prototype chain, instanceof, the
// fields the parent's body sets, a parent's body that returns an object
// of its own, the parent replaced, a class with fields of its own and one
// with a constructor of its own (neither skipped) -- and the class of no
// constructor of its own is constructed natively. AWFY's Havlak
// constructs a DictIdEntry, a DictEntry's subclass, for each entry of its
// dictionaries: each had been a call of Go.
func TestJITDerivedDefaultConstruct(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `class Entry { constructor(h, k, v, n) { this.hash = h; this.key = k; this.value = v; this.next = n } }
		class IdEntry extends Entry { match(h, k) { return this.hash === h && this.key === k } }
		class Other { constructor(h, k) { this.other = h + k } }
		class Own { constructor(h) { return { own: h } } }
		class OwnSub extends Own {}
		class Fielded extends Entry { extra = 7 }
		class Explicit extends Entry { constructor(h) { super(h, 'x', 1, null); this.z = h * 3 } }
		function chain(n){let head=null,s=0;for(let i=0;i<n;i++){const e=new IdEntry(i,'k'+i,i*2,head);head=e;s+=e.value}return s+':'+head.match(n-1,'k'+(n-1))+':'+(head instanceof Entry)+':'+(head instanceof IdEntry)+':'+(Object.getPrototypeOf(head)===IdEntry.prototype)+':'+Object.keys(head).join('/')}
		function own(n){let s='';for(let i=0;i<n;i++){const o=new OwnSub(i);s=o.own+':'+(o instanceof OwnSub)+':'+(o instanceof Own)}return s}
		function fielded(n){let s=0;for(let i=0;i<n;i++){const o=new Fielded(i,'k',i,null);s+=o.extra+o.value}return s}
		function explicit(n){let s=0;for(let i=0;i<n;i++){const o=new Explicit(i);s+=o.z+o.value+(o.key==='x'?1:0)}return s}
		function reparent(){Object.setPrototypeOf(IdEntry, Other);return 'moved'}`
	cases := []string{`chain(50)`, `chain(200)`, `own(50)`, `String(fielded(50))`, `String(explicit(50))`, `chain(100)`, `reparent()`, `chain(50)`, `chain(60)`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range cases {
		if c == `reparent()` {
			// The constructions so far, made natively: chain's calls of
			// Go are none but its method calls' and Object.keys's.
			cl := r.global.getOwn(r.atoms.intern("chain")).value().Object().fn().closure
			e := r.jit.hint(cl)
			if e == nil || e.ssa == nil {
				t.Fatalf("chain was not compiled: %+v", e)
			}
			found := false
			for _, x := range jitTargets(e) {
				found = found || x.parent != nil
			}
			if !found {
				t.Errorf("chain constructs no derived class natively: %+v", jitTargets(e))
			}
			// Made natively, a round of 200 constructions makes a few calls
			// of Go -- Object.keys, its join -- not one each.
			before := e.ssaStats.goCalls
			if _, err := r.Run(compileForTest(t, `chain(200)`)); err != nil {
				t.Fatal(err)
			}
			// A compile meanwhile counts afresh.
			after := r.jit.hint(cl).ssaStats.goCalls
			calls := after - before
			if after < before {
				calls = after
			}
			if calls > 20 {
				t.Errorf("200 constructions of a derived class made %d calls of Go", calls)
			}
		}
		wv, err := want.Run(compileForTest(t, c))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, c))
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if got, w := gv.String().Go(), wv.String().Go(); got != w {
			t.Fatalf("case %d, %s: got %s, interpreter %s", i, c, got, w)
		}
	}
}

// jitDerivedForTest is the entry of the derived class name's constructor:
// a class is a lexical binding, not the global object's, so it is found
// by its function's name.
func jitDerivedForTest(t *testing.T, r *Runtime, name string) *jitEntry {
	t.Helper()
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == name && fn.Kind == bytecode.KindDerivedConstructor {
			return x
		}
	}
	t.Fatalf("%s has no entry", name)
	return nil
}

// jitDerivedCalls is how many times native callers called e's code.
func jitDerivedCalls(e *jitEntry) uint64 {
	if e.link == nil {
		return 0
	}
	return e.link.in
}

// A derived class's construction from native code is a native call of its
// constructor's code (ssa.CallSite's Derived), as V8 constructs one: the
// instance from a pool made for the class, the constructor's this unbound
// until its super() -- Go's -- binds it, the result the bound this. A class
// with no constructor of its own runs its parent's code, derived too, with
// itself as new.target; one with fields has them from its super(). Every
// answer is the interpreter's, and the constructions are native calls, not
// calls of Go.
func TestJITDerivedNativeConstruct(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		class A { constructor(x) { this.x = x } }
		class B extends A { constructor(x, y) { super(x); this.y = y } }
		class C extends B { constructor(x) { super(x, 2); this.z = x * this.y } }
		class S extends B {}
		class F extends A { f = 3; constructor(x) { super(x); this.g = this.f + 1 } }
		function make(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				const b = new B(i, 1), c = new C(i), d = new S(i, 1), f = new F(i)
				s += b.y + c.z + d.y + f.g + (d.constructor === S ? 1 : 0)
			}
			return s
		}
		function shape() { const c = new C(1); return Object.keys(c).join() + ':' + Object.getPrototypeOf(c).constructor.name }
		// Each construction its own object: a native run's constructions,
		// each super() made by Go on its level's frame, one instance each.
		// One class's in a row, the frame Go made for the last's level not
		// the next's.
		function distinct(n) { const a = []; for (let i = 0; i < n; i++) a.push(new B(i, 1)); for (let i = 0; i < n; i++) a.push(new C(i)); return new Set(a).size + ':' + a[0].x + ':' + a[a.length - 1].x }
	`
	rounds := []string{}
	for range 10 {
		rounds = append(rounds, `make(500)`, `distinct(300)`)
	}
	rounds = append(rounds, `shape()`)
	r := jitAgreesForTest(t, setup, rounds...)
	b, c, f := jitDerivedForTest(t, r, "B"), jitDerivedForTest(t, r, "C"), jitDerivedForTest(t, r, "F")
	m := jitNativeForTest(t, r, "make")
	// Counted with the collector off, which the counts are not about.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	bIn, cIn, fIn, goCalls := jitDerivedCalls(b), jitDerivedCalls(c), jitDerivedCalls(f), m.ssaStats.goCalls
	v, err := r.Run(compileForTest(t, `''+make(500)`))
	if err != nil || v.String().Go() != "253000" {
		t.Fatalf("make(500) = %v, %v; want 253000", v, err)
	}
	// B's code three times a pass, for new B, new S and C's super(); C's
	// and F's once.
	if d := jitDerivedCalls(b) - bIn; d != 1500 {
		t.Errorf("B's code was called natively %d times in a round, want 1500", d)
	}
	if d := jitDerivedCalls(c) - cIn; d != 500 {
		t.Errorf("C's code was called natively %d times in a round, want 500", d)
	}
	if d := jitDerivedCalls(f) - fIn; d != 500 {
		t.Errorf("F's code was called natively %d times in a round, want 500", d)
	}
	if d := m.ssaStats.goCalls - goCalls; d != 0 {
		t.Errorf("make made %d calls of Go in a round, want none", d)
	}
	if v, err := r.Run(compileForTest(t, `distinct(300)`)); err != nil || v.String().Go() != "600:0:299" {
		t.Errorf("distinct(300) = %v, %v; want 600:0:299", v, err)
	}
	// S's prototype changed: its construction runs the code of the class
	// it extends now, A's, which a base class's construction calls.
	jitAgreesForTest(t, setup, append(rounds, `(Object.setPrototypeOf(S, A), make(50))`, `make(50)`)...)
}

// What a derived constructor natively constructed does wrong throws as the
// interpreter throws it, with its error's type and message, at the
// construction: this read before super(), a second super() -- after the
// parent ran again --, a return before super(), and a return of a
// primitive; an object returned is the result, and undefined the this.
// Each does so at a loop's last construction, the others natively made
// and well: a construction that throws at every call is left to the
// existing tiers.
func TestJITDerivedNativeErrors(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		class A { constructor(x) { this.x = x } }
		class Count { constructor() { Count.n = (Count.n || 0) + 1 } }
		class Early extends A { constructor(e) { if (e) this.q = 1; super(1) } }
		class Twice extends Count { constructor(t) { super(); if (t) super() } }
		class NoSuper extends A { constructor(n) { if (!n) super(1) } }
		class RetObj extends A { constructor(o) { super(1); if (o) return { other: true } } }
		class RetPrim extends A { constructor(p) { super(1); if (p) return 5 } }
		class RetUndef extends A { constructor(u) { super(7); if (u) return undefined } }
	`
	// A function of its own for each, its construction's site calling one
	// constructor.
	for _, k := range []string{"Early", "Twice", "NoSuper", "RetObj", "RetPrim", "RetUndef"} {
		setup += `function errs` + k + `(n) {
			let last = ''
			for (let i = 0; i < n; i++) {
				try { last = JSON.stringify(new ` + k + `(i === n - 1)) } catch (e) { last = e.name + ': ' + e.message }
			}
			return last
		}
		`
	}
	want := map[string]string{
		"Early":    `ReferenceError: "this" is not bound until super() has been called`,
		"Twice":    `ReferenceError: super() has already been called`,
		"NoSuper":  `ReferenceError: a derived constructor must call super() before returning`,
		"RetObj":   `{"other":true}`,
		"RetPrim":  `TypeError: a derived constructor may return only an object or undefined`,
		"RetUndef": `{"x":7}`,
	}
	names := []string{"Early", "Twice", "NoSuper", "RetObj", "RetPrim", "RetUndef"}
	rounds := []string{}
	for range 3 {
		for _, k := range names {
			rounds = append(rounds, `errs`+k+`(50)`)
		}
	}
	rounds = append(rounds, `Count.n`)
	// With the collector off, which the counts are not about: Go had made
	// the constructions while it marked, and under GOGC=5 it marked
	// through every one of Early's.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitAgreesForTest(t, setup, rounds...)
	for _, k := range names {
		v, err := r.Run(compileForTest(t, `''+errs`+k+`(50)`))
		if err != nil || v.String().Go() != want[k] {
			t.Errorf("errs(%s) = %v, %v; want %s", k, v, err, want[k])
		}
	}
	// The parent ran for each super(), the second's too: four runs of
	// errsTwice(50), each 51 times.
	if v, err := r.Run(compileForTest(t, `''+Count.n`)); err != nil || v.String().Go() != "204" {
		t.Errorf("Count.n = %v, %v; want 204", v, err)
	}
	for _, k := range names {
		if e := jitDerivedForTest(t, r, k); jitDerivedCalls(e) == 0 {
			t.Errorf("%s's code was never called natively", k)
		}
	}
}

// A derived constructor natively constructed that leaves native code
// before its super() or after it has Go make its frame as a derived
// construction's (jitDerivedFrame): unbound before, the instance its
// super() gives the parent; bound after, the this the rest of its body
// writes. Each answer is the interpreter's.
func TestJITDerivedNativeUnwind(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		class P { constructor(k) { this.k = k } }
		class Mid extends P { constructor(o, p) { const k = o.k; super(k); this.m = p.m + this.k } }
		var o1 = { k: 1 }, p1 = { m: 10 }, o2 = { z: 0, k: 2 }, p2 = { y: 0, m: 20 }
		function run(n, o, p) { let s = 0; for (let i = 0; i < n; i++) { const x = new Mid(o, p); s += x.k * 100 + x.m } return s + ':' + Object.keys(new Mid(o, p)).join() }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `run(200, o1, p1)`)
	}
	rounds = append(rounds, `run(200, o2, p1)`, `run(200, o1, p2)`, `run(200, o2, p2)`, `run(200, o1, p1)`)
	r := jitAgreesForTest(t, setup, rounds...)
	if e := jitDerivedForTest(t, r, "Mid"); jitDerivedCalls(e) == 0 {
		t.Errorf("Mid's code was never called natively")
	}
	if r.jit.unwound == 0 {
		t.Errorf("no native callee left native code")
	}
}

// TestJITDerivedNativeConstruct's constructions, the collector marking:
// native, as when it does not mark, each receiver shaded in its pool
// before the call takes it (mir's poolShade), every register native code
// may keep given junk across the stores the barrier makes
// (jit.StoreRefClobber); every answer is the interpreter's. Go had made
// each, entering the constructor's code (jitMarkingCallee).
func TestJITDerivedNativeMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	setup := `
		class A { constructor(x) { this.x = x } }
		class B extends A { constructor(x, y) { super(x); this.y = y } }
		class C extends B { constructor(x) { super(x, 2); this.z = x * this.y } }
		function make(n) { let s = 0; for (let i = 0; i < n; i++) { const b = new B(i, 1), c = new C(i); s += b.y + c.z } return s }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `make(300)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	for _, name := range []string{"B", "C"} {
		if e := jitDerivedForTest(t, r, name); e.ssa == nil || jitDerivedCalls(e) == 0 {
			t.Errorf("%s was not constructed natively while the collector marked: code %v, slow %v, callee %v, %+v", name, e.ssa != nil, e.entrySlow, e.ssaCallee, e.ssaStats)
		}
	}
}

// A derived constructor's super() of a constructor of the script's is a
// native construction of the parent, as V8's super() calls it: the
// parent found by the running function's prototype, checked as the code
// was compiled for; its this the instance, a derived parent's unbound;
// the this bound its result, an object, or else the instance. A class of
// no constructor of its own skips to the code of the one it extends, as
// V8 skips a default derived constructor. DeltaBlue's constraints'
// shape: a base of their strength, a binary and a unary class, each
// subclass's super() natively the next one's, none a call of Go. A
// parent changed meanwhile is constructed with as super() then reads it.
func TestJITDerivedNativeSuper(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		class Abs { constructor(s) { this.strength = s; this.k = s.length } }
		class Bin extends Abs { constructor(a, b, s) { super(s); this.v1 = a; this.v2 = b } }
		class Eq extends Bin { constructor(a, b, s, list) { super(a, b, s); list.push(this) } }
		class Un extends Abs { constructor(v, s) { super(s); this.out = v } }
		class Stay extends Un {}
		class Other { constructor(s) { this.other = s } }

		function chain(n) {
			const list = []
			let s = 0
			for (let i = 0; i < n; i++) {
				const e = new Eq(i, i + 1, 's', list), u = new Stay(i, 'u')
				s += e.v1 + e.v2 + u.out + (list[i] === e ? 1 : 0) + (u.constructor === Stay ? 1 : 0) + (e.strength === 's' ? 1 : 0)
			}
			return s + ':' + new Set(list).size + ':' + Object.keys(new Eq(1, 2, 'x', [])).join() + ':' + Object.keys(new Stay(1, 'y')).join()
		}
	`
	rounds := []string{}
	for range 10 {
		rounds = append(rounds, `chain(300)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	abs := r.jit.hint(jitClassForTest(t, r, "Abs"))
	bin, eq, un := jitDerivedForTest(t, r, "Bin"), jitDerivedForTest(t, r, "Eq"), jitDerivedForTest(t, r, "Un")
	if abs == nil {
		t.Fatal("Abs has no entry")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	absIn, binIn, unIn := jitDerivedCalls(abs), jitDerivedCalls(bin), jitDerivedCalls(un)
	goCalls := []uint64{eq.ssaStats.goCalls, bin.ssaStats.goCalls, un.ssaStats.goCalls}
	v, err := r.Run(compileForTest(t, `chain(300)`))
	// 3i + 4 a pass, summed over 300: 3 * 44850 + 1200.
	if err != nil || v.String().Go() != "135750:300:strength,k,v1,v2:strength,k,out" {
		t.Fatalf("chain(300) = %q, %v", v.String().Go(), err)
	}
	// Abs's code by Bin's super() and Un's, Bin's by Eq's, Un's by the
	// construction of Stay, which runs its code -- each pass, and the two
	// constructions after the loop.
	for _, c := range []struct {
		name  string
		got   uint64
		was   uint64
		want  uint64
		entry *jitEntry
	}{{"Abs", jitDerivedCalls(abs), absIn, 602, abs}, {"Bin", jitDerivedCalls(bin), binIn, 301, bin}, {"Un", jitDerivedCalls(un), unIn, 301, un}} {
		if c.got-c.was != c.want {
			t.Errorf("%s's code was called natively %d times in a round, want %d", c.name, c.got-c.was, c.want)
		}
	}
	for i, e := range []*jitEntry{eq, bin, un} {
		if d := e.ssaStats.goCalls - goCalls[i]; d != 0 {
			t.Errorf("%s made %d calls of Go in a round, want none", []string{"Eq", "Bin", "Un"}[i], d)
		}
	}
	// Bin made to extend Other: its super() constructs Other, as it reads
	// it then; and back again.
	jitAgreesForTest(t, setup, append(rounds, `(Object.setPrototypeOf(Bin, Other), chain(50))`, `chain(50)`,
		`(Object.setPrototypeOf(Bin, Abs), chain(50))`)...)
	// A parent that leaves native code in a super() native code made, at a
	// shape it had not met: Go finishes it, and binds the subclass's this
	// to what it gave (jitSuperBound), whether the subclass's code goes on
	// natively after the call or not.
	leave := `
		class Leave { constructor(o) { this.v = o.k; this.n = o.k * 2 } }
		class Sub extends Leave { constructor(o) { super(o); this.w = this.v + this.n } }
		class Sub2 extends Sub { constructor(o, x) { super(o); this.x = x + this.w } }
		var o1 = { k: 1 }, o2 = { q: 0, k: 2 }, o3 = { r: 0, s: 0, k: 3 }
		function make(n, o) { let s = 0; for (let i = 0; i < n; i++) { const a = new Sub(o), b = new Sub2(o, i); s += a.w + b.x + (b instanceof Sub2 ? 1 : 0) } return s + ':' + Object.keys(new Sub2(o, 1)).join() }
	`
	lr := []string{}
	for range 6 {
		lr = append(lr, `make(200, o1)`)
	}
	jitAgreesForTest(t, leave, append(lr, `make(200, o2)`, `make(200, o3)`, `make(200, o1)`, `make(200, o2)`)...)
}

// jitClassForTest is the closure of the class name's constructor: a
// class is a lexical binding, not the global object's.
func jitClassForTest(t *testing.T, r *Runtime, name string) *closure {
	t.Helper()
	v, err := r.Run(compileForTest(t, name))
	if err != nil || !v.IsObject() || v.Object().fn() == nil || v.Object().fn().closure == nil {
		t.Fatalf("%s: %v, %v", name, v, err)
	}
	return v.Object().fn().closure
}

// One super() made twice, natively, its second time this bound
// (mir's superGuards): the parent runs again, then it throws, as the
// interpreter's super() does, the constructor catching it.
func TestJITDerivedNativeSuperTwice(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Counted with the collector off, which the counts are not about.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `
		class Abs { constructor(s) { this.strength = s; this.k = s.length } }
		class Twice extends Abs { constructor(n) { let err = ''; for (let i = 0; i < n; i++) { try { super('a' + i) } catch (e) { err = e.name + ': ' + e.message } } this.err = err } }
		function twice(n) { let last = '', s = 0; for (let i = 0; i < n; i++) { const o = new Twice(i % 5 === 4 ? 2 : 1); s += o.k; last = JSON.stringify(o) } return s + ':' + last }
	`
	rounds := []string{}
	for range 10 {
		rounds = append(rounds, `twice(100)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	if v, err := r.Run(compileForTest(t, `''+twice(100)`)); err != nil || v.String().Go() != `200:{"strength":"a1","k":2,"err":"ReferenceError: super() has already been called"}` {
		t.Errorf("twice(100) = %q, %v", v.String().Go(), err)
	}
	// Its super() native, the second time left to Go, which throws.
	if e := jitDerivedForTest(t, r, "Twice"); jitDerivedCalls(e) < 500 || e.ssaStats.goCalls > 2 {
		t.Errorf("Twice's super() was not made natively: %d native calls of it, %d calls of Go", jitDerivedCalls(e), e.ssaStats.goCalls)
	}
}
