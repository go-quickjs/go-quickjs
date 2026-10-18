//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
)

// jitMegaSetup makes receivers of more shapes than a read is compiled for
// (jitPropertyCases): objects of nine classes, each a subclass of Base two
// prototypes down, as Box2D's contacts are, or three, a class of its
// own's; some give themselves another field, some their prototype a
// method of Base's name, a getter, or another prototype.
const jitMegaSetup = `function inherit(y,w){function K(){};K.prototype=w.prototype;y.prototype=new K;y.prototype.constructor=y}
	function Base(){this.m_flags=0;this.m_next=null}
	Base.prototype.IsSensor=function(){return (this.m_flags&1)==1};
	Base.prototype.kind=7;
	var classes=[];
	for(let k=0;k<9;k++){const C=function(){Base.call(this);this['f'+k]=k;if(k&1)this.extra=k};
		if(k==8){const M=function(){};inherit(M,Base);inherit(C,M)}else inherit(C,Base);
		C.prototype['Evaluate'+k]=function(){};classes.push(C)}
	function list(n){let l=null;for(let i=0;i<n;i++){const o=new classes[i%9]();o.m_flags=i&3;o.m_next=l;l=o}return l}
	var objs=list(90);
	function walk(n){let s=0;for(let j=0;j<n;j++)for(let b=objs;b;b=b.m_next){if(b.IsSensor()==true)s++}return s}
	function kinds(n){let s=0;for(let j=0;j<n;j++)for(let b=objs;b;b=b.m_next)s+=b.kind;return s}
	function shadow(n){const before=walk(n);classes[3].prototype.IsSensor=function(){return true};const after=walk(n);
		delete classes[3].prototype.IsSensor;return before+":"+after+":"+walk(n)}
	function replaced(n){const f=Base.prototype.IsSensor,before=walk(n);Base.prototype.IsSensor=function(){return this.m_flags>1};
		const after=walk(n);Base.prototype.IsSensor=f;return before+":"+after+":"+walk(n)}
	function kindGetter(n){const before=kinds(n);Object.defineProperty(classes[5].prototype,"kind",{get(){return 100},configurable:true});
		const after=kinds(n);delete classes[5].prototype.kind;return before+":"+after+":"+kinds(n)}
	function reproto(n){const o=objs.m_next.m_next;const p=Object.getPrototypeOf(o);const before=kinds(n);
		Object.setPrototypeOf(o,{kind:-50,IsSensor(){return false}});const after=kinds(n)+":"+walk(n);
		Object.setPrototypeOf(o,p);return before+":"+after+":"+kinds(n)}
	function removed(n){const before=kinds(n);delete Base.prototype.kind;const after=kinds(n);Base.prototype.kind=7;return before+":"+after+":"+kinds(n)}
	function own(n){const before=kinds(n);objs.kind=1;const after=kinds(n);delete objs.kind;return before+":"+after+":"+kinds(n)}
	for(let b=objs,i=0;b;b=b.m_next,i++)if(i%3==0)b.child1=b;
	function leaves(n){let c=0;for(let j=0;j<n;j++)for(let b=objs;b;b=b.m_next){if(b.child1==null)c++}return c}
	function protoChild(n){const before=leaves(n);classes[4].prototype.child1=1;const mid=leaves(n);delete classes[4].prototype.child1;
		Object.prototype.child1=2;const after=leaves(n);delete Object.prototype.child1;return before+":"+mid+":"+after+":"+leaves(n)}
	function nullProto(n){const o=Object.create(null);o.m_next=null;o.m_flags=0;const keep=objs.m_next;objs.m_next=o;
		const r=leaves(n);objs.m_next=keep;return r}`

// A read of a property of its receivers' prototypes, at a site that met
// more shapes than it is compiled for, finds it natively in the runtime's
// megamorphic cache (abi.MegaCache), along the prototypes the entry names
// with their shapes, as V8's megamorphic loads probe its stub cache: a
// method of a base class, read of objects of nine subclasses; and so does
// a read of a property none of them has (abi.MegaAbsent), a child only
// some have. Every answer is the interpreter's, after a prototype's method
// is shadowed, replaced, made a getter, deleted, a prototype given the
// missing property, or an object given another prototype, none, or a
// property of its own -- with the collector marking too.
func TestJITMegamorphicPrototypes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	cases := []string{`walk(3)`, `kinds(3)`, `walk(4)`, `leaves(3)`, `shadow(3)`, `replaced(3)`, `kindGetter(3)`, `reproto(3)`, `removed(3)`, `own(3)`,
		`walk(2)`, `leaves(4)`, `protoChild(3)`, `nullProto(3)`, `leaves(2)`}
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		want := New(Config{})
		r := jitRuntimeForTest(t, Config{JIT: true})
		for _, rt := range []*Runtime{want, r} {
			if _, err := rt.Run(compileForTest(t, jitMegaSetup)); err != nil {
				t.Fatal(err)
			}
		}
		for round := 0; round < 3; round++ {
			for _, c := range cases {
				wv, werr := want.Run(compileForTest(t, "String("+c+")"))
				gv, gerr := r.Run(compileForTest(t, "String("+c+")"))
				if werr != nil || gerr != nil {
					t.Fatalf("marking %v, round %d, %s: %v, interpreter's %v", marking, round, c, gerr, werr)
				}
				if got, w := gv.String().Go(), wv.String().Go(); got != w {
					t.Fatalf("marking %v, round %d, %s: got %s, interpreter %s", marking, round, c, got, w)
				}
			}
		}
		want.Close()
		want.ReleaseClosed()
		// Once the cache has the entries, the reads never leave native
		// code, though they met nine shapes: in a runtime of its own,
		// whose code nothing has made leave for Go too often.
		r = jitRuntimeForTest(t, Config{JIT: true})
		if _, err := r.Run(compileForTest(t, jitMegaSetup)); err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{`walk(3)`, `kinds(3)`, `leaves(3)`, `walk(3)`, `kinds(3)`, `leaves(3)`} {
			if _, err := r.Run(compileForTest(t, c)); err != nil {
				t.Fatal(err)
			}
		}
		// A method added to Base.prototype gives it another shape, which
		// the code's cases no longer name: the reads find the chain as it
		// is in the cache, filled at their first exits, and leave no more.
		// (Twice: code a deoptimization invalidated meanwhile is compiled
		// again once the tree tier has run it its budget, and its
		// megamorphic reads' first exits fill the cache then.)
		for _, c := range []string{`Base.prototype.Other=function(){};kinds(2)`, `walk(2)`, `leaves(2)`, `kinds(2)`, `walk(2)`, `leaves(2)`} {
			if _, err := r.Run(compileForTest(t, c)); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"walk", "kinds", "leaves"} {
			cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
			e := r.jit.hint(cl)
			if e == nil || e.ssa == nil {
				t.Fatalf("marking %v: %s was not compiled: %+v", marking, name, e)
			}
			before := e.ssaStats
			if _, err := r.Run(compileForTest(t, name+`(50)`)); err != nil {
				t.Fatal(err)
			}
			st := e.ssaStats
			if r.jit.hint(cl) != e || st.work-before.work < 4500 {
				t.Fatalf("marking %v: %s(50) did not run natively: %+v, before %+v", marking, name, st, before)
			}
			if left := st.hosts - before.hosts; left != 0 {
				t.Errorf("marking %v: %s(50) left native code %d times", marking, name, left)
			}
		}
	}
}
