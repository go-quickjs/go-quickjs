//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
)

// jitAccessorSetup defines the accessors TestJITAccessors reads and writes:
// a class's, inherited one and two levels down; Object.defineProperty's on
// a prototype, a getter and a setter defined apart, as Box2D defines its
// contact IDs' key; a setter that returns a value; getters and setters
// that throw, read arguments, take a stack trace, read past an object
// that may be missing, call a function; a built-in's (Map's size); a
// getter-only accessor written in strict and sloppy code; and an object
// literal's own getter.
const jitAccessorSetup = `class P{constructor(x){this._x=x;this.log=0}get x(){return this._x}set x(v){this._x=v*2;this.log++}get self(){return this}}
	class Q extends P{constructor(x){super(x);this.q=1}}
	class S extends Q{constructor(x){super(x);this.s=2}}
	function L(){this._key=0;this.features={_a:0,_b:0}}
	Object.defineProperty(L.prototype,"key",{enumerable:false,configurable:true,get:function(){return this._key}});
	Object.defineProperty(L.prototype,"key",{enumerable:false,configurable:true,set:function(b){if(b===undefined)b=0;this._key=b;this.features._a=this._key&255;this.features._b=(this._key>>8)&255}});
	function R(){this._y=0}
	Object.defineProperty(R.prototype,"y",{get(){return this._y},set(v){this._y=v+1;return "ignored"}});
	function T(id,bad){this.id=id;this.bad=bad}
	Object.defineProperty(T.prototype,"t",{get(){if(this.bad)throw new RangeError("bad "+this.id);return this.id}});
	Object.defineProperty(T.prototype,"u",{set(v){if(v%7==3)throw new TypeError("no "+v);this.id=v}});
	function A(){this.k=0}
	Object.defineProperty(A.prototype,"n",{get(){return arguments.length+(arguments[0]===undefined?10:0)}});
	Object.defineProperty(A.prototype,"m",{set(v){this.k=arguments.length*100+v}});
	function E(o){this.o=o}
	Object.defineProperty(E.prototype,"deep",{get(){return this.o.v}});
	Object.defineProperty(E.prototype,"st",{get(){return new Error("at "+this.o).stack}});
	function half(o){return o.h/2}
	function H(h){this.h=h}
	Object.defineProperty(H.prototype,"half",{get(){return half(this)}});
	let scale=3;
	function N(n){this.n=n;this.last=0}
	Object.defineProperty(N.prototype,"sum",{get(){let s=0;for(let i=0;i<this.n;i++)s+=i;if(this.n<0)throw new RangeError("neg "+this.n);return s}});
	Object.defineProperty(N.prototype,"far",{get(){let s=0;for(let i=0;i<this.n;i++)s+=i;return s+this.o.v}});
	Object.defineProperty(N.prototype,"total",{set(v){let s=0;for(let i=0;i<v;i++)s+=i*scale;this.last=s}});
	Object.defineProperty(N.prototype,"scaled",{get(){return this.n*scale}});
	function sums(os,n){let s=0,e="";for(let i=0;i<n;i++){try{s+=os[i%os.length].sum}catch(x){e+=x.name+x.message+"|"+x.stack+"|"}}return s+":"+e}
	function nf(n,o){const x=new N(n);x.o=o;return x}
	function fars(os,n){let s=0,e="";for(let i=0;i<n;i++){try{s+=os[i%os.length].far}catch(x){e+=x.name+x.message+"|"+x.stack+"|"}}return s+":"+e}
	function totals(n){const o=new N(0);let s=0;for(let i=0;i<n;i++){o.total=i%20;s+=o.last}return s}
	function scaleds(n){const os=[new N(1),new N(2)];let s=0;for(let i=0;i<n;i++){s+=os[i&1].scaled;if(i==n>>1)scale=5}scale=3;return s}
	function G(){this.g0=0}
	Object.defineProperty(G.prototype,"g",{get(){return 5}});
	const ps=[new P(1),new Q(2),new S(3),new P(4)], qs=[new Q(5),new Q(6)], ss=[new S(7),new S(8)];
	const ls=[new L(),new L(),new L()];
	function readX(os,n){let s=0;for(let i=0;i<n;i++)s+=os[i%os.length].x;return s}
	function writeX(os,n){for(let i=0;i<n;i++)os[i%os.length].x=i;let s=0;for(const o of os)s+=o._x*1000+o.log;return s}
	function writes(os,n){for(let i=0;i<n;i++)os[i%os.length].x=i;return os[0]._x}
	function selves(os,n){let k=0;for(let i=0;i<n;i++){const o=os[i%os.length];if(o.self===o)k++}return k}
	function keys(n){let s=0;for(let j=0;j<n;j++)for(let i=0;i<ls.length;i++){const d=ls[i];d.key=j+i*256;s+=d.key+d.features._b}return s}
	function setRet(n){const o=new R();let s=0;for(let i=0;i<n;i++)s+=(o.y=i);return s+":"+o.y}
	function readT(os,n){let s=0,e="";for(let i=0;i<n;i++){try{s+=os[i%os.length].t}catch(x){e+=x.name+x.message+";"}}return s+":"+e}
	function writeU(n){const o=new T(0,false);let e="";for(let i=0;i<n;i++){try{o.u=i}catch(x){e+=x.name+x.message+";"}}return o.id+":"+e}
	function args(n){const a=new A();let s=0;for(let i=0;i<n;i++){s+=a.n;a.m=i;s+=a.k}return s}
	function deeps(os,n){let s="";for(let i=0;i<n;i++){try{s+=os[i%os.length].deep}catch(x){s+=x.name+":"+x.message+"|"+x.stack+"|"}}return s}
	function stacks(n){const e=new E("o");let s="";for(let i=0;i<n;i++)s=e.st;return s}
	function halves(n){const os=[new H(2),new H(4)];let s=0;for(let i=0;i<n;i++)s+=os[i&1].half;return s}
	function sizes(n){const ms=[new Map([[1,1]]),new Map(),new Map([[1,1],[2,2]])];let s=0;for(let i=0;i<n;i++){let t=i;for(let j=0;j<20;j++)t=(t*31+j)%1009;s+=ms[i%3].size+t}return s}
	function strictWrite(n){"use strict";const o=new G();let e="";for(let i=0;i<n;i++){try{o.g=i}catch(x){e=x.name+":"+x.message}}return e+o.g}
	function sloppyWrite(n){const o=new G();for(let i=0;i<n;i++)o.g=i;return o.g+":"+Object.keys(o)}
	function literal(n){let s=0;for(let i=0;i<n;i++){const o={_v:i,get v(){return this._v*2}};s+=o.v}return s}
	function replaced(n){const before=readX(ps,n);const d=Object.getOwnPropertyDescriptor(P.prototype,"x");
		Object.defineProperty(P.prototype,"x",{get(){return -this._x},configurable:true});
		const after=readX(ps,n);Object.defineProperty(P.prototype,"x",d);return before+":"+after+":"+readX(ps,n)}
	function W(w){this.w=w}
	Object.defineProperty(W.prototype,"w2",{get(){return this.w*2},enumerable:true,configurable:true});
	const ws=[new W(1),new W(2)];
	function readW(n){let s=0;for(let i=0;i<n;i++)s+=ws[i&1].w2;return s}
	function inPlace(n){const before=readW(n);const g=Object.getOwnPropertyDescriptor(W.prototype,"w2").get;
		W.prototype.__defineGetter__("w2",function(){return this.w*3});const after=readW(n);
		W.prototype.__defineGetter__("w2",g);return before+":"+after+":"+readW(n)}
	function setInPlace(n){const before=writes(ps,n);const s=Object.getOwnPropertyDescriptor(P.prototype,"x").set;
		P.prototype.__defineSetter__("x",function(v){this._x=-v});const after=writes(ps,n);
		P.prototype.__defineSetter__("x",s);return before+":"+after+":"+writes(ps,n)}
	function shadowed(n){const o=new Q(9);let s=readX([o],n);Object.defineProperty(o,"x",{value:100,writable:true});s+=":"+readX([o],n);delete o.x;return s+":"+readX([o],n)}`

// A read or write whose site found an accessor on a prototype -- a
// class's getter or setter, one Object.defineProperty put there -- calls
// it from native code, as V8's code calls an accessor its feedback knows:
// inlined where it may be, the receiver its this; Go's otherwise, called
// from native code. Every answer, thrown error and stack trace is the
// interpreter's: getters and setters that throw, that read arguments,
// that are replaced or shadowed after the code is compiled, a setter's
// result dropped, a write strict code makes to a getter alone, a
// built-in's getter, the original receiver of one two prototypes up --
// with the collector marking too, which has Go make every store.
func TestJITAccessors(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	cases := []string{`readX(ps,40)`, `readX(qs,30)`, `readX(ss,30)`, `writeX(ps,40)`, `writeX(ss,9)`, `selves(ps,40)`, `keys(30)`,
		`setRet(30)`, `readT([new T(1,false),new T(2,true),new T(3,false)],30)`, `writeU(30)`, `args(30)`,
		`deeps([new E({v:1}),new E(undefined),new E({v:"s"})],12)`, `deeps([new E({v:1}),new E({get v(){return 7}}),new E({v:2})],30)`, `stacks(20)`, `String(halves(30))`, `String(sizes(30))`,
		`strictWrite(30)`, `sloppyWrite(30)`, `String(literal(30))`, `sums([new N(3),new N(5)],30)`, `sums([new N(3),new N(-2),new N(4)],30)`, `fars([nf(2,{v:1}),nf(3,{v:2})],30)`, `fars([nf(2,{v:1}),nf(3,{v:2}),nf(1,undefined)],30)`, `totals(40)`, `scaleds(40)`, `replaced(20)`, `inPlace(20)`, `setInPlace(20)`, `shadowed(20)`, `readX(ps,40)`}
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		want := New(Config{})
		r := jitRuntimeForTest(t, Config{JIT: true})
		for _, rt := range []*Runtime{want, r} {
			if _, err := rt.Run(compileForTest(t, jitAccessorSetup)); err != nil {
				t.Fatal(err)
			}
		}
		for round := 0; round < 3; round++ {
			for _, c := range cases {
				wv, werr := want.Run(compileForTest(t, "String("+c+")"))
				gv, gerr := r.Run(compileForTest(t, "String("+c+")"))
				if (werr == nil) != (gerr == nil) || werr != nil && werr.Error() != gerr.Error() {
					t.Fatalf("marking %v, round %d, %s: error %v, interpreter's %v", marking, round, c, gerr, werr)
				}
				if werr != nil {
					continue
				}
				if got, w := gv.String().Go(), wv.String().Go(); got != w {
					t.Fatalf("marking %v, round %d, %s:\n got %.400s\nwant %.400s", marking, round, c, got, w)
				}
			}
		}
		want.Close()
		want.ReleaseClosed()
	}
}

// A getter that throws uncaught throws from native code as from the
// interpreter: its error, and its stack, the getter's frame on top of the
// caller's.
func TestJITAccessorThrows(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := jitAccessorSetup + `
		function crash(os,n){let s=0;for(let i=0;i<n;i++)s+=os[i%os.length].t;return s}
		function crashDeep(os,n){let s=0;for(let i=0;i<n;i++)s+=os[i%os.length].deep;return s}
		function crashSum(os,n){let s=0;for(let i=0;i<n;i++)s+=os[i%os.length].sum;return s}
		function crashFar(os,n){let s=0;for(let i=0;i<n;i++)s+=os[i%os.length].far;return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []string{`crash([new T(1,false),new T(2,false)],20)`, `crash([new T(1,false),new T(2,false),new T(3,true)],60)`,
		`crashDeep([new E({v:1}),new E({v:2})],20)`, `crashDeep([new E({v:1}),new E({v:2}),new E(null)],40)`,
		`crashSum([new N(1),new N(4)],40)`, `crashSum([new N(1),new N(4),new N(-1)],40)`,
		`crashFar([nf(2,{v:1}),nf(3,{v:2})],40)`, `crashFar([nf(2,{v:1}),nf(3,{v:2}),nf(1,undefined)],40)`} {
		for round := 0; round < 3; round++ {
			// Uncaught in the loop, caught by a caller the interpreter runs:
			// its error, message and stack the interpreter's.
			src := "(function(){try{return String(" + c + ")}catch(e){return 'threw '+e.name+': '+e.message+'|'+e.stack}})()"
			wv, werr := want.Run(compileForTest(t, src))
			gv, gerr := r.Run(compileForTest(t, src))
			if werr != nil || gerr != nil {
				t.Fatalf("round %d, %s: %v, interpreter's %v", round, c, gerr, werr)
			}
			if got, w := gv.String().Go(), wv.String().Go(); got != w {
				t.Fatalf("round %d, %s:\n got %s\nwant %s", round, c, got, w)
			}
		}
	}
}

// Reading and writing an accessor in a loop never leaves native code: a
// small getter and setter are inlined, no call of Go made; a built-in's
// getter is called by Go from native code, which goes on after it.
func TestJITAccessorsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, jitAccessorSetup)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, warm, run string
		goCalls         bool
	}{
		{"readX", `readX(ps,40)`, `readX(ps,4000)`, false},
		{"readX", `readX(ss,40)`, `readX(ss,4000)`, false},
		{"writes", `writes(ps,40)`, `writes(ps,4000)`, false},
		{"writes", `writes(ss,40)`, `writes(ss,4000)`, false},
		{"selves", `selves(ps,40)`, `selves(ps,4000)`, false},
		{"setRet", `setRet(40)`, `setRet(4000)`, false},
		{"keys", `keys(40)`, `keys(1000)`, false},
		{"sizes", `sizes(40)`, `sizes(3000)`, true},
		{"sums", `sums([new N(3),new N(5)],40)`, `sums([new N(3),new N(5)],2000)`, false},
		{"totals", `totals(40)`, `totals(2000)`, false},
		{"scaleds", `scaleds(40)`, `scaleds(2000)`, false},
	} {
		// Warm until its caches' feedback has settled and its code, compiled
		// again for it on the existing tiers' budget (jitTierUp), is valid.
		for i := 0; i < 8; i++ {
			if _, err := r.Run(compileForTest(t, c.warm)); err != nil {
				t.Fatal(err)
			}
		}
		cl := r.global.getOwn(r.atoms.intern(c.name)).value().Object().fn().closure
		e := r.jit.hint(cl)
		if e == nil || e.ssa == nil {
			t.Fatalf("%s was not compiled: %+v", c.run, e)
		}
		before := e.ssaStats
		if _, err := r.Run(compileForTest(t, c.run)); err != nil {
			t.Fatal(err)
		}
		st := e.ssaStats
		if r.jit.hint(cl) != e || st.work-before.work < 1000 {
			t.Fatalf("%s did not run natively: %+v, before %+v", c.run, st, before)
		}
		if left := st.hosts - before.hosts; left != 0 {
			t.Errorf("%s left native code %d times", c.run, left)
		}
		if made := st.goCalls - before.goCalls; (made != 0) != c.goCalls {
			t.Errorf("%s made %d calls of Go", c.run, made)
		}
	}
}
