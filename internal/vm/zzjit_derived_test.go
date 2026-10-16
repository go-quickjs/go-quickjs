//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

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
