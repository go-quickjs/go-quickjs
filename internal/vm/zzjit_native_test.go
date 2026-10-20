//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

const jitSumSource = `function sum(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s } sum(10000)`

func TestJITPropertyABI(t *testing.T) {
	var p Property
	var cell ir.PropertyCell
	if unsafe.Sizeof(p) != unsafe.Sizeof(cell) || unsafe.Offsetof(p.key) != unsafe.Offsetof(cell.Key) || unsafe.Offsetof(p.flags) != unsafe.Offsetof(cell.Flags) || unsafe.Offsetof(p.value) != unsafe.Offsetof(cell.Bits) || unsafe.Offsetof(p.value)+unsafe.Offsetof(p.value.ref) != unsafe.Offsetof(cell.Reference) {
		t.Fatal("VM property layout differs from borrowed native cells")
	}
	if propWritable != 1 || propDefault != 7 || propAccessor|propDeleted|propPrivate|propNamespaceExport|propUninit != 0xf8 {
		t.Fatal("VM property permissions differ from native attributes")
	}
}

// Numeric fields are read and written natively: compiled at its first
// call, before its sites have run, f deoptimizes once at each (their map
// checks, which feed the sites' caches) and is compiled again for them;
// called again, it never leaves.
func TestJITNumericProperties(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	v, err := r.Run(compileForTest(t, `function f(o,n){let s=0;for(let i=0;i<n;i++){s=(s+o.x)|0;o.x=o.x+1}return s}let o={a:7,x:3};f(o,1000)===502500&&o.x===1003&&o.a===7`))
	if err != nil || !v.IsBool() || !v.Truthy() || r.jit == nil || r.jit.entries == 0 || jitOtherHosts(r) != 0 || jitOtherGuards(r) != 0 || r.jit.shapeDeopts > 3 {
		t.Fatalf("numeric fields did not stay native: %v, %v, %+v, %d map checks", v, err, r.JITStats(), r.jit.shapeDeopts)
	}
	entries, hosts, guards := r.jit.entries, r.jit.hosts, r.jit.guards
	v, err = r.Run(compileForTest(t, `o.x=3;f(o,1000)===502500&&o.x===1003&&o.a===7`))
	if err != nil || !v.IsBool() || !v.Truthy() || r.jit.entries == entries || r.jit.hosts != hosts || r.jit.guards != guards {
		t.Fatalf("numeric fields did not stay native once fed: %v, %v, %+v", v, err, r.JITStats())
	}
}

func TestJITThisSnapshot(t *testing.T) {
	for _, source := range []string{
		`function f(a){'use strict';let n=this;for(let i=0;i<4;i++)a[i]=n*2;return a[3]}f.call(3,[0,0,0,0])===6`,
		`function f(a){a[0]+=1;return this}let o={x:3};f.call(o,[0])===o`,
		`function f(a){'use strict';a[0]+=1;return this}f.call(null,[0])===null&&f.call(undefined,[0])===undefined`,
		`function f(a){a[0]+=1;return this}let a=[0];f.call(null,a)===globalThis&&a[0]===1`,
		`function f(a,cb){let x=this.x;cb();a[0]+=1;return this.x+x}let o={x:3};f.call(o,[0],()=>{o.x=7})===10`,
		`let o={x:3,f(a){a[0]+=1;return ()=>{a[0]+=1;return this.x+a[0]}}};o.f([0])()===5`,
		`let a=[0],good=false;class B{}class D extends B{constructor(){let f=()=>{a[0]+=1;return this};try{f()}catch(e){good=e instanceof ReferenceError}super();if(f()!==this)throw Error('receiver')}}new D();good&&a[0]===2`,
	} {
		for _, enabled := range []bool{false, true} {
			r := jitRuntimeForTest(t, Config{JIT: enabled})
			v, err := r.Run(compileForTest(t, source))
			if err != nil || !v.IsBool() || !v.Truthy() || enabled && (r.jit == nil || r.jit.entries == 0) {
				t.Fatalf("receiver JIT %v: %v, %v; %s", enabled, v, err, source)
			}
		}
	}
}

func TestJITPropertyFallbacks(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"reference", `function f(o,a){let v=o.x;a[0]+=1;return v}let x={};f({x},[0])===x`},
		{"negative zero", `function f(o,a,v){o.x=v;a[0]+=1;return o.x}let o={x:2};Object.is(f(o,[0],-0),-0)&&Object.is(o.x,-0)`},
		{"NaN", `function f(o,a,v){o.x=v;a[0]+=1;return o.x}let o={x:2};Number.isNaN(f(o,[0],NaN))&&Number.isNaN(o.x)`},
		{"readonly", `function f(o,a){o.x=7;a[0]+=1;return o.x}let o={x:3};Object.defineProperty(o,'x',{writable:false});f(o,[0])===3`},
		{"readonly strict", `function f(o,a){'use strict';a[0]+=1;o.x=7;return o.x}let a=[0],o={x:3};Object.defineProperty(o,'x',{writable:false});let good=false;try{f(o,a)}catch(e){good=e instanceof TypeError}good&&a[0]===1&&o.x===3`},
		{"accessor", `let hits=0;function f(o,a){o.x=7;a[0]+=1;return o.x}let o={get x(){hits++;return 3},set x(v){hits+=v}};f(o,[0])===3&&hits===8`},
		{"inherited", `function f(o,a){o.x=7;a[0]+=1;return o.x}let p={x:3},o=Object.create(p);f(o,[0])===7&&o.x===7&&p.x===3`},
		{"deleted", `function f(o,a){let v=o.x;a[0]+=1;return v}let o={x:3};delete o.x;f(o,[0])===undefined`},
		{"nonextensible", `function f(o,a){o.x=7;a[0]+=1;return o.x}let o={x:3};Object.preventExtensions(o);f(o,[0])===7`},
		{"large table", `function f(o,a){o.x=7;a[0]+=1;return o.x}let o={x:3,a:1,b:2,c:3,d:4,e:5,f:6,g:7,h:8,i:9};f(o,[0])===7&&o.i===9`},
		{"proxy", `let hits=0;function f(o,a){o.x=7;a[0]+=1;return o.x}let target={x:3},o=new Proxy(target,{get(t,k){hits++;return t[k]},set(t,k,v){hits++;t[k]=v;return true}});f(o,[0])===7&&hits===2`},
		{"alias mutation", `function f(o,p,a){o.x=7;a[0]+=1;return p.x}let o={x:3};f(o,o,[0])===7`},
		{"shape mutation", `function f(o,a,cb){o.x=7;cb(o);a[0]+=1;return o.x}let o={x:3};f(o,[0],o=>{for(let i=0;i<30;i++)o['p'+i]=i;Object.defineProperty(o,'x',{get(){return 11}})})===11`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				r := jitRuntimeForTest(t, Config{JIT: enabled})
				v, err := r.Run(compileForTest(t, tc.source))
				if err != nil || !v.IsBool() || !v.Truthy() || enabled && (r.jit == nil || r.jit.entries == 0 || jitOtherGuards(r) != 0) {
					t.Fatalf("property JIT %v: %v, %v", enabled, v, err)
				}
			}
		})
	}
}

func TestJITPropertyViewNotArray(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	v, err := r.Run(compileForTest(t, `function f(o,a){return o.length+a[0]}f({length:7},[3])===10`))
	if err != nil || !v.IsBool() || !v.Truthy() || r.jit == nil || r.jit.guards != 1 {
		t.Fatalf("ordinary property view granted array length access: %v, %v", v, err)
	}
}

func TestJITPropertyCallbackRelease(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 0,
		func(rt *Runtime, _ Value, _ []Value) (Value, error) {
			if rt.jit == nil {
				t.Fatal("callback retained native property views")
			}
			rt.releaseJIT()
			runtime.GC()
			return Undefined, nil
		}), propDefault)
	v, err := r.Run(compileForTest(t, `function f(o,a,cb){o.x=7;cb();a[0]+=1;return o.x+a[0]}let o={x:3};f(o,[0],()=>{host();delete o.x;o.y=9;o.x=11})===12&&o.y===9`))
	if err != nil || !v.IsBool() || !v.Truthy() || r.jit == nil || jitOtherGuards(r) != 0 {
		t.Fatalf("property view release: %v, %v", v, err)
	}
}

func TestJITRemainderBoundaries(t *testing.T) {
	for _, tc := range []struct{ left, right, check string }{
		{`13`, `5`, `v===3`}, {`-12`, `3`, `Object.is(v,-0)`},
		{`-0`, `2`, `Object.is(v,-0)`}, {`13.5`, `2.5`, `v===1`},
		{`1e100`, `3`, `v===1`}, {`Infinity`, `3`, `Number.isNaN(v)`},
		{`13`, `0`, `Number.isNaN(v)`}, {`13`, `Infinity`, `v===13`},
		{`'13'`, `5`, `v===3`}, {`13n`, `5n`, `v===3n`},
	} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		if _, err := r.Run(compileForTest(t, `function f(a,x,y){a[0]+=1;return x%y}`)); err != nil {
			t.Fatal(err)
		}
		fn := compileForTest(t, `let v=f([1],`+tc.left+`,`+tc.right+`);`+tc.check)
		v, err := r.Run(fn)
		if err != nil || !v.IsBool() || !v.Truthy() || r.jit == nil || r.jit.guards != 0 {
			t.Fatalf("%s %% %s: %v, %v", tc.left, tc.right, v, err)
		}
	}
	for _, source := range []string{
		`function f(a,x){a[0]+=1;return x%3}f([1],14)===2`,
		`function f(a,x){a[0]+=1;let y=x%3;return y}f([1],14)===2`,
		`function f(a,x,y){a[0]+=1;return x%y}let good=false;try{f([1],1n,0n)}catch(e){good=e instanceof RangeError}good`,
		`function f(a,x,y){a[0]+=1;return x%y}let good=false;try{f([1],1n,2)}catch(e){good=e instanceof TypeError}good`,
	} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, source))
		if err != nil || !v.IsBool() || !v.Truthy() || r.jit == nil || r.jit.entries == 0 || r.jit.guards != 0 {
			t.Fatalf("remainder fused/error result: %v, %v", v, err)
		}
	}
}

func TestJITRemainderCoercionRelease(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 0,
		func(rt *Runtime, _ Value, _ []Value) (Value, error) {
			if rt.jit == nil {
				t.Fatal("remainder coercion retained native roots")
			}
			rt.releaseJIT()
			runtime.GC()
			return Undefined, nil
		}), propDefault)
	v, err := r.Run(compileForTest(t, `let order='';let a=[1];function f(a,x,y){a[0]+=1;let rem=x%y;return rem+a[0]+a.length}f(a,{valueOf(){order+='x';host();a.push(7);return 13}},{valueOf(){order+='y';return 5}})===7&&order==='xy'`))
	if err != nil || !v.IsBool() || !v.Truthy() || r.jit == nil || r.jit.guards != 0 {
		t.Fatalf("remainder callback order/release: %v, %v", v, err)
	}
}

func TestJITGlobalReads(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		r := jitRuntimeForTest(t, Config{JIT: enabled})
		calls := 0
		r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 0,
			func(rt *Runtime, _ Value, _ []Value) (Value, error) {
				calls++
				return Undefined, nil
			}), propDefault)
		for _, source := range []string{
			`globalThis.g=3;function f(a){return g+a[0]}f([1])===4`,
			`g=7;f([1])===8`,
			`Object.defineProperty(globalThis,'g',{get(){host();return 9},configurable:true});f([1])===10`,
			`delete globalThis.g;let good=false;try{f([1])}catch(e){good=e instanceof ReferenceError}good`,
			`globalThis.g=20;let g=13;f([1])===14`,
		} {
			v, err := r.Run(compileForTest(t, source))
			if err != nil || !v.IsBool() || !v.Truthy() {
				t.Fatalf("global read JIT %v: %v, %v", enabled, v, err)
			}
		}
		if calls != 1 || enabled && (r.jit == nil || r.jit.guards != 0) {
			t.Fatal("global resolution did not preserve accessors or lexical shadowing")
		}
	}
}

func TestJITNumericGlobalLoop(t *testing.T) {
	for _, keyword := range []string{"var", "let", "const"} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, keyword+` scale=3,offset=7;function f(a,n){for(let i=0;i<n;i++)a[i]=i*scale+offset;return a[n-1]}let a=[];for(let i=0;i<512;i++)a[i]=0;f(a,512)===1540&&a[0]===7`))
		if err != nil || !v.IsBool() || !v.Truthy() || r.jit == nil || r.jit.entries == 0 || r.jit.hosts != 0 || r.jit.guards != 0 {
			t.Fatalf("%s numeric bindings did not stay native: %v, %v", keyword, v, err)
		}
	}
}

func TestJITNumericGlobalMutation(t *testing.T) {
	for _, source := range []string{
		`var g=1;function f(o,a,n){for(let i=0;i<n;i++){a[i]=g;o.g=g+1}return g+a[n-1]}let a=[];for(let i=0;i<128;i++)a[i]=0;f(globalThis,a,128)===257&&g===129&&a[0]===1`,
		`let g=3;function f(a,cb){a[0]+=g;cb();return g+a[0]}f([1],()=>{g=7})===11`,
		`globalThis.g=3;function f(a,cb){a[0]+=g;cb();return g+a[0]}let hits=0;f([1],()=>{for(let i=0;i<100;i++)globalThis['p'+i]=i;Object.defineProperty(globalThis,'g',{get(){hits++;return 9}})})===13&&hits===1`,
		`globalThis.g=3;function f(a,cb){a[0]+=g;cb();return g+a[0]}let a=[1],good=false;try{f(a,()=>{delete globalThis.g})}catch(e){good=e instanceof ReferenceError}good&&a[0]===4`,
		`function f(a){a[0]+=1;return g+a[0]}let a=[1],good=false;try{f(a)}catch(e){good=e instanceof ReferenceError}let g=7;good&&a[0]===2&&f([1])===9`,
		`let scale=3,offset=7;function f(a,n){'use strict';for(let i=0;i<n;i++)a[i]=this*scale+offset;return a[n-1]}let a=[];for(let i=0;i<128;i++)a[i]=0;f.call(2,a,128)===13`,
		`globalThis.g=3;function f(a,cb){a[0]+=g;cb();return g+a[0]}f([1],()=>{g={valueOf(){return 7}}})===11`,
	} {
		for _, enabled := range []bool{false, true} {
			r := jitRuntimeForTest(t, Config{JIT: enabled})
			v, err := r.Run(compileForTest(t, source))
			if err != nil || !v.IsBool() || !v.Truthy() || enabled && (r.jit == nil || r.jit.entries == 0) {
				t.Fatalf("binding mutation JIT %v: %v, %v; %s", enabled, v, err, source)
			}
		}
	}
}

// A script's let, const and class bindings, read and written by native
// code where they are (ssa.OpLexCell): each answer the interpreter's, and
// those that need no Go read natively. A binding in its temporal dead zone
// leaves to Go, which throws, as does a write to a const. A class's
// constructor is inlined in the loop that constructs with it, which leaves
// for Go at its first constructions -- its pool filled, the transition its
// store adds x by learned -- as a function's is (learning): it had been
// refused, and each construction was a call of Go.
func TestJITLexicalGlobals(t *testing.T) {
	for _, tc := range []struct {
		source   string
		native   bool
		learning uint64
	}{
		{`let k=3;function f(n){let s=0;for(let i=0;i<n;i++)s+=k;return s}f(100)`, true, 0},
		{`const k=2.5;function f(n){let s=0;for(let i=0;i<n;i++)s+=k;return s}f(100)`, true, 0},
		{`let c=0;function f(n){for(let i=0;i<n;i++)c+=i;return c}f(100)+c`, true, 0},
		{`'use strict';let c=0;function f(n){for(let i=0;i<n;i++)c=c*3+i|0;return c}f(100)+c`, true, 0},
		{`class P{constructor(x){this.x=x}}function f(n){let s=0;for(let i=0;i<n;i++)s+=new P(i).x;return s}f(100)`, true, 3},
		{`const g=x=>x*2;function f(n){let s=0;for(let i=0;i<n;i++)s+=g(i);return s}f(100)`, true, 0},
		{`function f(n){let s=0;for(let i=0;i<n;i++)s+=i;return s+late}let r=0;try{f(10)}catch(e){r=e instanceof ReferenceError?1:2}let late=5;r*1000+f(10)`, false, 0},
		{`function f(n){for(let i=0;i<n;i++)late=i;return late}let r=0;try{f(10)}catch(e){r=e instanceof ReferenceError?1:2}let late=5;r*1000+f(10)`, false, 0},
		{`const K=1;function f(n){let s=0;for(let i=0;i<n;i++){s+=i;if(i===n-1)K=s}return s}let r=0;try{f(10)}catch(e){r=e instanceof TypeError?1:2}r*1000+K+f.length`, false, 0},
		{`let k=1;function f(n){let s=0;for(let i=0;i<n;i++){s+=k;if(i===50)k='x'}return s}f(100)`, false, 0},
	} {
		want := New(Config{})
		wv, err := want.Run(compileForTest(t, tc.source))
		want.Close()
		if err != nil {
			t.Fatalf("interpreter: %v; %s", err, tc.source)
		}
		r := jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, tc.source))
		if err != nil {
			t.Fatalf("JIT: %v; %s", err, tc.source)
		}
		ws, _ := want.ToString(wv)
		gs, _ := r.ToString(v)
		if gs.Go() != ws.Go() {
			t.Fatalf("JIT %s, interpreter %s; %s", gs.Go(), ws.Go(), tc.source)
		}
		if r.jit == nil || r.jit.ssaEntries == 0 || tc.native && (jitOtherHosts(r) > tc.learning || jitOtherGuards(r) != 0 || r.jit.interpreted > tc.learning+r.jit.shapeDeopts+r.jit.shapeHosts) {
			t.Fatalf("not native: %+v; %s", r.JITStats(), tc.source)
		}
		if tc.learning != 0 {
			// What it learned, it learned: the constructor inlined, run
			// natively after.
			e := r.jit.hint(r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure)
			if e == nil || len(e.ssaInlined) == 0 {
				t.Fatalf("P not inlined in f: %+v; %s", e, tc.source)
			}
		}
	}
}

// A regular expression literal's object is taken natively from a pool of
// RegExps of its pattern the VM fills (jitLiteralAt): each evaluation a
// fresh one, its lastIndex its own and 0, its prototype RegExp.prototype,
// past many refills of the pool. Every answer is the interpreter's. While
// the collector marks, native code takes each from the pool itself
// (markingTake), and leaves for Go no more often: under GOGC=5 one loop
// had left at 686 of its 1,000 iterations.
func TestJITSSARegExpLiterals(t *testing.T) {
	t.Run("marking", func(t *testing.T) {
		jitMarkingForTest(t)
		testJITSSARegExpLiterals(t)
	})
	testJITSSARegExpLiterals(t)
}

func testJITSSARegExpLiterals(t *testing.T) {
	for _, source := range []string{
		`function f(n){let a=[];for(let i=0;i<n;i++)a.push(/x/g);let d=0;for(let i=1;i<n;i++)if(a[i]!==a[i-1])d++;return d}f(1000)`,
		`function f(n){let c=0;for(let i=0;i<n;i++){let r=/b/g;r.test('abb');c+=r.lastIndex}return c}f(1000)`,
		`function f(n){let c=0;for(let i=0;i<n;i++){let r=/b/g;if(i%2)r.lastIndex=2;c+=r.test('abb')?r.lastIndex:-1}return c}f(1000)`,
		`function f(n){let c=0;for(let i=0;i<n;i++){let r=/(\d+)-(\d+)/;let m=r.exec('x'+i+'-'+(i*2));c+=m[1]*1+m[2]*1+(Object.getPrototypeOf(r)===RegExp.prototype?1:0)}return c}f(1000)`,
		`function f(s,n){let c=0;for(let i=0;i<n;i++)c+=s.replace(/b+/g,'x').length+(/^a/.test(s)?1:0);return c}f('abbc',1000)`,
	} {
		want := New(Config{})
		wv, err := want.Run(compileForTest(t, source))
		want.Close()
		if err != nil {
			t.Fatalf("interpreter: %v; %s", err, source)
		}
		r := jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, source))
		if err != nil || v.Number() != wv.Number() {
			t.Fatalf("JIT %v, %v; interpreter %v; %s", v, err, wv, source)
		}
		if r.jit == nil || r.jit.ssaEntries == 0 || r.jit.hosts >= 100 {
			t.Fatalf("not native: %+v; %s", r.JITStats(), source)
		}
	}
}

func TestJITGlobalCacheOwner(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `globalThis.g=3;function f(a){return g+a[0]}f([1])`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	for _, in := range cl.fn.Code {
		if in.Op != bytecode.OpGetGlobal {
			continue
		}
		// Native code reads the global where the closure's scope has it,
		// whatever the site's cache holds.
		site := &cl.ic[in.B]
		site.p1 = r.NewObject()
		entries := r.jit.ssaEntries
		v, err := r.Run(compileForTest(t, `f([1])`))
		if err != nil || v.Number() != 4 || r.jit.ssaEntries == entries || r.jit.hosts != 0 {
			t.Fatalf("global cache retained a stale environment: %v, %v", v, err)
		}
		return
	}
	t.Fatal("missing global cache site")
}

func TestJITCallbackTierSelection(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function cb(){};function f(a,n,cb){for(let i=0;i<n;i++){a[0]+=1;cb()}return a[0]}f([0],64,cb)`)); err != nil {
		t.Fatal(err)
	}
	for range 7 {
		if _, err := r.Run(compileForTest(t, `f([0],64,cb)`)); err != nil {
			t.Fatal(err)
		}
	}
	// Its calls are native code's: Go enters it and it runs to the end.
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	if e := r.entryOf(cl); e == nil || e.ssa == nil || e.entrySlow || cl.jitRefused {
		t.Fatal("callback-heavy entry did not stay native")
	}
	hosts, interpreted := r.jit.hosts, r.jit.interpreted
	v, err := r.Run(compileForTest(t, `f([0],10000,cb)`))
	if err != nil || v.Number() != 10000 || r.jit.guards != 0 || r.jit.hosts != hosts || r.jit.interpreted != interpreted {
		t.Fatalf("later long call left native code: %v, %v", v, err)
	}
}

// A call site that has seen closures of several functions checks each by
// its code in turn, and one that misses the first goes on to the next with
// the callee it was given: each's calls stay native, none left to Go.
func TestJITCallByCodeSecondCandidate(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	setup := `function each(a, fn) { let s = 0; for (let i = 0; i < a.length; i++) s += fn(a[i]); return s }
		function run(k) { const f = k % 3 == 0 ? (x => x + k) : k % 3 == 1 ? (x => x * k) : (x => x - k); return each([1, 2, 3, 4], f) }
		function runs(n) { let r = 0; for (let k = 0; k < n; k++) r += run(k); return r }
		runs(200)`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	e := jitNativeForTest(t, r, "each")
	hosts, slow, guards := e.ssaStats.hosts, e.slowCalls, r.jit.guards
	v, err := r.Run(compileForTest(t, `runs(300)`))
	if err != nil || v.Number() != 150700 || e.ssaStats.hosts != hosts || e.slowCalls != slow || r.jit.guards != guards {
		t.Fatalf("runs(300) = %v, %v; each ended in Go %d times, called slowly %d times, %d guards failed",
			v, err, e.ssaStats.hosts-hosts, e.slowCalls-slow, r.jit.guards-guards)
	}
}

// An array made a WeakMap's key keeps its elements where native code reads
// them, in the middle of a loop that reads them, and so do one made a key
// before the code was compiled and a function called through a site that
// knows its code.
func TestJITWeakMapKeyArray(t *testing.T) {
	setup := `function sum(a, m, n) { let s = 0; for (let i = 0; i < n; i++) { s += a[i % a.length]; if (i == 50) m.set(a, i); if (i == 70) a.push(i) } return s }
		function each(a, fn) { let s = 0; for (let i = 0; i < a.length; i++) s += fn(a[i]); return s }
		const keyed = [5, 6, 7], m = new WeakMap(), g = x => x * 3
		m.set(keyed, 1); m.set(g, 2)`
	r := jitAgreesForTest(t, setup,
		`sum([1, 2, 3], new WeakMap(), 200)`, `sum([1, 2, 3], new WeakMap(), 200)`, `sum([1, 2, 3], new WeakMap(), 200)`,
		`sum(keyed, m, 200) + m.get(keyed)`, `sum(keyed, m, 200) + m.get(keyed)`,
		`each([1, 2, 3, 4], g) + m.get(g)`, `each([1, 2, 3, 4], g) + m.get(g)`, `each([1, 2, 3, 4], g) + m.get(g)`,
	)
	jitNativeForTest(t, r, "sum")
	jitNativeForTest(t, r, "each")
}

func TestJITBoundaryDensityBudget(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	v, err := r.Run(compileForTest(t, `function f(a,n){let s=0;for(let i=0;i<n;i++){s+=a[0]%3;a[0]+=3}return s}let a=[2];f(a,20000)===40000&&a[0]===60002`))
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	// The loop's budget runs out, and native code polls and goes on.
	if e := r.entryOf(cl); err != nil || !v.IsBool() || !v.Truthy() || e == nil || e.ssa == nil || e.entrySlow ||
		r.jit.budgets == 0 || r.jit.guards != 0 || r.jit.hosts != 0 || r.jit.interpreted != 0 {
		t.Fatalf("boundary density changed effects or left native code: %v, %v", v, err)
	}
}

func TestJITClosureCacheRelease(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function f(a){return a[0]+1}`)); err != nil {
		t.Fatal(err)
	}
	call := compileForTest(t, `f([2])`)
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	for range 3 {
		v, err := r.Run(call)
		if err != nil || v.Number() != 3 || r.entryOf(cl) == nil || r.entryOf(cl).ssa.Size() == 0 {
			t.Fatalf("closure cache entry: %v, %v", v, err)
		}
		e := r.entryOf(cl)
		r.releaseJIT()
		if r.jit != nil || e.ssa.Size() != 0 {
			t.Fatal("cached closure kept released executable storage")
		}
	}
}

func TestJITArrayGrowth(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"descending", `function f(a){for(let i=31;i>=0;i--)a[i]=i;return a[0]+a[31]+a.length}f([])===63`},
		{"aliases", `function f(a,b){let s=0;for(let i=0;i<300;i++){a[i]=i;s+=b[i]+b.length}return s}let a=[];f(a,a)===90000`},
		{"holes", `function f(a){a[4]=7;a[0]=3;a[2]=5;return a.length+a[0]+a[2]+a[4]}let a=[];f(a)===20&&!(1 in a)&&!(3 in a)`},
		{"reference barrier", `function f(a,v){a[0]=v;a[16]=v;return a.length}let v={x:9},a=[];f(a,v)===17&&a[0]===v&&a[16]===v`},
		{"readonly length", `function f(a){a[1]=9;return a.length}let a=[2];Object.defineProperty(a,'length',{writable:false});f(a)===1&&!(1 in a)`},
		{"nonextensible hole", `function f(a){a[0]=9;return a.length}let a=[,];Object.preventExtensions(a);f(a)===1&&!(0 in a)`},
		{"inherited readonly", `Object.defineProperty(Array.prototype,'2',{value:8,writable:false});function f(a){a[2]=9;return a.length}let a=[];f(a)===0&&a[2]===8`},
		{"inherited setter", `let hits=0;Object.defineProperty(Array.prototype,'2',{set(v){hits+=v}});function f(a){a[2]=9;return a.length}let a=[];f(a)===0&&hits===9&&!(Object.hasOwn(a,'2'))`},
		{"sparse gap", `function f(a){a[2000]=9;return a.length}let a=[];f(a)===2001&&a[2000]===9&&!(1999 in a)`},
		{"nonindex", `function f(a,k){a[k]=9;return a.length}let a=[];f(a,4294967295)===0&&a[4294967295]===9&&f(a,-1)===0&&a[-1]===9`},
		{"proxy", `let hits=0;function f(a){a[2]=9;return 7}let a=[];let p=new Proxy(a,{set(t,k,v){hits++;return Reflect.set(t,k,v)}});f(p)===7&&hits===1&&a[2]===9`},
		{"typed array", `function f(a){a[0]=257;return 7}let a=new Uint8Array(1);f(a)===7&&a[0]===1`},
		{"strict error", `function f(a){'use strict';a[1]=9;return 7}let a=[2];Object.freeze(a);let good=false;try{f(a)}catch(e){good=e instanceof TypeError}good&&a.length===1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				r := jitRuntimeForTest(t, Config{JIT: enabled})
				v, err := r.Run(compileForTest(t, tc.source))
				if err != nil || !v.IsBool() || !v.Truthy() {
					t.Fatalf("JIT %v: %v, %v", enabled, v, err)
				}
				if enabled && (r.jit == nil || r.jit.entries == 0) {
					t.Fatal("array write did not enter native code or retained roots")
				}
				if enabled && (tc.name == "descending" || tc.name == "aliases" || tc.name == "holes") && r.jit.guards != 0 {
					t.Fatal("ordinary array growth permanently guarded")
				}
			}
		})
	}
}

func TestJITArrayHolePermissionCallbacks(t *testing.T) {
	for _, source := range []string{
		`let hits=0;function cb(){Object.defineProperty(Array.prototype,'1',{set(v){hits+=v}})}function f(a,cb){a[0]=2;cb();a[1]=3;return a.length}let a=new Array(2);f(a,cb)===2&&hits===3&&!Object.hasOwn(a,'1')&&a[0]===2`,
		`function cb(a){Object.defineProperty(a,'length',{writable:false});Object.preventExtensions(a)}function f(a,cb){a[0]=2;cb(a);a[1]=3;return a.length}let a=new Array(2);f(a,cb)===2&&!Object.hasOwn(a,'1')&&a[0]===2`,
		`function cb(a){a.length=40;a[39]=9}function f(a,b,cb){a[0]=2;cb(a);b[1]=3;return b[0]+b[1]+b[39]+b.length}let a=new Array(2);f(a,a,cb)===54`,
	} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, source))
		if err != nil || !v.IsBool() || !v.Truthy() || r.jit == nil || jitOtherGuards(r) != 0 {
			t.Fatalf("hole permission after callback: %v, %v", v, err)
		}
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	v, err := r.Run(compileForTest(t, `function f(a){for(let i=31;i>=0;i--)a[i]=i;return a[31]+a.length}f([])`))
	if err != nil || v.Number() != 63 || r.jit == nil || r.jit.hosts != 0 || r.jit.guards != 0 {
		t.Fatalf("descending initialization did not fill holes natively: %v, %v", v, err)
	}
}

func TestJITArrayWriteCoercionRelease(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	calls := 0
	r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 0,
		func(rt *Runtime, _ Value, _ []Value) (Value, error) {
			calls++
			if rt.jit == nil {
				t.Fatal("array coercion released the JIT")
			}
			rt.releaseJIT()
			runtime.GC()
			return Undefined, nil
		}), propDefault)
	v, err := r.Run(compileForTest(t, `let a=[1];function f(a,k){a[k]=9;return a[0]+a[1]+a.length}f(a,{toString(){host();a.push(3);a[0]=5;return '1'}})`))
	if err != nil || v.Number() != 16 || calls != 1 {
		t.Fatalf("array coercion: %v, %v, calls %d", v, err, calls)
	}
}

func TestJITArrayGrowthLimits(t *testing.T) {
	for _, memory := range []bool{false, true} {
		cfg := Config{JIT: true}
		if memory {
			cfg.MemoryLimit = 8 << 20
		}
		r := jitRuntimeForTest(t, cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if !memory {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
		}
		defer cancel()
		r.SetContext(ctx)
		_, err := r.Run(compileForTest(t, `function f(a){let i=0;for(;;){a[i]=i;i++}}f([])`))
		want := error(context.DeadlineExceeded)
		if memory {
			want = ErrMemoryLimit
		}
		if !errors.Is(err, want) || r.jit == nil || r.jit.budgets == 0 {
			t.Fatalf("array growth memory %v: %v", memory, err)
		}
	}
}

func TestJITReferenceReturns(t *testing.T) {
	for _, tc := range []struct{ expression, check string }{
		{`{x:9}`, `v.x===9`},
		{`[7,8]`, `v.length===2&&v[0]===7&&v[1]===8`},
		{`'kept'`, `v==='kept'`},
		{`12345678901234567890n`, `v===12345678901234567890n`},
		{`Symbol('kept')`, `typeof v==='symbol'&&v.description==='kept'`},
		{`function(){return 11}`, `v()===11`},
	} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		if _, err := r.Run(compileForTest(t, `function f(a,out){a[0]+=1;return out}`)); err != nil {
			t.Fatal(err)
		}
		fn := compileForTest(t, `f([1],`+tc.expression+`)`)
		for range 12 {
			v, err := r.Run(fn)
			if err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			r.global.setOwnRaw(r.atoms.intern("v"), v, propDefault)
			check, err := r.Run(compileForTest(t, tc.check))
			if err != nil || !check.IsBool() || !check.Truthy() {
				t.Fatalf("returned %s did not survive root clearing: %v, %v", tc.expression, check, err)
			}
		}
		if r.jit == nil || r.jit.guards != 0 {
			t.Fatal("reference return guarded or retained scratch roots")
		}
	}
}

// The new pipeline carries references it never touches: native code holds
// a reference's number word only, and an exit has Go copy it from the slot
// it came from, or clear it where a primitive replaces it (abi.Record). Each
// function here moves, returns or overwrites a reference across polls,
// which a budget of one makes at every back-edge, and must answer as the
// interpreter does, natively.
func TestJITSSAReferences(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, tc := range []struct {
		name, fn, run string
		records       bool // whether exits must move or clear a reference
	}{
		{"return", `function f(o,n){let s=0;for(let i=0;i<n;i++)s+=i;return o}`,
			`var o={k:1};[f(o,5)===o,f('str',5),f(7n,3)]`, false},
		{"copy", `function f(o,n){let x=0;for(let i=0;i<n;i++){x=o}return x}`,
			`var o=[1];[f(o,5)===o,f('a',2),f(o,0)]`, true},
		// A remainder by 1.5 exits to Go at every iteration: the first exit
		// has Go copy o into x's slot, and the next replaces it with a
		// number there, which Go must clear.
		{"overwrite", `function f(o,n){let x=o,y=0;for(let i=0;i<n;i++){if(i>0)x=i;y+=i%1.5}return y>99?y:x}`,
			`var o={};[f(o,5),f(o,1)===o,f(o,0)===o,f(Symbol.iterator,1)===Symbol.iterator]`, true},
		{"merge", `function f(o,n){let r=o;for(let i=0;i<n;i++){if(i==3)r=i}return r}`,
			`var o={};[f(o,2)===o,f(o,6),f('s',1)]`, true},
		{"swap", `function f(a,b,n){for(let i=0;i<n;i++){let t=a;a=b;b=t}return a}`,
			`var a={},b=[];[f(a,b,3)===b,f(a,b,4)===a,f(1,2,3)]`, false},
		{"rotate", `function f(a,b,c,n){for(let i=0;i<n;i++){let t=a;a=b;b=c;c=t}return [a,b,c]}`,
			`var a={},b=[],c='c';var r=f(a,b,c,4);[r[0]===b,r[1]===c,r[2]===a]`, false},
	} {
		// Native code applies an exit's records itself unless the collector
		// marks; marking, they are Go's.
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/marking=%v", tc.name, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				src := tc.fn + ";" + tc.run + ".map(String).join()"
				want := New(Config{})
				defer func() { want.Close(); want.ReleaseClosed() }()
				wv, err := want.Run(compileForTest(t, src))
				if err != nil {
					t.Fatal(err)
				}
				r := jitRuntimeForTest(t, Config{JIT: true})
				r.jitStress = jitStressConfig{threshold: true, budget: 1}
				gv, err := r.Run(compileForTest(t, src))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := gv.String().Go(), wv.String().Go(); got != want {
					t.Fatalf("got %q, interpreter %q", got, want)
				}
				if st := r.JITStats(); st.SSAEntries == 0 || marking && tc.records && st.SSARecords == 0 {
					t.Fatalf("never entered the new pipeline, or left no reference to Go: %+v", st)
				}
			})
		}
	}
}

// The new pipeline reads arrays in place: an array's elements that
// hold numbers, read and written natively, and everything else -- holes,
// other values, objects that are not arrays, keys that are not indices --
// left to Go or to the interpreter. Each function must answer as the
// interpreter does, under polls at every back-edge, and run natively.
func TestJITSSAArrays(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, tc := range []struct{ name, src string }{
		{"sum", `function f(a){let s=0;for(let i=0;i<a.length;i++)s+=a[i];return s}
			[f([1,2,3,4.5]),f([]),f([1,,3]),f([1,'2',3]),f({length:2,0:5,1:6}),f(new Float64Array([1,2]))]`},
		{"write", `function f(a,n){for(let i=0;i<n;i++)a[i]=a[i]*2+1;return a}
			[f([1,2,3],3),f([1,2,3],5),f([1,,3],3),f(['x',2],2),f([0.5,NaN],2)].map(a=>a.join(':'))`},
		{"update", `function f(a){let i=0,s=0;while(i<a.length)s+=a[i++];return s+i}
			[f([1,2,3]),f([5]),f([])]`},
		{"keys", `function f(a,k){let s=0;for(let i=0;i<3;i++)s+=a[k];return s}
			[f([7,8],1),f([7,8],1.5),f([7,8],-0),f([7,8],-1),f([7,8],2**32),f([7,8],NaN),f([7,8],'1')]`},
		{"sparse", `function f(a){let s=0;for(let i=0;i<a.length;i++)s+=a[i]|0;return s}
			var b=[1,2];b[50000]=3;[f(b),f([1,2])]`},
		{"swap", `function f(a,b,n){let s=0;for(let i=0;i<n;i++){let t=a;a=b;b=t;s+=a[0]}return s}
			[f([1],[2],5),f([1],[2],4)]`},
		{"arguments", `function f(a){let s=0;for(let i=0;i<a.length;i++)s+=a[i];return s}
			function g(){return f(arguments)}
			[g(1,2,3),f([4,5]),g()]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.src + ".map(v=>JSON.stringify(v)).join('|')"
			want := New(Config{})
			defer func() { want.Close(); want.ReleaseClosed() }()
			wv, err := want.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitStress = jitStressConfig{threshold: true, budget: 1}
			gv, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := gv.String().Go(), wv.String().Go(); got != want {
				t.Fatalf("got %s, interpreter %s", got, want)
			}
			if st := r.JITStats(); st.SSAEntries == 0 {
				t.Fatalf("never entered the new pipeline: %+v", st)
			}
		})
	}
}

// The new pipeline reads captured bindings in place, through their cells:
// a count, an array, a binding another closure changes between calls, a
// reference that leaves through a record, and one still in its temporal
// dead zone.
func TestJITSSACaptured(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, tc := range []struct{ name, src string }{
		{"count", `function make(n){return function(a){let s=0;for(let i=0;i<n;i++)s+=a[i];return s}}
			var f=make(3);[f([1,2,3,4]),f([5,6,7]),make(0)([1])]`},
		{"array", `function make(a){return function(n){let s=0;for(let i=0;i<n;i++)s+=a[i];a[0]=s;return s}}
			var f=make([1,2,3]);[f(3),f(3),f(1)]`},
		{"changed", `function make(){let k=1;return [function(){let s=0;for(let i=0;i<10;i++)s+=k;return s},function(v){k=v}]}
			var p=make();var r=[p[0]()];p[1](2);r.push(p[0]());p[1]('x');r.push(p[0]());r`},
		{"reference", `function make(o){return function(n){let x=0;for(let i=0;i<n;i++)x=o;return x}}
			var o={k:1};var f=make(o);[f(3)===o,f(0),make('s')(2)]`},
		{"dead", `function g(){let f=function(n){let s=0;for(let i=0;i<n;i++)s+=x;return s};let r;try{r=f(3)}catch(e){r=e.name}let x=2;return [r,f(3)]}
			g()`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.src + ".map(v=>JSON.stringify(v)).join('|')"
			want := New(Config{})
			defer func() { want.Close(); want.ReleaseClosed() }()
			wv, err := want.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitStress = jitStressConfig{threshold: true, budget: 1}
			gv, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := gv.String().Go(), wv.String().Go(); got != want {
				t.Fatalf("got %s, interpreter %s", got, want)
			}
			if st := r.JITStats(); st.SSAEntries == 0 {
				t.Fatalf("never entered the new pipeline: %+v", st)
			}
		})
	}
}

// The new pipeline reads and writes properties in place where the site's
// cache knows the shape. Each case warms its sites with the JIT off,
// so that the caches are filled when the function compiles, then runs
// under polls at every back-edge, with objects of the cached shape and
// others: another layout, an accessor, a value that is not a number.
func TestJITSSAProperties(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, tc := range []struct{ name, setup, warm, run string }{
		{"read", `function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.x*o.y;return s}`,
			`f({x:1,y:2},3)`,
			`[f({x:1.5,y:2},10),f({y:2,x:3},4),f({x:'2',y:3},2),f({get x(){return 4},y:1},2),f({x:1,y:2,z:3},2)]`},
		{"write", `function f(o,n){for(let i=0;i<n;i++)o.x=o.x+o.y;return o}`,
			`f({x:1,y:2},3)`,
			`[f({x:1,y:0.5},10),f({y:1,x:2},3),f(Object.freeze({x:1,y:2}),2),f({x:'a',y:1},2)].map(o=>JSON.stringify(o))`},
		{"method", `function P(x){this.x=x;this.v=1}P.prototype.run=function(n){let s=0;for(let i=0;i<n;i++)s+=this.x*this.v;return s}`,
			`new P(1).run(3)`,
			`[new P(2).run(10),new P(0.5).run(4)]`},
		// References read from objects, which native code carries by their
		// cells: nested, followed down a list, an array held in a
		// property, one returned, and one a method reads through this.
		{"nested", `function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.p.x*o.p.y;return s}`,
			`f({p:{x:1,y:2}},3)`,
			`[f({p:{x:1.5,y:2}},10),f({p:{y:2,x:3}},4),f({p:{x:1}},2),f({p:{x:'s',y:1}},2),f({q:1,p:{x:2,y:3}},1)]`},
		{"list", `function f(h){let s=0,n=h;while(n){s+=n.v;n=n.next}return s}`,
			`f({v:1,next:{v:2,next:null}})`,
			`[f({v:1,next:{v:2,next:{v:3,next:null}}}),f(null),f({v:4,next:{v:'x',next:null}}),f({v:5,next:{w:1,next:undefined}})]`},
		{"array", `function f(o){let s=0;for(let i=0;i<o.a.length;i++)s+=o.a[i];return s}`,
			`f({a:[1,2]})`,
			`[f({a:[1,2,3]}),f({a:[]}),f({a:[1,'2']}),f({a:{length:1,0:5}})]`},
		{"returned", `function f(o,n){let r=0;for(let i=0;i<n;i++)r=o.p;return r}`,
			`f({p:{}},2)`,
			`var q={k:1};[f({p:q},3)===q,f({p:'s'},2),f({p:q},0)]`},
		{"items", `function B(){this.items=[1,2,3]}B.prototype.sum=function(){let s=0;for(let i=0;i<this.items.length;i++)s+=this.items[i];return s}`,
			`new B().sum()`,
			`[new B().sum(),new B().sum()]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.setup + ";" + tc.warm + ";"
			run := tc.run + ".map(v=>JSON.stringify(v)).join('|')"
			want := New(Config{})
			defer func() { want.Close(); want.ReleaseClosed() }()
			if _, err := want.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			wv, err := want.Run(compileForTest(t, run))
			if err != nil {
				t.Fatal(err)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitEnabled = false
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			r.jitEnabled = true
			r.jitStress = jitStressConfig{threshold: true, budget: 1}
			gv, err := r.Run(compileForTest(t, run))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := gv.String().Go(), wv.String().Go(); got != want {
				t.Fatalf("got %s, interpreter %s", got, want)
			}
			st := r.JITStats()
			t.Logf("%+v", st)
			if st.SSAEntries == 0 {
				t.Fatalf("never entered the new pipeline: %+v", st)
			}
		})
	}
}

// The new pipeline reads globals where the interpreter last found them, in
// place, as cells: numbers, a function a call is given, an object whose
// property is read, Math. Later scripts declare script-level lexical
// bindings -- one of them shadowing a global property, which reads must
// then see past -- and assign the globals new values.
func TestJITSSAGlobals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var N=10,O={x:2};function g(x){return x+1}
		function count(){let s=0;for(let i=0;i<N;i++)s+=i;return s}
		function calls(n){let s=0;for(let i=0;i<n;i++)s=g(s);return s}
		function field(n){let s=0;for(let i=0;i<n;i++)s+=O.x;return s}
		function math(n){let s=0;for(let i=0;i<n;i++)s+=Math.abs(-i);return s}
		globalThis.P=5;function readP(n){let s=0;for(let i=0;i<n;i++)s+=P;return s}
		count();calls(2);field(2);math(2);readP(2)`
	rounds := []string{
		`[count(),calls(5),field(5),math(5),readP(3)].join()`,
		`let L=1;N=4;O={y:1,x:3};g=function(x){return x+2};[count(),calls(5),field(5),math(5),L].join()`,
		// A lexical binding shadows the global property P from here on.
		`let P=7;[readP(3),count()].join()`,
		`globalThis.N='3';[count(),field(2)].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitEnabled = false
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	r.jitEnabled = true
	r.jitStress = jitStressConfig{threshold: true, budget: 1}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	if st := r.JITStats(); st.SSAEntries == 0 {
		t.Fatalf("never entered the new pipeline: %+v", st)
	} else {
		t.Logf("%+v", st)
	}
}

// A call goes to Go once: the callee, a global, and an argument read from
// an object stay native, carried by their cells, though only Go uses them;
// Go makes the call from native code (ssa.GoCall), or after an exit. (h stores, so it is not inlined; TestJITSSAInline
// inlines.) The loops do enough besides to be worth running natively
// (jitSSAProfit). A function compiled at its first call, before the
// interpreter has run its global reads, finds the global where the global
// object has it; deleting and defining it again moves it, which the cell's
// key check sees.
func TestJITSSACallExits(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// h reads arguments: Go calls it, never native code (jitNativeCallee).
	setup := `var O={p:[1,2,3]};function h(a){a.n=arguments.length;return a.length}
		function f(n){let s=0;for(let i=0;i<n;i++){s=(s+g(i))|0;s=(s*31+i)|0;s^=s>>>3;s=(s+i*7)|0;s^=s<<2}return s}
		function k(n){let s=0;for(let i=0;i<n;i++){s=(s+h(O.p))|0;s=(s*31+i)|0;s^=s>>>3;s=(s+i*7)|0;s^=s<<2}return s}`
	rounds := []struct {
		src   string
		hosts uint64 // the exits it makes, if it must make that many
	}{
		{`f(100)`, 100},
		{`k(100)`, 100},
		{`delete globalThis.g;globalThis.x0=1;globalThis.g=function(i){return i*2};f(10)`, 0},
		{`O={q:0,p:'abcd'};k(10)`, 0},
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitCallThreshold, r.jitICThreshold = 1, 1
	for _, rt := range []*Runtime{want, r} {
		rt.global.setOwnRaw(rt.atoms.intern("g"), rt.NewFunction("g", 1,
			func(_ *Runtime, _ Value, args []Value) (Value, error) { return args[0], nil }), propDefault)
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, round := range rounds {
		wv, err := want.Run(compileForTest(t, round.src))
		if err != nil {
			t.Fatal(err)
		}
		before, goCalls := r.JITStats(), r.jit.goCalls
		gv, err := r.Run(compileForTest(t, round.src))
		if err != nil {
			t.Fatal(err)
		}
		if !jitSameValueForTest(gv, wv) {
			t.Fatalf("round %d: got %v, interpreter %v", i, gv, wv)
		}
		st := r.JITStats()
		if st.SSAEntries == before.SSAEntries {
			t.Fatalf("round %d never entered the new pipeline: %+v", i, st)
		}
		if hosts := st.Hosts - before.Hosts + r.jit.goCalls - goCalls; round.hosts != 0 && hosts != round.hosts {
			t.Fatalf("round %d: %d exits to Go and calls of Go, want one per call (%d)", i, hosts, round.hosts)
		}
	}
}

// The new pipeline reads a string's length and code units in place, where
// charCodeAt is still the intrinsic: ASCII strings, strings with code units
// past ASCII, a rope, an empty string, and, after a script replaces
// String.prototype.charCodeAt, the replacement, which Go calls.
// A string's methods are read natively where String.prototype has them
// (ssa.OpStringProp), and called by a call of Go: a loop calling replace,
// split or indexOf on a string leaves native code at none of its reads --
// it had at every one, and was no longer entered after 64 -- whether the
// read's cache knew nothing (taken for a string's, jitFeedback's
// StringProperty) or the read learned it at an exit. A method replaced
// meanwhile is the one called; a method String.prototype's shape no
// longer says is where it was, and a read that meets an array, leave for
// Go, which reads it. Every answer is the interpreter's.
func TestJITSSAStringMethods(t *testing.T) {
	for _, tc := range []struct {
		source string
		native bool
	}{
		{`function f(re,s,n){let c=0;for(let i=0;i<n;i++)c+=s.replace(re,'x').length;return c}f(/b+/g,'abbc',1000)`, true},
		{`function f(s,n){let c=0;for(let i=0;i<n;i++)c+=s.split(',').length+s.indexOf('c');return c}f('a,b,c',1000)`, true},
		{`function f(s,n){let c=0;for(let i=0;i<n;i++)c+=s.slice(i%3).length+s.toUpperCase().charCodeAt(0);return c}f('abc',1000)`, false},
		{`function f(s,n){let c=0;for(let i=0;i<n;i++)c+=s.slice(i%3).length+s.toUpperCase().length;return c}f('abc',1000)`, true},
		{`function seven(){return 7}function swap(){String.prototype.indexOf=seven}
			function f(s,n){let c=0;for(let i=0;i<n;i++){if(i===500)swap();c+=s.indexOf('b')}return c}
			let saved=String.prototype.indexOf;let r=f('abc',1000);String.prototype.indexOf=saved;r`, false},
		{`function grow(){String.prototype.extra=1}
			function f(s,n){let c=0;for(let i=0;i<n;i++){if(i===500)grow();c+=s.indexOf('b')}return c}f('abc',1000)`, false},
		{`function f(xs,n){let c=0;for(let i=0;i<n;i++)c+=xs[i%2].indexOf('b');return c}f(['abc',['a','b']],1000)`, false},
		{`function f(s,n){let c=0;for(let i=0;i<n;i++)c+=s.toString().length;return c}f('abc',1000)`, false},
	} {
		want := New(Config{})
		wv, err := want.Run(compileForTest(t, tc.source))
		want.Close()
		if err != nil {
			t.Fatalf("interpreter: %v; %s", err, tc.source)
		}
		r := jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, tc.source))
		if err != nil || v.Number() != wv.Number() {
			t.Fatalf("JIT %v, %v; interpreter %v; %s", v, err, wv, tc.source)
		}
		if r.jit == nil || r.jit.ssaEntries == 0 || tc.native && (r.jit.hosts != 0 || r.jit.guards != 0 || r.jit.interpreted != 0) {
			t.Fatalf("not native: %+v; %s", r.JITStats(), tc.source)
		}
	}
}

func TestJITSSAStrings(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function hash(s){let h=0;for(let i=0;i<s.length;i++)h=(h*31+s.charCodeAt(i))|0;return h}
		var rope='ab';for(let i=0;i<6;i++)rope+=rope+i;hash('warm')`
	rounds := []string{
		`[hash('hello'),hash('αβγ'),hash(''),hash(rope),hash('mixed é')].join()`,
		`String.prototype.charCodeAt=function(i){return 7};[hash('hello'),hash('αβγ')].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitEnabled = false
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	r.jitEnabled = true
	r.jitStress = jitStressConfig{threshold: true, budget: 1}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	st := r.JITStats()
	t.Logf("%+v", st)
	if st.SSAEntries == 0 {
		t.Fatalf("never entered the new pipeline: %+v", st)
	}
}

// TestJITSSACellsUnderGC stresses reading the heap in place: native code carries a
// reference read from an object by its cell's address, which Go reads back
// at exit. That holds while Go's heap does not move and the graph does not
// change while native code runs. Here the collector runs continuously, on
// another goroutine, while lists are built, walked natively and dropped.
func TestJITSSACellsUnderGC(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	src := `function walk(h){let s=0,n=h;while(n){s+=n.v;n=n.next}return s}
		function last(h){let n=h;while(n.next)n=n.next;return n}
		function list(k){let h=null;for(let i=0;i<k;i++)h={v:i,next:h,pad:[i,i]};return h}
		let ok=true;
		for(let round=0;round<300;round++){
			const h=list(50);
			ok=ok&&walk(h)===1225&&last(h).v===0&&walk(list(3))===3;
		}
		ok`
	v, err := r.Run(compileForTest(t, src))
	if err != nil || !v.IsBool() || !v.Truthy() {
		t.Fatalf("= %v, %v", v, err)
	}
	if st := r.JITStats(); st.SSAEntries == 0 || st.SSARecords == 0 {
		t.Fatalf("the walks did not run natively or leave cells to Go: %+v", st)
	}
}

// TestJITObjectLayout holds Object's fields to the widths native code
// loads them with (jitEncoding).
func TestJITObjectLayout(t *testing.T) {
	var o Object
	if unsafe.Sizeof(o.class) != 1 || unsafe.Sizeof(o.flags) != 1 || unsafe.Sizeof(o.arrayLen) != 4 ||
		unsafe.Sizeof(o.elemsOf()) != 3*unsafe.Sizeof(uintptr(0)) || unsafe.Sizeof(Value{}) != 16 {
		t.Fatal("an Object field native code reads changed its width; update jitEncoding and mir")
	}
	if int(ClassArray) > 255 || int(objHasSparseElements) > 255 {
		t.Fatal("a class or flag native code tests is out of a byte")
	}
	// A loop's cursor, which a step reads in place (mir's iterStep): its
	// data an interface, the type word then the pointer; the state's done
	// a byte, its array a pointer, its index an int it adds one to.
	var st iterState
	if unsafe.Sizeof(o.dataOf()) != 2*unsafe.Sizeof(uintptr(0)) || unsafe.Sizeof(st.done) != 1 ||
		unsafe.Sizeof(st.arr) != unsafe.Sizeof(uintptr(0)) || unsafe.Sizeof(st.idx) != 8 {
		t.Fatal("an iteration state field native code reads changed its width; update jitEncoding and mir")
	}
	var x any = &st
	if jitTypeWord(x) != jitEncoding.IterState || (*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1] != unsafe.Pointer(&st) {
		t.Fatal("an interface's words are not the type's and the pointer, as native code reads them")
	}
}

func TestJITEqualityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		left, op, right string
		want            bool
	}{
		{`null`, `==`, `undefined`, true}, {`null`, `===`, `undefined`, false},
		{`dda`, `==`, `null`, true}, {`undefined`, `==`, `dda`, true},
		{`dda`, `===`, `undefined`, false}, {`dda`, `!==`, `null`, true},
		{`{}`, `!=`, `null`, true}, {`false`, `==`, `null`, false},
		{`true`, `===`, `true`, true}, {`true`, `==`, `1`, true},
		{`'3'`, `==`, `3`, true}, {`3n`, `==`, `'3'`, true},
		{`3n`, `===`, `3`, false}, {`Symbol()`, `==`, `3`, false},
		{`NaN`, `===`, `NaN`, false}, {`-0`, `===`, `0`, true},
	} {
		for _, branch := range []bool{false, true} {
			r := jitRuntimeForTest(t, Config{JIT: true})
			dda := r.NewObject()
			dda.MarkHTMLDDA()
			r.global.setOwnRaw(r.atoms.intern("dda"), Obj(dda), propDefault)
			body := `return x` + tc.op + `y`
			if branch {
				body = `if(x` + tc.op + `y)return true;return false`
			}
			fn := compileForTest(t, `function f(a,x,y){a[0]+=1;`+body+`};f([1],`+tc.left+`,`+tc.right+`)`)
			for range 12 {
				v, err := r.Run(fn)
				if err != nil || !v.IsBool() || v.Truthy() != tc.want {
					t.Fatalf("%s %s %s branch %v: %v, %v; want %v", tc.left, tc.op, tc.right, branch, v, err, tc.want)
				}
			}
			if r.jit == nil || r.jit.entries == 0 || r.jit.guards != 0 {
				t.Fatal("equality did not resume native execution or retained roots")
			}
		}
	}
}

func TestJITEqualityBranchBudget(t *testing.T) {
	for _, value := range []string{`null`, `{}`} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, `function f(a,x){let n=0;for(let i=0;i<20000;i++){if(x!=null)n+=a[0]}return n}f([2],`+value+`)`))
		want := float64(40000)
		if value == `null` {
			want = 0
		}
		if err != nil || v.Number() != want || r.jit == nil || r.jit.budgets == 0 || r.jit.guards != 0 {
			t.Fatalf("branch budget %s: %v, %v", value, v, err)
		}
	}
}

func TestJITEqualityCoercionRelease(t *testing.T) {
	for _, branch := range []bool{false, true} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		calls := 0
		r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 0,
			func(rt *Runtime, _ Value, _ []Value) (Value, error) {
				calls++
				if rt.jit == nil {
					t.Fatal("equality coercion released the JIT")
				}
				rt.releaseJIT()
				runtime.GC()
				return Undefined, nil
			}), propDefault)
		body := `let same=o==7;return same?a[0]+a.length:0`
		if branch {
			body = `if(o!=7)return 0;return a[0]+a.length`
		}
		v, err := r.Run(compileForTest(t, `let a=[1];let o={valueOf(){host();a.push(9);a[0]=5;return 7}};function f(a,o){a[0]+=1;`+body+`}f(a,o)`))
		if err != nil || v.Number() != 7 || calls != 1 || r.jit == nil {
			t.Fatalf("equality callback branch %v: %v, %v, calls %d", branch, v, err, calls)
		}
	}
	for _, body := range []string{`return o==7`, `if(o!=7)return 0;return 1`} {
		var baseline string
		for _, enabled := range []bool{false, true} {
			r := jitRuntimeForTest(t, Config{JIT: enabled})
			_, err := r.Run(compileForTest(t, `function f(a,o){a[0]+=1;`+body+`}f([1],{valueOf(){throw new RangeError('compare')}})`))
			if err == nil {
				t.Fatal("equality coercion did not throw")
			}
			if !enabled {
				baseline = err.Error()
			} else if err.Error() != baseline {
				t.Fatalf("equality error %v; want %s", err, baseline)
			}
		}
	}
}

func TestJITBitwiseAndReceiver(t *testing.T) {
	for _, tc := range []struct{ left, op, right, want string }{
		{"4294967297", "|", "2", "3"},
		{"1e20", "|", "0", "1661992960"},
		{"9223372036854777856", "|", "0", "2048"},
		{"-9223372036854777856", "|", "0", "-2048"},
		{"NaN", "^", "Infinity", "0"},
		{"-1", ">>>", "0", "4294967295"},
		{"-2147483648", ">>>", "-1", "1"},
		{"4294967295", "<<", "33", "-2"},
		{"-8", ">>", "4294967297", "-4"},
		{"3.9", "&", "-1.9", "3"},
		{"2147483648", "|", "1", "-2147483647"},
		{"1", "<<", "-1", "-2147483648"},
		{"1", "<<", "NaN", "1"},
	} {
		for _, trees := range []bool{false, true} {
			previous := treeTier.Swap(trees)
			r := jitRuntimeForTest(t, Config{JIT: true})
			source := `let obj={array:[` + tc.left + `],f:function(a){return this.array[0]` + tc.op + `a[0]}};obj.f([` + tc.right + `])`
			v, err := r.Run(compileForTest(t, source))
			treeTier.Store(previous)
			if err != nil {
				t.Fatal(err)
			}
			s, err := r.toString(v)
			if err != nil || s.Go() != tc.want {
				t.Fatalf("%s %s %s: %v, %v; want %s", tc.left, tc.op, tc.right, v, err, tc.want)
			}
			if r.jit == nil || r.jit.entries == 0 || jitOtherGuards(r) != 0 || jitOtherHosts(r) != 0 {
				t.Fatalf("did not exercise receiver/property and native bitwise execution: %+v", r.JITStats())
			}
		}
	}
}

func TestJITReceiverCallbacks(t *testing.T) {
	for _, tc := range []struct {
		source, want string
		guard        bool
	}{
		{`let hits=0,obj={get array(){hits++;return [9]}};function f(a){return this.array[0]+a[0]}let v=f.call(obj,[2]);v*10+hits`, "111", false},
		{`let obj=new Proxy({array:[9]},{get(t,k){return [7]}});function f(a){return this.array[0]+a[0]}f.call(obj,[2])`, "9", false},
		{`let events='';function f(a,b){let s=0;for(let i=0;i<2;i++)s+=a|b;return s}let v=f({valueOf(){events+='a';return 3}},{valueOf(){events+='b';return 4}});v+':'+events`, "14:abab", true},
		{`function f(a,b){let s=a[0]&b[0];return s}f([3n],[2n])`, "2", true},
	} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		v, err := r.Run(compileForTest(t, tc.source))
		if err != nil {
			t.Fatal(err)
		}
		s, err := r.toString(v)
		if err != nil || s.Go() != tc.want {
			t.Fatalf("result %v error %v, want %s", v, err, tc.want)
		}
		if r.jit == nil || r.jit.entries == 0 || (jitOtherGuards(r) > 0) != tc.guard {
			t.Fatal("did not exercise native callbacks or expected guard")
		}
	}
	for _, source := range []string{
		`function f(a){return this.array[0]+a[0]}f.call(null,[1])`,
		`let obj={get array(){throw new RangeError('getter')}};function f(a){return this.array[0]+a[0]}f.call(obj,[1])`,
		`function f(a,b){let s=0;for(let i=0;i<2;i++)s+=a>>>b;return s}f(3n,1n)`,
	} {
		var want string
		for _, enabled := range []bool{false, true} {
			r := jitRuntimeForTest(t, Config{JIT: enabled})
			_, err := r.Run(compileForTest(t, source))
			if err == nil {
				t.Fatal("expected exception")
			}
			if !enabled {
				want = err.Error()
			} else if err.Error() != want || r.jit == nil || r.jit.entries == 0 {
				t.Fatalf("native exception %v, want %s", err, want)
			}
		}
	}
}

func TestJITDataProperties(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   float64
	}{
		{`let o={x:0,f:function(a){let v=this.x=a[0];return v+this.x}};o.f([7])`, 14},
		{`let o={f:function(a){let v=this.x=a[0];return v+this.x}};o.f([7])`, 14},
		{`let o=Object.freeze({x:0});function f(a){let v=this.x=a[0];return v+this.x}f.call(o,[7])`, 7},
		{`let a=[2],o={set x(v){a[0]=v+1;a.push(9)}};function f(a){let v=this.x=a[0];return v+a[0]+a.length}f.call(o,a)`, 7},
		{`let o=new Proxy({x:0},{set(t,k,v){t[k]=v+1;return true}});function f(a){let v=this.x=a[0];return v+this.x}f.call(o,[7])`, 15},
		{`let o={x:[1]};function f(a){this.x=a;return this.x[0]+a[0]}f.call(o,[7])`, 14},
	} {
		for _, trees := range []bool{false, true} {
			previous := treeTier.Swap(trees)
			r := jitRuntimeForTest(t, Config{JIT: true})
			v, err := r.Run(compileForTest(t, tc.source))
			treeTier.Store(previous)
			if err != nil || !v.IsNumber() || v.Number() != tc.want {
				t.Fatalf("data properties: %v, %v; want %v", v, err, tc.want)
			}
			if r.jit == nil || r.jit.entries == 0 {
				t.Fatal("data property test never entered native code or retained roots")
			}
		}
	}
	for _, source := range []string{
		`let o=Object.freeze({x:0});function f(a){'use strict';this.x=a[0];return this.x}f.call(o,[7])`,
		`let o={set x(v){throw new TypeError('setter')}};function f(a){this.x=a[0];return 1}f.call(o,[7])`,
	} {
		var want string
		for _, enabled := range []bool{false, true} {
			r := jitRuntimeForTest(t, Config{JIT: enabled})
			_, err := r.Run(compileForTest(t, source))
			if err == nil {
				t.Fatal("expected property write exception")
			}
			if !enabled {
				want = err.Error()
			} else if err.Error() != want || r.jit == nil || r.jit.entries == 0 {
				t.Fatalf("property write error %v, want %s", err, want)
			}
		}
	}
}

func TestJITDataPropertyRootLimit(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	// Repeated reads reuse a rooted handle rather than filling the root table.
	v, err := r.Run(compileForTest(t, `function f(o,n){let s=0;for(let i=0;i<n;i++){let a=o.array;for(let j=0;j<16;j++)s+=a[0]}return s}f({array:[1]},2000)`))
	if err != nil || v.Number() != 32000 {
		t.Fatalf("root limit: %v, %v", v, err)
	}
	if r.jit == nil || r.jit.hosts != 0 || r.jit.budgets == 0 {
		t.Fatal("property loop did not reuse its bounded root table")
	}
}

func TestJITReferenceFields(t *testing.T) {
	for _, source := range []string{
		`function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.array[0];return s}f({array:[3]},1000)===3000`,
		`function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.m.array[0];return s}f({m:{array:[3]}},1000)===3000`,
		`function f(o,n){let s=0;for(let i=0;i<n;i++){s+=o.array[0];o.array[0]++}return s}let a=[3];f({array:a},1000)===502500&&a[0]===1003`,
		`function f(o,n){for(let i=0;i<n;i++)o.x=(o.x+o.array[0])|0;return o.x+0}let o={array:[3],x:7};f(o,1000)===3007&&o.x===3007`,
		`function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.next.array[0];return s}let o={array:[3]};o.next=o;f(o,1000)===3000`,
		`function f(n){let s=0;for(let i=0;i<n;i++)s+=this.array[0];return s}f.call({array:[3]},1000)===3000`,
		`var scale=7;function f(n){let s=0;for(let i=0;i<n;i++)s+=this.array[0]*scale;return s}f.call({array:[3]},1000)===21000`,
	} {
		for _, enabled := range []bool{false, true} {
			r := jitRuntimeForTest(t, Config{JIT: enabled})
			v, err := r.Run(compileForTest(t, source))
			if err != nil || !v.IsBool() || !v.Truthy() {
				t.Fatalf("JIT %v: %v, %v; %s", enabled, v, err, source)
			}
			if enabled && (r.jit == nil || r.jit.entries == 0 || jitOtherHosts(r) != 0 || jitOtherGuards(r) != 0) {
				t.Fatalf("reference field left native execution: %s", source)
			}
		}
	}
}

func TestJITReferenceFieldFallbacks(t *testing.T) {
	for _, source := range []string{
		`let hits=0;function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.array[0];return s}let o={get array(){hits++;return [3]}};f(o,10)===30&&hits===10`,
		`let hits=0;function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.array[0];return s}let o=new Proxy({array:[3]},{get(t,k){hits++;return t[k]}});f(o,10)===30&&hits===10`,
		`function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.array[0];return s}f(Object.create({array:[3]}),100)===300`,
		`function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.array[0];return s}let o={array:[3]};Object.defineProperty(o,'array',{writable:false});f(o,100)===300`,
		`function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.array[0];return s}let o={array:[3]};delete o.array;let good=false;try{f(o,10)}catch(e){good=e instanceof TypeError}good`,
		`function f(o,n,cb){cb();let s=0;for(let i=0;i<n;i++)s+=o.array[0];return s}let o={array:[3]};f(o,100,()=>{o.array=[7]})===700`,
		`function f(o,n,b){let a=o.array,s=0;for(let i=0;i<n;i++)s+=a[i];o.array=b;for(let i=0;i<n;i++)s+=o.array[0];return s}let o={array:[3,3,3]};f(o,3,[7])===30&&o.array[0]===7`,
		`function f(o,n){let s=0;for(let i=0;i<n;i++){s+=o.array[0];o=o.next}return s}let o={array:[1]},head=o;for(let i=0;i<300;i++){o.next={array:[1]};o=o.next}f(head,300)===300`,
	} {
		for _, enabled := range []bool{false, true} {
			r := jitRuntimeForTest(t, Config{JIT: enabled})
			v, err := r.Run(compileForTest(t, source))
			if err != nil || !v.IsBool() || !v.Truthy() {
				t.Fatalf("fallback JIT %v: %v, %v; %s", enabled, v, err, source)
			}
			if enabled && (r.jit == nil || r.jit.entries == 0) {
				t.Fatalf("missing native fallback: %s", source)
			}
		}
	}
}

func TestJITReferenceCallbackRelease(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	calls := 0
	r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
		calls++
		rt.releaseJIT()
		runtime.GC()
		return Undefined, nil
	}), propDefault)
	v, err := r.Run(compileForTest(t, `function f(o,n,cb){let s=0;for(let i=0;i<n;i++)s+=o.array[0];cb();for(let i=0;i<n;i++)s+=o.array[0];return s}let o={array:[3]};f(o,100,()=>{host();o.array=[7]})===1000`))
	if err != nil || !v.IsBool() || !v.Truthy() || calls != 1 || r.jit == nil {
		t.Fatalf("reference callback: %v, %v, calls %d", v, err, calls)
	}
}

func TestJITReferenceCancellation(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	p := compileForTest(t, `function forever(o){let s=0;for(;;)s+=o.array[0]}forever({array:[1]})`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r.SetContext(ctx)
	_, err := r.Run(p)
	if !errors.Is(err, context.DeadlineExceeded) || r.jit == nil || r.jit.entries == 0 || r.jit.budgets == 0 || r.jit.hosts != 0 {
		t.Fatalf("reference loop cancellation: %v", err)
	}
}

func TestJITReferenceMemoryRefusal(t *testing.T) {
	for _, limit := range []int64{256 << 10, 32 << 20} {
		r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: limit})
		v, err := r.Run(compileForTest(t, `function f(o,n){let s=0;for(let i=0;i<n;i++)s+=o.array[0];return s}f({array:[3]},1000)`))
		if err != nil || v.Number() != 3000 {
			t.Fatalf("reference memory refusal: %v, %v", v, err)
		}
		if limit == 256<<10 {
			if r.jitCodeBytes() != 0 {
				t.Fatal("compiled native code beyond optional allowance")
			}
		} else if r.jit == nil || r.jit.entries == 0 || r.jit.hosts != 0 {
			t.Fatal("reference loop did not enter native code")
		}
	}
}

func TestJITCryptoCorpus(t *testing.T) {
	dir := os.Getenv("QUICKJS_JIT_V8_DIR")
	if dir == "" {
		t.Skip("set QUICKJS_JIT_V8_DIR to the external V8 v7 suite")
	}
	var source strings.Builder
	for _, name := range []string{"base.js", "crypto.js"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		source.Write(data)
		source.WriteByte('\n')
	}
	source.WriteString(`for(let i=0;i<8;i++){encrypt();decrypt()}true`)
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	v, err := r.Run(compileForTest(t, source.String()))
	if err != nil || !v.IsBool() || !v.Truthy() {
		t.Fatalf("Crypto corpus: %v, %v", v, err)
	}
	prop := r.global.getOwn(r.atoms.intern("am3"))
	if !prop.value().IsObject() || prop.value().Object().fn() == nil {
		t.Fatal("missing am3 benchmark function")
	}
	fn := prop.value().Object().fn().closure.fn
	entry := r.jit.cache[weak.Make(fn)]
	if entry == nil || entry.ssa == nil || entry.notNative || r.JITStats().SSAEntries == 0 {
		t.Fatal("am3 did not run natively")
	}
	t.Logf("native entries=%d hosts=%d guards=%d budgets=%d am3 code=%d bytes",
		r.jit.entries, r.jit.hosts, r.jit.guards, r.jit.budgets, entry.ssa.Size())
}

func TestJITDenseArraysAndCallbacks(t *testing.T) {
	var value Value
	if unsafe.Sizeof(value) != 16 || unsafe.Offsetof(value.num) != 0 || unsafe.Offsetof(value.ref) != 8 {
		t.Fatal("VM dense cell ABI changed")
	}
	for _, tc := range []struct {
		name, source string
		want         float64
		leaves       bool
	}{
		{"captured dimensions", `let n=3; function f(a,b){for(let i=0;i<n;i++)a[i]+=2*b[i];return a[0]+a[1]+a[2]} f([1,2,3],[4,5,6])`, 36, false},
		{"alias", `function f(a,b){for(let i=0;i<a.length;i++)a[i]+=b[i];return a[0]+a[1]} let a=[2,3];f(a,a)`, 10, false},
		{"prefix and postfix", `function f(a){let i=0,s=a[i++]+a[++i]+a[i--]+a[--i];return s*10+i}f([2,3,5])`, 140, false},
		{"assignment result", `function f(a){let x=a[1]=7;return x+a[1]}f([2,3])`, 14, false},
		{"callback snapshots", `let n=4;function step(a){n=2;a.push(9);a[0]=8}function f(a,cb){let s=0;for(let i=0;i<n;i++){s+=a[i];cb(a)}return s*10+a.length}f([1,2,3,4],step)`, 36, true},
		{"method receiver", `function f(a){return Math.sqrt(a[0])+a.length}f([9,2])`, 5, false},
		{"negative zero key", `function f(a){return a[-0]}f([7])`, 7, false},
		{"NaN write", `function f(a){a[0]=0/0;return a[0]}f([1])`, math.NaN(), false},
		{"numeric int32", `function f(a){return (a[0]|0)+(a[1]|0)}f([2.9,-1.9])`, 1, false},
		{"int32 wrap", `function f(a){return a[0]|0}f([4294967297])`, 1, false},
		{"hole inherited read", `Array.prototype[0]=7;function f(a){return a[0]}f([,])`, 7, true},
		{"accessor", `function f(a){let i=0;return a[i++]+i*10}let a=[1];Object.defineProperty(a,'0',{get(){return 7}});f(a)`, 17, true},
		{"negative key", `function f(a){return a[-1]}let a=[1];a[-1]=9;f(a)`, 9, true},
		{"fractional key", `function f(a){return a[.5]}let a=[1];a[.5]=8;f(a)`, 8, true},
		{"proxy", `function f(a){return a[0]}f(new Proxy([1],{get(){return 8}}))`, 8, true},
		{"inherited setter", `let hits=0;Object.defineProperty(Array.prototype,'0',{set(v){hits+=v}});function f(a){a[0]=3;return hits}f([,])`, 3, true},
		{"frozen", `function f(a){a[0]=3;return a[0]}f(Object.freeze([7]))`, 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, trees := range []bool{false, true} {
				r := jitRuntimeForTest(t, Config{JIT: true})
				previous := treeTier.Swap(trees)
				t.Cleanup(func() { treeTier.Store(previous) })
				v, err := r.Run(compileForTest(t, tc.source))
				if err != nil || !v.IsNumber() || v.Number() != tc.want && !(math.IsNaN(v.Number()) && math.IsNaN(tc.want)) {
					t.Fatalf("result %v error %v, want %v", v, err, tc.want)
				}
				// What native code leaves to Go: an exit, or a call of Go.
				if r.jit == nil || r.jit.entries == 0 || (r.jit.guards+r.jit.hosts+r.jit.goCalls > 0) != tc.leaves {
					t.Fatalf("native state: entries %d guards %d hosts %d Go calls %d", r.jit.entries, r.jit.guards, r.jit.hosts, r.jit.goCalls)
				}
			}
		})
	}
}

func TestJITHostReleaseAndGC(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	calls := 0
	r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 1,
		func(rt *Runtime, _ Value, args []Value) (Value, error) {
			calls++
			if rt.jit == nil {
				t.Fatal("callback retained native roots")
			}
			rt.releaseJIT()
			runtime.GC()
			return args[0], nil
		}), propDefault)
	v, err := r.Run(compileForTest(t, `function f(a){let s=0;for(let i=0;i<a.length;i++){s+=host(a[i]);a[i]=s}return s}f([1,2,3])`))
	if err != nil || v.Number() != 6 || calls != 3 || r.jit == nil {
		t.Fatalf("result %v err %v calls %d", v, err, calls)
	}
}

func TestJITPropertyReleaseAndGC(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	calls := 0
	r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 0,
		func(rt *Runtime, _ Value, _ []Value) (Value, error) {
			calls++
			if rt.jit == nil {
				t.Fatal("property callback retained native roots")
			}
			rt.releaseJIT()
			runtime.GC()
			return Undefined, nil
		}), propDefault)
	v, err := r.Run(compileForTest(t, `
		function f(a,o){let s=0;for(let i=0;i<3;i++){s+=o.plain+a[0];o.x=s;s+=o.x}return s+a.length}
		let a=[1],o={plain:0,get x(){host();a.push(8);return a[0]},set x(v){host();a[0]=v+1}};
		f(a,o)`))
	if err != nil || !v.IsNumber() || v.Number() != 39 || calls != 6 || r.jit == nil {
		t.Fatalf("result %v err %v property callbacks %d", v, err, calls)
	}
}

func TestJITHostErrors(t *testing.T) {
	for _, source := range []string{
		`function f(a,cb){for(let i=0;i<a.length;i++)a[i]=cb();return 1} f([1],()=>{throw new RangeError('callback')})`,
		`function f(a){return missing+a[0]} f([1])`,
		`function f(a){return Math.noSuchMethod(a[0])} f([1])`,
		`let a=[1]; Object.defineProperty(globalThis,'mathlike',{get(){throw new SyntaxError('getter')}});function f(a){return mathlike+a[0]}f(a)`,
	} {
		var want string
		for _, enabled := range []bool{false, true} {
			r := jitRuntimeForTest(t, Config{JIT: enabled})
			_, err := r.Run(compileForTest(t, source))
			if err == nil {
				t.Fatal("expected error")
			}
			if !enabled {
				want = err.Error()
			} else if err.Error() != want {
				t.Fatalf("native error %q; want %q", err, want)
			}
			if enabled && (r.jit == nil || r.jit.entries == 0) {
				t.Fatal("did not exercise native host exit")
			}
		}
	}
}

func jitRuntimeForTest(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	r := New(cfg)
	// Boundary tests force promotion so guard and budget assertions cannot
	// pass by running only the existing tiers.
	r.jitCallThreshold, r.jitICThreshold = 1, 1
	t.Cleanup(func() {
		// A compile the backend refused for its own panic is a bug, which a
		// refusal hides.
		if r.jit != nil && r.jit.backendPanics != 0 {
			t.Errorf("%d compiles refused for a backend panic", r.jit.backendPanics)
		}
		r.Close()
		r.ReleaseClosed()
	})
	return r
}

// jitOtherGuards is how many guards failed in r's code but the map checks
// of property sites, which deoptimize for a shape the code was not compiled
// for, or at a site that knew none, and feed the sites' caches
// (jitPropertyDeopt): code compiled at its first call, before its sites
// have run, deoptimizes once at each, as V8's code does for insufficient
// feedback. jitOtherHosts is so for host exits, where an inlined callee
// leaves such an operation to Go.
func jitOtherGuards(r *Runtime) uint64 { return r.jit.guards - r.jit.shapeDeopts }
func jitOtherHosts(r *Runtime) uint64  { return r.jit.hosts - r.jit.shapeHosts }

// The new pipeline compares with null and undefined natively, whatever the
// other operand is: strictly by the word, loosely by either word or an
// object's [[IsHTMLDDA]] (Annex B), as values and as branches. It used to
// take both operands for numbers, and an entry speculation followed from
// that: Richards' list walks failed a guard on every call. Each answer must
// be the interpreter's, with no guard failing.
func TestJITSSANullish(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function P(link){this.link=link}
		P.prototype.addTo=function(queue){this.link=null;if(queue==null)return this;var peek,next=queue;while((peek=next.link)!=null)next=peek;next.link=this;return queue};
		function count(x){let n=0;for(let i=0;i<3;i++){
			if(x==null)n+=1;if(x!=undefined)n+=10;if(x===null)n+=100;if(x!==undefined)n+=1000;
			n+=(x==null?1:0)*10000+(undefined===x?1:0)*100000}return n}
		var values=[null,undefined,0,'',false,{},[],'s',NaN,1];
		function walk(n){var q=null;for(var i=0;i<n;i++){q=new P(null).addTo(q);if(i%7==0)q=null}var k=0;while(q!=null){k++;q=q.link}return k}`
	rounds := []string{
		`values.map(count).join()`,
		`[dda,dda,null].map(count).join()`,
		`''+walk(50)`,
		`[count(1),count(null),walk(12)].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		dda := rt.NewObject()
		dda.MarkHTMLDDA()
		rt.global.setOwnRaw(rt.atoms.intern("dda"), Obj(dda), propDefault)
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	if st := r.JITStats(); st.SSAEntries == 0 || jitOtherGuards(r) != 0 {
		var sites []string
		for key, e := range r.jit.cache {
			if fn := key.Value(); fn != nil && e.ssaStats.guards != 0 {
				sites = append(sites, fmt.Sprintf("%s: %v", fn.Name, e.ssaStats.guardsAt))
			}
		}
		t.Fatalf("native comparisons with null failed guards: %+v %v", st, sites)
	}
	// count only compares: it never leaves native code.
	count := r.global.getOwn(r.atoms.intern("count")).value().Object().fn().closure
	if e := r.jit.hint(count); e == nil || e.ssa == nil || e.ssaStats.entries == 0 || e.ssaStats.hosts != 0 {
		t.Fatalf("count did not compare natively: %+v", e)
	}
}

// A function that keeps more values live than there are registers spills
// some, and reuses a spill slot once its value is dead, as it reuses a
// register: NavierStokes' project and lin_solve2 spilled more values over
// their length than the context has slots, though far fewer at once, and
// were refused.
func TestJITSSAManySpills(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var src strings.Builder
	const n = 64
	src.WriteString("function f(m){")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "let a%d=%d;", i, i+1)
	}
	src.WriteString("for(let k=0;k<m;k++){")
	for round := 0; round < 5; round++ {
		for i := 0; i < n; i++ {
			fmt.Fprintf(&src, "a%d=(a%d*3+a%d+k)|0;", i, i, (i+round+1)%n)
		}
	}
	src.WriteString("}let s=0;")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "s=(s^a%d)|0;", i)
	}
	src.WriteString("return s}")
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src.String())); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []string{"''+f(5)", "''+f(200)"} {
		wv, err := want.Run(compileForTest(t, m))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, m))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%s: got %s, interpreter %s", m, got, want)
		}
	}
	fn := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	if e := r.jit.hint(fn); e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
		t.Fatalf("f did not run in the new pipeline: %+v", e)
	}
}

// A function whose native stretches mostly end leaving for Go after little
// work runs in the tree tier once its first stretches show it
// (jitSSAProfit): a loop leaving at every iteration for a call of more
// arguments than Go is called with from native code (abi.MaxGoArgs) costs
// more at the exits than native code saves. A loop doing real work stays
// native, and so does one doing real work around calls Go makes from
// native code (ssa.GoCall), each of which costs what an exit would.
func TestJITSSAProfitability(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// m reads arguments: Go calls it, never native code (jitNativeCallee).
	setup := `function O(){this.v=0}O.prototype.m=function(i){this.v+=arguments[0];return this.v};
		function calls(o,n){let s=0;for(let i=0;i<n;i++)s+=o.m(i,1,2,3,4,5,6);return s}
		function gocalls(o,n){let s=0;for(let i=0;i<n;i++){s=(s*31+i)|0;s^=s>>>7;s=(s+i*i)|0;s^=s<<3;s=(s*7+(i>>1))|0;s^=s>>>5;s=(s+o.m(i))|0}return s}
		function work(n){let s=0;for(let i=0;i<n;i++){s=(s*31+i)|0;s^=s>>>7;s=(s+i*i)|0;if(i%100==0)s+=Math.abs(i)}return s}
		var o=new O;`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		src := `[calls(o,500),work(2000),gocalls(o,500)].join()`
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.hint(cl)
	}
	if e := entry("calls"); e == nil || e.ssa == nil || !e.entrySlow || e.ssaStats.entries > 2*jitSSAProbe {
		t.Fatalf("calls kept running natively: %+v", e)
	}
	if e := entry("work"); e == nil || e.ssa == nil || e.entrySlow {
		t.Fatalf("work left native code: %+v", e)
	}
	if e := entry("gocalls"); e == nil || e.ssa == nil || e.entrySlow || e.ssaStats.goCalls == 0 || e.ssaStats.hosts != 0 {
		t.Fatalf("gocalls did not stay native, its calls Go's: %+v", e)
	}
}

// Calls of Go count toward judging code as stretches do (jitSSAProfit):
// builtins, entered three times, each a loop of 40,000 calls of
// String.prototype.codePointAt and little else, was judged every jitSSAProbe
// entries, so never, and ran natively, slower than the tree tier, as
// RegExp's run blocks did; now it is judged after its first calls and
// the tree tier runs it. mixed, as many calls with real work around each,
// stays native. And a native callee is judged by its own calls of Go
// (jitCalleeProfit): heavy, called natively by outer, ten calls of Go and
// nothing else each time, had its native callers call it natively
// whatever it cost; now Go runs it, in the tree tier, and outer's code
// calls it through Go (notNative). light, called so too, does real work
// around its calls and is called natively still. Each answer is the
// interpreter's.
func TestJITSSAProfitabilityOfCallsOfGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var str='abcdefgh';
		function builtins(n){let s=0;for(let i=0;i<n;i++)s=(s+str.codePointAt('q')+str.codePointAt('c'))|0;return s}
		function mixed(n){let s=0;for(let i=0;i<n;i++){s=(s*31+i)|0;s^=s>>>7;s=(s+i*i)|0;s^=s<<3;s=(s*7+(i>>1))|0;s^=s>>>5;s=(s+str.codePointAt('c'))|0}return s}
		function heavy(k){return str.codePointAt('q')+str.codePointAt('c')+str.codePointAt('d')+str.codePointAt('e')+str.codePointAt('f')+
			str.codePointAt('g')+str.codePointAt('h')+str.codePointAt('a')+str.codePointAt('b')+str.codePointAt('x')+k}
		function light(k){let x=k;for(let j=0;j<20;j++){x=(x*31+j)|0;x^=x>>>7;x=(x+j*j)|0;x^=x<<3}return (x+str.codePointAt('c'))|0}
		function outer(n){let s=0;for(let i=0;i<n;i++)s=(s+heavy(i)+light(i))|0;return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		src := `[builtins(20000),mixed(20000),outer(3000)].join()`
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	if e := entry("builtins"); e == nil || e.ssa == nil || !e.entrySlow || e.ssaStats.entries >= jitSSAProbe {
		t.Fatalf("builtins kept running natively: %+v", e)
	}
	if e := entry("mixed"); e == nil || e.ssa == nil || e.entrySlow || e.ssaStats.goCalls == 0 {
		t.Fatalf("mixed did not stay native: %+v", e)
	}
	if e := entry("heavy"); e == nil || e.ssa == nil || e.ssaStats.calleeGoCalls == 0 || !e.notNative || !e.entrySlow {
		t.Fatalf("heavy kept being called natively: %+v", e)
	}
	if e := entry("light"); e == nil || e.ssa == nil || e.ssaStats.calleeGoCalls == 0 || e.notNative {
		t.Fatalf("light was not called natively: %+v", e)
	}
}

// The new pipeline compares any two values natively where their words tell
// (ssa.OpEqTagged): objects by identity, as EarleyBoyer's association lists
// do with ===, numbers as numbers, primitives by their words. Strings, and
// loose comparisons of values of different kinds, go to Go. Each answer is
// the interpreter's, and the list walk never leaves native code.
func TestJITSSAEquality(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function assq(o,al){while(al!==null){if(al.car.car===o)return al.car;al=al.cdr}return false}
		function cons(a,d){return {car:a,cdr:d}}
		var k1={},k2={},k3={},al=cons(cons(k1,1),cons(cons(k2,2),cons(cons(k3,3),null)));
		function walk(n){let s=0;for(let i=0;i<n;i++){const p=assq([k1,k2,k3][i%3],al);s+=p.cdr}return s}
		function mixed(a,b,n){let s=0;for(let i=0;i<n;i++){if(a==b)s+=1;if(a===b)s+=10;if(a!=b)s+=100;if(a!==b)s+=1000}return s}`
	rounds := []string{
		`''+walk(60)`,
		`[mixed(1,1,3),mixed(0,-0,3),mixed(NaN,NaN,3),mixed(k1,k1,3),mixed(k1,k2,3),mixed('a','a',3),mixed('a','b',3),mixed('a','bb',3),mixed('a'+String(1),'a1',3),
			mixed(1,'1',3),mixed(true,1,3),mixed(null,undefined,3),mixed(true,false,3),mixed(k1,1,3)].join()`,
		`''+(assq(k2,al)===al.cdr.car)+assq({},al)`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	assq := r.global.getOwn(r.atoms.intern("assq")).value().Object().fn().closure
	if e := r.jit.hint(assq); e == nil || e.ssa == nil || e.ssaStats.entries == 0 || e.ssaStats.hosts != 0 || e.ssaStats.guards != 0 {
		t.Fatalf("assq left native code: %+v", e)
	}
}

// A property or element compared with == or != is read as it is, whatever
// it holds: the new pipeline compares any value, so lowering does not take
// the operands for numbers, which sent `o.next != null` to Go at every
// iteration where next holds an object.
func TestJITSSACompareReferences(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function f(o,a,n){let k=0;for(let i=0;i<n;i++){if(o.next!=null)k++;if(a[i%2]==null)k+=10;if(o.v==3)k+=100}return k}
		var o={next:{},v:3},a=[{},null];`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for _, src := range []string{`''+f(o,a,40)`, `o.next=null;o.v=4;''+f(o,a,40)`, `o.next=7;o.v='3';''+f(o,a,40)`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%s: got %s, interpreter %s", src, got, want)
		}
		if src == `''+f(o,a,40)` {
			fn := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
			if e := r.jit.hint(fn); e == nil || e.ssa == nil || e.ssaStats.hosts != 0 || e.ssaStats.guards != 0 {
				t.Fatalf("comparisons of references left native code: %+v", e)
			}
		}
	}
}

// The new pipeline reads an element it carries as a reference by the
// element's cell, whatever it holds -- objects read from an array and used
// as receivers, as Richards' scheduler does, or returned -- where it took
// every element for a number and failed a guard at each object. A hole,
// an index past the end and an object that is not an array go to Go.
func TestJITSSAElementReferences(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function T(p){this.p=p}
		function sum(ts,n){let s=0;for(let i=0;i<n;i++){const t=ts[i%3];s+=t.p}return s}
		function pick(a,i){let r=null;for(let k=0;k<3;k++)r=a[i];return r}
		var dense=[new T(1),new T(2),new T(3)],holey=[new T(1),,new T(3)],notArray={0:new T(5),1:new T(6),2:new T(7)};`
	rounds := []string{
		`''+sum(dense,30)`,
		`[pick(dense,1)===dense[1],pick(holey,1),pick(dense,7),pick(notArray,0).p,pick([1,'s',null],1)].join()`,
		`''+sum(holey,3)`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, werr := want.Run(compileForTest(t, src))
		gv, gerr := r.Run(compileForTest(t, src))
		if (werr == nil) != (gerr == nil) || werr == nil && gv.String().Go() != wv.String().Go() {
			t.Fatalf("round %d: got %v, %v; interpreter %v, %v", i, gv, gerr, wv, werr)
		}
	}
	sum := r.global.getOwn(r.atoms.intern("sum")).value().Object().fn().closure
	e := r.jit.hint(sum)
	if e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
		t.Fatalf("sum: %+v", e)
	}
	for site := range e.ssaStats.guardsAt {
		if op := sum.fn.Code[site].Op; !jitPropertyOp(op) {
			t.Fatalf("sum failed a guard at %d, %v: %v", site, op, e.ssaStats.guardsAt)
		}
	}
	if st := r.JITStats(); jitOtherGuards(r) != 0 {
		t.Fatalf("element reads failed guards: %+v, %d at property sites", st, r.jit.shapeDeopts)
	}
}

// A runtime's compiled functions share a few mappings, its arena's chunks,
// rather than taking one each; closing the runtime releases every one.
func TestJITArenaMappings(t *testing.T) {
	for _, ssa := range []bool{true} {
		r := New(Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		var src strings.Builder
		for i := 0; i < 100; i++ {
			fmt.Fprintf(&src, "function f%d(n){let s=%d;for(let i=0;i<n;i++)s+=i;return s}\nf%d(3);\n", i, i, i)
		}
		if _, err := r.Run(compileForTest(t, src.String())); err != nil {
			t.Fatal(err)
		}
		if r.jit == nil || len(r.jit.cache) < 100 {
			t.Fatalf("ssa %v: compiled too few functions", ssa)
		}
		arena, bytes := r.jit.arena, 0
		for _, e := range r.jit.cache {
			bytes += e.ssa.Size()
		}
		if chunks := arena.Chunks(); chunks == 0 || chunks > bytes/(64<<10)+1 {
			t.Fatalf("ssa %v: %d functions, %d bytes of code, in %d mappings", ssa, len(r.jit.cache), bytes, chunks)
		} else {
			t.Logf("ssa %v: %d functions, %d bytes of code, in %d mappings", ssa, len(r.jit.cache), bytes, chunks)
		}
		r.Close()
		r.ReleaseClosed()
		if arena.Chunks() != 0 {
			t.Fatalf("ssa %v: a closed runtime keeps %d mappings", ssa, arena.Chunks())
		}
	}
}

func TestJITRuntimeOptIn(t *testing.T) {
	p := compileForTest(t, jitSumSource)
	for _, enabled := range []bool{false, true} {
		r := jitRuntimeForTest(t, Config{JIT: enabled})
		if r.jit != nil {
			t.Fatal("runtime creation allocated JIT state")
		}
		v, err := r.Run(p)
		if err != nil || v.Number() != 49995000 {
			t.Fatalf("sum = %v, %v", v, err)
		}
		if enabled {
			if r.jit == nil || r.jit.entries == 0 || r.jit.budgets == 0 || r.jitCodeBytes() == 0 {
				t.Fatal("opt-in did not execute bounded native code")
			}
			r.Close()
			r.ReleaseClosed()
			if r.jit != nil {
				t.Fatal("close retained native code")
			}
		} else if r.jit != nil {
			t.Fatal("opt-out allocated JIT state")
		}
	}
}

func TestJITCachedTreeCall(t *testing.T) {
	r := jitRuntimeForTest(t, Config{})
	if _, err := r.Run(compileForTest(t, `function sum(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s } sum(10000); sum(10); sum(20)`)); err != nil {
		t.Fatal(err)
	}
	o := r.global.getOwn(r.atoms.intern("sum"))
	if o.value().Object().fn().treeCall == nil {
		t.Fatal("test did not establish cached tree call")
	}
	// Enable on the same runtime after repeated calls have cached their tree.
	r.initJIT(true, 0)
	v, err := r.Run(compileForTest(t, `for (var j=0;j<8;j++) sum(10000); sum(10000)`))
	if err != nil || v.Number() != 49995000 {
		t.Fatalf("cached call = %v, %v", v, err)
	}
	if r.jit == nil || r.jit.entries == 0 {
		t.Fatal("cached tree bypassed native selection")
	}
}

func TestJITNestedTreeOSR(t *testing.T) {
	for _, tail := range []bool{false, true} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 100, 100
		body := `let value=sum(10000); return value+7`
		want := float64(49995007)
		if tail {
			body = `return sum(10000)`
			want = 49995000
		}
		source := `function sum(n){var s=0;for(var i=0;i<n;i++)s+=i;return s}
			sum(2);sum(2);function outer(){` + body + `}outer()`
		v, err := r.Run(compileForTest(t, source))
		if err != nil || v.Number() != want {
			t.Fatalf("tail=%v: result %v error %v, want %v", tail, v, err, want)
		}
		if r.jit == nil || r.jit.osrs == 0 {
			t.Fatal("nested call never promoted its loop")
		}
		if r.frameDepth != 0 || r.stackTop != 0 {
			t.Fatalf("nested promotion leaked frames: depth %d stack %d", r.frameDepth, r.stackTop)
		}
		v, err = r.Run(compileForTest(t, `sum(3)+11`))
		if err != nil || v.Number() != 14 {
			t.Fatalf("subsequent call: %v, %v", v, err)
		}
	}
}

func TestJITDebuggerFallback(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true, Debug: true})
	// Hidden host scripts need not contain debugger instructions. The runtime
	// still has debugger callbacks at interrupt checks, so it cannot borrow views.
	v, err := r.Run(compileForTest(t, jitSumSource))
	if err != nil || v.Number() != 49995000 {
		t.Fatalf("debugger fallback: %v, %v", v, err)
	}
	if r.jitEnabled || r.jit != nil {
		t.Fatal("debugger runtime allocated native state")
	}
}

func TestJITNestedTreeOSRThrow(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitCallThreshold, r.jitICThreshold = 100, 100
	v, err := r.Run(compileForTest(t, `
		function sum(n,a){var s=0;for(var i=0;i<n;i++)s+=i;return s+a}
		sum(2,0);sum(2,0);
		function outer(){return sum(10000,{valueOf(){throw new RangeError('nested')}})+7}
		let result='';try{outer()}catch(e){result=e.name+':'+e.message}result`))
	if err != nil || !v.IsString() || v.String().Go() != "RangeError:nested" {
		t.Fatalf("nested exception: %v, %v", v, err)
	}
	if r.jit == nil || r.jit.osrs == 0 || r.jit.guards == 0 {
		t.Fatal("nested call did not promote and guard to coercion")
	}
	if r.frameDepth != 0 || r.stackTop != 0 {
		t.Fatalf("exception leaked frames: depth %d stack %d", r.frameDepth, r.stackTop)
	}
	v, err = r.Run(compileForTest(t, `sum(3,11)`))
	if err != nil || v.Number() != 14 {
		t.Fatalf("call after exception: %v, %v", v, err)
	}
}

func TestJITRuntimeGuardReentry(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	v, err := r.Run(compileForTest(t, `
		function sum(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }
		function f(a) { let s=0; for(let i=0;i<3;i++) { s++; s+=a } return s }
		let count=0;
		let result=f({valueOf() { count++; return sum(3) }});
		result*10+count`))
	if err != nil || v.Number() != 123 {
		t.Fatalf("guard reentry = %v, %v", v, err)
	}
	if r.jit == nil || r.jit.guards != 1 || r.jit.entries < 4 {
		t.Fatal("guard or nested native call did not execute, or roots survived")
	}
}

func TestJITRuntimeCancellation(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	p := compileForTest(t, `function forever() { let i=0; for(;;) i++ } forever()`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r.SetContext(ctx)
	_, err := r.Run(p)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel = %v", err)
	}
	if r.jit == nil || r.jit.entries == 0 || r.jit.budgets == 0 {
		t.Fatal("loop never entered native code")
	}
}

func TestJITPropertyLoopCancellation(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	p := compileForTest(t, `function forever(o){for(;;){o.x=(o.x+1)|0}}forever({x:0})`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r.SetContext(ctx)
	_, err := r.Run(p)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("property loop cancellation: %v", err)
	}
	if r.jit == nil || r.jit.entries == 0 || r.jit.hosts != 0 || r.jit.budgets == 0 {
		t.Fatal("native property loop did not check its budget")
	}
}

func TestJITRuntimeCloseAfterGuard(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	called := false
	r.global.setOwnRaw(r.atoms.intern("closeNow"), r.NewFunction("closeNow", 0,
		func(rt *Runtime, _ Value, _ []Value) (Value, error) {
			called = true
			rt.Halt(context.Canceled)
			rt.Close()
			rt.ReleaseClosed()
			if rt.jitCodeBytes() == 0 {
				t.Fatal("released code while frame active")
			}
			return Int32(2), nil
		}), propDefault)
	_, err := r.Run(compileForTest(t, `function f(a) { let s=0; for(let i=0;i<10000;i++) s+=a; return s }
		f({valueOf() { return closeNow() }})`))
	if !called || !errors.Is(err, context.Canceled) {
		t.Fatalf("close = %v, called %v", err, called)
	}
	r.ReleaseClosed()
	if r.jit != nil {
		t.Fatal("native owners survived unwind")
	}
}

// Halt stops a script at its next call or backward jump: the straight-line
// code after the host call that halted it still runs, and nothing after. A
// native loop must stop where the interpreter does, whether the host exit is
// an ordinary call (the call coordinator) or an accessor (the plain loop).
func TestJITHaltAfterHostExit(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"call", `var arr = [0,0,0,0,0,0,0,0,0,0];
			function f(a) { for (let i = 0; i < a.length; i++) { a[i] = 1; stop(i) } return 0 }
			f(arr)`},
		// Warmed until the getter's read, an object's own accessor, is
		// megamorphic, Go making it at a host exit: deoptimized at it, the
		// code is invalidated, and the interpreter would halt.
		{"accessor", `var arr = [0,0,0,0,0,0,0,0,0,0], n = -6;
			function mk() { return { get x() { stop(n++); return 1 } } }
			function f(o, a) { let s = 0; for (let i = 0; i < a.length; i++) { a[i] = 1; s += o.x } return s }
			for (let k = 0; k < 6; k++) f(mk(), [0]);
			f(mk(), arr)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(jit bool) (string, *Runtime) {
				r := jitRuntimeForTest(t, Config{JIT: jit})
				r.global.setOwnRaw(r.atoms.intern("stop"), r.NewFunction("stop", 1,
					func(rt *Runtime, _ Value, args []Value) (Value, error) {
						if len(args) > 0 && args[0].Number() == 3 {
							rt.Halt(context.Canceled)
						}
						return Undefined, nil
					}), propDefault)
				if _, err := r.Run(compileForTest(t, tc.source)); !errors.Is(err, context.Canceled) {
					t.Fatalf("jit=%v: run = %v, want context.Canceled", jit, err)
				}
				r.ClearStop()
				v, err := r.Run(compileForTest(t, `arr.join("")`))
				if err != nil {
					t.Fatal(err)
				}
				return v.String().Go(), r
			}
			want, _ := run(false)
			got, r := run(true)
			if r.jit == nil || r.jit.entries == 0 || r.jit.hosts == 0 {
				t.Fatal("loop never reached a native host exit")
			}
			if got != want || want != "1111000000" {
				t.Fatalf("after Halt: native %q, interpreter %q, want 1111000000", got, want)
			}
		})
	}
}

func TestJITMemoryRefusal(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 256 << 10})
	v, err := r.Run(compileForTest(t, jitSumSource))
	if err != nil || v.Number() != 49995000 {
		t.Fatalf("optimization refusal stopped script: %v, %v", v, err)
	}
	if r.jitCodeBytes() != 0 {
		t.Fatal("compiled beyond optional memory allowance")
	}
	_, err = r.Run(compileForTest(t, `new ArrayBuffer(1024*1024)`))
	if !errors.Is(err, ErrMemoryLimit) {
		t.Fatalf("script exhaustion = %v", err)
	}

	large := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 32 << 20})
	if _, err := large.Run(compileForTest(t, jitSumSource)); err != nil {
		t.Fatal(err)
	}
	if large.jitCodeBytes() == 0 {
		t.Fatal("ample memory prevented native compilation")
	}
	// Native code is not the script's memory: the script's measure, and so
	// whether it fails with ErrMemoryLimit, is the same with the JIT on and off.
	with := large.meter.walk(large)
	large.releaseJIT()
	if without := large.meter.walk(large); with != without {
		t.Fatalf("meter counted native code: %d bytes with it, %d without", with, without)
	}
	if int64(large.jitBudget()) != large.meter.limit/8 {
		t.Fatalf("JIT budget %d under a %d-byte limit", large.jitBudget(), large.meter.limit)
	}
}

// Compiling must not measure the heap: before, every attempt walked it, and
// a refusal retried every warmup, so a memory-limited runtime walked its whole
// heap every few calls of each hot function. The meter walks once a megabyte
// has been allocated in the process, by any goroutine -- another test's,
// still running, had it walk in one of CI's runs -- so each call starts its
// count afresh: only what the call does may make it walk.
func TestJITNoHeapWalks(t *testing.T) {
	walks := func(jit bool, limit int64) uint16 {
		r := jitRuntimeForTest(t, Config{JIT: jit, MemoryLimit: limit})
		v, err := r.Run(compileForTest(t, `function sum(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s } sum`))
		if err != nil {
			t.Fatal(err)
		}
		before := r.meter.epoch
		for i := 0; i < 200; i++ {
			r.meter.allocsAt = r.meter.allocated()
			if got, err := r.Call(v, Undefined, []Value{Int32(10)}); err != nil || got.Number() != 45 {
				t.Fatalf("sum = %v, %v", got, err)
			}
		}
		if jit && limit > 1<<20 && (r.jit == nil || r.jit.entries == 0) {
			t.Fatal("the function never ran natively")
		}
		return r.meter.epoch - before
	}
	// A small limit refuses every attempt; a large one compiles.
	for _, limit := range []int64{256 << 10, 32 << 20} {
		if off, on := walks(false, limit), walks(true, limit); on != off {
			t.Fatalf("limit %d: %d heap walks with the JIT, %d without", limit, on, off)
		}
	}
}

// A program refused for want of code budget is not recompiled at every
// warmup: the refusal is cached until the cache has released code. The
// refusal holds no more than itself -- not the program lowered for it,
// which had left no room to remember it in -- and leaves the JIT within its
// budget. The budget is set from what the JIT holds, room for a refusal
// short of a compile's, so that how large a target's code is decides
// nothing: one where code was larger had no refusal at all.
func TestJITBudgetRefusalWaitsForRelease(t *testing.T) {
	// The code compiled stays young: none is dropped for another's.
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return 0 }
	// The functions are kept: a collection would drop the code of those
	// already compiled (the cache's keys are weak).
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	copyOf := jitCopiesForTest(jitFunctionForTest(t, `function f(n) { let s=0,t=1; for(let i=0;i<n;i++){ s+=i*3; t=(t*3+s)|0; s=(s^t)+1; t=t-s; s=(s+t*7)|0; t=(t^s)>>1; } return s+t }`), &keep)
	r := jitRuntimeForTest(t, Config{JIT: true})
	s := r.jitState()
	held := s.held
	// Enough code for a budget a compile of f fits (jitCompiles), and what
	// each compile of it adds.
	refused := copyOf()
	grows := 0
	for held() < len(refused.Code)*ir.MaxSlots*8+32<<10 {
		before := held()
		if e := r.jitForFunction(copyOf(), nil); e == nil || e.ssa == nil {
			t.Fatal("native compilation refused")
		}
		grows = held() - before
	}
	if grows/2 < jitEntryFixed+jitLinkFixed+1<<10 {
		t.Fatalf("a compile adds %d bytes: no room for a refusal short of it", grows)
	}
	r.jitMemoryLimit = int64(held() + grows/2)
	// The room a compile is likely to need short of its code's, as for a
	// function unlike those compiled before: the compile is made, and
	// refused room for its code.
	s.perInstr = 0
	deferred := r.jitForFunction(refused, nil)
	if deferred == nil || !deferred.deferred || deferred.ssa != nil {
		t.Fatalf("no refusal for want of budget within %d bytes: %+v", r.jitBudget(), deferred)
	}
	if s.held() > r.jitBudget() {
		t.Fatalf("%d bytes held after a refusal, of %d", s.held(), r.jitBudget())
	}
	compiled := s.compiled
	s.perInstr = 0
	if e := r.jitForFunction(refused, nil); e != deferred || s.compiled != compiled {
		t.Fatal("a budget refusal was retried before any code was released")
	}
	for held()+grows > r.jitBudget() {
		var key weak.Pointer[bytecode.Function]
		var victim *jitEntry
		for k, e := range s.cache {
			if e.ssa != nil {
				key, victim = k, e
				break
			}
		}
		if victim == nil || !s.dropEntry(key, victim) {
			t.Fatal("could not release code")
		}
	}
	s.perInstr = 0
	if e := r.jitForFunction(refused, nil); e == nil || e == deferred || e.ssa == nil {
		t.Fatal("a budget refusal was not retried once code was released")
	}
}

//go:noinline
func jitWeakOwnerForTest(t *testing.T, r *Runtime) weak.Pointer[bytecode.Function] {
	fn := jitFunctionForTest(t, `function f(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }`)
	if e := r.jitForFunction(fn, nil); e == nil || e.ssa == nil {
		t.Fatal("native compilation refused")
	}
	return weak.Make(fn)
}

func TestJITWeakCache(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	p := jitWeakOwnerForTest(t, r)
	deadline := time.Now().Add(3 * time.Second)
	for p.Value() != nil && time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
	}
	if p.Value() != nil {
		t.Fatal("native cache retained function graph")
	}
	fn := jitFunctionForTest(t, `function g(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }`)
	if e := r.jitForFunction(fn, nil); e == nil || e.ssa == nil {
		t.Fatal("compilation refused")
	}
	if len(r.jit.cache) != 1 {
		t.Fatal("dead native owner was not swept")
	}
	runtime.KeepAlive(fn)
}

// jitCopiesForTest makes copies of template, each a function of its own to
// the cache, kept alive in keep: a collection would drop the code of those
// already compiled (the cache's keys are weak).
func jitCopiesForTest(template *bytecode.Function, keep *[]*bytecode.Function) func() *bytecode.Function {
	return func() *bytecode.Function {
		fn := new(bytecode.Function)
		*fn = *template
		*keep = append(*keep, fn)
		return fn
	}
}

// The cache counts no entries: it holds as many functions' code as the
// JIT's budget has room for -- 400 here, where it had held 128 -- and,
// with a memory limit, never more than its eighth of it, the least
// recently used old code dropped to make room.
func TestJITCacheBounds(t *testing.T) {
	epoch := uint64(0)
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return epoch }
	jitNoFlushForTest(t)
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	copyOf := jitCopiesForTest(jitFunctionForTest(t, `function f(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s }`), &keep)
	r := jitRuntimeForTest(t, Config{JIT: true})
	for i := 0; i < 400; i++ {
		if e := r.jitForFunction(copyOf(), nil); e == nil || e.ssa == nil {
			t.Fatalf("entry %d refused", i)
		}
	}
	if len(r.jit.cache) != 400 || r.jit.evicted != 0 || r.jitHeldBytes() > int64(r.jitBudget()) {
		t.Fatalf("%d entries, %d evicted, %d bytes held", len(r.jit.cache), r.jit.evicted, r.jitHeldBytes())
	}
	limited := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 8 << 20})
	budget := int64(limited.jitBudget())
	var last []*jitEntry
	for i := 0; i < 600; i++ {
		e := limited.jitForFunction(copyOf(), nil)
		if e == nil || e.ssa == nil {
			t.Fatalf("entry %d refused", i)
		}
		if held := limited.jitHeldBytes(); held > budget {
			t.Fatalf("entry %d: %d bytes held, past the budget's %d", i, held, budget)
		}
		last = append(last, e)
		// Each function's code old by the next's.
		epoch += jitCodeAge
	}
	if limited.jit.evicted == 0 {
		t.Fatal("test did not reach eviction")
	}
	// The newest stayed, the oldest went.
	if n := len(limited.jit.cache); limited.jit.cache[weak.Make(keep[len(keep)-1])] != last[len(last)-1] ||
		limited.jit.cache[weak.Make(keep[len(keep)-600])] != nil || n < 16 {
		t.Fatalf("cache of %d entries kept the wrong ones", n)
	}
}

// The cache's epochs follow the collector's cycles (jitGCEpoch), as V8 ages
// code by them, once a runtime has run the JIT, and pass every
// jitEpochTime besides, for a program that allocates little.
func TestJITEpochFollowsCollector(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function f(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}f(100)`)); err != nil {
		t.Fatal(err)
	}
	if r.jit == nil {
		t.Fatal("the JIT did not run")
	}
	before := jitGCEpoch.Load()
	for i := 0; i < 100 && jitGCEpoch.Load() < before+2; i++ {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if jitGCEpoch.Load() < before+2 {
		t.Fatalf("epoch %d after collections, from %d", jitGCEpoch.Load(), before)
	}
	e, gc := r.jit.epoch(), jitGCEpoch.Load()
	time.Sleep(jitEpochTime + 10*time.Millisecond)
	if r.jit.epoch()-(jitGCEpoch.Load()-gc) < e+1 {
		t.Fatal("time does not make an epoch pass")
	}
}

// jitChurnSource is a program with more functions run hot enough to be
// compiled than the JIT's cache holds: a hot core of chains native calls
// make -- top_c's loop calls mid_c, which calls leaf_c twice, each kept
// from being inlined by a captured binding -- and a stream of cold
// functions, each run a few times, then left. round(phases, window) runs
// every chain in each phase, then the next window of cold functions.
func jitChurnSource(chains, cold int) string {
	var b strings.Builder
	for c := 0; c < chains; c++ {
		fmt.Fprintf(&b, "var leaf_%d=(function(){let k=%d;return function(x){return (x*3+k)|0}})();\n", c, c)
		fmt.Fprintf(&b, "var mid_%d=(function(){let k=%d;return function(x){return (leaf_%d(x)+leaf_%d(x+k))|0}})();\n", c, c, c, c)
		fmt.Fprintf(&b, "function top_%d(n){let t=0;for(let i=0;i<n;i++)t=(t+mid_%d(i))|0;return t}\n", c, c)
	}
	for j := 0; j < cold; j++ {
		fmt.Fprintf(&b, "function cold_%d(n){let t=%d;for(let i=0;i<n;i++)t=(t*31+i+%d)|0;return t}\n", j, j, j)
	}
	b.WriteString("var tops=[")
	for c := 0; c < chains; c++ {
		fmt.Fprintf(&b, "top_%d,", c)
	}
	b.WriteString("],colds=[")
	for j := 0; j < cold; j++ {
		fmt.Fprintf(&b, "cold_%d,", j)
	}
	b.WriteString(`],next=0;
function phase(window){let t=0;for(const f of tops)t=(t+f(200))|0;for(let w=0;w<window;w++){const f=colds[next];next=(next+1)%colds.length;for(let k=0;k<10;k++)t=(t+f(20))|0}return t}
function round(phases,window){let t=0;for(let p=0;p<phases;p++)t=(t+phase(window))|0;return t}
function half(h,phases){const n=tops.length>>1;let t=0;for(let p=0;p<phases;p++)for(let c=h*n;c<(h+1)*n;c++)t=(t+tops[c](200))|0;return t}
`)
	return b.String()
}

// A hot core of native call chains stays native while a stream of cold
// functions, more than the cache holds, comes and goes (makeRoom, jitLink):
// each chain's functions are cached with code at the end of every round,
// and no call of them leaves for Go (abi.ExitEnter). The cache had dropped
// a random entry, hot or not, and native callers had called dropped ones
// through Go at every call.
func TestJITCacheKeepsHotCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	epoch := uint64(0)
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return epoch }
	const chains = 30
	// A budget of 1 MB, which the cold functions fill.
	r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 8 << 20})
	if _, err := r.Run(compileForTest(t, jitChurnSource(chains, 400))); err != nil {
		t.Fatal(err)
	}
	call := compileForTest(t, `round(10,20)`)
	for round := 0; round < 6; round++ {
		entered := uint64(0)
		if r.jit != nil {
			entered = r.jit.entered
		}
		if _, err := r.Run(call); err != nil {
			t.Fatal(err)
		}
		epoch++
		if round < 2 {
			continue
		}
		if n := r.jit.entered - entered; n != 0 {
			t.Fatalf("round %d: %d native calls left for Go", round, n)
		}
		for c := 0; c < chains; c++ {
			for _, name := range []string{"top_%d", "mid_%d", "leaf_%d"} {
				name = fmt.Sprintf(name, c)
				fn := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
				if e := r.jit.cache[weak.Make(fn)]; e == nil || e.ssa == nil {
					t.Fatalf("round %d: %s was dropped: %+v", round, name, e)
				}
			}
		}
	}
	if r.jit.evicted == 0 && r.jit.refusedRoom == 0 {
		t.Fatal("the cold functions never filled the cache")
	}
}

// BenchmarkJITCacheChurn runs a hot core of native call chains among a
// stream of cold functions, more than the JIT's cache holds
// (jitChurnSource): the cost of keeping the hot code native, and of the
// cold code's compiles.
func BenchmarkJITCacheChurn(b *testing.B) {
	benchJITCache(b, jitChurnSource(30, 600), `round(30,20)`)
}

// BenchmarkJITCacheHotSet runs programs with more hot functions than the
// JIT's cache had held (128): 180 and 600 hot functions in native call
// chains, every one run each round, and two halves of 135 taking turns,
// each run a few hundred milliseconds -- a program's phases.
func BenchmarkJITCacheHotSet(b *testing.B) {
	b.Run("hot180", func(b *testing.B) { benchJITCache(b, jitChurnSource(60, 0), `round(30,0)`) })
	b.Run("hot600", func(b *testing.B) { benchJITCache(b, jitChurnSource(200, 0), `round(10,0)`) })
	b.Run("halves", func(b *testing.B) { benchJITCache(b, jitChurnSource(90, 0), `half(0,200)+half(1,200)`) })
}

// BenchmarkJITMemoryLimit runs the 600 hot functions of
// BenchmarkJITCacheHotSet under JIT memory limits (Config.JITMemoryLimit)
// below what they hold at the default, and without the JIT: what a lower
// limit costs.
func BenchmarkJITMemoryLimit(b *testing.B) {
	source := jitChurnSource(200, 0)
	for _, limit := range []int64{0, 4 << 20, 2 << 20, 1 << 20, 512 << 10} {
		b.Run(fmt.Sprintf("limit=%dK", limit>>10), func(b *testing.B) {
			benchJITCacheIn(b, Config{JIT: true, JITMemoryLimit: limit}, source, `round(10,0)`)
		})
	}
	b.Run("nojit", func(b *testing.B) { benchJITCacheIn(b, Config{}, source, `round(10,0)`) })
	// BenchmarkJITCacheChurn's stream of cold functions, which keeps
	// coming.
	churn := jitChurnSource(30, 600)
	for _, limit := range []int64{0, 2 << 20, 1 << 20} {
		b.Run(fmt.Sprintf("churn/limit=%dK", limit>>10), func(b *testing.B) {
			benchJITCacheIn(b, Config{JIT: true, JITMemoryLimit: limit}, churn, `round(30,20)`)
		})
	}
	// Two halves of 135 hot functions taking turns, each for longer than
	// code takes to grow old: what one half holds goes for the other's.
	halves := jitChurnSource(90, 0)
	for _, limit := range []int64{0, 1 << 20} {
		b.Run(fmt.Sprintf("halves/limit=%dK", limit>>10), func(b *testing.B) {
			benchJITCacheIn(b, Config{JIT: true, JITMemoryLimit: limit}, halves, `half(0,200)+half(1,200)`)
		})
	}
}

// benchJITCache runs call in a runtime that ran source, reporting what
// the JIT did an op -- its compiles, the entries its cache dropped to make
// room, the compiles refused room -- and, at the end, what it holds and
// the process's live heap.
func benchJITCache(b *testing.B, source, call string) {
	benchJITCacheIn(b, Config{JIT: true}, source, call)
}

// benchJITCacheIn is benchJITCache in a runtime made with cfg.
func benchJITCacheIn(b *testing.B, cfg Config, source, call string) {
	if !jitSSABackend {
		b.Skip("no SSA backend on this architecture")
	}
	r := New(cfg)
	defer r.Close()
	if _, err := r.Run(compileForTest(b, source)); err != nil {
		b.Fatal(err)
	}
	p := compileForTest(b, call)
	var compiled, evicted, refused uint64
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if r.jit != nil {
			compiled, evicted, refused = r.jit.compiled, r.jit.evicted, r.jit.refusedRoom
		}
		if _, err := r.Run(p); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if s := r.jit; s != nil {
		// The last op's: the steady state, not the warm-up's.
		b.ReportMetric(float64(s.compiled-compiled), "compiles/op")
		b.ReportMetric(float64(s.evicted-evicted), "evicted/op")
		b.ReportMetric(float64(s.refusedRoom-refused), "refused/op")
		b.ReportMetric(float64(len(s.cache)), "entries")
		b.ReportMetric(float64(r.jitCodeBytes()), "code-B")
		b.ReportMetric(float64(r.jitHeldBytes()), "held-B")
		mapped, _ := s.arena.Mapped()
		b.ReportMetric(float64(mapped), "mapped-B")
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	b.ReportMetric(float64(ms.HeapAlloc), "live-B")
}

// jitFillForTest compiles copies of a function in r until the JIT's budget
// has no room for another, every entry young, and returns the entries.
func jitFillForTest(t *testing.T, r *Runtime, copyOf func() *bytecode.Function) []*jitEntry {
	t.Helper()
	var entries []*jitEntry
	for {
		e := r.jitForFunction(copyOf(), nil)
		if e == nil {
			break
		}
		if e.ssa == nil {
			t.Fatal("a compile was refused")
		}
		entries = append(entries, e)
		if len(entries) > 1<<14 {
			t.Fatalf("%d entries in a budget of %d bytes", len(entries), r.jitBudget())
		}
	}
	if r.jit.refusedRoom == 0 || r.jit.evicted != 0 || len(r.jit.cache) != len(entries) {
		t.Fatalf("filling: %d refused, %d evicted, %d cached of %d", r.jit.refusedRoom, r.jit.evicted, len(r.jit.cache), len(entries))
	}
	return entries
}

// The cache drops the least recently used code to make room, as V8
// flushes code by its age (makeRoom): code used since -- entered, called
// natively, or called by the existing tiers that no longer enter it --
// stays, and the oldest of the rest goes. Code used within jitCodeAge
// epochs is not dropped for another function's: that one is refused, the
// cache unchanged, until some is old. What holds no code goes first,
// young or not: a function's lowering the cache has no entry for, then a
// refusal remembered.
// Top-level code is compiled for its loops, and the cache remembers it only
// while its script lives: a loop evaluating thousands of scripts, as
// test262's BMP tests do, had left as many refusals, which made each
// compile's room-making scan them all once the budget was full. A refusal
// of top-level code is its closure's, and code compiled for a script goes
// with it.
func TestJITNoEntryForTopLevelCode(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	for i := range 300 {
		v, err := r.Run(compileForTest(t, fmt.Sprintf(`var c=0;for(let i=0;i<100;i++)c+=i*%d;c`, i)))
		if err != nil || v.Number() != float64(4950*i) {
			t.Fatalf("script %d: %v, %v", i, v, err)
		}
		// A with statement's names, which lowering refuses.
		v, err = r.Run(compileForTest(t, fmt.Sprintf(`var d=0;for(let i=0;i<100;i++)with({v:i})d+=v*%d;d`, i)))
		if err != nil || v.Number() != float64(4950*i) {
			t.Fatalf("refused script %d: %v, %v", i, v, err)
		}
	}
	if r.jit == nil || r.jit.compiled == 0 {
		t.Fatal("no top-level code compiled")
	}
	for k, e := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.TopLevel && e.ssa == nil {
			t.Fatalf("a refusal of top-level code remembered: %d entries", len(r.jit.cache))
		}
	}
	// A context native code ran in last holds its closure (abi.Context's
	// Closure) until the next run there: one level's, here.
	runtime.GC()
	r.jit.age(r.jit.agedAt + 1)
	kept := 0
	for k := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.TopLevel {
			kept++
		}
	}
	if kept > 2 {
		t.Fatalf("top-level code kept past its script: %d entries", kept)
	}
}

// Making room, with the budget full, lists what may be reclaimed once in
// an epoch and takes from that order look after look, rather than listing
// and sorting all the cache holds at each compile: a run of compiles each
// finding the budget full of refusals was quadratic.
// What a compile adds is all in the room made for it: the JIT holds no
// more than its budget after any compile -- its hint table's growth too,
// which, once the table was full, took it past a budget a compile had just
// fitted, by half the table again. The budgets tried are those about the
// point where the table is full.
func TestJITCompileKeepsBudget(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return 0 }
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	copyOf := jitCopiesForTest(jitFunctionForTest(t, `function f(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s }`), &keep)
	// What is held when the table is next full, and what a compile adds.
	r := jitRuntimeForTest(t, Config{JIT: true})
	full, grows := 0, 0
	for i := 0; i < 1000 && full == 0; i++ {
		before := r.jitState().held()
		if e := r.jitForFunction(copyOf(), nil); e == nil || e.ssa == nil {
			t.Fatal("native compilation refused")
		}
		grows = r.jit.held() - before
		if s := r.jit; i > 8 && len(s.hints) == cap(s.hints) {
			full = s.held()
		}
	}
	if full == 0 {
		t.Fatal("the hint table never filled")
	}
	for d := 0; d < grows+1024; d += 64 {
		r := jitRuntimeForTest(t, Config{JIT: true, JITMemoryLimit: int64(full + d)})
		for i := 0; i < 1000; i++ {
			e := r.jitForFunction(copyOf(), nil)
			if held := r.jit.held(); held > r.jitBudget() {
				t.Fatalf("budget %d, compile %d: %d bytes held", r.jitBudget(), i, held)
			}
			if e == nil {
				break
			}
		}
	}
}

func TestJITMakeRoomListsOncePerEpoch(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 1 << 20})
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return 0 }
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	copyOf := jitCopiesForTest(jitFunctionForTest(t, `function f(n) { var s=0; with({}) { for(var i=0;i<n;i++) s+=i } return s }`), &keep)
	const n = 3000
	for range n {
		if e := r.jitForFunction(copyOf(), nil); e == nil || e.ssa != nil {
			t.Fatalf("not a refusal: %+v", e)
		}
	}
	if r.jit.evicted == 0 || r.jit.held() > r.jitBudget() {
		t.Fatalf("the budget never filled, or was passed: %d evicted, %d held of %d", r.jit.evicted, r.jit.held(), r.jitBudget())
	}
	if r.jit.scans > n/20 {
		t.Fatalf("%d listings for %d compiles, %d evicted", r.jit.scans, n, r.jit.evicted)
	}
}

func TestJITCacheDropsLeastRecentlyUsed(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 8 << 20})
	epoch := uint64(0)
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return epoch }
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	copyOf := jitCopiesForTest(jitFunctionForTest(t, `function f(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s }`), &keep)
	entries := jitFillForTest(t, r, copyOf)
	cached := func(e *jitEntry) bool {
		for _, c := range r.jit.cache {
			if c == e {
				return true
			}
		}
		return false
	}
	// All young: no room, again.
	if e := r.jitForFunction(copyOf(), nil); e != nil || len(r.jit.cache) != len(entries) || r.jit.evicted != 0 {
		t.Fatalf("a function was given room among young code: %+v", e)
	}
	// Old now; the even ones used since, each a way.
	epoch += jitCodeAge
	for i := 0; i < len(entries); i += 2 {
		switch i % 6 {
		case 0:
			entries[i].starts++
		case 2:
			entries[i].link.in++
		case 4:
			entries[i].slowCalls++
		}
	}
	// A third as many new ones: each takes the room of the least recently
	// used, the odd ones -- with the workspaces' room, now and then.
	for range len(entries) / 3 {
		if e := r.jitForFunction(copyOf(), nil); e == nil || e.ssa == nil {
			t.Fatal("no room made among old code")
		}
	}
	dropped := 0
	for i, e := range entries {
		if !cached(e) {
			if i%2 == 0 {
				t.Fatalf("entry %d, used since, was dropped", i)
			}
			dropped++
		}
	}
	if dropped < len(entries)/4 {
		t.Fatalf("%d of %d unused entries dropped for %d new ones", dropped, len(entries)/2, len(entries)/3)
	}
	// What holds no code goes before old code: a lowering without an
	// entry -- one made here, as a call site's inlining decision makes
	// them, since whether the entries dropped above left theirs depends on
	// the sizes making room took -- then a refusal, young.
	epoch += jitCodeAge
	if _, err := r.jitLower(copyOf(), jitLowerSSA); err != nil {
		t.Fatal(err)
	}
	refusal := &jitEntry{link: &jitLink{}, used: epoch, serial: 1 << 40}
	r.jit.remember(weak.Make(copyOf()), refusal)
	var code []*jitEntry
	for _, e := range r.jit.cache {
		if e.ssa != nil {
			code = append(code, e)
		}
	}
	ownerless := func() []weak.Pointer[bytecode.Function] {
		var keys []weak.Pointer[bytecode.Function]
		for key := range r.jit.lowered {
			if l := r.jit.links[key]; l == nil || l.owner == nil {
				keys = append(keys, key)
			}
		}
		return keys
	}
	if len(ownerless()) == 0 {
		t.Fatal("no lowering without an entry")
	}
	for range len(entries) {
		before := ownerless()
		if e := r.jitForFunction(copyOf(), nil); e == nil || e.ssa == nil {
			t.Fatal("no room made among old code")
		}
		codeGone := false
		for _, e := range code {
			codeGone = codeGone || !cached(e)
		}
		if !codeGone {
			continue
		}
		if cached(refusal) {
			t.Fatal("code was dropped before a refusal")
		}
		for _, key := range before {
			if r.jit.lowered[key] != nil {
				t.Fatal("code was dropped before a lowering without an entry")
			}
		}
		return
	}
	t.Fatal("no code was dropped")
}

// What the JIT holds is counted, each kind of it, against its budget
// (jitState.held): each entry's code with what describes its entries and
// exits, what it was compiled with and what it learned, its literals' and
// constructions' pools with their objects; the programs it lowered; the
// contexts native code ran in, the hint table and the links -- not the
// compiles' workspaces (sharedBytes). The sums the cache keeps are what
// measuring each afresh finds.
func TestJITHeldCountsEachKind(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// The clock stopped: the workspaces, let go once the runtime has
	// compiled nothing for jitWorkAge epochs (idle), are held to be
	// counted -- under checkptr, the script ran past them.
	now := jitEpochNow
	jitEpochNow = func() uint64 { return 100 }
	t.Cleanup(func() { jitEpochNow = now })
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function P(x){this.x=x}
		function mk(n){let s=0;for(let i=0;i<n;i++){const o={a:i,b:i+1};s=(s+new P(o.a).x+o.b)|0}return s}
		var h=(function(){let k=1;return function(x){return (x+k)|0}})();
		function caller(n){let t=0;for(let i=0;i<n;i++)t=(t+h(i))|0;return t}
		for(let r=0;r<4;r++){mk(500);caller(500)}`)); err != nil {
		t.Fatal(err)
	}
	s := r.jit
	s.agedAt = 0
	s.age(s.epoch())
	entries, pools := 0, 0
	for _, e := range s.cache {
		entries += e.measure()
		if e.bytes != e.measure() {
			t.Fatalf("an entry counted at %d holds %d", e.bytes, e.measure())
		}
		if e.ssa == nil {
			continue
		}
		if e.ssa.Footprint() <= e.ssa.Size() || e.bytes < jitEntryFixed+e.ssa.Footprint()+e.compiledBytes() {
			t.Fatalf("an entry's code: %d bytes of %d held, %d counted", e.ssa.Size(), e.ssa.Footprint(), e.bytes)
		}
		// Each pool, with its objects: the literals' and the
		// constructions'.
		held := 0
		for _, l := range e.literals {
			if l.pool.Count > 0 {
				pools++
				held += int(unsafe.Sizeof(*l.pool)) + int(l.pool.Size)*int(objectSize)
			}
		}
		for _, x := range append(slices.Clone(e.inlines), e.nativeCalls...) {
			if x.pool != nil && x.pool.Count > 0 {
				pools++
				held += int(unsafe.Sizeof(*x.pool)) + int(x.pool.Size)*int(objectSize)
			}
		}
		if e.learnedBytes() < held {
			t.Fatalf("pools of %d bytes counted at %d", held, e.learnedBytes())
		}
	}
	if entries != s.entryBytes || pools == 0 {
		t.Fatalf("entries hold %d, counted %d; %d pools", entries, s.entryBytes, pools)
	}
	lowered := 0
	for _, l := range s.lowered {
		n := jitLoweringFixed + jitCapBytes(l.hints.StringAdds) + jitCapBytes(l.hints.ElementReads)
		for _, p := range l.progs {
			if p != nil {
				n += p.Footprint()
				if p.Footprint() < len(p.Code)*int(unsafe.Sizeof(ir.Instruction{})) {
					t.Fatal("a program's instructions uncounted")
				}
			}
		}
		if l.bytes != n {
			t.Fatalf("a lowering holds %d, counted %d", n, l.bytes)
		}
		lowered += n
	}
	if lowered != s.lowerBytes || lowered == 0 {
		t.Fatalf("lowerings hold %d, counted %d", lowered, s.lowerBytes)
	}
	if len(s.ssaCtxs) == 0 || s.workBytes() == 0 || len(s.links) == 0 || len(s.hints) == 0 {
		t.Fatalf("%d contexts, %d bytes of workspaces, %d links, %d hints", len(s.ssaCtxs), s.workBytes(), len(s.links), len(s.hints))
	}
	shared := len(s.ssaCtxs)*jitContextBytes + len(s.links)*jitLinkFixed + jitCapBytes(s.hints)
	if s.sharedBytes() < shared || s.held() != s.entryBytes+s.lowerBytes+s.sharedBytes() {
		t.Fatalf("shared %d counted, at least %d held", s.sharedBytes(), shared)
	}
	// The workspaces are not counted: without them, the JIT holds the same.
	before := s.sharedBytes()
	s.ssaWork, s.mirWork = ssa.Workspace{}, mir.Workspace{}
	if s.workBytes() != 0 || s.sharedBytes() != before {
		t.Fatalf("shared %d counted with the workspaces, %d without", before, s.sharedBytes())
	}
	if r.jitHeldBytes() > int64(r.jitBudget()) {
		t.Fatalf("%d bytes held, past the budget's %d", r.jitHeldBytes(), r.jitBudget())
	}
}

// A runtime with a memory limit keeps its JIT within an eighth of it, and
// one with a JIT memory limit (Config.JITMemoryLimit) within that,
// running a program with far more hot functions than fit -- a hot core of
// native call chains among a stream of cold functions -- the least recently
// used old code dropped as the stream goes on, the hot core kept. Nothing
// it keeps by function grows past what its entries need: hint slots in use
// are the entries', links are the entries' and their code's, the rest free.
func TestJITMemoryLimitKeepsEighth(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	for _, cfg := range []Config{{JIT: true, MemoryLimit: 16 << 20}, {JIT: true, JITMemoryLimit: 2 << 20}, {JIT: true, MemoryLimit: 1 << 30, JITMemoryLimit: 2 << 20}} {
		epoch := uint64(0)
		jitEpochNow = func() uint64 { return epoch }
		r := jitRuntimeForTest(t, cfg)
		budget := int64(r.jitBudget())
		if budget != 2<<20 {
			t.Fatalf("%+v: budget %d", cfg, budget)
		}
		if _, err := r.Run(compileForTest(t, jitChurnSource(30, 1500))); err != nil {
			t.Fatal(err)
		}
		call := compileForTest(t, `round(10,40)`)
		for round := 0; round < 8; round++ {
			if _, err := r.Run(call); err != nil {
				t.Fatal(err)
			}
			epoch++
			s := r.jit
			if held := r.jitHeldBytes(); held > budget {
				t.Fatalf("%+v round %d: %d bytes held, past %d", cfg, round, held, budget)
			}
			if used := len(s.hints) - 1 - len(s.hintFree); used != len(s.cache) {
				t.Fatalf("%+v round %d: %d hint slots in use, %d entries", cfg, round, used, len(s.cache))
			}
			calls := 0
			for _, e := range s.cache {
				calls += len(e.ssaLinks)
			}
			if len(s.links) > len(s.cache)+calls {
				t.Fatalf("%+v round %d: %d links for %d entries calling through %d", cfg, round, len(s.links), len(s.cache), calls)
			}
		}
		if r.jit.evicted == 0 {
			t.Fatalf("%+v: the stream never filled the budget", cfg)
		}
		for c := 0; c < 30; c++ {
			fn := r.global.getOwn(r.atoms.intern(fmt.Sprintf("top_%d", c))).value().Object().fn().closure.fn
			if e := r.jit.cache[weak.Make(fn)]; e == nil || e.ssa == nil {
				t.Fatalf("%+v: hot top_%d was dropped", cfg, c)
			}
		}
	}
}

// A callee's list of native callers grows as a caller's code that calls it
// natively is placed, in the room the caller's compile made: what each
// entry holds is counted as it grows, never later. The list had grown when
// the call was first seen, counted only when the cache measured the callee
// again, so that a runtime held more than its budget between -- a few
// hundred bytes past 2 MB, at budgets that left no slack, such as macOS's.
func TestJITCallersCountedAsTheyJoin(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, jitChurnSource(6, 4))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(compileForTest(t, `round(4,4)`)); err != nil {
		t.Fatal(err)
	}
	s, callers := r.jit, 0
	for _, e := range s.cache {
		callers += len(e.nativeCallers)
		if e.hint != 0 && e.bytes != e.measure() {
			t.Fatalf("an entry holds %d bytes, %d counted", e.measure(), e.bytes)
		}
	}
	if callers == 0 {
		t.Fatal("no native callers: the chains were not called natively")
	}
	for _, e := range s.cache {
		for _, c := range e.nativeCallers {
			if !slices.Contains(c.ssaLinks, e.link) {
				t.Fatal("a native caller whose code does not call the entry through its link")
			}
		}
	}
}

// The JIT refuses a compile room only when nothing it may reclaim would
// make it: then it reclaims nothing -- not the old code there is, not the
// lowerings -- and the newcomer runs in the existing tiers. Once code is
// old, it goes for the newcomer, and only as much as the room needs.
func TestJITRefusesRoomOnlyWhenNothingIsOld(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 8 << 20})
	epoch := uint64(0)
	defer func(was func() uint64) { jitEpochNow = was }(jitEpochNow)
	jitEpochNow = func() uint64 { return epoch }
	var keep []*bytecode.Function
	defer runtime.KeepAlive(&keep)
	copyOf := jitCopiesForTest(jitFunctionForTest(t, `function f(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s }`), &keep)
	entries := jitFillForTest(t, r, copyOf)
	// Two entries old, the others used since: only they may go.
	epoch += jitCodeAge
	for _, e := range entries[2:] {
		e.starts++
	}
	young := func() {
		t.Helper()
		for _, e := range entries[2:] {
			if e.hint == 0 {
				t.Fatal("young code was dropped")
			}
		}
	}
	if e := r.jitForFunction(copyOf(), nil); e == nil || e.ssa == nil {
		t.Fatal("no room made from old code")
	}
	if r.jit.evicted == 0 || entries[0].hint != 0 && entries[1].hint != 0 {
		t.Fatalf("no old entry went: %d evicted", r.jit.evicted)
	}
	young()
	// Compiles until one is refused: then nothing is old, and the refusal
	// reclaims nothing -- not the lowerings without entries.
	for i := 0; ; i++ {
		evicted, refused, lowered, held := r.jit.evicted, r.jit.refusedRoom, len(r.jit.lowered), jitHeldButScratch(r)
		e := r.jitForFunction(copyOf(), nil)
		young()
		if e != nil {
			if e.ssa == nil || i > 2 {
				t.Fatalf("compile %d: %+v", i, e)
			}
			continue
		}
		if r.jit.refusedRoom == refused || r.jit.evicted != evicted || len(r.jit.lowered) != lowered || jitHeldButScratch(r) != held {
			t.Fatalf("a refusal reclaimed: %d evicted of %d, %d lowerings of %d, %d bytes held of %d",
				r.jit.evicted, evicted, len(r.jit.lowered), lowered, jitHeldButScratch(r), held)
		}
		break
	}
	// Old code stays only where reclaiming it too would not make the room:
	// the refusal counted it (jitState's floor).
	if e := jitOldStaysUncounted(r, entries[:2]); e != nil {
		t.Fatalf("old code of %d bytes stayed, not counted reclaimable: floor %d, %d held", e.bytes, r.jit.floor, jitHeldButScratch(r))
	}
}

func TestJITSharedTemplateRuntimes(t *testing.T) {
	p := compileForTest(t, jitSumSource)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := New(Config{JIT: true})
			r.jitCallThreshold, r.jitICThreshold = 1, 1
			defer func() { r.Close(); r.ReleaseClosed() }()
			for j := 0; j < 3; j++ {
				v, err := r.Run(p)
				if err != nil || v.Number() != 49995000 {
					t.Errorf("shared program = %v, %v", v, err)
					return
				}
			}
			if r.jit == nil || r.jit.entries == 0 {
				t.Error("shared program did not enter native code")
			}
		}()
	}
	wg.Wait()
}

func TestJITBudgetAllocations(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	p := compileForTest(t, jitSumSource)
	if _, err := r.Run(p); err != nil {
		t.Fatal(err)
	}
	if r.jit == nil || r.jit.budgets == 0 {
		t.Fatal("loop did not enter bounded native execution")
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 10; i++ {
		if _, err := r.Run(p); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	// Preparing a new top-level closure allocates; each native budget exit
	// must not box the 4 KiB root array into an interface. The byte bound
	// also allows small ABI temporaries checkptr forces onto the heap.
	if bytes := (after.TotalAlloc - before.TotalAlloc) / 10; bytes > 16<<10 {
		t.Fatalf("native budget loop allocated %d bytes per call", bytes)
	}
}

func TestJITClosureRemembersRefusal(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	v, err := r.Run(compileForTest(t, `function f(n) { var s=0; for(var i=0;i<n;i++) with({}) s+=i; return s } f(3); f`))
	if err != nil {
		t.Fatal(err)
	}
	cl := v.Object().fn().closure
	if !cl.jitRefused {
		t.Fatal("unsupported function did not remember its refusal")
	}
	r.releaseJIT()
	got, err := r.Call(v, Undefined, []Value{Int32(10)})
	if err != nil || got.Number() != 45 {
		t.Fatalf("refused call = %v, %v", got, err)
	}
	if r.jit != nil {
		t.Fatal("permanent refusal repeated the weak-cache lookup")
	}
}

// A function is given native state after jitInvocationsForFeedback calls,
// as V8 allocates a feedback vector, and is compiled once its calls have
// spent jitInvocationsForMaglev invocations' worth of its budget, as V8
// tiers up to Maglev: not before. Its caches' feedback, filled at its
// first call, does not change after, and delays nothing.
func TestJITCallPromotion(t *testing.T) {
	for _, keyword := range []string{"var", "let"} {
		t.Run(keyword, func(t *testing.T) {
			r := New(Config{JIT: true})
			t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
			if r.jitCallThreshold <= 1 {
				t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
			}
			v, err := r.Run(compileForTest(t, "function sum(n) { "+keyword+" s=0; for("+keyword+" i=0;i<n;i++) s+=i; return s } sum"))
			if err != nil {
				t.Fatal(err)
			}
			cl := v.Object().fn().closure
			for i := 1; i <= jitInvocationsForMaglev; i++ {
				// One iteration: its back edges spend next to nothing.
				got, err := r.Call(v, Undefined, []Value{Int32(1)})
				if err != nil || got.Number() != 0 {
					t.Fatalf("call %d = %v, %v", i, got, err)
				}
				if want := min(i, jitInvocationsForFeedback); int(cl.jitCalls) != want {
					t.Fatalf("call %d: hotness = %d", i, cl.jitCalls)
				}
				if i < jitInvocationsForFeedback && r.jit != nil {
					t.Fatalf("cold call %d allocated native state", i)
				}
				if i < jitInvocationsForMaglev && r.jit != nil && (r.jit.entries != 0 || r.jit.compiled != 0) {
					t.Fatalf("call %d: compiled before its budget was spent", i)
				}
			}
			if r.jit.entries != 1 || r.jit.compiled != 1 {
				t.Fatalf("hot function did not promote at call %d", jitInvocationsForMaglev)
			}
			for i := 0; i < 300; i++ {
				got, err := r.Call(v, Undefined, []Value{Int32(10)})
				if err != nil || got.Number() != 45 {
					t.Fatalf("hot call = %v, %v", got, err)
				}
			}
			if cl.jitCalls != jitInvocationsForFeedback || r.jit.entries != 301 {
				t.Fatal("hot counter overflowed or native selection stopped")
			}
		})
	}
}

// A compile refused for memory is not remembered: the function warms up
// again, given native state after another jitInvocationsForFeedback calls,
// and is compiled once its budget is spent again.
func TestJITPromotionRetriesMemoryRefusal(t *testing.T) {
	r := New(Config{JIT: true, MemoryLimit: 256 << 10})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	v, err := r.Run(compileForTest(t, `function sum(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s } sum`))
	if err != nil {
		t.Fatal(err)
	}
	cl := v.Object().fn().closure
	for i := 0; i < jitInvocationsForMaglev; i++ {
		if _, err := r.Call(v, Undefined, []Value{Int32(10)}); err != nil {
			t.Fatal(err)
		}
	}
	if cl.jitRefused || r.jit != nil && r.jit.entries != 0 {
		t.Fatal("memory refusal was permanent or ran native code")
	}
	r.meter.limit = 32 << 20
	for i := 1; i <= 2*jitInvocationsForMaglev; i++ {
		got, err := r.Call(v, Undefined, []Value{Int32(10)})
		if err != nil || got.Number() != 45 {
			t.Fatalf("retry %d = %v, %v", i, got, err)
		}
	}
	if r.jit == nil || r.jit.entries == 0 {
		t.Fatal("temporary refusal never retried")
	}
}

func TestJITPromotionRuntimeIsolation(t *testing.T) {
	p := compileForTest(t, `function sum(n) { var s=0; for(var i=0;i<n;i++) s+=i; return s } sum`)
	var cold *Runtime
	for _, calls := range []int{1, jitInvocationsForMaglev} {
		r := New(Config{JIT: true})
		t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
		v, err := r.Run(p)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < calls; i++ {
			got, err := r.Call(v, Undefined, []Value{Int32(10)})
			if err != nil || got.Number() != 45 {
				t.Fatalf("call = %v, %v", got, err)
			}
		}
		if calls == 1 {
			cold = r
		} else if r.jit == nil || r.jit.entries == 0 {
			t.Fatal("hot runtime did not promote")
		}
	}
	if cold.jit != nil && cold.jitCallThreshold > 1 {
		t.Fatal("shared template propagated promotion to a cold runtime")
	}
}

func TestJITPromotionKeepsFramelessCalls(t *testing.T) {
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	v, err := r.Run(compileForTest(t, `function f(a) { return a+1 } var result; for(var i=0;i<1000;i++) result=f(i); result`))
	if err != nil || v.Number() != 1000 {
		t.Fatalf("frameless calls = %v, %v", v, err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	if cl.jitCalls != 0 || r.jit != nil {
		t.Fatal("frameless calls accumulated framed hotness")
	}
}

// A long first call promotes its loop, from the tree tier, which runs
// functions with var and let bindings alike, or from the interpreter, with
// the tier off.
func TestJITLoopPromotionFirstCall(t *testing.T) {
	for _, tc := range []struct {
		name, keyword string
		tree          bool
	}{{"var", "var", true}, {"let", "let", true}, {"interpreter", "let", false}} {
		keyword := tc.keyword
		t.Run(tc.name, func(t *testing.T) {
			if !tc.tree {
				SetTreeTier(false)
				defer SetTreeTier(true)
			}
			r := New(Config{JIT: true})
			t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
			source := "function sum(n) { " + keyword + " s=0; for(" + keyword + " i=0;i<n;i++) s+=i; return s } sum(10000)"
			v, err := r.Run(compileForTest(t, source))
			if err != nil || v.Number() != 49995000 {
				t.Fatalf("first call = %v, %v", v, err)
			}
			if r.jit == nil || r.jit.osrs != 1 || r.jit.budgets == 0 {
				t.Fatal("first long call did not complete bounded OSR")
			}
			cl := r.global.getOwn(r.atoms.intern("sum")).value().Object().fn().closure
			tree := (*tree)(atomic.LoadPointer(&cl.fn.VMCode))
			if tc.tree && (tree == nil || tree == noTree) || !tc.tree && tree != nil && tree != noTree {
				t.Fatal("test did not exercise its expected existing tier")
			}
			if r.jitDeoptDepth != 0 {
				t.Fatal("completed call retained a deoptimized frame")
			}
		})
	}
}

func TestJITLoopPromotionAtEntryPC(t *testing.T) {
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	v, err := r.Run(compileForTest(t, `function f(n) { while(n) n--; return n } f(10000)`))
	if err != nil || v.Number() != 0 {
		t.Fatalf("entry loop = %v, %v", v, err)
	}
	if r.jit == nil || r.jit.osrs != 1 {
		t.Fatal("loop targeting entry did not promote")
	}
}

func TestJITLoopPromotionLiveOperands(t *testing.T) {
	for _, keyword := range []string{"var", "let"} {
		t.Run(keyword, func(t *testing.T) {
			r := New(Config{JIT: true})
			t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
			original := jitFunctionForTest(t, "function f(a,n) { "+keyword+" s=0; for("+keyword+" i=0;i<n;i++) s+=i; return s }")
			fn := *original
			// Carry an opaque operand through the loop, then return it after
			// a swap. Native return must decode that exact rooted operand.
			fn.Code = []bytecode.Instr{{Op: bytecode.OpGetLocal}}
			mapping := make([]uint32, len(original.Code))
			for i, in := range original.Code {
				mapping[i] = uint32(len(fn.Code))
				if in.Op == bytecode.OpReturn {
					fn.Code = append(fn.Code, bytecode.Instr{Op: bytecode.OpSwap})
				}
				fn.Code = append(fn.Code, in)
			}
			for i := 1; i < len(fn.Code); i++ {
				switch fn.Code[i].Op {
				case bytecode.OpJump, bytecode.OpJumpIfTrue, bytecode.OpJumpIfFalse,
					bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfCmpFalse:
					fn.Code[i].A = mapping[fn.Code[i].A]
				}
			}
			fn.MaxStack++
			o := r.NewObject()
			v, err := r.run(r.prepare(&fn), Undefined, []Value{Obj(o), Int32(10000)}, Undefined, nil)
			if err != nil || !v.IsObject() || v.Object() != o {
				t.Fatalf("live operand = %v, %v", v, err)
			}
			if r.jit == nil || r.jit.osrs != 1 || r.jit.guards != 0 {
				t.Fatal("live operand was lost, guarded, or retained")
			}
		})
	}
}

func TestJITLoopGuardSuppressesReentry(t *testing.T) {
	for _, keyword := range []string{"var", "let"} {
		t.Run(keyword, func(t *testing.T) {
			r := New(Config{JIT: true})
			t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
			count := 0
			released := false
			o := r.NewObject()
			// The first coercion releases the code that called it, which
			// goes on, pinned, to the loop's end; the invocation compiles
			// nothing again.
			o.setOwnRaw(r.atoms.intern("valueOf"), r.NewFunction("valueOf", 0,
				func(rt *Runtime, _ Value, _ []Value) (Value, error) {
					count++
					if rt.jit != nil && len(rt.jit.cache) != 0 {
						if released {
							t.Fatal("invocation recompiled after eviction")
						}
						released = true
						rt.releaseJIT()
					}
					return Int32(1), nil
				}), propDefault)
			r.global.setOwnRaw(r.atoms.intern("operand"), Obj(o), propDefault)
			source := "function f(n,a) { " + keyword + " s=0; for(" + keyword + " i=0;i<n;i++) s+=a; return s } f(10000,operand)"
			v, err := r.Run(compileForTest(t, source))
			if err != nil || v.Number() != 10000 || count != 10000 {
				t.Fatalf("guard = %v, %v; coercions %d", v, err, count)
			}
			if !released || r.jitDeoptDepth != 0 || r.jit != nil && r.jit.ctxTop != 0 {
				t.Fatal("native code was not released, or did not restore ownership")
			}
		})
	}
}

func TestJITLoopPromotionCancellation(t *testing.T) {
	for _, keyword := range []string{"var", "let"} {
		t.Run(keyword, func(t *testing.T) {
			r := New(Config{JIT: true})
			t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			r.SetContext(ctx)
			_, err := r.Run(compileForTest(t, "function f() { "+keyword+" i=0; for(;;) i++ } f()"))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancel = %v", err)
			}
			if r.jit == nil || r.jit.osrs != 1 || r.jit.budgets == 0 {
				t.Fatal("first infinite call never entered bounded native code")
			}
		})
	}
}

func TestJITLoopGuardErrors(t *testing.T) {
	for _, tc := range []struct{ name, source, message string }{
		{"bigint", `function f(n,a) { var s=0; for(var i=0;i<n;i++) { s+=i; if(i===2000) s+=a } return s } f(10000,1n)`,
			"TypeError: cannot mix BigInt and other types"},
		{"tdz", `function f(n) { let s=0; for(let i=0;i<n;i++) { s+=i; if(i===2000) { let x=x; s+=x } } return s } f(10000)`,
			`ReferenceError: cannot access "x" before initialization`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := compileForTest(t, tc.source)
			var baseline *Thrown
			for _, enabled := range []bool{false, true} {
				r := New(Config{JIT: enabled})
				t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
				_, err := r.Run(p)
				var thrown *Thrown
				if !errors.As(err, &thrown) || thrown.Error() != tc.message {
					t.Fatalf("enabled %v: error = %v", enabled, err)
				}
				if !enabled {
					baseline = thrown
					continue
				}
				if r.jit == nil || r.jit.osrs != 1 || r.jit.guards != 1 || r.jitDeoptDepth != 0 {
					t.Fatal("error did not follow OSR and guard fallback")
				}
				if len(thrown.trace.frames) != len(baseline.trace.frames) {
					t.Fatal("OSR changed the thrown trace depth")
				}
				for i, frame := range thrown.trace.frames {
					want := baseline.trace.frames[i]
					if frame.fn != want.fn || frame.pc != want.pc {
						t.Fatalf("trace frame %d = %+v, want %+v", i, frame, want)
					}
				}
			}
		})
	}
}

func BenchmarkJITNumericLoop(b *testing.B) {
	for _, keyword := range []string{"var", "let"} {
		for _, enabled := range []bool{false, true} {
			name := keyword + "/existing"
			if enabled {
				name = keyword + "/native"
			}
			b.Run(name, func(b *testing.B) {
				source := "function sum(n) { " + keyword + " s=0; for(" + keyword + " i=0;i<n;i++) s+=i; return s }"
				ast, err := parser.Parse(source, parser.Options{})
				if err != nil {
					b.Fatal(err)
				}
				p, err := compiler.Compile(ast, compiler.Options{})
				if err != nil {
					b.Fatal(err)
				}
				r := New(Config{JIT: enabled})
				defer func() { r.Close(); r.ReleaseClosed() }()
				if _, err := r.Run(p); err != nil {
					b.Fatal(err)
				}
				ast, err = parser.Parse("sum(10000)", parser.Options{})
				if err != nil {
					b.Fatal(err)
				}
				p, err = compiler.Compile(ast, compiler.Options{})
				if err != nil {
					b.Fatal(err)
				}
				for i := 0; i < jitInvocationsForFeedback; i++ {
					if _, err := r.Run(p); err != nil {
						b.Fatal(err)
					}
				}
				if enabled && (r.jit == nil || r.jit.entries == 0) {
					b.Fatal("benchmark did not enter native code")
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					v, err := r.Run(p)
					if err != nil || v.Number() != 49995000 {
						b.Fatalf("sum = %v, %v", v, err)
					}
				}
			})
		}
	}
}

func BenchmarkJITCallPromotion(b *testing.B) {
	for _, iterations := range []int{100, 10000} {
		for _, calls := range []int{1, 4, 16, 64} {
			for _, mode := range []string{"existing", "eager", "automatic"} {
				b.Run(fmt.Sprintf("iterations%d/calls%d/%s", iterations, calls, mode), func(b *testing.B) {
					source := fmt.Sprintf(`function sum(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }
					var result; for(var k=0;k<%d;k++) result=sum(%d); result`, calls, iterations)
					ast, err := parser.Parse(source, parser.Options{})
					if err != nil {
						b.Fatal(err)
					}
					p, err := compiler.Compile(ast, compiler.Options{})
					if err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						r := New(Config{JIT: mode != "existing"})
						if mode == "eager" {
							r.jitCallThreshold, r.jitICThreshold = 1, 1
						}
						v, err := r.Run(p)
						native := r.jit != nil && r.jit.entries != 0
						coldState := r.jit != nil
						r.Close()
						r.ReleaseClosed()
						if err != nil || v.Number() != float64(iterations*(iterations-1)/2) {
							b.Fatalf("sum = %v, %v", v, err)
						}
						if mode != "existing" && (mode == "eager" || calls >= jitInvocationsForFeedback || iterations > backEdgeCheckInterval+1) && !native {
							b.Fatal("hot benchmark did not enter native code")
						}
						if mode == "automatic" && calls < jitInvocationsForFeedback && iterations < backEdgeCheckInterval && coldState {
							b.Fatal("cold benchmark allocated native state")
						}
					}
				})
			}
		}
	}
}

func jitNumericKernelCases() []struct{ name, body string } {
	return []struct{ name, body string }{
		{"logistic", `VAR x=0.123; for(VAR i=0;i<n;i++) x=3.99*x*(1-x); return x`},
		{"newton", `VAR x=1,s=0; for(VAR i=0;i<n;i++) { x=(x+2/x)*0.5; s+=x } return s`},
		{"particle", `VAR x=0,y=0,vx=0.2,vy=0.3,s=0; for(VAR i=0;i<n;i++) { vx-=x*0.001; vy-=y*0.001; x+=vx; y+=vy; if(x>10||x< -10) vx=-vx; if(y>10||y< -10) vy=-vy; s+=x*x+y*y } return s`},
	}
}

func jitDenseKernelCases() []struct{ name, body string } {
	return []struct{ name, body string }{
		{"vector", `for(var i=0;i<n;i++)a[i]=2*b[i]+1;return a[n-1]`},
		{"stencil", `for(var i=1;i<n-1;i++)a[i]=(b[i-1]+b[i]+b[i+1])/3;return a[n-2]`},
		{"helper", `for(var k=0;k<4;k++){for(var i=1;i<n-1;i++)a[i]=(b[i-1]+b[i]+b[i+1])/3;boundary(a)}return a[0]`},
	}
}

func BenchmarkJITDenseKernels(b *testing.B) {
	for _, tc := range jitDenseKernelCases() {
		for _, mode := range []string{"interpreter", "existing", "ssa"} {
			if mode == "ssa" && !jitSSABackend {
				continue
			}
			b.Run(tc.name+"/"+mode, func(b *testing.B) {
				previous := treeTier.Swap(mode != "interpreter")
				defer treeTier.Store(previous)
				compile := func(src string) *bytecode.Function {
					ast, err := parser.Parse(src, parser.Options{})
					if err != nil {
						b.Fatal(err)
					}
					p, err := compiler.Compile(ast, compiler.Options{})
					if err != nil {
						b.Fatal(err)
					}
					return p
				}
				setup := compile(`function make(n){function boundary(a){a[0]=a[1];a[n-1]=a[n-2]}return function kernel(a,b){` + tc.body + `}}var kernel=make(8192);var a=new Array(8192),b=new Array(8192);for(var i=0;i<8192;i++){a[i]=1;b[i]=1}`)
				call := compile(`kernel(a,b)`)
				r := New(Config{JIT: mode == "ssa"})
				defer func() { r.Close(); r.ReleaseClosed() }()
				if _, err := r.Run(setup); err != nil {
					b.Fatal(err)
				}
				var want Value
				for i := 0; i < jitInvocationsForFeedback; i++ {
					v, err := r.Run(call)
					if err != nil {
						b.Fatal(err)
					}
					want = v
				}
				if mode == "ssa" && (r.jit == nil || r.jit.entries == 0 || r.jit.guards != 0) {
					b.Fatal("did not stay native")
				}
				if mode == "ssa" && (r.jit.ssaEntries == 0 || r.jit.guards != 0) {
					b.Fatalf("did not stay in the new pipeline: %+v", r.JITStats())
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					v, err := r.Run(call)
					if err != nil || !jitSameValueForTest(v, want) {
						b.Fatalf("result %v error %v want %v", v, err, want)
					}
				}
				b.StopTimer()
				if r.jit != nil {
					b.ReportMetric(float64(r.jitCodeBytes()), "code+metadata-B")
				}
			})
		}
	}
}

// BenchmarkJITArrayKernels runs array kernels whose every binding is a
// parameter or a local, which the new pipeline compiles, in the tree tier
// ("existing"), the old pipeline ("native") and the new one ("ssa").
func BenchmarkJITArrayKernels(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"vector", `for(var i=0;i<n;i++)a[i]=2*b[i]+1;return a[n-1]`},
		{"stencil", `for(var i=1;i<n-1;i++)a[i]=(b[i-1]+b[i]+b[i+1])/3;return a[n-2]`},
		{"dot", `var s=0;for(var i=0;i<n;i++)s+=a[i]*b[i];return s`},
	} {
		for _, mode := range []string{"existing", "ssa"} {
			if mode == "ssa" && !jitSSABackend {
				continue
			}
			b.Run(tc.name+"/"+mode, func(b *testing.B) {
				r := New(Config{JIT: mode != "existing"})
				defer func() { r.Close(); r.ReleaseClosed() }()
				setup := compileForTest(b, `function kernel(a,b,n){`+tc.body+`}var a=new Array(8192),b=new Array(8192);for(var i=0;i<8192;i++){a[i]=1;b[i]=i%7}`)
				call := compileForTest(b, `kernel(a,b,8192)`)
				if _, err := r.Run(setup); err != nil {
					b.Fatal(err)
				}
				var want Value
				for i := 0; i < jitInvocationsForFeedback; i++ {
					v, err := r.Run(call)
					if err != nil {
						b.Fatal(err)
					}
					want = v
				}
				if mode == "ssa" && (r.jit.ssaEntries == 0 || r.jit.guards != 0) {
					b.Fatalf("did not stay in the new pipeline: %+v", r.JITStats())
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					v, err := r.Run(call)
					if err != nil || !jitSameValueForTest(v, want) {
						b.Fatalf("result %v error %v want %v", v, err, want)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/8192, "ns/elem")
			})
		}
	}
}

// The external corpus keeps Tom Wu's license with its implementation. This
// benchmark executes its actual am3 method, and checks every output limb using
// an independent integer multiply/add model after stopping the timer.
func BenchmarkJITCryptoWorkload(b *testing.B) {
	dir := os.Getenv("QUICKJS_JIT_V8_DIR")
	if dir == "" {
		b.Skip("set QUICKJS_JIT_V8_DIR to the external V8 v7 suite")
	}
	var source strings.Builder
	for _, name := range []string{"base.js", "crypto.js"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			b.Fatal(err)
		}
		source.Write(data)
		source.WriteByte('\n')
	}
	compile := func(text string) *bytecode.Function {
		ast, err := parser.Parse(text, parser.Options{})
		if err != nil {
			b.Fatal(err)
		}
		fn, err := compiler.Compile(ast, compiler.Options{})
		if err != nil {
			b.Fatal(err)
		}
		return fn
	}
	// The external decrypt function asserts the recovered plaintext, so every
	// timed pair validates the whole RSA path, not only its limb kernel.
	for _, mode := range []string{"interpreter", "existing", "native"} {
		b.Run(mode, func(b *testing.B) {
			previous := treeTier.Swap(mode != "interpreter")
			defer treeTier.Store(previous)
			// Bytecode caches its first tree decision; each tier needs fresh code.
			setup := compile(source.String())
			call := compile(`encrypt();decrypt();true`)
			r := New(Config{JIT: mode == "native"})
			defer func() { r.Close(); r.ReleaseClosed() }()
			if _, err := r.Run(setup); err != nil {
				b.Fatal(err)
			}
			run := func() {
				if v, err := r.Run(call); err != nil || !v.IsBool() || !v.Truthy() {
					b.Fatalf("Crypto pair: %v, %v", v, err)
				}
			}
			for range jitInvocationsForFeedback {
				run()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				run()
			}
			b.StopTimer()
			if mode == "native" {
				cl := r.global.getOwn(r.atoms.intern("am3")).value().Object().fn().closure
				e := r.jit.cache[weak.Make(cl.fn)]
				if e == nil || e.ssa == nil || e.notNative {
					b.Fatal("Crypto limb method did not stay native")
				}
				b.ReportMetric(float64(r.jitCodeBytes()), "code+metadata-B")
			}
		})
	}
}

func BenchmarkJITCryptoLimb(b *testing.B) {
	dir := os.Getenv("QUICKJS_JIT_V8_DIR")
	if dir == "" {
		b.Skip("set QUICKJS_JIT_V8_DIR to the external V8 v7 suite")
	}
	var source strings.Builder
	for _, name := range []string{"base.js", "crypto.js"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			b.Fatal(err)
		}
		source.Write(data)
		source.WriteByte('\n')
	}
	for _, n := range []int{1, 4, 16, 32, 8192} {
		for _, mode := range []string{"interpreter", "existing", "native"} {
			b.Run(fmt.Sprintf("n%d/%s", n, mode), func(b *testing.B) {
				previous := treeTier.Swap(mode != "interpreter")
				defer treeTier.Store(previous)
				compile := func(text string) *bytecode.Function {
					ast, err := parser.Parse(text, parser.Options{})
					if err != nil {
						b.Fatal(err)
					}
					fn, err := compiler.Compile(ast, compiler.Options{})
					if err != nil {
						b.Fatal(err)
					}
					return fn
				}
				setup := compile(source.String() + fmt.Sprintf(`var lhs={array:new Array(%d)},rhs={array:new Array(%d)};
					for(var i=0;i<%d;i++){lhs.array[i]=(i*1664525+1013904223)&268435455;rhs.array[i]=0}`, n, n, n))
				call := compile(fmt.Sprintf(`am3.call(lhs,0,67123451,rhs,0,0,%d)`, n))
				r := New(Config{JIT: mode == "native"})
				defer func() { r.Close(); r.ReleaseClosed() }()
				if _, err := r.Run(setup); err != nil {
					b.Fatal(err)
				}
				input, output := make([]uint32, n), make([]uint32, n)
				for i := range input {
					input[i] = uint32(uint64(i)*1664525+1013904223) & 268435455
				}
				advance := func() uint64 {
					carry := uint64(0)
					for i, x := range input {
						total := uint64(x)*67123451 + uint64(output[i]) + carry
						output[i] = uint32(total & 268435455)
						carry = total >> 28
					}
					return carry
				}
				for range jitInvocationsForFeedback {
					v, err := r.Run(call)
					if err != nil || v.Number() != float64(advance()) {
						b.Fatalf("warm carry: %v, %v", v, err)
					}
				}
				var last Value
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var err error
					last, err = r.Run(call)
					if err != nil || !last.IsNumber() {
						b.Fatalf("carry: %v, %v", last, err)
					}
				}
				b.StopTimer()
				var want uint64
				for i := 0; i < b.N; i++ {
					want = advance()
				}
				if last.Number() != float64(want) {
					b.Fatalf("carry %v, want %v", last.Number(), want)
				}
				obj := r.global.getOwn(r.atoms.intern("rhs")).value().Object()
				array, ok := plainOwn(obj, r.atoms.intern("array"))
				if !ok || len(array.Object().elemsOf()) != n {
					b.Fatal("missing output limbs")
				}
				for i, want := range output {
					if got := array.Object().elemsOf()[i]; !got.IsNumber() || got.Number() != float64(want) {
						b.Fatalf("limb %d: %v, want %v", i, got, want)
					}
				}
				if mode == "native" {
					if r.jit == nil || n > 1 && r.jit.entries == 0 || r.jit.guards != 0 {
						b.Fatal("limb loop did not stay native")
					}
					b.ReportMetric(float64(r.jitCodeBytes()), "code+metadata-B")
				}
			})
		}
	}
}

func BenchmarkJITNumericFields(b *testing.B) {
	for _, mode := range []string{"interpreter", "existing", "ssa"} {
		if mode == "ssa" && !jitSSABackend {
			continue
		}
		b.Run(mode, func(b *testing.B) {
			previous := treeTier.Swap(mode != "interpreter")
			defer treeTier.Store(previous)
			compile := func(source string) *bytecode.Function {
				ast, err := parser.Parse(source, parser.Options{})
				if err != nil {
					b.Fatal(err)
				}
				fn, err := compiler.Compile(ast, compiler.Options{})
				if err != nil {
					b.Fatal(err)
				}
				return fn
			}
			setup := compile(`function fields(o,n){let s=0;for(let i=0;i<n;i++){s=(s+o.x)|0;o.x=o.x+1}return s}var fieldObject={x:3}`)
			call := compile(`fields(fieldObject,4096)`)
			r := New(Config{JIT: mode == "ssa"})
			defer func() { r.Close(); r.ReleaseClosed() }()
			if _, err := r.Run(setup); err != nil {
				b.Fatal(err)
			}
			x := uint64(3)
			run := func() {
				want := float64(int32(uint32(x*4096 + 4096*4095/2)))
				v, err := r.Run(call)
				if err != nil || !v.IsNumber() || v.Number() != want {
					b.Fatalf("fields: %v, %v; want %v", v, err, want)
				}
				x += 4096
			}
			for range jitInvocationsForFeedback {
				run()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				run()
			}
			b.StopTimer()
			if mode == "ssa" {
				if r.jit == nil || r.jit.hosts != 0 || r.jit.guards != 0 {
					b.Fatal("numeric fields left native execution")
				}
				b.ReportMetric(float64(r.jitCodeBytes()), "code+metadata-B")
			}
			if mode == "ssa" && (r.jit.ssaEntries == 0 || r.jit.hosts != 0 || r.jit.guards != 0) {
				b.Fatalf("numeric fields left the new pipeline: %+v", r.JITStats())
			}
		})
	}
}

func BenchmarkJITNumericGlobals(b *testing.B) {
	for _, mode := range []string{"interpreter", "existing", "native"} {
		b.Run(mode, func(b *testing.B) {
			previous := treeTier.Swap(mode != "interpreter")
			defer treeTier.Store(previous)
			compile := func(source string) *bytecode.Function {
				ast, err := parser.Parse(source, parser.Options{})
				if err != nil {
					b.Fatal(err)
				}
				fn, err := compiler.Compile(ast, compiler.Options{})
				if err != nil {
					b.Fatal(err)
				}
				return fn
			}
			setup := compile(`var scale=3,offset=7;function globals(n){let s=0;for(let i=0;i<n;i++)s+=i*scale+offset;return s}`)
			call := compile(`globals(4096)`)
			r := New(Config{JIT: mode == "native"})
			defer func() { r.Close(); r.ReleaseClosed() }()
			if _, err := r.Run(setup); err != nil {
				b.Fatal(err)
			}
			run := func() {
				v, err := r.Run(call)
				const want = 3*4096*4095/2 + 7*4096
				if err != nil || !v.IsNumber() || v.Number() != want {
					b.Fatalf("globals: %v, %v; want %d", v, err, want)
				}
			}
			for range jitInvocationsForFeedback {
				run()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				run()
			}
			b.StopTimer()
			if mode == "native" {
				if r.jit == nil || r.jit.entries == 0 || r.jit.hosts != 0 || r.jit.guards != 0 {
					b.Fatal("numeric globals left native execution")
				}
				b.ReportMetric(float64(r.jitCodeBytes()), "code+metadata-B")
			}
		})
	}
}

func BenchmarkJITReferenceFields(b *testing.B) {
	for _, mode := range []string{"interpreter", "existing", "native"} {
		b.Run(mode, func(b *testing.B) {
			previous := treeTier.Swap(mode != "interpreter")
			defer treeTier.Store(previous)
			compile := func(source string) *bytecode.Function {
				ast, err := parser.Parse(source, parser.Options{})
				if err != nil {
					b.Fatal(err)
				}
				fn, err := compiler.Compile(ast, compiler.Options{})
				if err != nil {
					b.Fatal(err)
				}
				return fn
			}
			setup := compile(`function references(o,n){let s=0;for(let i=0;i<n;i++)s+=o.m.array[0];return s}var referenceObject={m:{array:[3]}}`)
			call := compile(`references(referenceObject,4096)`)
			r := New(Config{JIT: mode == "native"})
			defer func() { r.Close(); r.ReleaseClosed() }()
			if _, err := r.Run(setup); err != nil {
				b.Fatal(err)
			}
			run := func() {
				v, err := r.Run(call)
				const want = 3 * 4096
				if err != nil || !v.IsNumber() || v.Number() != want {
					b.Fatalf("references: %v, %v; want %d", v, err, want)
				}
			}
			for range jitInvocationsForFeedback {
				run()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				run()
			}
			b.StopTimer()
			if mode == "native" {
				if r.jit == nil || r.jit.entries == 0 || r.jit.hosts != 0 || r.jit.guards != 0 {
					b.Fatal("reference fields left native execution")
				}
				b.ReportMetric(float64(r.jitCodeBytes()), "code+metadata-B")
			}
		})
	}
}

func BenchmarkJITReferenceFirstUse(b *testing.B) {
	compile := func(source string) *bytecode.Function {
		ast, err := parser.Parse(source, parser.Options{})
		if err != nil {
			b.Fatal(err)
		}
		fn, err := compiler.Compile(ast, compiler.Options{})
		if err != nil {
			b.Fatal(err)
		}
		return fn
	}
	setup := compile(`function references(o,n){let s=0;for(let i=0;i<n;i++)s+=o.m.array[0];return s}var referenceObject={m:{array:[3]}}`)
	call := compile(`references(referenceObject,4096)`)
	for _, enabled := range []bool{false, true} {
		mode := "existing"
		if enabled {
			mode = "automatic"
		}
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for range b.N {
				r := New(Config{JIT: enabled})
				if _, err := r.Run(setup); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				v, err := r.Run(call)
				b.StopTimer()
				native := r.jit != nil && r.jit.entries != 0 && r.jit.hosts == 0 && r.jit.guards == 0
				r.Close()
				r.ReleaseClosed()
				if err != nil || v.Number() != 12288 || enabled && !native {
					b.Fatalf("first reference call: %v, %v, native %v", v, err, native)
				}
			}
		})
	}
}

func BenchmarkJITDenseFirstUse(b *testing.B) {
	for _, tc := range jitDenseKernelCases() {
		for _, enabled := range []bool{false, true} {
			mode := "existing"
			if enabled {
				mode = "automatic"
			}
			b.Run(tc.name+"/"+mode, func(b *testing.B) {
				compile := func(src string) *bytecode.Function {
					ast, err := parser.Parse(src, parser.Options{})
					if err != nil {
						b.Fatal(err)
					}
					p, err := compiler.Compile(ast, compiler.Options{})
					if err != nil {
						b.Fatal(err)
					}
					return p
				}
				setup := compile(`function make(n){function boundary(a){a[0]=a[1];a[n-1]=a[n-2]}return function kernel(a,b){` + tc.body + `}}var kernel=make(8192);var a=new Array(8192),b=new Array(8192);for(var i=0;i<8192;i++){a[i]=1;b[i]=1}`)
				call := compile(`kernel(a,b)`)
				b.ReportAllocs()
				b.ResetTimer()
				b.StopTimer()
				for i := 0; i < b.N; i++ {
					r := New(Config{JIT: enabled})
					if _, err := r.Run(setup); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					v, err := r.Run(call)
					b.StopTimer()
					native := r.jit != nil && r.jit.entries != 0 && r.jit.guards == 0
					r.Close()
					r.ReleaseClosed()
					want := float64(1)
					if tc.name == "vector" {
						want = 3
					}
					if err != nil || v.Number() != want || enabled && !native {
						b.Fatalf("first call %v error %v native %v", v, err, native)
					}
				}
			})
		}
	}
}

func BenchmarkJITNumericKernels(b *testing.B) {
	for _, tc := range jitNumericKernelCases() {
		for _, keyword := range []string{"var", "let"} {
			source := "function kernel(n) { " + strings.ReplaceAll(tc.body, "VAR", keyword) + " }"
			compile := func(source string) *bytecode.Function {
				ast, err := parser.Parse(source, parser.Options{})
				if err != nil {
					b.Fatal(err)
				}
				p, err := compiler.Compile(ast, compiler.Options{})
				if err != nil {
					b.Fatal(err)
				}
				return p
			}
			definition, call := compile(source), compile("kernel(10000)")
			baseline := New(Config{})
			if _, err := baseline.Run(definition); err != nil {
				b.Fatal(err)
			}
			want, err := baseline.Run(call)
			baseline.Close()
			baseline.ReleaseClosed()
			if err != nil || !want.IsNumber() {
				b.Fatalf("baseline = %v, %v", want, err)
			}
			for _, mode := range []string{"existing", "ssa"} {
				if mode == "ssa" && !jitSSABackend {
					continue
				}
				enabled := mode != "existing"
				b.Run(tc.name+"/"+keyword+"/"+mode, func(b *testing.B) {
					r := New(Config{JIT: enabled})
					defer func() { r.Close(); r.ReleaseClosed() }()
					if _, err := r.Run(definition); err != nil {
						b.Fatal(err)
					}
					for i := 0; i < jitInvocationsForFeedback; i++ {
						v, err := r.Run(call)
						if err != nil || !jitSameValueForTest(v, want) {
							b.Fatalf("warmup = %v, %v; want %v", v, err, want)
						}
					}
					if enabled && (r.jit == nil || r.jit.entries == 0 || r.jit.guards != 0) {
						b.Fatal("benchmark did not stay in native code")
					}
					if mode == "ssa" && r.JITStats().SSAEntries == 0 {
						b.Fatal("benchmark did not run through the new pipeline")
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						v, err := r.Run(call)
						if err != nil || !jitSameValueForTest(v, want) {
							b.Fatalf("kernel = %v, %v; want %v", v, err, want)
						}
					}
					b.StopTimer()
					if enabled {
						var codeBytes int
						for _, entry := range r.jit.cache {
							codeBytes += entry.ssa.Size()
						}
						b.ReportMetric(float64(codeBytes), "native-B")
					}
				})
			}
		}
	}
}

func BenchmarkJITKernelPromotion(b *testing.B) {
	for _, tc := range jitNumericKernelCases() {
		for _, keyword := range []string{"var", "let"} {
			for _, calls := range []int{1, 2, 4, 8, 16} {
				source := "function kernel(n) { " + strings.ReplaceAll(tc.body, "VAR", keyword) + " }" +
					fmt.Sprintf("var result; for(var k=0;k<%d;k++) result=kernel(10000); result", calls)
				ast, err := parser.Parse(source, parser.Options{})
				if err != nil {
					b.Fatal(err)
				}
				p, err := compiler.Compile(ast, compiler.Options{})
				if err != nil {
					b.Fatal(err)
				}
				baseline := New(Config{})
				want, err := baseline.Run(p)
				baseline.Close()
				baseline.ReleaseClosed()
				if err != nil || !want.IsNumber() {
					b.Fatalf("baseline = %v, %v", want, err)
				}
				for _, enabled := range []bool{false, true} {
					mode := "existing"
					if enabled {
						mode = "automatic"
					}
					b.Run(fmt.Sprintf("%s/%s/calls%d/%s", tc.name, keyword, calls, mode), func(b *testing.B) {
						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							r := New(Config{JIT: enabled})
							v, err := r.Run(p)
							native := r.jit != nil && r.jit.entries != 0 && r.jit.guards == 0
							r.Close()
							r.ReleaseClosed()
							if err != nil || !jitSameValueForTest(v, want) {
								b.Fatalf("kernel = %v, %v; want %v", v, err, want)
							}
							if enabled && !native {
								b.Fatal("benchmark did not stay in native code")
							}
						}
					})
				}
			}
		}
	}
}

// A nested tree call keeps runTree's recover only while its loop may still
// promote to native code (see jitTreeRecovery): otherwise enabling the JIT
// would undo runTreeNested for every function it never runs.
func TestJITTreeRecoveryNarrow(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	fn := jitFunctionForTest(t, `function f(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }`)
	compiled := r.jitForFunction(fn, nil)
	if compiled == nil || compiled.ssa == nil {
		t.Fatal("no program")
	}
	frameWith := func(set func(cl *closure)) *frame {
		cl := &closure{fn: fn, closureCode: &closureCode{}}
		set(cl)
		return &frame{cl: cl}
	}
	for _, tc := range []struct {
		name string
		set  func(cl *closure)
		want bool
	}{
		{"not yet compiled", func(cl *closure) {}, true},
		{"compiled", func(cl *closure) { cl.setHint(compiled.hint) }, true},
		{"refused", func(cl *closure) { cl.jitRefused = true }, false},
		{"refusal cached", func(cl *closure) {
			e := &jitEntry{link: &jitLink{}}
			cl.fn = jitFunctionForTest(t, `function g() {}`)
			r.jit.remember(weak.Make(cl.fn), e)
			cl.setHint(e.hint)
		}, false},
	} {
		if got := r.jitTreeRecovery(frameWith(tc.set)); got != tc.want {
			t.Errorf("%s: recovery %v, want %v", tc.name, got, tc.want)
		}
	}
	r.jitEnabled = false
	if r.jitOn() && r.jitTreeRecovery(frameWith(func(*closure) {})) {
		t.Error("recovery with the JIT off")
	}
}

// A hint names its entry until the entry leaves the cache; a slot reused for
// another function's entry is not the hint's, however often it is reused,
// so the old hint misses rather than aliasing -- a tag had wrapped. A slot
// holding the function's own entry is the hint's.
func TestJITHintReuse(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	source := `function f(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s }`
	first := jitFunctionForTest(t, source)
	e := r.jitForFunction(first, nil)
	cl := &closure{fn: first, closureCode: &closureCode{}}
	cl.setHint(hintFor(e))
	if e == nil || r.jit.hint(cl) != e {
		t.Fatal("fresh hint misses")
	}
	drop := func(e *jitEntry) {
		t.Helper()
		for key, cached := range r.jit.cache {
			if cached == e && !r.jit.dropEntry(key, e) {
				t.Fatal("could not release")
			}
		}
	}
	old := e.hint
	drop(e)
	if r.jit.hint(cl) != nil {
		t.Fatal("hint survived its entry")
	}
	// The slot is reused for other functions' entries, again and again:
	// more times than a tag of the bits a slot leaves had.
	var keep []*bytecode.Function
	for i := range 1 << 10 {
		fn := jitFunctionForTest(t, `function g(n) { let s=0; for(let i=0;i<n;i++) s-=i; return s }`)
		keep = append(keep, fn)
		other := r.jitForFunction(fn, nil)
		if other == nil || other.hint != old {
			t.Fatalf("reuse %d: the freed slot was not reused", i)
		}
		if r.jit.hint(cl) != nil {
			t.Fatalf("reuse %d: a stale hint aliased the slot's new entry", i)
		}
		if r.jit.hint(&closure{fn: fn, closureCode: &closureCode{jitCodeFields: jitCodeFields{jitHint: cl.jitHint}}}) != other {
			t.Fatalf("reuse %d: the slot's own function's hint misses", i)
		}
		drop(other)
	}
	// The function's own entry, made again, in the slot: the hint finds it.
	again := r.jitForFunction(first, nil)
	if again == nil || again.hint != old || r.jit.hint(cl) != again {
		t.Fatal("a hint missed its function's entry in its slot")
	}
	runtime.KeepAlive(keep)
}

func TestJITStressParse(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want jitStressConfig
	}{
		{"", jitStressConfig{}},
		{"1", jitStressConfig{threshold: true}},
		{"threshold, budget=3 ,deopt=5", jitStressConfig{threshold: true, budget: 3, deopt: 5}},
		{"budget=99999", jitStressConfig{budget: 65535}},
		{"budget=x,unknown,deopt=", jitStressConfig{}},
	} {
		if got := parseJITStress(tc.spec); got != tc.want {
			t.Errorf("parseJITStress(%q) = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
}

// jitStressCorpus is a set of programs over what the JIT compiles: numbers,
// arrays, bitwise operations, properties, globals, calls, strings, and the
// guards, callbacks and exceptions at their edges. Each ends with a string
// that describes everything it did, so that runs can be compared.
var jitStressCorpus = []string{
	`function f(n){let s=0;for(let i=0;i<n;i++)s+=i*0.5;return s}String([f(100),f(1000),f(0)])`,
	`function f(n){var x=0.1;for(var i=0;i<n;i++)x=3.7*x*(1-x);return x}String(f(500))`,
	`function f(a){for(let i=1;i<a.length-1;i++)a[i]=(a[i-1]+a[i]+a[i+1])/3;return a}let a=[];for(let i=0;i<64;i++)a[i]=i%7;f(a).join()`,
	`function f(a,n){for(let i=0;i<n;i++)a[i]=i*i;return a.length}let a=[];String([f(a,40),a.join()])`,
	`function f(n){let h=0;for(let i=0;i<n;i++){h=(h<<5)-h+i|0;h^=h>>>13}return h}String([f(1000),f(1)])`,
	`function f(o,n){for(let i=0;i<n;i++)o.x=(o.x*31+i)|0;return o.x}let o={x:1};String([f(o,500),o.x])`,
	`var scale=3;function f(a){let s=0;for(let i=0;i<a.length;i++)s+=a[i]*scale;return s}String(f([1,2,3,4,5,6,7,8]))`,
	`function g(x){return x*2+1}function f(n){let s=0;for(let i=0;i<n;i++)s+=g(i);return s}String(f(300))`,
	`function f(s){let h=0;for(let i=0;i<s.length;i++)h=(h*33+s.charCodeAt(i))|0;return h}String(f('the quick brown fox'.repeat(5)))`,
	`function f(a){let s=0;for(let i=0;i<a.length;i++)s+=a[i];return s}String([f([1,2,3]),f([1,'2',3]),f([1,,3]),f([1.5,{valueOf(){return 4}},3])])`,
	`let log=[];let o={get x(){log.push('g');return 2}};function f(o,a){let s=0;for(let i=0;i<a.length;i++)s=(s+o.x+a[i])|0;return s}String([f(o,[1,2,3,4,5]),log.join('')])`,
	`function boom(s){throw new RangeError('at '+s)}function f(a){let s=0;for(let i=0;i<10;i++){if(i===7)boom(s);s+=a[i]}return s}let r;try{f([1,2,3,4,5,6,7,8,9,10])}catch(e){r=e.name+':'+e.message}r`,
	`function f(n){let s=0;for(let i=0;i<n;i++){s+=i;if(s>1e3)s-=0.25}return s}String([f(200),Object.is(f(0),0)])`,
	`function f(n){let x=-0;for(let i=0;i<n;i++)x=x*-1;return x}String([Object.is(f(1),-0),Object.is(f(2),0)])`,
	`function f(a){for(let i=0;i<a.length;i++)a[i]=a[i]|0;return a}f([1.7,-2.5,NaN,Infinity,2**33+0.5,-0]).join()`,
	`function f(a){let s=0;for(let i=0;i<a.length;i++)s+=a[i];return s}let r=[String(f([1,2,3]))];try{f([1,2,3n])}catch(e){r.push(e.name+':'+e.message)}r.join()`,
}

// Every program in the corpus gives the same answer with the JIT off and
// under each stress setting, and the settings reach what they are for: an
// exit after a handful of instructions, and fallback from wherever native
// code had got to.
func TestJITStressDifferential(t *testing.T) {
	configs := []jitStressConfig{
		{threshold: true},
		{threshold: true, budget: 1},
		{threshold: true, budget: 1, deopt: 2},
		{threshold: true, budget: 7, deopt: 3},
		{threshold: true, budget: 3, deopt: 11},
	}
	run := func(src string, jit bool, c jitStressConfig) (string, JITStats) {
		r := jitRuntimeForTest(t, Config{JIT: jit})
		r.jitStress = c
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			return "error: " + err.Error(), r.JITStats()
		}
		str, err := r.ToString(v)
		if err != nil {
			return "error: " + err.Error(), r.JITStats()
		}
		return str.Go(), r.JITStats()
	}
	var total JITStats
	for i, src := range jitStressCorpus {
		want, _ := run(src, false, jitStressConfig{})
		for _, c := range configs {
			got, st := run(src, true, c)
			if got != want {
				t.Errorf("program %d under %+v: %q, want %q\n%s", i, c, got, want, src)
			}
			if c == configs[0] && st.Entries == 0 {
				t.Errorf("program %d never ran natively, so the corpus no longer tests it\n%s", i, src)
			}
			total.Entries += st.Entries
			total.Budgets += st.Budgets
			total.Interpreted += st.Interpreted
		}
	}
	if total.Entries == 0 || total.Budgets == 0 || total.Interpreted == 0 {
		t.Fatalf("stress never reached its paths: %+v", total)
	}
}

// BenchmarkJITHostRoundTrip prices a helper call as the VM makes one today:
// the same loop with and without one host operation per iteration, a call to
// a Go function or a remainder (which the JIT hands to Go). The difference,
// per iteration, is what a return to Go costs with publication and
// re-encoding, which the profitability model weighs.
// BenchmarkJITCompile is what compiling a function costs, as the VM does at
// first use -- lowering, building, optimizing, emitting and placing the code
// -- in each pipeline: a small loop, a numeric kernel, and a loop of about
// 200 instructions, the size the compile budget names (below 50 us and
// 64 KB of transient memory).
func BenchmarkJITCompile(b *testing.B) {
	var region strings.Builder
	region.WriteString("function f(n){let a=1,b=2,c=3,d=4;for(let i=0;i<n;i++){")
	for range 8 {
		region.WriteString("a=(a+b*i)|0;b=b+c*0.5;c=(c^a)&255;d=d+a-b;")
	}
	region.WriteString("}return a+b+c+d}")
	particle := jitNumericKernelCases()[2].body
	for _, tc := range []struct{ name, src string }{
		{"sum", `function f(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}`},
		{"particle", "function f(n){" + strings.ReplaceAll(particle, "VAR", "let") + "}"},
		{"region", region.String()},
	} {
		for _, mode := range []string{"ssa"} {
			if mode == "ssa" && !jitSSABackend {
				continue
			}
			b.Run(tc.name+"/"+mode, func(b *testing.B) {
				r := New(Config{JIT: true})
				defer func() { r.Close(); r.ReleaseClosed() }()
				if _, err := r.Run(compileForTest(b, tc.src+";f(2)")); err != nil {
					b.Fatal(err)
				}
				cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
				compile := func() *jitEntry {
					if r.jit != nil {
						for key, e := range r.jit.cache {
							r.jit.dropEntry(key, e)
						}
					}
					return r.jitFor(cl)
				}
				if e := compile(); e == nil || mode == "ssa" && e.ssa == nil {
					b.Fatalf("%s did not compile in the %s pipeline", tc.name, mode)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					compile()
				}
				b.ReportMetric(float64(len(cl.fn.Code)), "instructions")
			})
		}
	}
}

func BenchmarkJITHostRoundTrip(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"pure", `s=(s+i*3)|0`},
		{"remainder", `s=(s+i%3)|0`},
		{"go-call", `s=(s+g(i))|0`},
	} {
		for _, mode := range []string{"existing", "ssa"} {
			if mode == "ssa" && !jitSSABackend {
				continue
			}
			enabled := mode != "existing"
			b.Run(tc.name+"/"+mode, func(b *testing.B) {
				r := New(Config{JIT: enabled})
				defer func() { r.Close(); r.ReleaseClosed() }()
				r.jitCallThreshold, r.jitICThreshold = 1, 1
				r.global.setOwnRaw(r.atoms.intern("g"), r.NewFunction("g", 1,
					func(_ *Runtime, _ Value, args []Value) (Value, error) { return args[0], nil }), propDefault)
				if _, err := r.Run(compileForTest(b, `function f(n){let s=0;for(let i=0;i<n;i++)`+tc.body+`;return s}`)); err != nil {
					b.Fatal(err)
				}
				call := compileForTest(b, `f(1000)`)
				want, err := r.Run(call)
				if err != nil {
					b.Fatal(err)
				}
				if enabled && (r.jit == nil || r.jit.entries == 0) {
					b.Fatal("did not run natively")
				}
				if mode == "ssa" && r.JITStats().SSAEntries == 0 {
					b.Fatal("did not run through the new pipeline")
				}
				hostsBefore := r.JITStats().Hosts
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					v, err := r.Run(call)
					if err != nil || !jitSameValueForTest(v, want) {
						b.Fatalf("f = %v, %v", v, err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1000, "ns/iter")
				if enabled {
					b.ReportMetric(float64(r.JITStats().Hosts-hostsBefore)/float64(b.N)/1000, "hosts/iter")
				}
			})
		}
	}
}

// A guard that fails invalidates the code, which is compiled again, with
// the site generic, once the existing tiers have spent their budget: an
// arithmetic operation or a comparison of what are not numbers goes to Go,
// which resumes after it, and an entry whose slots were not numbers loads
// them as they are. The loop then stays native, and no guard fails again.
// A function whose speculations keep failing somewhere new -- each call
// once, before its loop -- has each failure noted, as V8's feedback widens,
// and is compiled again for them all, within its deoptimizations' limit
// (jitMaxDeopts). Each answer, and each valueOf call, is the
// interpreter's.
func TestJITSSAReoptimize(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var calls=0,v={valueOf(){calls++;return 2}};
		function scale(k,n){let s=0;for(let i=0;i<n;i++){s+=i*2;if(i==n-1)s=s+k}return s}
		function cmp(a,b,n){let s=0;for(let i=0;i<n;i++){if(a<b)s+=1;s+=a*b}return s}
		function many(a,b,c,d,e,f,n){let s=a*3;s+=b*3;s+=c*3;s+=d*3;s+=e*3;s+=f*3;for(let i=0;i<n;i++){s=(s*7+i)|0}return s}`
	rounds := []string{
		`[scale(1,300),scale(2,300),cmp(1,2,300)].join()`,
		`[scale('x',300),scale(v,300),cmp('1',2,300),cmp(v,3,300),calls].join()`,
		`[scale('y',300),scale(3,300),cmp(1,v,300),cmp(4,3,300),calls].join()`,
		`[scale('z',300),scale(v,300),cmp('5',v,300),calls].join()`,
		`[many(1,1,1,1,1,1,300),many('1',1,1,1,1,1,300),many(1,'1',1,1,1,1,300),many(1,1,'1',1,1,1,300)].join()`,
		`[many(1,1,1,'1',1,1,300),many(1,1,1,1,'1',1,300),many(1,1,1,1,1,v,300),calls].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.hint(cl)
	}
	// cmp's comparisons of objects go to Go at every iteration, which makes
	// the tree tier run it (jitSSAProfit); scale's addition goes once a call.
	for _, name := range []string{"scale", "cmp"} {
		e := entry(name)
		if e == nil || e.ssa == nil || e.reopts == 0 || e.deopts > jitMaxDeopts || e.invalid || name == "scale" && e.entrySlow ||
			e.ssaStats.entries == 0 || e.ssaStats.guards != 0 {
			t.Fatalf("%s was not compiled again to run without failing: %+v", name, e)
		}
	}
	// Its six multiplications' failures, each noted: four had been, the
	// compiles for failed speculations capped.
	if e := entry("many"); e == nil || e.ssa == nil || len(e.failed) < 6 || e.deopts > jitMaxDeopts {
		t.Fatalf("many's failures were not all noted: %+v", e)
	}
}

// A method's read is native in the new pipeline where the site's cache
// found it, on a prototype too, one or two levels up: the receiver's shape,
// its prototype and each prototype's shape are checked, and the method is
// read from its cell, so a method reassigned in place, even mid-loop, is
// the new one, and one shadowed, a prototype replaced or a method added
// between them fails a check and is read by Go. Each answer is the
// interpreter's, and only the calls go to Go, called from native code
// (ssa.GoCall): m stores, so it is not inlined (TestJITSSAInline inlines). Its store gives the receiver a
// shape after run was compiled for the one it had, so run deoptimizes and
// is compiled again for the new one (jitPropertyDeopt).
func TestJITSSAPrototypeMethods(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// m reads arguments: Go calls it, never native code (jitNativeCallee).
	setup := `function A(){this.v=1}A.prototype.m=function(){this.n=arguments.length;return this.v+1};
		function B(){this.v=2}B.prototype=Object.create(A.prototype);
		function run(o,n){let s=0;for(let i=0;i<n;i++){s+=o.m();for(let j=0;j<12;j++)s=(s*3+j)%1000003}return s}
		function swap(o,n){let s=0;for(let i=0;i<n;i++){s+=o.m();for(let j=0;j<12;j++)s=(s*3+j)%1000003;if(i==20)change()}return s}
		function change(){A.prototype.m=function(){return 1000}}
		var a=new A,b=new B,c=new A,d=new B;`
	rounds := []string{
		`''+run(a,40)`,
		`''+run(a,40)`,
		`''+run(a,40)`,
		`[run(b,40),run(a,40)].join()`,
		`A.prototype.m=function(){return 100};[run(a,40),run(b,40)].join()`,
		`a.m=function(){return 5};[run(a,40),run(c,40)].join()`,
		`Object.setPrototypeOf(c,{m(){return 9}});[run(c,40),run(d,40)].join()`,
		`B.prototype.m=function(){return 50};[run(d,40),run(new A,40)].join()`,
		`A.prototype.m=function(){return 7};[swap(new A,40),swap(new B,40),run(new A,40)].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		hosts, goCalls := r.jit.hosts, r.jit.goCalls
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 2 {
			// The third call runs compiled -- the first's read, compiled
			// before it ran, deoptimized, and the code was compiled again
			// once the second had run with its cache's feedback settled
			// (jitTierUp): a call of Go for each of its calls, and no exit,
			// for them or the reads.
			if n := r.jit.goCalls - goCalls; n != 40 || r.jit.hosts-hosts > 1 {
				t.Fatalf("run called Go %d times for 40 calls, and left native code %d times", n, r.jit.hosts-hosts)
			}
		}
	}
	// Every round ran natively: none was demoted to the tree tier.
	for _, name := range []string{"run", "swap"} {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		if e := r.jit.hint(cl); e == nil || e.ssa == nil || e.entrySlow || e.ssaStats.entries == 0 {
			t.Fatalf("%s did not stay native: %+v", name, e)
		}
	}
}

// Two strings of one length are compared natively by their bytes when both
// are flat, as String.Equals compares them: symbols made at run time, as
// EarleyBoyer's are, each its own string, find their match without
// leaving native code. A rope, which Go flattens, and a string longer than
// abi.MaxEqualUnits go to Go. Each answer is the interpreter's.
func TestJITSSAStringEquality(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function count(a,k,n){let c=0;for(let i=0;i<n;i++){const s=a[i%a.length];if(s===k)c+=1;if(s==k)c+=10;if(s!==k)c+=100}return c}
		var syms=[],u=[],long=[];for(let i=0;i<8;i++){syms.push(String.fromCharCode(0x1E9C)+'sym'+i);u.push('āĂ'+i+'\uD800')}
		const big='x'.repeat(300);long.push(big+'a',big+'b');
		var key=['ẜsym',3].join(''),ukey=['āĂ',5,'\uD800'].join('');`
	rounds := []string{
		`''+count(syms,key,200)`,
		`''+count(syms,key,200)`,
		`[count(u,ukey,200),count(syms,'ẜsym'+'7',200),count(long,'x'.repeat(300)+'b',50)].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		hosts := r.jit.hosts
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 1 && r.jit.hosts != hosts {
			t.Fatalf("count left native code %d times", r.jit.hosts-hosts)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("count")).value().Object().fn().closure
	if e := r.jit.hint(cl); e == nil || e.ssa == nil || e.entrySlow || e.ssaStats.entries == 0 {
		t.Fatalf("count did not stay native: %+v", e)
	}
}

// Native code stores any value in a property, references too, while the
// collector is not marking (abi.Encoding's WriteBarrier): links copied
// from an array, which with collection off never leaves native code; and
// a list reversed in place and two properties swapped, whose reads are
// from the cells their stores then write while what was read is still
// needed, which native code keeps first (ssa's keep.go), round the loop
// too; walk stops at 1,000 links, so that a list a lost pointer made
// circular fails rather than hangs. Each answer is the interpreter's.
func TestJITSSAReferenceStores(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function list(k){let h=null;for(let i=0;i<k;i++)h={v:i,next:h};return h}
		function walk(h){let s=0,n=h,i=1;while(n&&i<1000){s+=n.v*i;i++;n=n.next}return s}
		function rev(h){let p=null,n=h;while(n){const x=n.next;n.next=p;p=n;n=x}return p}
		function relink(a,n,tags){for(let i=0;i<n;i++){const o=a[i];o.next=a[i+1];o.tag=tags[i&3];o.v=i}return a[0]}
		function swap(o){const t=o.a;o.a=o.b;o.b=t;return t}
		var arr=[];for(let i=0;i<40;i++)arr.push({v:0,next:null,tag:''});arr.push(null);
		var pair={a:{n:1},b:'two'},tags=['s0','s1',{s:2},null];`
	rounds := []string{
		`[walk(rev(list(30))),walk(relink(arr,40,tags)),swap(pair).n].join()`,
		`[walk(rev(list(30))),walk(relink(arr,40,tags)),swap(pair),pair.a.n,arr[5].tag].join()`,
		`[walk(rev(rev(list(30)))),walk(relink(arr,20,tags)),swap(pair).n].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.hint(cl)
	}
	if e := entry("relink"); e == nil || e.ssa == nil || e.entrySlow || e.ssaStats.entries == 0 || e.ssaStats.hosts != 0 || e.ssaStats.guards != 0 {
		t.Fatalf("relink's stores left native code: %+v", e)
	}
	if e := entry("rev"); e == nil || e.ssa == nil || e.ssaStats.guards != 0 || e.ssaStats.hosts != 0 {
		t.Fatalf("rev's store to the cell it read left native code: %+v", e)
	}
}

// TestJITSSAReferenceStoresUnderGC stresses native reference stores: the
// collector runs continuously while lists are reversed and relinked in
// place, natively while it is not marking and through Go while it is.
// Nothing must be lost or freed early.
func TestJITSSAReferenceStoresUnderGC(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	src := `function list(k){let h=null;for(let i=0;i<k;i++)h={v:i,next:h,pad:[i,i]};return h}
		function walk(h){let s=0,n=h,i=1;while(n){s+=n.v*i+n.pad[1];i++;n=n.next}return s}
		function rev(h){let p=null,n=h;while(n){const x=n.next;n.next=p;n.pad=[n.v,n.v];p=n;n=x}return p}
		let ok=true,stores=0;
		for(let round=0;round<300;round++){
			const h=list(50),w=walk(h);
			const r=rev(h);stores+=50;
			ok=ok&&walk(rev(r))===w&&walk(r)!==w;
		}
		ok`
	v, err := r.Run(compileForTest(t, src))
	if err != nil || !v.IsBool() || !v.Truthy() {
		t.Fatalf("= %v, %v", v, err)
	}
	if st := r.JITStats(); st.SSAEntries == 0 {
		t.Fatalf("the reversals did not run natively: %+v", st)
	}
}

// While the collector marks, native code stores a pointer word with Go's
// write barrier (jit's storeRef), as V8's code calls its write barrier, and
// goes on: with the write-barrier flag native code reads
// (jitEncoding.WriteBarrier) pointed at a byte that is set, relink makes
// every store natively, none a call of Go, nor leaves native code. The
// stores fill every register native code may keep with junk first
// (jit.StoreRefClobber), so that one the code kept across a store and did
// not save gives another answer. Its reference stores had each been a call
// of Go (ssa.GoStore), and before that had each left.
func TestJITSSAReferenceStoresWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	calls := 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	setup := `function relink(a,n){let s=0;for(let i=0;i<n;i++){const o=a[i];o.next=a[i+1];o.v=i;s+=i*o.v}return s}
		function count(a,n){for(let i=0;i<n;i++)a[i].v=i*2;return a[n-1].v}
		function total(){let s=0;for(let n=arr[0];n;n=n.next)s+=n.v;return s}
		var arr=[];for(let i=0;i<40;i++)arr.push({v:0,next:null});arr.push(null);`
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	before := 0
	for i := 0; i < 6; i++ {
		before = calls
		v, err := r.Run(compileForTest(t, `relink(arr,40)+''`))
		if err != nil || v.String().Go() != "20540" {
			t.Fatalf("round %d: relink %v, %v", i, v, err)
		}
		if v, err := r.Run(compileForTest(t, `total()+':'+count(arr,40)`)); err != nil || v.String().Go() != "780:78" {
			t.Fatalf("round %d: %v, %v", i, v, err)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.hint(cl)
	}
	if e := entry("relink"); e == nil || e.ssa == nil || e.ssaStats.entries == 0 || e.ssaStats.hosts != 0 || calls != before {
		t.Fatalf("relink's stores while the collector marked: %d calls of Go in the last round, %+v", calls-before, e)
	}
	if e := entry("count"); e == nil || e.ssa == nil || e.ssaStats.entries == 0 || e.ssaStats.hosts != 0 {
		t.Fatalf("count's numbers went to Go: %+v", e)
	}
}

// While the collector marks, a keep (ssa's OpKeep) is written by Go, called
// from native code, as the stores are: rev keeps n.next's old value, x,
// across n.next=p. A keep that wrote nothing then left x read from n.next
// itself, safe while every pointer store left native code as the collector
// marked; with Go making the store meanwhile (ssa.GoStore), the array
// literal's exit after it read x's pointer word from the cell overwritten,
// and rev made a cycle (a test that ran forever). With the write-barrier
// flag set throughout, every reversal is the interpreter's.
func TestJITSSAKeepsWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	calls := 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	setup := `function list(k){let h=null;for(let i=0;i<k;i++)h={v:i,next:h,pad:[i,i]};return h}
		function walk(h){let s=0,n=h,i=1,c=0;while(n){if(++c>1000)return -1;s+=n.v*i+n.pad[1];i++;n=n.next}return s}
		function rev(h){let p=null,n=h,c=0;while(n){if(++c>1000)return null;const x=n.next;n.next=p;n.pad=[n.v,n.v];p=n;n=x}return p}
		function round(){const h=list(30),w=walk(h),r=rev(h);return r===null?'cycle':walk(r)+'/'+walk(rev(r))+'/'+w}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 40; i++ {
		wv, err := want.Run(compileForTest(t, `round()`))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, `round()`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("rev")).value().Object().fn().closure
	if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || calls == 0 {
		t.Fatalf("rev did not run natively, calling Go (%d calls): %+v", calls, e)
	}
}

// A construction with a receiver from a pool and more operands than a
// call of Go passes (abi.MaxGoArgs) is made natively while the collector
// marks, as any is, and leaves native code no more: it had left then
// (abi.Context's ExitMarking, gone), every iteration of a long marking,
// which the profit check (jitSSAProfit) was not to judge -- code judged on
// such exits had Go stop entering it for good (entrySlow), as CI met at
// two CPUs.
func TestJITSSAWideConstructionsWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	setup := `function G(a,b,c,d,e,f,h,k,l,m,o,p,q,u,w,x){let t=a;for(let j=0;j<2;j++)t=(t+b+c+d+e+f+h+k+l+m+o+p+q+u+w+x)|0;this.t=t}
		function lit(n){let s=0;for(let i=0;i<n;i++){s=(s+new G(i,1,2,3,4,5,6,7,0,0,0,0,0,0,0,0).t)|0}return s}`
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	hosts := uint64(0)
	for i := 0; i < 200; i++ {
		if i == 190 {
			hosts = r.jit.hosts
		}
		v, err := r.Run(compileForTest(t, `lit(50)`))
		if err != nil || v.Number() != 4025 {
			t.Fatalf("round %d: %v, %v", i, v, err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("lit")).value().Object().fn().closure
	if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || e.entrySlow || len(e.nativeCalls) == 0 || r.jit.hosts != hosts {
		t.Fatalf("lit's constructions while the collector marked: %d exits in its last rounds: %+v", r.jit.hosts-hosts, e)
	}
}

// jitMarkingForTest has code compiled for the rest of the test see the
// collector's write-barrier flag set (jitEncoding.WriteBarrier), as while
// it marks: every store that changes a pointer word goes through Go's
// write barrier, and every exit's records Go applies. The flag is
// returned, for a test to clear and set again while code runs.
// jitClobberStoresForTest has the code the test compiles store pointer
// words while the collector marks through jit.StoreRefClobber, which fills
// every register native code may keep with junk first.
func jitClobberStoresForTest(t *testing.T) {
	old := jitEncoding.StoreRef
	jitEncoding.StoreRef = jit.StoreRefClobber()
	t.Cleanup(func() { jitEncoding.StoreRef = old })
}

func jitMarkingForTest(t *testing.T) *uint8 {
	marking := new(uint8)
	*marking = 1
	old := jitEncoding.WriteBarrier
	jitEncoding.WriteBarrier = uint64(uintptr(unsafe.Pointer(marking)))
	t.Cleanup(func() {
		jitEncoding.WriteBarrier = old
		runtime.KeepAlive(marking)
	})
	return marking
}

// A small method is inlined at a call the new pipeline has seen call it
// (jitCallSeen, ssa.InlineSite): the loop then never leaves native code, a
// missing argument is undefined, and the receiver is the call's. A method
// replaced fails the call's check, Go makes the call, and the call is no
// longer inlined but made natively, to either function, as V8's feedback
// goes polymorphic (jitInlinedCalls). Each answer is the interpreter's.
func TestJITSSAInline(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function T(s){this.state=s;this.link=null}
		T.prototype.isHeld=function(){return (this.state&4)!=0||this.state==2};
		T.prototype.plus=function(a,b){return this.state+a+(b===undefined?100:b)};
		function count(ts,n){let c=0;for(let i=0;i<n;i++){const t=ts[i%ts.length];if(t.isHeld())c+=1;c+=t.plus(i,2)}return c}
		function missing(t,n){let c=0;for(let i=0;i<n;i++)c+=t.plus(i);return c}
		var ts=[new T(0),new T(4),new T(2),new T(5)],one=new T(7);`
	rounds := []string{
		`[count(ts,300),missing(one,50)].join()`,
		`[count(ts,300),missing(one,50)].join()`,
		`[count(ts,300),missing(one,50)].join()`,
		`T.prototype.isHeld=function(){return this.state>3};[count(ts,300),missing(one,50)].join()`,
		`[count(ts,300),missing(one,50)].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.hint(cl)
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		hosts := r.jit.hosts
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 2 {
			// Both calls inlined: nothing leaves native code.
			if n := r.jit.hosts - hosts; n != 0 {
				t.Fatalf("round %d: %d exits to Go; count %+v", i, n, entry("count"))
			}
		}
	}
	// isHeld, replaced, is called natively instead; plus stays inlined.
	e := entry("count")
	if e == nil || e.ssa == nil || len(e.inlines) != 2 || len(e.notInline) != 1 || e.notInline[0] != e.inlines[0].pc ||
		!slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return x.pc == e.inlines[0].pc }) || len(e.failed) != 0 {
		t.Fatalf("count was not inlined, then compiled again calling isHeld natively: %+v", e)
	}
}

// A callee that writes properties is inlined too, and an exit inside it
// makes its frame, as V8's deoptimizer does (ssa.InlineState): what it did
// before the exit is not done again, Go does what it left for, and native
// code goes on after the call. Here bump's multiplication fails its guard
// once x is a string, after bump has counted the call in n, with operands
// and a receiver to put in the frame and t, a reference, live in it; in a
// callee mid calls natively too, two levels down. Run with the collector
// marking, the exit leaves its records to Go, which applies them before it
// makes the frame: code compiled while it marks from the start; from the
// fourth round, only code compiled again, which a native call from older
// code may reach, whose records Go applies too. Each answer, n's count
// among them, is the interpreter's.
func TestJITSSAInlineEffects(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []int{-1, 0, 3} {
		t.Run(fmt.Sprint("marking=", marking), func(t *testing.T) {
			setup := `function C(){this.n=0;this.t={x:3}}
				C.prototype.bump=function(k){const t=this.t;this.n=this.n+1;return t.x*k+this.n};
				function run(o,n){let s=0;for(let i=0;i<n;i++)s=(s+o.bump(i))|0;return s}
				function mid(o,i){return o.bump(i)+o.bump(1)}
				function deep(o,n){let s=0;for(let i=0;i<n;i++)s=(s+mid(o,i))|0;return s}
				var a=new C,b=new C;`
			rounds := []string{
				`[run(a,200),deep(b,100),a.n,b.n].join()`,
				`[run(a,200),deep(b,100),a.n,b.n].join()`,
				`[run(a,200),deep(b,100),a.n,b.n].join()`,
				`a.t.x='5';b.t.x='7';[run(a,200),deep(b,100),a.n,b.n].join()`,
				`[run(a,200),deep(b,100),a.n,b.n].join()`,
			}
			want := New(Config{})
			defer func() { want.Close(); want.ReleaseClosed() }()
			r := jitRuntimeForTest(t, Config{JIT: true})
			for _, rt := range []*Runtime{want, r} {
				if _, err := rt.Run(compileForTest(t, setup)); err != nil {
					t.Fatal(err)
				}
			}
			entry := func(name string) *jitEntry {
				cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
				return r.jit.hint(cl)
			}
			if marking == 0 {
				jitMarkingForTest(t)
			}
			for i, src := range rounds {
				wv, err := want.Run(compileForTest(t, src))
				if err != nil {
					t.Fatal(err)
				}
				var hosts uint64
				if i == 2 {
					runtime.GC()
					defer debug.SetGCPercent(debug.SetGCPercent(-1))
					hosts = entry("run").ssaStats.hosts
				}
				if i == 3 && marking == 3 {
					jitMarkingForTest(t)
				}
				unwound := r.jit.unwound
				gv, err := r.Run(compileForTest(t, src))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := gv.String().Go(), wv.String().Go(); got != want {
					t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
				}
				switch i {
				case 2:
					// bump inlined, stores and all: run never leaves. (While the
					// collector marks, its reference stores leave.)
					if e := entry("run"); len(e.inlines) == 0 || marking != 0 && e.ssaStats.hosts != hosts {
						t.Fatalf("run left native code %d times; inlines %d", e.ssaStats.hosts-hosts, len(e.inlines))
					}
				case 3:
					if r.jit.unwound == unwound {
						t.Fatal("no exit inside an inlined callee made its frame")
					}
				}
			}
		})
	}
}

// A callee's calls are inlined in it, as V8 inlines them, to three levels:
// run inlines outer, which inlines hold, which inlines mark, each of them
// reading a global or writing a property; once its code has them all, run
// never leaves. An exit inside the innermost makes the three frames, which
// Go finishes, each with what the one it called returned.
func TestJITSSANestedInline(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var HELD=4;
		function T(){this.state=0;this.count=0}
		T.prototype.mark=function(){this.state=this.state|HELD;return this.state};
		T.prototype.hold=function(){this.count=this.count+1;return this.mark()+1};
		T.prototype.outer=function(k){return this.hold()*2+k};
		function run(o,n){let s=0;for(let i=0;i<n;i++){o.state=i&3;s=(s+o.outer(i))|0}return s}
		var a=new T;`
	rounds := []string{
		`[run(a,300),a.state,a.count].join()`,
		`[run(a,300),a.state,a.count].join()`,
		`[run(a,300),a.state,a.count].join()`,
		`[run(a,300),a.state,a.count].join()`,
		`HELD='3';[run(a,300),a.state,a.count].join()`,
		`HELD=8;[run(a,300),a.state,a.count].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	method := func(name string) *closure {
		proto := r.global.getOwn(r.atoms.intern("T")).value().Object().getOwn(r.atoms.intern("prototype")).value().Object()
		return proto.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		e := r.jit.hint(cl)
		if e != nil {
			hosts = e.ssaStats.hosts
		}
		unwound := r.jit.unwound
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		switch i {
		case 3:
			e = r.jit.hint(cl)
			for _, name := range []string{"outer", "hold", "mark"} {
				if e == nil || !slices.Contains(e.ssaInlined, method(name)) {
					t.Fatalf("run's code does not inline %s", name)
				}
			}
			if e.ssaStats.hosts != hosts {
				t.Fatalf("run left native code %d times", e.ssaStats.hosts-hosts)
			}
		case 4:
			if r.jit.unwound-unwound < 3 {
				t.Fatalf("an exit inside mark made %d frames, not three", r.jit.unwound-unwound)
			}
		case 5:
			// Left too often, outer is called natively instead, not by Go.
			if e := r.jit.hint(cl); e == nil || !slices.Contains(e.callSites, jitCallNative) || slices.Contains(e.callSites, jitCallDone) {
				t.Fatalf("run's call to outer: %v", e.callSites)
			}
		}
	}
}

// A construction of a plain function, `new V(...)`, is made natively: its
// receiver comes from the site's pool (abi.ObjectPool), made as the VM
// makes one, and the constructor is called natively with it, or inlined
// with it, V's here, as V8 inlines a constructor that returns nothing; native code
// leaves only for the pool to be filled again, once in abi.PoolSize at
// first, then, the pool given more each time it runs out, once in
// abi.PoolCapacity. A
// result that is an object is the construction's, any other its receiver,
// natively and when the constructor leaves native code (L's String every
// fiftieth); a construction after the function's prototype changed takes
// the new one, the pool made again for it.
func TestJITSSANativeConstruct(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function V(x,y){this.x=x;this.y=y}
		V.prototype.len=function(){return this.x+this.y};
		function R(x){this.x=x;return {boxed:x}}
		function P(x){this.x=x;return 5}
		function L(x){this.x=x;if(x%50===0)this.s=String(x)}
		function run(n){let s=0,v;for(let i=0;i<n;i++){v=new V(i,1);s=(s+v.x+v.y)|0}return [s,v.len()].join()}
		function runR(n){let s=0;for(let i=0;i<n;i++)s=(s+new R(i).boxed)|0;return s}
		function runP(n){let s=0;for(let i=0;i<n;i++)s=(s+new P(i).x)|0;return s}
		function runL(n){let s=0;for(let i=0;i<n;i++){const l=new L(i);s=(s+l.x+(l.s===undefined?0:1))|0}return s}
		function last(n){let v;for(let i=0;i<n;i++)v=new V(i,2);return v}
		function first(n){let f;for(let i=0;i<n;i++){const v=new V(i,3);if(i===0)f=v}return f}`
	src := `[run(400),runR(300),runP(300),runL(300),Object.getPrototypeOf(last(100))===V.prototype,
		Object.getPrototypeOf(first(100))===V.prototype].join()`
	rounds := []string{src, src, src, src, src,
		`V.prototype={len(){return 7}};` + src,
		src, src}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	entry := func(name string) *jitEntry {
		return r.jit.hint(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure)
	}
	checked := false
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts, entries uint64
		if e := entry("run"); e != nil {
			hosts, entries = e.ssaStats.hosts, e.ssaStats.entries
		}
		reoptimized := r.jit.reoptimized
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if (i == 3 || i == 4) && r.jit.reoptimized == reoptimized {
			// A round no code was compiled again in, which counts afresh.
			e := entry("run")
			if e == nil || !slices.ContainsFunc(slices.Concat(e.nativeCalls, e.inlines), func(x jitInline) bool { return x.pool != nil }) {
				t.Fatal("run does not construct natively")
			}
			// Entered again after each construction Go makes, its pool
			// filled again, as full as it may be.
			if e.entrySlow || e.ssaStats.entries == entries {
				t.Fatalf("round %d: run's code is entered %d times for 400 constructions", i, e.ssaStats.entries-entries)
			}
			if left := e.ssaStats.hosts - hosts; left > 400/abi.PoolCapacity+3 {
				t.Fatalf("round %d: run left native code %d times for 400 constructions", i, left)
			}
			checked = true
		}
		if i == len(rounds)-1 {
			proto := r.global.getOwn(r.atoms.intern("V")).value().Object().getOwnVisible(atomPrototype).value().Object()
			for _, x := range slices.Concat(entry("run").nativeCalls, entry("run").inlines) {
				if x.pool != nil && x.pool.Proto != unsafe.Pointer(proto) {
					t.Fatal("run's pool was not made again for V's new prototype")
				}
			}
		}
	}
	if !checked {
		t.Fatal("code was compiled again in every round")
	}
}

// `new Array()` is made natively from its site's pool: a fresh empty array
// each time, of the realm's prototype, as the built-in makes one, never the
// last one made there, which the code holds still (the site's result cell,
// written again by each, keeps it: mayOwnCell); native code leaves only for
// the pool to be filled again, not to read a method of an array it made,
// which has the layout property caches compare arrays' with.
func TestJITSSANativeArrayConstruct(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function arrays(n){let s=0,prev=null,same=0,a;for(let i=0;i<n;i++){a=new Array();a.push(i);if(a===prev)same++;s+=a.length+a[0]+(prev===null?0:1);prev=a}
			return [s,same,Array.isArray(a),Object.getPrototypeOf(a)===Array.prototype,a.length].join()}`
	src := `arrays(400)`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	cl := r.global.getOwn(r.atoms.intern("arrays")).value().Object().fn().closure
	checked := false
	for i := range 6 {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts, entries uint64
		if e := r.jit.hint(cl); e != nil {
			hosts, entries = e.ssaStats.hosts, e.ssaStats.entries
		}
		reoptimized := r.jit.reoptimized
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i >= 3 && r.jit.reoptimized == reoptimized {
			e := r.jit.hint(cl)
			if e == nil || !slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return x.pool != nil && x.cl == nil }) {
				t.Fatal("arrays does not make its arrays natively")
			}
			if e.entrySlow || e.ssaStats.entries == entries || e.ssaStats.hosts-hosts > 400/abi.PoolSize+6 {
				t.Fatalf("round %d: arrays entered %d times, left %d for 400 arrays", i, e.ssaStats.entries-entries, e.ssaStats.hosts-hosts)
			}
			checked = true
		}
	}
	if !checked {
		t.Fatal("code was compiled again in every round")
	}
}

// A call through Function.prototype.call, f.call(this, ...), calls f
// natively, its first argument the receiver, as a parent constructor is
// called from its child's (Sub's) -- which is itself made natively; one of
// another function, or with a receiver f must coerce (add's write to the
// number's wrapper is seen), is Go's.
func TestJITSSANativeCallThroughCall(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function add(a,b){this.j=a;return (this.k|0)+this.j+b}
		function other(a,b){return this.k*a-b}
		function viaRun(o,n,f){let t=0;for(let i=0;i<n;i++)t=(t+f.call(o,i,1))|0;return t}
		function opt(a,b){return b===undefined?a+this.k:a+b}
		function viaOne(o,n){let t=0;for(let i=0;i<n;i++)t=(t+opt.call(o,i))|0;return t}
		function Base(s){this.s=s}
		function Sub(v,s){Sub.parent.call(this,s);this.v=v}
		Sub.parent=Base;
		function subs(n){let t=0;for(let i=0;i<n;i++){const o=new Sub(i,2);t=(t+o.v+o.s)|0}return t}
		var o={k:3};`
	src := `[viaRun(o,300,add),subs(300),viaOne(o,300)].join()`
	rounds := []string{src, src, src, src, src,
		`[viaRun(o,300,other),viaRun(5,300,add),subs(300)].join()`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	entry := func(name string) *jitEntry {
		return r.jit.cache[weak.Make(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn)]
	}
	checked := false
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts, entries uint64
		if e := entry("viaRun"); e != nil {
			hosts, entries = e.ssaStats.hosts, e.ssaStats.entries
		}
		reoptimized := r.jit.reoptimized
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i >= 3 && i <= 4 && r.jit.reoptimized == reoptimized {
			e := entry("viaRun")
			if e == nil || !slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return x.via }) {
				t.Fatal("viaRun does not call add natively through call")
			}
			if e.ssaStats.entries == entries || e.ssaStats.hosts != hosts {
				t.Fatalf("round %d: viaRun entered %d times, left %d", i, e.ssaStats.entries-entries, e.ssaStats.hosts-hosts)
			}
			if s := entry("Sub"); s == nil || !slices.ContainsFunc(s.nativeCalls, func(x jitInline) bool { return x.via }) {
				t.Fatal("Sub does not call its parent natively through call")
			}
			checked = true
		}
	}
	if !checked {
		t.Fatal("code was compiled again in every round")
	}
}

// a.pop() pops natively where Array.prototype.pop's fast path does: the
// last element, a number or a reference, the length one less. An element
// read before from the cell pop clears, x, is kept across it (mayElemCell):
// x === y. A hole last, or a length not writable, is Go's.
func TestJITSSANativePop(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var o1={v:1},o2={v:2};
		function mk(n){const a=[];for(let i=0;i<n;i++)a.push(i%3===0?o1:i%3===1?o2:i);return a}
		function drain(a){let t=0,last=null;while(a.length>0){const x=a[a.length-1];const y=a.pop();
			t=(t+(x===y?1:0)+(y===o1?10:y===o2?20:(y|0)))|0;last=x}return [t,last===o1].join()}
		function tryDrain(a){try{return drain(a)}catch(e){return e.constructor.name+a.length}}`
	src := `drain(mk(300))`
	rounds := []string{src, src, src, src, src,
		`{const a=mk(10);a.length=12;drain(a)}`,
		`{const a=mk(10);Object.defineProperty(a,'length',{writable:false});tryDrain(a)}`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	cl := r.global.getOwn(r.atoms.intern("drain")).value().Object().fn().closure
	checked := false
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts, entries uint64
		if e := r.jit.hint(cl); e != nil {
			hosts, entries = e.ssaStats.hosts, e.ssaStats.entries
		}
		reoptimized := r.jit.reoptimized
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i >= 3 && i <= 4 && r.jit.reoptimized == reoptimized {
			e := r.jit.hint(cl)
			if e == nil || !slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return x.pop }) {
				t.Fatal("drain does not pop natively")
			}
			// It leaves after its loop, for the array and join of its
			// answer: a few times, not once a pop.
			if e.entrySlow || e.ssaStats.entries == entries || e.ssaStats.hosts-hosts > 4 {
				t.Fatalf("round %d: drain entered %d times, left %d for 300 pops", i, e.ssaStats.entries-entries, e.ssaStats.hosts-hosts)
			}
			checked = true
		}
	}
	if !checked {
		t.Fatal("code was compiled again in every round")
	}
	// The cells it popped hold nothing, as Go's pop leaves them: no object
	// kept alive past its array's length.
	if _, err := r.Run(compileForTest(t, `var kept=mk(300);drain(kept)`)); err != nil {
		t.Fatal(err)
	}
	a := r.global.getOwn(r.atoms.intern("kept")).value().Object()
	if len(a.elemsOf()) != 0 || cap(a.elemsOf()) < 300 {
		t.Fatalf("kept: length %d, capacity %d", len(a.elemsOf()), cap(a.elemsOf()))
	}
	for i, v := range a.elemsOf()[:300] {
		if !v.IsUndefined() || v.ref != nil {
			t.Fatalf("kept[%d] still holds %#x, %p after its pop", i, math.Float64bits(v.num), v.ref)
		}
	}
}

// a.push(v) appends natively where Array.prototype.push's fast path does:
// numbers and references, the length its result; native code leaves only
// for the array to grow. One whose array or prototypes it may not -- a
// setter for an index on Array.prototype, an array that is not
// extensible, or whose length is not writable -- is Go's.
func TestJITSSANativePush(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Each case its own copy of fill, compiled first: what one leaves to
	// Go must not have another demoted.
	const fill = `function(a,n){let t=0;for(let i=0;i<n;i++)t=(t+a.push(i&1?o:i))|0;return t}`
	setup := `var o={k:1},hits=0;
		function show(a){let t=0;for(let i=0;i<a.length;i++)t=(t+(a[i]===o?1000:a[i]))|0;return [t,a.length].join()}
		var plain=` + fill + `, setter=` + fill + `, stopped=` + fill + `, fixed=` + fill + `;`
	cases := []struct{ name, fn, src string }{
		{"plain", "plain", `{const a=[];[plain(a,300),show(a)].join()}`},
		{"not extensible", "stopped", `{const a=[];Object.preventExtensions(a);try{stopped(a,10)}catch(e){e.constructor.name+a.length}}`},
		{"length not writable", "fixed", `{const a=[];Object.defineProperty(a,'length',{writable:false});try{fixed(a,10)}catch(e){e.constructor.name+a.length}}`},
		// Last: once a prototype has had an index, its fast path, the VM's
		// and native code's, is not taken again.
		{"a setter for an index on Object.prototype", "setter",
			`Object.defineProperty(Object.prototype,'250',{set(v){hits++},configurable:true});` +
				`try{const a=[];[setter(a,300),show(a),hits].join()}finally{delete Object.prototype['250']}`},
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	run := func(name, src string) {
		t.Helper()
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%s: got %s, interpreter %s", name, got, want)
		}
	}
	for _, c := range cases {
		warm := `{const a=[];[` + c.fn + `(a,300),show(a)].join()}`
		// Until the push's read's cache has settled, which the code is
		// compiled again on (jitTierUp).
		for range 8 {
			run(c.name, warm)
		}
		e := r.jit.hint(r.global.getOwn(r.atoms.intern(c.fn)).value().Object().fn().closure)
		if e == nil || !slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return x.push }) {
			t.Fatalf("%s: %s does not push natively", c.name, c.fn)
		}
		hosts, entries, reoptimized := e.ssaStats.hosts, e.ssaStats.entries, r.jit.reoptimized
		run(c.name, c.src)
		if c.fn == "plain" && r.jit.reoptimized == reoptimized {
			// The array grows a doubling at a time: a dozen times for 300.
			if e.entrySlow || e.ssaStats.entries == entries || e.ssaStats.hosts-hosts > 16 {
				t.Fatalf("plain entered %d times, left %d for 300 pushes", e.ssaStats.entries-entries, e.ssaStats.hosts-hosts)
			}
		}
	}
}

// a[i] = n at an array's length appends natively where Go's jitStoreElem
// would append, as a push does (ssa.Func's ArrayProtos): native code
// leaves only for the array to grow. One whose array or prototypes it may
// not -- a setter for an index on Object.prototype, an array that is not
// extensible, or whose length is not writable, of another prototype, a
// write past the length -- is Go's, and answers as the interpreter does.
func TestJITSSANativeAppend(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Each case its own copy of fill, compiled first: what one leaves to
	// Go must not have another demoted.
	const fill = `function(a,n,d){let t=0;for(let i=0;i<n;i++){a[a.length+d]=i;t=(t+a.length)|0}return t}`
	const strictFill = `function(a,n,d){'use strict';let t=0;for(let i=0;i<n;i++){a[a.length+d]=i;t=(t+a.length)|0}return t}`
	setup := `var hits=0,P=[];
		function show(a){let t=0;for(let i=0;i<a.length;i++)t=(t+(a[i]===undefined?-1:a[i]))|0;return [t,a.length,Object.keys(a).length,hits].join()}
		var plain=` + fill + `, setter=` + fill + `, stopped=` + fill + `, fixed=` + fill + `, other=` + fill + `, gap=` + fill +
		`, strict=` + strictFill + `;`
	cases := []struct{ name, fn, src string }{
		{"plain", "plain", `{const a=[];[plain(a,300,0),show(a)].join()}`},
		{"not extensible", "stopped", `{const a=[1];Object.preventExtensions(a);[stopped(a,10,0),show(a)].join()}`},
		{"not extensible, strict", "strict", `{const a=[1];Object.preventExtensions(a);try{strict(a,10,0)}catch(e){e.constructor.name+a.length}}`},
		{"length not writable", "fixed", `{const a=[1];Object.defineProperty(a,'length',{writable:false});[fixed(a,10,0),show(a)].join()}`},
		{"another prototype", "other", `{const a=Object.setPrototypeOf([],P);[other(a,300,0),show(a)].join()}`},
		{"past the length", "gap", `{const a=[];[gap(a,300,1),show(a)].join()}`},
		// Last: once a prototype has had an index, its fast path, the VM's
		// and native code's, is not taken again.
		{"a setter for an index on Object.prototype", "setter",
			`Object.defineProperty(Object.prototype,'250',{set(v){hits++},configurable:true});` +
				`try{const a=[];[setter(a,300,0),show(a)].join()}finally{delete Object.prototype['250']}`},
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer func() { jitGoCallWork = nil }()
	run := func(name, src string) {
		t.Helper()
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%s: got %s, interpreter %s", name, got, want)
		}
	}
	for _, c := range cases {
		warm := `{const a=[];[` + c.fn + `(a,300,0),show(a)].join()}`
		for range 4 {
			run(c.name, warm)
		}
		e := r.jit.hint(r.global.getOwn(r.atoms.intern(c.fn)).value().Object().fn().closure)
		if e == nil || e.ssa == nil {
			t.Fatalf("%s: %s was not compiled", c.name, c.fn)
		}
		hosts, entries, reoptimized, calls := e.ssaStats.hosts, e.ssaStats.entries, r.jit.reoptimized, 0
		jitGoCallWork = func() { calls++ }
		run(c.name, c.src)
		jitGoCallWork = nil
		if c.fn == "plain" && r.jit.reoptimized == reoptimized {
			// The array grows a doubling at a time: a dozen times for 300,
			// each a call of Go; Go had made each append.
			left := e.ssaStats.hosts - hosts + uint64(calls)
			if e.entrySlow || e.ssaStats.entries == entries || left > 16 {
				t.Fatalf("plain entered %d times, left %d for 300 appends", e.ssaStats.entries-entries, left)
			}
		}
	}
}

// An array's length read after an element is written at it is the length
// the write made, as the interpreter has it: the optimizer had taken the
// length read before the write for it (ssa's readsMemory), a.length short,
// whether the element was a number, a reference or a string, and made at
// the length or past it -- in each function inlined into a loop, where
// both reads are of the one array the call was given.
func TestJITSSALengthAfterAppend(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `var o={k:1};
		function num(a){const s=a.length;a[s]=1;return s*1000+a.length}
		function ref(a){const s=a.length;a[s]=o;return s*1000+a.length}
		function str(a){const s=a.length;a[s]='x';return s*1000+a.length}
		function past(a){const s=a.length;a[s+1]=2;return s*1000+a.length}
		function loop(a,n){let t=0;for(let i=0;i<n;i++){const s=a.length;a[s]=i;t+=a.length-s}return t*1000+a.length}
		function drive(n){let r;for(let k=0;k<n;k++)r=[num([1,2,3]),ref([1,2]),str([]),past([1]),loop([],40)].join();return r}`
	// Each called on its own, and inlined into drive's loop.
	const src = `[num([1,2,3]),ref([1,2]),str([]),past([1]),loop([],40),drive(300)].join()`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 6 {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("run %d: got %s, interpreter %s", i, got, want)
		}
	}
	for _, name := range []string{"num", "ref", "str", "past", "loop", "drive"} {
		if e := r.jit.hint(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure); e == nil || e.ssa == nil {
			t.Errorf("%s was not compiled", name)
		}
	}
}

// A setter for an index Object.prototype already has where a.push, or
// a[a.length] = n, is compiled is asked for that index, as the interpreter
// asks it: neither is made natively, whose checks compare the prototype's
// shape, which had been taken with the setter in it.
func TestJITSSANativePushPastAPrototypeSetter(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `var hits=0;
		Object.defineProperty(Object.prototype,'250',{set(v){hits++},get(){return 'proto'},configurable:true});
		function fill(a,n){let t=0;for(let i=0;i<n;i++)t=(t+a.push(i))|0;return t}
		function put(a,n){let t=0;for(let i=0;i<n;i++){a[a.length]=i;t=(t+a.length)|0}return t}
		function show(a){return [a.length,a[250],Object.keys(a).length,hits].join()}`
	const src = `{const a=[],b=[];[fill(a,300),show(a),put(b,300),show(b)].join()}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 6 {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("run %d: got %s, interpreter %s", i, got, want)
		}
	}
	for _, name := range []string{"fill", "put"} {
		if e := r.jit.hint(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure); e == nil || e.ssa == nil {
			t.Fatalf("%s was not compiled", name)
		}
	}
}

// A write whose cache adds its property adds it natively, as V8's stores do
// along a map's transition: fill never leaves for its fresh objects. One
// that may not -- a prototype's setter intercepts the name, the object is
// not extensible, the collector marks -- is Go's, as is one past the
// table's room, fill4's fourth; one the object has already is stored.
func TestJITSSAPropertyAdds(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Each case its own copy of fill, compiled first: what one leaves to
	// Go must not have another demoted.
	const fill = `function(a,n,v){for(let i=0;i<n;i++){const o=a[i];o.x=i;o.r=v}return a}`
	const fill4 = `function(a,n){for(let i=0;i<n;i++){const o=a[i];o.p=i;o.q=2;o.s=3;o.t=4}return a}`
	setup := `function mk(n){const a=[];for(let i=0;i<n;i++)a.push({});return a}
		var P={set x(v){this.y=v}};
		function mkP(n){const a=[];for(let i=0;i<n;i++)a.push(Object.create(P));return a}
		function show(a,v){const l=a[a.length-1];return [l.x,l.y,l.r===v,Object.keys(l).join('')].join()}
		function show4(a){const l=a[a.length-1];return [l.p,l.q,l.s,l.t,Object.keys(l).join('')].join()}
		var v={k:1}, fresh=` + fill + `, inProto=` + fill + `, protoSet=` + fill + `, stopped=` + fill + `, owned=` + fill + `, marked=` + fill + `, four=` + fill4 + `;`
	cases := []struct{ name, fn, warm, src string }{
		{"fresh", "fresh", `show(fresh(mk(300),300,v),v)`, `show(fresh(mk(300),300,v),v)`},
		{"prototype's setter", "inProto", `show(inProto(mk(300),300,v),v)`, `show(inProto(mkP(300),300,v),v)`},
		{"setter added to Object.prototype", "protoSet", `show(protoSet(mk(300),300,v),v)`,
			`Object.defineProperty(Object.prototype,'x',{set(w){this.y=w},configurable:true});` +
				`try{show(protoSet(mk(300),300,v),v)}finally{delete Object.prototype.x}`},
		{"not extensible", "stopped", `show(stopped(mk(300),300,v),v)`, `{const a=mk(300);a.forEach(Object.preventExtensions);show(stopped(a,300,v),v)}`},
		{"already its own", "owned", `show(owned(mk(300),300,v),v)`, `{const a=mk(300);owned(a,300,v);show(owned(a,300,v),v)}`},
		{"past the room", "four", `show4(four(mk(300),300))`, `show4(four(mk(300),300))`},
		{"collector marking", "marked", `show(marked(mk(300),300,v),v)`, `show(marked(mk(300),300,v),v)`},
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	run := func(name, src string) {
		t.Helper()
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%s: got %s, interpreter %s", name, got, want)
		}
	}
	for _, c := range cases {
		for range 4 {
			run(c.name, c.warm)
		}
		e := r.jit.hint(r.global.getOwn(r.atoms.intern(c.fn)).value().Object().fn().closure)
		if e == nil || e.ssa == nil {
			t.Fatalf("%s: %s has no code", c.name, c.fn)
		}
		hosts := e.ssaStats.hosts
		if c.fn == "marked" {
			jitMarkingForTest(t)
		}
		run(c.name, c.src)
		if c.fn == "fresh" && e.ssaStats.hosts != hosts {
			t.Fatalf("fresh left native code adding properties %d times", e.ssaStats.hosts-hosts)
		}
	}
}

// What native calls' contexts share with the context code was entered in
// -- how deep calls may go among it -- Go writes into them when it
// changes, not each call (jitShareContexts): code entered shallow, then
// deep, recurses natively only as deep as the call depth limit allows,
// and throws where the interpreter does.
func TestJITSSASharedContextsFollowDepth(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function rec(n){if(n===0)return 0;let s=0;for(let j=0;j<2;j++)s+=j;return s+rec(n-1)}
		function at(d,n){return d===0?rec(n):at(d-1,n)}
		function tryAt(d,n){try{return String(at(d,n))}catch(e){return e.constructor.name}}`
	rounds := []string{`tryAt(0,10)`, `tryAt(0,10)`, `tryAt(0,10)`, `tryAt(0,10)`, `tryAt(185,20)`, `tryAt(0,10)`, `tryAt(185,20)`}
	want := New(Config{MaxCallDepth: 200})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true, MaxCallDepth: 200})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d, %s: got %s, interpreter %s", i, src, got, want)
		}
	}
	if wv, _ := want.Run(compileForTest(t, `tryAt(185,20)`)); wv.String().Go() != "RangeError" {
		t.Fatalf("the interpreter did not run out of depth: %s", wv.String().Go())
	}
	cl := r.global.getOwn(r.atoms.intern("rec")).value().Object().fn().closure
	if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || !slices.Contains(e.callSites, jitCallNative) {
		t.Fatal("rec does not call itself natively")
	}
}

// A function whose calls are on branches taken one after another learns
// each at a different time, and is compiled again for each, up to
// jitInlineReoptimizations; a call met after those that keeps leaving
// native code is settled all the same (jitCallsHot), not left to Go for
// good: f7's, which has a loop, is made natively.
func TestJITSSACallsSettleLate(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function f0(x){return x+1} function f1(x){return x+2} function f2(x){return x+3} function f3(x){return x+4}
		function f4(x){return x+5} function f5(x){return x+6} function f6(x){return x+7} function f7(x){let s=x;for(let j=0;j<2;j++)s+=4;return s}
		function g(m,x){if(m===0)return f0(x);if(m===1)return f1(x);if(m===2)return f2(x);if(m===3)return f3(x);
			if(m===4)return f4(x);if(m===5)return f5(x);if(m===6)return f6(x);return f7(x)}
		function run(m,n){let s=0;for(let i=0;i<n;i++)s=(s+g(m,i))|0;return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var hosts uint64
	for m := range 9 {
		src := fmt.Sprintf("String(run(%d,400))", min(m, 7))
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		g := r.jit.cache[weak.Make(r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure.fn)]
		if g != nil {
			hosts = g.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("%s: got %s, interpreter %s", src, got, want)
		}
		if m == 8 {
			// f7's call, met last, is settled: g no longer leaves for it.
			if g == nil {
				t.Fatal("g has no code")
			}
			if g.ssaStats.hosts != hosts {
				t.Fatalf("g still leaves native code: %d exits; calls %v", g.ssaStats.hosts-hosts, g.callSites)
			}
		}
	}
}

// A read that meets objects of several shapes, its property on their
// prototypes, is compiled for each, up to jitPropertyCases, as V8's
// polymorphic inline caches are (jitPolySeen, ssa.PropertyCase): a loop over
// four kinds of object then never leaves native code for its reads, a
// method's or a field's. A fifth leaves for Go; a prototype's property
// changed is read again, and one a nearer prototype comes to have fails
// that one's shape check. Each answer is the interpreter's.
func TestJITSSAPolymorphicReads(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, and a function all
	// of whose calls Go then makes is rightly demoted: not here.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function A(){this.a=1}A.prototype.k=1;A.prototype.m=function(){return 10};
		function B(){this.b=2;this.a=0}B.prototype.k=2;B.prototype.m=function(){return 20};
		function C(){}C.prototype=Object.create({k:3,m(){return 30}});
		function D(){this.d=4}D.prototype.k=4;D.prototype.m=function(){return 40};
		function E(){}E.prototype.k=5;E.prototype.m=function(){return 50};
		function reads(os,n){let s=0;for(let i=0;i<n;i++){const o=os[i%os.length];s=(s*7+o.k)|0;s^=s>>>5}return s}
		function calls(os,n){let s=0;for(let i=0;i<n;i++){const o=os[i%os.length];s=(s*3+o.m())|0;s^=s>>>4}return s}
		var four=[new A,new B,new C,new D],five=[new A,new B,new C,new D,new E];`
	src := `[reads(four,400),calls(four,400)].join()`
	rounds := []string{src, src, src, src, `[reads(five,400),calls(five,400)].join()`,
		`B.prototype.k=7;Object.getPrototypeOf(C.prototype).k=9;[reads(four,400),calls(four,400)].join()`,
		`C.prototype.k=99;C.prototype.m=function(){return 99};[reads(four,400),calls(four,400)].join()`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.hint(cl)
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if i == 3 {
			hosts = entry("reads").ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			if e := entry("reads"); len(e.poly) != 1 || len(e.poly[0].cases) < 3 || e.ssaStats.hosts != hosts {
				t.Fatalf("reads left native code %d times for four shapes; %+v", e.ssaStats.hosts-hosts, e.poly)
			}
		}
	}
}

// A store to a cell a live value was read from leaves for Go if the cell
// holds a pointer word, which the value needs (storeChecks), and only then
// (the native harness's "read, store, use" checks which): swap's a, an
// object, needs its cell, and Go writes it. append, Richards' Packet.addTo,
// whose walk reads the cell it then writes, runs natively. Each answer is
// the interpreter's.
func TestJITSSAStoreOverLiveCell(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, and a function all
	// of whose calls Go then makes is rightly demoted: not here.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function L(v){this.v=v;this.link=null}
		L.prototype.append=function(queue){this.link=null;if(queue==null)return this;var peek,next=queue;while((peek=next.link)!=null)next=peek;next.link=this;return queue};
		var append=L.prototype.append;
		function run(ps,n){let s=0;for(let k=0;k<n;k++){let q=ps[0];q.link=null;for(let i=1;i<ps.length;i++)q=ps[i].append(q);for(let p=q;p!=null;p=p.link)s=(s+p.v)|0}return s}
		function swap(o,n){let s=0;for(let i=0;i<n;i++){const a=o.x;o.x=o.y;o.y=a;s=(s*3+a.v)|0}return s}
		var ps=[new L(1),new L(2),new L(3),new L(4),new L(5)],o={x:{v:1},y:{v:2}};`
	src := `[run(ps,40),swap(o,60)].join()`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) *jitEntry {
		// By its function: a native callee's closure need not know it.
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	for i := 0; i < 4; i++ {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if i == 3 {
			hosts = entry("append").ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if e := entry("append"); i == 3 && (e == nil || e.ssa == nil || e.ssaStats.hosts != hosts) {
			t.Fatalf("append's store left native code: %+v", e)
		}
	}
}

// A function of more literals than a context has keep cells
// (abi.MaxKeeps): each takes its cell when the code is planned, and one
// past the last is Go's from the plan on. Taken only when it was built, a
// literal past the last was dropped -- planning had made no exit for it --
// and what followed took the value its slot held before: a wrong sum, or
// a method of the wrong object.
func TestJITSSAKeepCellsRunOut(t *testing.T) {
	for _, body := range []string{
		"c+=[N][0];\n",
		"c+=[N][0];c+=String(c).length;\n",
	} {
		var b strings.Builder
		b.WriteString("function f(){var c=0;\n")
		for i := 0; i < 300; i++ {
			b.WriteString(strings.ReplaceAll(body, "N", strconv.Itoa(i)))
		}
		b.WriteString("return c}\nvar t=0;for(var k=0;k<12;k++)t+=f();t")
		src := b.String()
		want := New(Config{})
		wv, err := want.Run(compileForTest(t, src))
		want.Close()
		if err != nil {
			t.Fatal(err)
		}
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = jitInvocationsForFeedback, jitInvocationsForFeedback
		v, err := r.Run(compileForTest(t, src))
		if err != nil || v.Number() != wv.Number() {
			t.Fatalf("%q: %v, %v; interpreter %v", body, v, err, wv)
		}
		if r.jit == nil || r.jit.ssaEntries == 0 {
			t.Fatalf("%q: no native code ran", body)
		}
	}
}

// A value kept before a store (ssa's keep.go) that goes round a loop is
// kept again at the store from its own keep cell, whose pointer word is
// read before the cell is written: here o, the object stored to, once its
// phi merges the copy. Found by the differential fuzzer; every tier's
// answer is the interpreter's (jitDifferential).
func TestJITSSAKeepRoundLoop(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitDifferential(t, `var log=[];
function h(s,i){return (s+i)|0}
function f(a,o,n){let s=0.5,t=0;for(let i=0;i<n;i++){s=h(s,i);if(a.length){if(i===3)continue;}else{s=h(s,i);}o.x=(a.length !== a.length ? NaN : i);}return [s,t]}
function show(v){return typeof v==='number'&&Object.is(v,-0)?'-0':String(v)}
function run(a,o,n){try{let r=f(a,o,n);log.push(show(r[0])+','+show(r[1]))}catch(e){log.push(e.name+':'+e.message)}log.push(a.length+':'+Array.from(a,show).join(','),show(o.x))}
run([3,undefined,(-2147483649),,0],{x:5n},35);
run([(-1.5),true,(-1),(-2147483649)],{x:0.5},34);
log.join('|')`)
}

// A native call goes on after it (ssa's OpCall): what is live across it in
// registers is saved and restored -- s, a double, and t, an int32, here --
// and p, read from o.p, which the callee writes, is kept, so that p.v is
// the first object's still. Every fiftieth call the callee leaves native
// code, and Go makes f's frame from the call's records (abi.RecordDirect):
// its double, its int and its reference. zero takes no arguments. Each
// answer is the interpreter's.
func TestJITSSANativeCallsGoOn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, and a function all
	// of whose calls Go then makes is rightly demoted: not here.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var A={v:3},B={v:5},o={p:A},c={n:0},L={get n(){return String(c.n).length}};
		function tick(){c.n++;o.p=(c.n&1)?B:A;if(c.n%50===0)return L.n;return c.n&3}
		function zero(){let z=0;for(let j=0;j<1;j++)z=tick();return z}
		function f(n){let s=0.5,t=0,p=o.p;for(let i=0;i<n;i++){s=s*1.0001+zero();t=(t+p.v)|0;s+=i*0.5}return [s,t,p.v,c.n].join()}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		src := `o.p=A;f(300)`
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("zero")).value().Object().fn().closure
	e := r.jit.cache[weak.Make(cl.fn)]
	if e == nil || e.link.in == 0 {
		t.Fatalf("zero was never called natively: %+v", e)
	}
	// tick's getter read, inlined in zero, leaves native code; its cache
	// knows the getter, which is no feedback to compile again for: zero is
	// compiled again for the shapes tick's other reads met after zero was
	// compiled, before tick had run, and to call tick natively once its
	// exits pass jitInlineExits -- not at each exit, eight times.
	if e.inlineReopts > 2 {
		t.Errorf("zero was compiled again %d times for what it inlined", e.inlineReopts)
	}
	if r.jit.unwound == 0 {
		t.Fatal("tick never left native code inside a native call")
	}
}

// A native call whose callee left native code, which Go finished, goes on
// in its caller's code after the call, where the callee would have
// returned to (runSSA's resume), as V8's lazy deoptimization leaves the
// caller's frame alone: s, t and p, live across the call in registers and
// a keep cell, are as they were: tick, whose loop keeps it from being
// inlined, leaves at its addition of an object, L, whose conversion Go
// makes, once in fifty calls (a megamorphic read of a getter, Go's now
// without leaving, had been it). A callee that throws (boom, through raise) is finished as before,
// the throw going through its caller; as is one while the collector marks.
// Each answer is the interpreter's.
func TestJITSSANativeCallResumes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var A={v:3},B={v:5},o={p:A},c={n:0},L={get n(){return String(c.n).length}};
		function tick(){let k=0;for(let j=0;j<1;j++)k++;c.n++;o.p=(c.n&1)?B:A;if(c.n%50===0)return (L+"").length+k;return (c.n&3)+k}
		function f(n){let s=0.5,t=0,p=o.p;for(let i=0;i<n;i++){s=s*1.0001+tick();t=(t+p.v)|0;s+=i*0.5}return [s,t,p.v,c.n].join()}
		function raise(s){throw new Error("e"+s)}
		function boom(i){let s=i;for(let j=0;j<2;j++)s=s*2;if(c.n++%97===0)raise(s);return s}
		function g(n){let t=0,u=0.25;for(let i=0;i<n;i++){t=(t+boom(i))|0;u=u*1.5+1}return [t,u].join()}
		function tryG(n){try{return g(n)}catch(e){return e.message+c.n}}`
	src := `o.p=A;[f(300),tryG(300)].join()`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	// Rounds enough for the code to settle: each deoptimization has it
	// compiled again once a round has spent its budget (jitTierUp).
	resumedLate := uint64(0)
	for i := range 9 {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if i == 8 {
			jitMarkingForTest(t)
		}
		resumed := r.jit.resumed
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i >= 3 && i < 8 {
			resumedLate += r.jit.resumed - resumed
		}
	}
	if resumedLate == 0 {
		t.Fatal("no caller went on natively after its callee left native code")
	}
}

// A value read from an element and used after a native call is kept across
// it, as one read from a property is: the callee may write the element.
// put stores natively into the element f's x came from, then leaves native
// code at an addition of an object, after which f goes on natively with x, which
// a[0] no longer holds -- until put, which always leaves, is left to Go;
// take pops natively the element g's x came from.
func TestJITSSAElementsKeptAcrossCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var A={v:3},B={v:5},L={a:0,get n(){return String(this.a).length}};
		function put(a,i){a[0]=(i&1)?B:A;L.a=i;return (L+String(L.a)).length&1}
		function f(a,n){let t=0;for(let i=0;i<n;i++){const x=a[0];t=(t+put(a,i))|0;t=(t*3+x.v)|0}return t}
		function take(a){return a.pop()}
		function g(a,n){let t=0;for(let i=0;i<n;i++){const x=a[a.length-1];const y=take(a);a.push((i&1)?A:B);t=(t*3+x.v+(x===y?1:0))|0}return t}`
	for _, c := range []struct {
		name, src string
		resumes   bool
	}{{"f", `String(f([A],300))`, true}, {"g", `String(g([A,B],300))`, false}} {
		t.Run(c.name, func(t *testing.T) {
			want := New(Config{})
			defer func() { want.Close(); want.ReleaseClosed() }()
			r := jitRuntimeForTest(t, Config{JIT: true})
			for _, rt := range []*Runtime{want, r} {
				if _, err := rt.Run(compileForTest(t, setup)); err != nil {
					t.Fatal(err)
				}
			}
			cl := r.global.getOwn(r.atoms.intern(c.name)).value().Object().fn().closure
			native := false
			for i := range 6 {
				wv, err := want.Run(compileForTest(t, c.src))
				if err != nil {
					t.Fatal(err)
				}
				resumed := r.jit.resumed
				gv, err := r.Run(compileForTest(t, c.src))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := gv.String().Go(), wv.String().Go(); got != want {
					t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
				}
				if e := r.jit.hint(cl); e != nil && len(e.nativeCalls) != 0 && (!c.resumes || r.jit.resumed > resumed) {
					native = true
				}
			}
			if !native {
				t.Fatalf("%s does not call natively", c.name)
			}
		})
	}
}

// A call first seen while its callee leaves native code -- at its getter's
// read, an object's own accessor, made megamorphic, which Go makes; too
// often for native callers to call it (notNative); deoptimized, its code
// invalidated -- is made natively all the same: its calls leave for Go
// until the callee is called natively again, then do not. Here leaf leaves
// at every call from other, then outer's call is seen, then leaf stops
// leaving; outer's last round runs natively, never leaving.
func TestJITSSACallSeenWhileCalleeLeaves(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var mode=0,L={a:0,get n(){return String(this.a).length}};
		function leaf(a){if(mode)return (a*3)|0;L.a=a;return L.n}
		function other(n){let t=0;for(let i=0;i<n;i++)t=(t+leaf(i))|0;return t}
		function outer(n){let t=0;for(let i=0;i<n;i++){t=(t+leaf(i))|0;for(let j=0;j<40;j++)t=(t*3+j)|0}return t}`
	rounds := []string{`String(other(500))`, `String(other(500))`, `String(outer(500))`, `mode=1;String(outer(2000))`}
	for range 4 {
		rounds = append(rounds, `String(outer(2000))`)
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	outer := r.global.getOwn(r.atoms.intern("outer")).value().Object().fn().closure
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		oe := r.jit.hint(outer)
		if oe != nil {
			hosts = oe.ssaStats.hosts
		}
		reoptimized := r.jit.reoptimized
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == len(rounds)-1 {
			oe = r.jit.hint(outer)
			if oe == nil || r.jit.reoptimized != reoptimized || oe.ssaStats.hosts != hosts {
				var left uint64
				if oe != nil {
					left = oe.ssaStats.hosts - hosts
				}
				t.Fatalf("outer left native code %d times in its last round", left)
			}
		}
	}
}

// A function whose reads meet new shapes for as long as it runs, one read
// after another, deoptimizes at each, its code invalidated, and is compiled
// again only once the existing tiers have spent their budget, its caches'
// feedback settled meanwhile (jitTierUp): at most once a phase, however
// many of the phase's calls meet the new shape, and still native after
// its 36 phases, within its deoptimizations' limit (jitMaxDeopts). Its
// answers are the interpreter's.
func TestJITSSAPolymorphicReadsBackOff(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const sites, kinds = 12, 4
	var b strings.Builder
	for k := 0; k < sites*kinds; k++ {
		fmt.Fprintf(&b, "function C%d(){}; C%d.prototype.v = %d;\n", k, k, k)
	}
	b.WriteString("var objs = [")
	for j := 0; j < sites; j++ {
		fmt.Fprintf(&b, "new C%d(),", j*kinds)
	}
	b.WriteString("];\nfunction sum(n) { let t = 0; for (let i = 0; i < n; i++) t = (t * 3")
	for j := 0; j < sites; j++ {
		fmt.Fprintf(&b, " + objs[%d].v", j)
	}
	b.WriteString(") | 0; return t }\n")
	// A phase gives one read a shape it has not met, then runs.
	b.WriteString(`function phase(j, k) { objs[j] = new globalThis["C" + k](); let t = 0; for (let i = 0; i < 30; i++) t = (t + sum(40)) | 0; return t }`)
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitFewCallsForTest(r)
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, b.String())); err != nil {
			t.Fatal(err)
		}
	}
	for phase := 0; phase < sites*(kinds-1); phase++ {
		src := fmt.Sprintf("''+phase(%d, %d)", phase%sites, phase%sites*kinds+1+phase/sites)
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("phase %d: got %s, interpreter %s", phase, got, want)
		}
	}
	e := jitNativeForTest(t, r, "sum")
	// Compiled once at first, and again at most once a phase that met a
	// new shape: a deoptimization each, its cache learning the shape.
	if e.ssaSerial < 2 || int(e.ssaSerial) > 1+sites*(kinds-1) || e.deopts > sites*(kinds-1) {
		t.Errorf("compiled %d times, deoptimized %d, for %d phases of new shapes", e.ssaSerial, e.deopts, sites*(kinds-1))
	}
}

// A read whose receivers turn out to be of several shapes, met one after
// another, deoptimizes at a new one, as V8's map checks do, its code
// invalidated, and is compiled again once the existing tiers have spent
// their budget, its cache's feedback settled meanwhile (jitTierUp): once
// for them all where an invocation meets them all, no more often than the
// new shapes in any case; the code then reads each natively. So too where the
// code runs called natively, its exits Go's to finish (jitFinishExit):
// many calls at, which reads one.
func TestJITSSAPolymorphicReadsSettle(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function A(){};A.prototype.v=1;function B(){};B.prototype.v=2;
		function C(){};C.prototype.v=3;function D(){};D.prototype.v=4;
		var one=[new A()],four=[new A(),new B(),new C(),new D()];
		function sum(objs,n){let t=0;for(let i=0;i<n;i++){t=(t*3+objs[i&3&(objs.length-1)].v)|0}return t}
		function at(objs,k){let t=0;for(let i=k;i<k+1;i++)t=objs[i&3&(objs.length-1)].v;return t}
		function many(objs){let t=0;for(let k=0;k<100;k++)t=(t*3+at(objs,k))|0;return t}`
	for _, c := range []struct{ name, call, fn string }{{"entered", "sum(%s,500)", "sum"}, {"called natively", "many(%s)", "at"}} {
		t.Run(c.name, func(t *testing.T) {
			var rounds []string
			for _, objs := range []string{"one", "one", "four", "four", "four"} {
				rounds = append(rounds, "String("+fmt.Sprintf(c.call, objs)+")")
			}
			want := New(Config{})
			defer func() { want.Close(); want.ReleaseClosed() }()
			r := jitRuntimeForTest(t, Config{JIT: true})
			for _, rt := range []*Runtime{want, r} {
				if _, err := rt.Run(compileForTest(t, setup)); err != nil {
					t.Fatal(err)
				}
			}
			cl := r.global.getOwn(r.atoms.intern(c.fn)).value().Object().fn().closure
			var compiles uint64
			for i, src := range rounds {
				wv, err := want.Run(compileForTest(t, src))
				if err != nil {
					t.Fatal(err)
				}
				reoptimized, allHosts := r.jit.reoptimized, r.jit.hosts
				gv, err := r.Run(compileForTest(t, src))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := gv.String().Go(), wv.String().Go(); got != want {
					t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
				}
				if r.jit.cache[weak.Make(cl.fn)] == nil {
					t.Fatalf("round %d: %s has no code", i, c.fn)
				}
				if i >= 2 {
					compiles += r.jit.reoptimized - reoptimized
				}
				// The three new shapes' deoptimizations, which are a native
				// call's leaving native code for its caller, called
				// natively, and nothing else.
				if i == 2 && r.jit.hosts-allHosts > 3 {
					t.Fatalf("%s left native code %d times before it was compiled for its shapes", c.fn, r.jit.hosts-allHosts)
				}
				if i == len(rounds)-1 && (r.jit.reoptimized != reoptimized || r.jit.hosts != allHosts) {
					t.Fatalf("native code left %d times in its last round", r.jit.hosts-allHosts)
				}
			}
			// Entered, sum's one invocation meets the three shapes in the
			// interpreter before its back edges have spent the budget, and
			// one compile takes them all. Called natively, at's budget, as
			// many invocations as a runtime compiling at the first call
			// waits for, one (jitTierBudget), is spent at its next call,
			// before that meets the next shape: a compile for each, as V8's
			// would be at such a threshold.
			if max := map[string]uint64{"sum": 1, "at": 3}[c.fn]; compiles == 0 || compiles > max {
				t.Fatalf("%s was compiled again %d times for its read's three new shapes", c.fn, compiles)
			}
		})
	}
}

// The calls the existing tiers have made are decided when a function's
// code is first compiled, from the caches of the reads of their callees,
// methods' and globals' (jitSeedCalls): run's loop, promoted after the
// tree tier ran it, inlines get and sq and calls big and heavy natively,
// which have code of their own by then, without being compiled again to
// learn any from its exits.
// A call whose read met no method, or another object's, is left to be
// learned as before: what takes the place of get later is called as it
// is.
func TestJITSSASeedsMethodCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function P(x){this.x=x}
		P.prototype.get=function(){return this.x};
		P.prototype.big=function(n){let s=0;for(let i=0;i<n;i++)s=(s+i*this.x)|0;return s};
		function Q(x){this.x=x}Q.prototype.get=function(){return this.x+1};
		function sq(x){return x*x}
		function heavy(n){let s=1;for(let i=0;i<n;i++)s=(s*3+i)|0;return s}
		var p=new P(3),q=new Q(5);
		for(let i=0;i<2000;i++){p.big(40);heavy(40)}
		function run(o,n){let t=0;for(let i=0;i<n;i++){t=(t+o.get()+o.big(4)+sq(i&7)+heavy(2))|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	proto := r.global.getOwn(r.atoms.intern("P")).value().Object().getOwn(atomPrototype).value().Object()
	get, big := proto.getOwn(r.atoms.intern("get")).value().Object(), proto.getOwn(r.atoms.intern("big")).value().Object()
	sq, heavy := r.global.getOwn(r.atoms.intern("sq")).value().Object(), r.global.getOwn(r.atoms.intern("heavy")).value().Object()
	targets := func(list []jitInline) []*Object {
		var objs []*Object
		for _, x := range list {
			objs = append(objs, x.obj)
		}
		return objs
	}
	for i, src := range []string{`String(run(p,20000))`, `String(run(p,20000))`, `Q.prototype.big=P.prototype.big;String(run(q,20000))`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		e := r.jit.hint(cl)
		if e == nil || e.ssa == nil {
			t.Fatalf("round %d: run has no code", i)
		}
		if i == 1 {
			if in, native := targets(e.inlines), targets(e.nativeCalls); !slices.Equal(in, []*Object{get, sq}) || !slices.Equal(native, []*Object{big, heavy}) {
				t.Fatalf("run inlines %d calls and makes %d natively", len(in), len(native))
			}
			if n := int(e.reopts) + int(e.inlineReopts) + int(e.upgradeReopts); n != 0 {
				t.Fatalf("run was compiled again %d times", n)
			}
		}
	}
}

// A construction the existing tiers have made is decided when a function's
// code is first compiled, as a call is (jitSeedCalls), once its
// constructor has code: mk1 learns new V from its exits, which compiles V
// for native callers; mk2's first code then makes it natively, from a
// pool, V inlined, without being compiled again for it.
func TestJITSSASeedsConstructions(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function V(x){this.x=x;this.y=x+1}
		function mk1(n){let t=0;for(let i=0;i<n;i++){const v=new V(i);t=(t+v.y)|0}return t}
		function mk2(n){let t=0;for(let i=0;i<n;i++){const v=new V(i&15);t=(t*3+v.x)|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	v := r.global.getOwn(r.atoms.intern("V")).value().Object()
	cl := r.global.getOwn(r.atoms.intern("mk2")).value().Object().fn().closure
	for i, src := range []string{`String(mk1(20000))`, `String(mk1(20000))`, `String(mk2(20000))`, `String(mk2(20000))`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil {
		t.Fatal("mk2 has no code")
	}
	if !slices.ContainsFunc(slices.Concat(e.nativeCalls, e.inlines), func(x jitInline) bool { return x.obj == v && x.pool != nil }) {
		t.Fatal("mk2 does not construct V natively")
	}
	if n := int(e.reopts) + int(e.inlineReopts) + int(e.upgradeReopts); n != 0 {
		t.Fatalf("mk2 was compiled again %d times", n)
	}
}

// A loop whose callees run loops of their own natively is promoted all
// the same. The back-edge budget is shared, and run's iteration takes 8
// back edges, 1 of its own and 7 in big's and heavy's native loops, which
// divides 1024: with the same budget every period, it ran out at the same
// place in each, never at run's back edge, and run was never compiled. It
// varies now (backEdgeBudget).
func TestJITLoopPromotedPastCalleesLoops(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function P(x){this.x=x}
		P.prototype.big=function(n){let s=0;for(let i=0;i<n;i++)s=(s+i*this.x)|0;return s};
		function heavy(n){let s=1;for(let i=0;i<n;i++)s=(s*3+i)|0;return s}
		var p=new P(3);
		for(let i=0;i<2000;i++){p.big(40);heavy(40)}
		function run(o,n){let t=0;for(let i=0;i<n;i++){t=(t+o.big(4)+heavy(3))|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	src := `String(run(p,20000))`
	wv, err := want.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	gv, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gv.String().Go(), wv.String().Go(); got != want {
		t.Fatalf("got %s, interpreter %s", got, want)
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	if e := r.jit.hint(cl); e == nil || e.ssa == nil {
		t.Fatal("run's loop was not promoted in 20000 iterations")
	}
	// About the interval, and not the same each time.
	seen := map[int]bool{}
	for range 64 {
		b := r.jit.backEdgeBudget()
		if b < backEdgeCheckInterval/2 || b >= backEdgeCheckInterval*3/2 {
			t.Fatalf("budget %d", b)
		}
		seen[b] = true
	}
	if len(seen) < 32 {
		t.Fatalf("%d budgets in 64", len(seen))
	}
}

// A call whose target has no code is decided at the caller's first
// compile all the same, its target compiled for native callers then
// (jitSeedCalls), which decides its own calls (jitSeedCallee): put, which
// makes a call, is then inlined with it, at once, not called natively
// until a compile of run's again for what put came to inline.
func TestJITSSASeedsCalleesWithoutCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function P(x){this.x=x}
		P.prototype.f=function(i){return (i*this.x)|0};
		P.prototype.put=function(i){return this.f(i)+1};
		var p=new P(3);
		function run(o,n){let t=0;for(let i=0;i<n;i++){t=(t+o.put(i))|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	proto := r.global.getOwn(r.atoms.intern("P")).value().Object().getOwn(atomPrototype).value().Object()
	put := proto.getOwn(r.atoms.intern("put")).value().Object()
	f := proto.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	for i, src := range []string{`String(run(p,20000))`, `String(run(p,20000))`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil {
		t.Fatal("run has no code")
	}
	if !slices.ContainsFunc(e.inlines, func(x jitInline) bool { return x.obj == put }) || !slices.Contains(e.ssaInlined, f) {
		t.Fatal("run does not inline put with f")
	}
	if n := int(e.reopts) + int(e.inlineReopts) + int(e.upgradeReopts); n != 0 {
		t.Fatalf("run was compiled again %d times", n)
	}
}

// Code compiled again for any reason decides the calls whose targets the
// existing tiers have met since (jitSeedCalls): f's call of o.m, on a
// branch first taken after f was compiled, leaves its read of m for Go,
// which fills the read's cache and has f compiled again at once (jitFed);
// that compile decides the call too, which is not left to leave native
// code four times more and have f compiled once again.
func TestJITSSASeedsWhenCompiledAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function P(x){this.x=x}
		P.prototype.m=function(){return this.x*2};
		var p=new P(3);
		function f(o,flag,n){let t=0;for(let i=0;i<n;i++){t=(t+o.x)|0;if(flag)t=(t+o.m())|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	m := r.global.getOwn(r.atoms.intern("P")).value().Object().getOwn(atomPrototype).value().Object().getOwn(r.atoms.intern("m")).value().Object()
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	for i, src := range []string{`String(f(p,false,20000))`, `String(f(p,false,20000))`, `String(f(p,true,20000))`, `String(f(p,true,20000))`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 1 {
			if e := r.jit.hint(cl); e == nil || e.ssa == nil {
				t.Fatal("f has no code before the branch is taken")
			}
		}
	}
	e := r.jit.hint(cl)
	if !slices.ContainsFunc(e.inlines, func(x jitInline) bool { return x.obj == m }) {
		t.Fatal("f does not inline m")
	}
	if e.inlineReopts != 0 {
		t.Fatalf("f was compiled again %d times for its calls", e.inlineReopts)
	}
	// A call still counted toward being decided from its exits is decided
	// when the code is compiled again all the same.
	pc := int(e.inlines[slices.IndexFunc(e.inlines, func(x jitInline) bool { return x.obj == m })].pc)
	e.inlines = slices.DeleteFunc(e.inlines, func(x jitInline) bool { return x.obj == m })
	e.callSites[pc] = jitCallsToInline - 1
	p, err := jitcompile.LowerSSA(cl.fn)
	if err != nil {
		t.Fatal(err)
	}
	r.jitSeedCalls(cl, e, p)
	if e.callSites[pc] != jitCallInlined {
		t.Fatalf("a call counted toward its decision is left at %d", e.callSites[pc])
	}
}

// Seeding compiles a call's target, but what it compiles does not compile
// in turn: ping and pong call each other, and each compile of one would
// otherwise compile the other, again and again, the first not yet known
// to have code.
func TestJITSSASeedsMutualRecursion(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function P(){}
		P.prototype.ping=function(n){return n<=0?0:(this.pong(n-1)+1)|0};
		P.prototype.pong=function(n){return n<=0?0:(this.ping(n-1)*2)|0};
		var p=new P();
		function run(o,n){let t=0;for(let i=0;i<n;i++)t=(t+o.ping(i&7))|0;return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range []string{`String(run(p,20000))`, `String(run(p,20000))`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	if r.jit.compiled > 16 {
		t.Fatalf("%d functions compiled for three", r.jit.compiled)
	}
}

// Two values compared loosely, where one is null or undefined, compare
// natively: the other must be null, undefined or an object with
// [[IsHTMLDDA]] (Annex B), with nothing converted -- as DeltaBlue's
// `next != determining`, determining sometimes null, compares; a number,
// a string or a boolean is unequal to either. Other loose comparisons of
// different kinds, which convert, are Go's. Each answer is the
// interpreter's.
func TestJITSSALooseNullishOperands(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var o={},p={};
		function eq(a,b){let n=0;for(let i=0;i<3;i++){if(a==b)n+=1;if(a!=b)n+=10;if(b==a)n+=100;if(a===b)n+=1000}return n}
		function pairs(xs){let r=[];for(const a of xs)for(const b of xs)r.push(eq(a,b));return r.join()}
		function prims(xs){let r=[];for(const a of [null,undefined])for(const b of xs)r.push(eq(a,b),eq(b,a));return r.join()}
		var nullish=[null,undefined,o,p,dda],mixed=[null,undefined,0,'',false,'0',o],others=[0,'',false,'0',1.5,true,'s'];`
	rounds := []string{`pairs(nullish)`, `pairs(nullish)`, `prims(others)`, `pairs(mixed)`, `pairs(nullish)`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		dda := rt.NewObject()
		dda.MarkHTMLDDA()
		rt.global.setOwnRaw(rt.atoms.intern("dda"), Obj(dda), propDefault)
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	eq := r.global.getOwn(r.atoms.intern("eq")).value().Object().fn().closure
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		// Entered from Go, or called natively, now that its callers'
		// for...of loops are compiled too.
		ran := func(e *jitEntry) uint64 {
			n := e.ssaStats.entries
			if e.link != nil {
				n += e.link.in
			}
			return n
		}
		var hosts, entries uint64
		if e := r.jit.hint(eq); e != nil {
			hosts, entries = e.ssaStats.hosts, ran(e)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 1 || i == 2 {
			// null or undefined against null, undefined, an object or a
			// primitive: native, never Go's (code that left for each was
			// demoted, and ran no more).
			if e := r.jit.hint(eq); e == nil || e.ssa == nil || e.entrySlow || ran(e) == entries || e.ssaStats.hosts != hosts {
				t.Fatalf("eq left native code to compare null, undefined and objects: %+v", e)
			}
		}
	}
}

// An assignment to a global native code does not write leaves for Go
// (jitHost), as the interpreter makes it, and the function around it
// compiles: DeltaBlue's drivers assign planner = new Planner(), then
// build their constraints in loops. A global var, a script's let, a name
// never declared (sloppily made a global), strictly the same (an assigned
// value that runs no code: set_global_strict), and strictly a name no
// longer declared, which throws: each the interpreter's.
func TestJITSSAAssignsGlobals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var count=0;let lex=0;function P(x){this.x=x}
		function bump(n){for(let i=0;i<n;i++){count=count+1;lex=lex+2;made=new P(i)}return [count,lex,made.x].join()}
		function sbump(n){'use strict';let s=0;for(let i=0;i<n;i++){const c=count+3,l=lex-1;count=c;lex=l;s=(s+c)|0}return (s*7+count*3+lex)|0}
		globalThis.maybe=0;function bad(n){'use strict';let s=0;for(let i=0;i<n;i++){s=(s*3+i)|0;s^=s>>>5;if((i&63)===0)maybe=s}return s}
		function tryBad(n){try{return String(bad(n))}catch(e){return e.name+":"+(typeof maybe)}}`
	rounds := []string{`bump(300)`, `bump(300)`, `String(sbump(300))`, `String(sbump(300))`, `[bump(50),sbump(50)].join()`,
		`tryBad(300)`, `tryBad(300)`, `delete globalThis.maybe;tryBad(300)`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	for _, name := range []string{"bump", "sbump", "bad"} {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || e.ssaStats.entries == 0 || name == "bad" && e.entrySlow {
			t.Fatalf("%s did not run natively", name)
		}
	}
}

// An assignment to a global variable, a writable data property of the
// global object, is a store to its cell, as V8 stores to a global's
// property cell: EarleyBoyer's unifier assigns unify_subst_nboyer in its
// loop, which kept it out of native code. What the store checks, each
// answered as the interpreter answers: a property made non-writable (a
// sloppy assignment does nothing), an accessor, one deleted (sloppily made
// again, strictly a ReferenceError), a script's let that comes to shadow
// it, and a value read from the cell before the store and used after it.
func TestJITSSAWritesGlobals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var g=0,h=1,o={v:1};globalThis.cur=o;globalThis.late=0;globalThis.gone=0;
		function w(n){for(let i=0;i<n;i++){g=g+1;h=g*2;cur=o}return [g,h,cur.v].join()}
		function sw(n){'use strict';let s=0;for(let i=0;i<n;i++){const old=g;g=old+1;s=(s+old*3+g)|0}return s}
		function lw(n){for(let i=0;i<n;i++){late=late+1}return [late,globalThis.late].join()}
		function dw(n){'use strict';let s=0;for(let i=0;i<n;i++){gone=i;s=(s+gone)|0}return s}
		function tdw(n){try{return String(dw(n))}catch(e){return e.name+":"+(typeof gone)}}
		globalThis.miss=0;function rw(n,make){'use strict';let s=0;for(let i=0;i<n;i++){s=(s*5+i)|0;if((i&63)===0)miss=make?(globalThis.miss=s):s+1}return s}
		function trw(n,make){try{return String(rw(n,make))}catch(e){return e.name+":"+(typeof miss)}}`
	rounds := []string{`w(300)`, `w(300)`, `w(300)`, `String(sw(300))`, `String(sw(300))`, `String(sw(300))`, `lw(300)`, `lw(300)`, `tdw(300)`, `tdw(300)`,
		`Object.defineProperty(globalThis,"h",{writable:false});w(300)`,
		`Object.defineProperty(globalThis,"cur",{get(){return {v:7}},set(x){globalThis.seen=x.v},configurable:true});[w(300),seen].join()`,
		`delete globalThis.cur;w(300)`,
		`let late=1000;lw(300)`, `lw(300)`,
		`delete globalThis.gone;tdw(300)`,
		`trw(300)`, `trw(300)`, `trw(300)`, `delete globalThis.miss;trw(300)`, `trw(300,true)`, `delete globalThis.miss;trw(300,true)`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("w")).value().Object().fn().closure
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 2 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.entrySlow {
				t.Fatalf("w did not run natively: %+v", e)
			}
			// After its loop: the array, join's read and call.
			if n := e.ssaStats.hosts - hosts; n > 3 {
				t.Fatalf("w left native code %d times", n)
			}
		}
	}
	for _, name := range []string{"sw", "lw", "dw", "rw"} {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
			t.Fatalf("%s did not run natively %+v", name, e)
		}
	}
}

// Every level of native calls Go finishes after the innermost left native
// code goes on natively where its call returns to, not only the outermost
// (jitUnwindNative, runSSAIn): top calls mid natively, which calls leaf,
// whose loop reads a getter, which Go calls; Go finishes leaf, then mid
// goes on in its native code, in its own context,
// then top. A throw from leaf, and a frame of a construction, are made as
// before. Each answer is the interpreter's.
func TestJITSSAResumesEveryLevel(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var G={get g(){return 1}};
		function leaf(n){let s=0;for(let i=0;i<n;i++){s=(s*3+i)|0;if((i&63)===63)s=(s+G.g)|0}if(n===13)throw new Error("x"+s);return s}
		function mid(n){let t=n,a=n*3,b=n^5,c=n+7,d=n*11,g=n-2;
			for(let j=0;j<2;j++){t=(t*5+leaf(n+j+(t&3)))|0;t=(t+a+b+c+d+g)|0;a=(a+1)|0;b^=t;c=(c*3)|0;d=(d+b)|0;g=(g^c)|0}
			return (t+a+b+c+d+g)|0}
		function P(n){let t=0;for(let j=0;j<2;j++)t=(t*7+mid(n+j))|0;this.v=t}
		function top(n,m){let t=0;for(let k=0;k<n;k++){t=(t+mid(m))|0;t=(t+new P(m).v)|0}return t}
		function tryTop(n,m){try{return String(top(n,m))}catch(e){return e.message}}`
	rounds := []string{`tryTop(200,300)`, `tryTop(200,300)`, `tryTop(200,300)`, `tryTop(3,13)`, `tryTop(200,300)`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	if r.jit.resumedLevels == 0 {
		t.Fatalf("no level but the outermost went on natively: resumed %d, unwound %d", r.jit.resumed, r.jit.unwound)
	}
}

// Native code that calls through Go another function's native code shares
// the context with it (jitState.ssaCtx): an exit's PC must be read before
// Go runs anything. Here inner's last exit, a call near its end, is past
// the end of outer's code, which reading the context after the call took
// for outer's PC.
func TestJITSSANestedExits(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function h(o){o.n=1;return o.v}
		function inner(o,n){let s=0;for(let i=0;i<n;i++){s=(s+i*3)|0;s^=i;s=(s*7)|0;s=(s+o.v)|0;s=(s-i)|0}s=(s+1)|0;s=(s*3)|0;return s+h(o)}
		function outer(o,n){let t=0;for(let i=0;i<n;i++)t=(t+inner(o,4))|0;return t}
		var o={v:5,n:0};`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		wv, err := want.Run(compileForTest(t, `outer(o,50)`))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, `outer(o,50)`))
		if err != nil {
			t.Fatal(err)
		}
		if !jitSameValueForTest(gv, wv) {
			t.Fatalf("round %d: got %v, interpreter %v", i, gv, wv)
		}
	}
	for _, name := range []string{"inner", "outer"} {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		e := r.jit.cache[weak.Make(cl.fn)]
		if e == nil || e.ssa == nil || e.ssaStats.entries == 0 && e.link.in == 0 {
			t.Fatalf("%s did not run natively: %+v", name, e)
		}
		if name == "outer" && len(cl.fn.Code) > 30 {
			t.Fatalf("outer has %d instructions: inner's last exit must be past its end", len(cl.fn.Code))
		}
	}
}

// A small method the tree tier calls, which calls out again and returns,
// costs more native than in the tree tier: each call is an entry, an exit
// for its call, an entry after it and a return, around a few instructions.
// Its stretches, returns counted (their exit site, the return's block), do
// less than jitSSAMinWork, and the tree tier runs it from then on; a loop
// that does enough work stays native.
func TestJITSSAProfitReturns(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// The collector off after one collection, which the judgments are not
	// about: stretches its marking ended had not been judged.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	// Go calls bump, which reads arguments, and step, through apply with
	// an array: native code calls neither (jitNativeCallee).
	setup := `function T(){this.n=0;this.v=1}T.prototype.bump=function(){this.n+=arguments.length+1};
		T.prototype.step=function(k){let s=this.v;for(let i=0;i<2;i++)s=(s*3+i)|0;this.bump();return s+k};
		function heavy(n){let s=0;for(let i=0;i<n;i++){s=(s*31+i)|0;s^=s>>>7}return s}
		function drive(){let s=0;for(let i=0;i<300;i++)s=(s+t.step.apply(t,[i]))|0;return [s,t.n,heavy(500)].join()}
		var t=new T;`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	src := `drive()`
	for i := 0; i < 2; i++ {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	proto := r.global.getOwn(r.atoms.intern("T")).value().Object().getOwn(r.atoms.intern("prototype")).value().Object()
	step := proto.getOwn(r.atoms.intern("step")).value().Object().fn().closure
	if e := r.jit.hint(step); e == nil || e.ssa == nil || !e.entrySlow || e.ssaStats.entries > 2*jitSSAProbe {
		t.Fatalf("step kept running natively: %+v", e)
	}
	heavy := r.global.getOwn(r.atoms.intern("heavy")).value().Object().fn().closure
	if e := r.jit.cache[weak.Make(heavy.fn)]; e == nil || e.ssa == nil || e.entrySlow {
		t.Fatalf("heavy left native code: %+v", e)
	}
}

// Native calls that run out of contexts have more of them at once -- as
// many as there are -- up to jitContextsMax, those in use left where they
// are: a recursion 40 deep, which ran out of the 4 at first, then 8, 16 and
// 32, Go finishing every level, stays in native code once there are 64.
// One deeper than the most still goes
// through Go, and one without end throws the interpreter's RangeError.
// Each answer is the interpreter's.
func TestJITSSAContextsGrow(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function depth(n){if(n===0)return 0;return (depth(n-1)+n)|0}
		function drive(k,n){let s=0;for(let i=0;i<k;i++)s=(s+depth(n))|0;return s}
		function forever(n){return forever(n+1)+1}
		function tryForever(){try{return String(forever(0))}catch(e){return e.name}}`
	rounds := []string{`String(drive(200,40))`, `String(drive(200,40))`, `String(drive(200,40))`, `String(drive(200,40))`,
		`String(drive(200,40))`, `String(drive(50,150))`, `String(drive(200,40))`, `tryForever()`, `String(drive(200,40))`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		unwound := r.jit.unwound
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 4 || i == 8 {
			if n, want := len(r.jit.ssaCtxs), map[int]int{4: 16 * jitContexts, 8: jitContextsMax}[i]; n != want {
				t.Fatalf("round %d: %d contexts, want %d", i, n, want)
			}
			if n := r.jit.unwound - unwound; n != 0 {
				t.Fatalf("round %d: Go finished %d levels", i, n)
			}
		}
	}
}

// A call the new pipeline has seen call one compiled function is made in
// native code (mir's native calls), the callee's frame and context made
// there, as V8's code calls another's: a loop of such calls never leaves
// native code. Arguments missing are undefined and extra ones dropped; a
// method gets its receiver; recursion goes as deep as there are contexts
// (jitContexts, more once it needs them), then through Go. A callee that leaves native code -- for
// a builtin, a throw the caller catches, a speculation that fails -- has
// its frame and its callers' made by Go (jitUnwindNative), which finishes
// them. Each answer is the interpreter's.
func TestJITSSANativeCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, and a function all
	// of whose calls Go then makes is rightly demoted: not here.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function add(a,b){let s=a;for(let i=0;i<3;i++)s=(s*3+(b===undefined?5:b))|0;return s}
		function P(v){this.v=v}P.prototype.m=function(k){let s=this.v;for(let i=0;i<2;i++)s=(s+k*i)|0;return s};
		function sum(n){let t=0;for(let i=0;i<n;i++){t=(t+add(i,t&7))|0;t^=t>>>3}return t}
		function few(n){let t=0;for(let i=0;i<n;i++){t=(t+add(i))|0;t=(t+add(i,1,2))|0;t^=t>>>3}return t}
		function meth(o,n){let t=0;for(let i=0;i<n;i++){t=(t+o.m(i))|0;t^=t>>>2}return t}
		function fib(n){if(n<2)return n;let a=fib(n-1);let b=fib(n-2);for(let i=0;i<1;i++)a=a|0;return a+b}
		function floor(x){let s=0;for(let i=0;i<2;i++)s=(s+Math.floor(x/3))|0;return s}
		function useFloor(n){let t=0;for(let i=0;i<n;i++){t=(t+floor(i))|0;t^=t>>>3}return t}
		function thrower(x){let s=x;for(let i=0;i<2;i++)s=s*2;if(x===37)throw new Error("x"+s);return s}
		function catcher(n){let t=0;for(let i=0;i<n;i++){try{t=(t+thrower(i))|0}catch(e){t=(t+e.message.length)|0}t^=t>>>3}return t}
		function num(x){let s=0;for(let i=0;i<2;i++)s=s+x*2;return s}
		function deopt(xs,n){let t=0;for(let i=0;i<n;i++){t=t+num(xs[i%xs.length]);t=t|0}return t}
		var p=new P(3),xs=[1,2,3,4,5,6,7,8];`
	rounds := []string{
		`[sum(200),few(100),meth(p,200),fib(12),useFloor(100),catcher(60),deopt(xs,100)].join()`,
		`[sum(200),few(100),meth(p,200),fib(12),useFloor(100),catcher(60),deopt(xs,100)].join()`,
		`[sum(200),few(100),meth(p,200),fib(20),useFloor(100),catcher(60),deopt(xs,100)].join()`,
		`xs[3]='s';[sum(200),meth(new P(9),200),catcher(60),deopt(xs,100)].join()`,
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.hint(cl)
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts, entries uint64
		if e := entry("sum"); e != nil {
			hosts, entries = e.ssaStats.hosts, e.ssaStats.entries
		}
		if i == 2 {
			// A budget that does not run out: the interrupt check, a loop's,
			// is not a call leaving.
			r.backEdges = 1 << 30
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 2 {
			r.backEdges = backEdgeCheckInterval
			// Native, entered, and never leaving native code for its calls.
			if e := entry("sum"); e == nil || e.ssa == nil || len(e.nativeCalls) == 0 || e.entrySlow ||
				e.ssaStats.entries == entries || e.ssaStats.hosts != hosts {
				t.Fatalf("sum's calls left native code: %+v", e)
			}
		}
	}
	// The callees that left native code -- floor for Math.floor, thrower,
	// num with a string -- had their frames made by Go.
	if r.jit.unwound == 0 {
		t.Fatal("no native call's callee left native code")
	}
}

// Native calls pass and return references, and make frames in the VM's
// stack, while the collector runs continuously: natively while it does not
// mark, through Go while it does (abi.Encoding's WriteBarrier); a callee
// leaving native code, dbl given a string, has its callers resumed
// natively, marking or not. Nothing is lost or freed early; run with
// GODEBUG=gccheckmark=1 too.
func TestJITSSANativeCallsUnderGC(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	src := `function link(a,b){let n=a;for(let i=0;i<1;i++)n.next=b;return a}
		function pick(o,k){let x=o;for(let i=0;i<k;i++)x=x.next;return x}
		function build(n){let h={v:-1,next:null};for(let i=0;i<n;i++){h=link({v:i,pad:[i,i]},h)}return h}
		function dbl(x){return x*2}
		function walk(h,n){let s=0;for(let i=0;i<n;i++){s=(s+pick(h,i%5).v+dbl(i%37===36?''+i:i))|0}return s}
		let ok=true;
		for(let round=0;round<200;round++){
			const h=build(40);
			ok=ok&&walk(h,100)===walk(h,100)&&pick(h,39).v===0;
		}
		ok`
	v, err := r.Run(compileForTest(t, src))
	if err != nil || !v.IsBool() || !v.Truthy() {
		t.Fatalf("= %v, %v", v, err)
	}
	cl := r.global.getOwn(r.atoms.intern("walk")).value().Object().fn().closure
	if e := r.jit.hint(cl); e == nil || e.ssa == nil || len(e.nativeCalls) == 0 {
		t.Fatalf("walk made no native calls: %+v", e)
	}
}

// While the collector marks, native code makes its native calls, as V8's
// code does: what a call's setup writes -- the records, the callee's
// context and frame -- outlives none of what it is written from, and
// overwrites only what nothing reads again; the result, which outlives the
// callee's frame it comes from, is stored with Go's write barrier (mir's
// refStore), every register native code may keep given junk across it
// (jit.StoreRefClobber). None leaves native code, nor is a call of Go.
// Go had made each (abi.GoOpMarking).
func TestJITSSANativeCallsWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	setup := `var objs=[{v:1},{v:2},{v:3},{v:4}];
		function pick(i){let o=objs[i&3];for(let j=0;j<2;j++)o=objs[(i+j)&3];return o}
		function add(a,b){let s=a;for(let i=0;i<3;i++)s=(s*3+b)|0;return s}
		function sum(n){let t=0;for(let i=0;i<n;i++){t=(t+add(i,t&7))|0;t^=t>>>3;t=(t*5+i+pick(i).v)|0;t^=t>>>7}return t}`
	want := New(Config{})
	defer want.Close()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	wv, err := want.Run(compileForTest(t, `sum(300)`))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.Run(compileForTest(t, `sum(300)`)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("sum")).value().Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil || len(e.nativeCalls) == 0 {
		t.Fatalf("sum was not compiled to call natively: %+v", e)
	}
	hosts, goCalls, made := e.ssaStats.hosts, e.ssaStats.goCalls, r.jit.goCalls
	v, err := r.Run(compileForTest(t, `sum(300)`))
	if err != nil || v.Number() != wv.Number() {
		t.Fatalf("sum(300) = %v, %v; interpreter %v", v, err, wv)
	}
	if e.ssaStats.hosts != hosts || r.jit.goCalls != made || e.ssaStats.goCalls != goCalls || e.entrySlow || len(e.nativeCalls) < 2 {
		t.Fatalf("sum's native calls while the collector marked: %d exits, %d calls of Go, %d counted, entrySlow %v",
			e.ssaStats.hosts-hosts, r.jit.goCalls-made, e.ssaStats.goCalls-goCalls, e.entrySlow)
	}
}

// A native call made while the collector marks whose callee throws is
// left for Go to finish at the call's exit, which writes the frame from
// the call's state, as when it does not mark; Go had made such calls then
// (markingCall), the operands saved across its call with every other
// register. The error, its stack included, is the interpreter's.
func TestJITSSAMarkingCallThrows(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	setup := `var bad=-1;
		function g(x,o){let s=x;for(let i=0;i<2;i++)s=(s+o.k)|0;if(x===bad)throw new Error('at '+x+' '+o.k);return s}
		function f(n){let t=0;const o={k:3};for(let i=0;i<n;i++){t=(t+g(i,o)+o.k)|0;t^=t>>>3}return t}
		function h(n){bad=n>>1;try{return f(n)}catch(e){return e.message+e.stack}finally{bad=-1}}`
	want := New(Config{})
	defer want.Close()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if _, err := r.Run(compileForTest(t, `f(300)`)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	if e := r.jit.hint(cl); e == nil || e.ssa == nil || len(e.nativeCalls) == 0 {
		t.Fatalf("f was not compiled to call natively: %+v", e)
	}
	for _, src := range []string{`String(h(300))`, `String(f(300))`, `String(h(301))`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.Run(compileForTest(t, src))
		if err != nil || v.String().Go() != wv.String().Go() {
			t.Fatalf("%s = %v, %v; interpreter %v", src, v, err, wv)
		}
	}
}

// The collector may begin marking while a native callee runs, at a call
// of Go it makes: its calls of Go are the callee's to be judged by
// (jitCalleeProfit), its constructions after it native all the same, as
// they had been Go's (markingCall), and not counted. Here mid, called
// natively, calls parseInt, a call of Go at which marking begins, then
// constructs a P: mid counts its 600 calls of parseInt alone. The answer is
// the interpreter's.
func TestJITSSAMarkingBeginsInNativeCallee(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	marking := jitMarkingForTest(t)
	*marking = 0
	setup := `function add(a,b){let s=a;for(let i=0;i<3;i++)s=(s*3+b)|0;return s}
		function P(x){this.x=x;for(let i=0;i<2;i++)this.x=(this.x+i)|0}
		function mid(i,t){let s=0;for(let j=0;j<2;j++)s=(s+parseInt('7')+new P(i+j).x)|0;return s}
		function sum(n){let t=0;for(let i=0;i<n;i++){t=(t+add(i,t&7))|0;t^=t>>>3;t=(t*5+mid(i,t))|0;t^=t>>>7}return t}`
	want := New(Config{})
	defer want.Close()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	wv, err := want.Run(compileForTest(t, `sum(300)`))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if v, err := r.Run(compileForTest(t, `sum(300)`)); err != nil || v.Number() != wv.Number() {
			t.Fatalf("round %d: sum(300) = %v, %v; interpreter %v", i, v, err, wv)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		e := r.jit.hint(cl)
		if e == nil || e.ssa == nil || len(e.nativeCalls) == 0 {
			t.Fatalf("%s was not compiled to call natively: %+v", name, e)
		}
		return e
	}
	e, mid := entry("sum"), entry("mid")
	hosts, midCalls := e.ssaStats.hosts, mid.ssaStats.calleeGoCalls
	jitGoCallWork = func() { *marking = 1 }
	defer func() { jitGoCallWork = nil }()
	v, err := r.Run(compileForTest(t, `sum(300)`))
	if err != nil || v.Number() != wv.Number() {
		t.Fatalf("sum(300) = %v, %v; interpreter %v", v, err, wv)
	}
	if e.ssaStats.hosts != hosts || mid.ssaStats.calleeGoCalls-midCalls != 600 {
		t.Fatalf("marking begun in mid: %d exits, mid counted %d calls of Go, not 600", e.ssaStats.hosts-hosts, mid.ssaStats.calleeGoCalls-midCalls)
	}
}

// The marking loops (jitMarkingSources) under the collector's real marking,
// its cycles back to back: the objects Go takes and the pointers it stores
// for native code while it marks are the collector's to see, as the
// interpreter's are -- every answer is the interpreter's, and the code
// leaves native code no more for the marking: under GOGC=5 the RegExp
// literals' loops had left at most iterations. GODEBUG=gccheckmark=1 has
// the collector check that it marked everything reachable.
func TestJITSSAMarkingUnderGC(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	for _, source := range jitMarkingSources {
		want := New(Config{})
		_, err := want.Run(compileForTest(t, source))
		wv, err2 := want.Run(compileForTest(t, `f(300)`))
		want.Close()
		if err != nil || err2 != nil {
			t.Fatalf("interpreter: %v, %v; %s", err, err2, source)
		}
		r := jitRuntimeForTest(t, Config{JIT: true})
		if _, err := r.Run(compileForTest(t, source)); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			v, err := r.Run(compileForTest(t, `f(300)`))
			if err != nil || v.Number() != wv.Number() {
				t.Fatalf("round %d: JIT %v, %v; interpreter %v; %s", i, v, err, wv, source)
			}
		}
		if st := r.JITStats(); st.SSAEntries == 0 {
			t.Fatalf("nothing ran natively: %+v; %s", st, source)
		}
	}
}

// No exit the collector's marking makes in a native callee: here marking
// begins in mid, called natively from sum, at its call of parseInt, and
// its constructions of G -- a receiver from a pool, more operands than a
// call of Go passes -- which had each left native code then, at mid's
// level (two exits, unjudged, counted), are made natively.
func TestJITSSAMarkingBeginsBeforeWideConstruction(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	marking := jitMarkingForTest(t)
	*marking = 0
	setup := `function add(a,b){let s=a;for(let i=0;i<3;i++)s=(s*3+b)|0;return s}
		function G(a,b,c,d,e,f,h,k,l,m,o,p,q,u,w,x){let t=a;for(let j=0;j<2;j++)t=(t+b+c+d+e+f+h+k+l+m+o+p+q+u+w+x)|0;this.t=t}
		function mid(i,t,big){let s=0;for(let j=0;j<2;j++){s=(s+parseInt('7')+add(i+j,t&7))|0;if(big)s=(s+new G(i,1,2,3,4,5,6,7,0,0,0,0,0,0,0,0).t)|0}return s}
		function sum(n){let t=0;for(let i=0;i<n;i++){t=(t+add(i,t&7))|0;t^=t>>>3;t=(t*5+mid(i,t,i<1))|0;t^=t>>>7}return t}`
	want := New(Config{})
	defer want.Close()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	wv, err := want.Run(compileForTest(t, `sum(300)`))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if v, err := r.Run(compileForTest(t, `sum(300)`)); err != nil || v.Number() != wv.Number() {
			t.Fatalf("round %d: sum(300) = %v, %v; interpreter %v", i, v, err, wv)
		}
	}
	exits := r.jit.hosts
	jitGoCallWork = func() { *marking = 1 }
	defer func() { jitGoCallWork = nil }()
	v, err := r.Run(compileForTest(t, `sum(300)`))
	if err != nil || v.Number() != wv.Number() {
		t.Fatalf("sum(300) = %v, %v; interpreter %v", v, err, wv)
	}
	if n := r.jit.hosts - exits; n != 0 {
		t.Fatalf("%d exits once the collector marked, not 0", n)
	}
}

// jitMarkingSources are loops that store pointers natively where the
// collector's marking had them leave for Go: native calls -- plain, a
// method's, through Function.prototype.call, a construction's with its
// receiver from a pool -- an inlined construction's receiver, object,
// array and RegExp literals, a property added along a transition, push and
// pop, a built-in's construction. Each defines f, and f(300) is a number.
var jitMarkingSources = []string{
	`function add(a,b){let s=a;for(let i=0;i<3;i++)s=(s*3+b)|0;return s}
	function P(x){this.x=x;for(let i=0;i<2;i++)this.x=(this.x+i)|0}
	P.prototype.get=function(){let s=this.x;for(let i=0;i<2;i++)s=(s+1)|0;return s};
	const box={k:5,m(i){let s=i;for(let j=0;j<2;j++)s=(s+this.k)|0;return s}};
	function f(n){let t=0;for(let i=0;i<n;i++){t=(t+add(i,t&7))|0;t=(t+add.call(null,i,3))|0;t=(t+box.m(i))|0;const p=new P(i);t=(t+p.get()+p.x)|0}return t}`,
	`function Q(x,y){this.x=x;this.y=y}
	function f(n){let t=0;const a=[];for(let i=0;i<n;i++){const q=new Q(i,a);const o={a:i,b:q};o.c=o;o.d=q;const l=[i,o,'s',q];a.push(l);if(a.length>4)a.pop();const z=new Array();z.push(o);const r=/b/g;r.lastIndex=1;t=(t+o.a+o.c.a+o.d.x+l[0]+l[3].x+q.y.length+z.length+r.lastIndex)|0}return t}`,
}

// While the collector marks, native code leaves for Go nowhere it stores
// a pointer: it stores it with Go's write barrier (jit's storeRef) -- a
// property's value, a property added along a transition, its shape too, a
// native call's result, a pool's object and what is written into it -- as
// V8's code calls its write barrier. The stores fill every
// register native code may keep with junk first (jit.StoreRefClobber).
// Each answer is the interpreter's, and once the code is compiled for its
// calls, a run leaves native code nowhere.
func TestJITSSANoExitsWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	marking := jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	for _, source := range jitMarkingSources {
		want := New(Config{})
		_, err := want.Run(compileForTest(t, source))
		wv, err2 := want.Run(compileForTest(t, `f(300)`))
		want.Close()
		if err != nil || err2 != nil {
			t.Fatalf("interpreter: %v, %v; %s", err, err2, source)
		}
		r := jitRuntimeForTest(t, Config{JIT: true})
		if _, err := r.Run(compileForTest(t, source)); err != nil {
			t.Fatal(err)
		}
		// Rounds enough for f to settle, each deoptimization compiled again
		// for once a round has spent its budget (jitTierUp); the last but
		// one with the collector idle, which the last, marking, leaves
		// native code no more often than -- a pool run out leaves at its
		// construction either way, for Go to fill it.
		idle := uint64(0)
		for i := 0; i < 10; i++ {
			*marking = 1
			if i == 8 {
				*marking = 0
			}
			var hosts uint64
			if r.jit != nil {
				hosts = r.jit.hosts
			}
			v, err := r.Run(compileForTest(t, `f(300)`))
			if err != nil || v.Number() != wv.Number() {
				t.Fatalf("round %d: JIT %v, %v; interpreter %v; %s", i, v, err, wv, source)
			}
			switch i {
			case 8:
				idle = r.jit.hosts - hosts
			case 9:
				if r.jit.hosts-hosts > idle {
					t.Fatalf("while the collector marked, %d exits, %d with it idle; %s", r.jit.hosts-hosts, idle, source)
				}
			}
		}
	}
}

// A call that calls several functions calls each natively, up to
// jitCallTargets, as V8's polymorphic call feedback does: one more leaves
// for Go. A callee with no loop, which Go would not enter (LowerSSA), is
// compiled for its native callers alone (LowerSSAInline); leaf's calls keep
// it from being inlined. One that leaves
// native code on too many of its calls (jitUnwindShare) is called by Go
// again. And Go finishes native calls that left native code inside native
// calls of frames it is finishing, its unwinds nested. Each answer is the
// interpreter's.
func TestJITSSANativeCallTargets(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, and a function all
	// of whose calls Go then makes is rightly demoted: not here.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function A(v){this.v=v}A.prototype.get=function(k){this.n=k;return this.v+k};
		function B(v){this.w=v;this.v=v*2}B.prototype.get=function(k){this.n=k;return this.v*k};
		function C(v){this.v=v}C.prototype.get=function(k){return (this.v^k)+this.v};
		function D(){}D.prototype.get=function(k){this.k=k;return k+1};
		function E(){}E.prototype.get=function(k){return -k};
		function poly(os,n){let t=0;for(let i=0;i<n;i++){t=(t+os[i%os.length].get(i))|0;t^=t>>>3}return t}
		function inc(x){x.c=(x.c|0)+1;return x.c}
		function leaf(x){return inc(x)+inc(x)}
		function useLeaf(o,n){let t=0;for(let i=0;i<n;i++)t=(t+leaf(o))|0;return t}
		var L={a:0,get n(){return String(this.a).length}};
		function leave(i){L.a=i;return (L+String(L.a)).length}
		function useLeave(n){let t=0;for(let i=0;i<n;i++)t=(t+leave(i))|0;return t}
		function two(n,k){if(n===0){if(k%16!==0)return 1;L.a=k;return (L+String(L.a)).length}let a=two(n-1,k);let b=two(n-1,k);return a+b}
		function drive(n){let t=0;for(let i=0;i<n;i++)t=(t+two(4,i))|0;return t}
		var four=[new A(1),new B(2),new C(3),new D],five=[new A(4),new B(5),new C(6),new D,new E],lo={c:0};`
	src := `[poly(four,400),useLeaf(lo,300),useLeave(300),drive(64)].join()`
	rounds := []string{src, src, src, src, `[poly(five,400),poly(four,40),drive(64)].join()`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		// The calls made natively to poly's callees: its exits are its
		// reads of get, which one shape answers natively.
		calledNatively := func() (n uint64) {
			for _, x := range entry("poly").nativeCalls {
				n += r.jit.cache[weak.Make(x.cl.fn)].link.in
			}
			return n
		}
		var polyCalls, leafHosts uint64
		if i == 3 {
			polyCalls, leafHosts = calledNatively(), entry("useLeaf").ssaStats.hosts
		}
		unwound := r.jit.unwound
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			if n := calledNatively() - polyCalls; len(entry("poly").nativeCalls) != 4 || n != 400 {
				t.Fatalf("poly called its four callees natively %d times of 400", n)
			}
			if e := entry("leaf"); e == nil || e.ssa == nil || !e.ssaCallee || entry("useLeaf").ssaStats.hosts != leafHosts {
				t.Fatalf("leaf, with no loop, was not called natively: %+v", e)
			}
			if e := entry("leave"); e == nil || e.nativeBackoff == 0 {
				t.Fatalf("leave, leaving native code at every call, was never called by Go instead: %+v", e)
			}
			if r.jit.unwound == unwound {
				t.Fatal("two's calls never left native code")
			}
		}
	}
	// A callee whose code is gone when its caller is compiled again is not
	// compiled then, inside the caller's compile, whose workspaces it would
	// share: the call leaves for Go.
	leaf := r.global.getOwn(r.atoms.intern("leaf")).value().Object().fn().closure
	if !r.jit.dropEntry(weak.Make(leaf.fn), entry("leaf")) {
		t.Fatal("leaf's code was not dropped")
	}
	entry("useLeaf").inlineReopt = true
	src = `[useLeaf(lo,300),useLeaf(lo,300)].join()`
	wv, err := want.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	gv, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gv.String().Go(), wv.String().Go(); got != want {
		t.Fatalf("after leaf's code was dropped: got %s, interpreter %s", got, want)
	}
}

// A native callee the cache drops -- to make room, its code closed -- and
// compiles again is called natively again by its callers' code, compiled
// before, through the function's link (jitLink), as V8's code calls a
// function through the code it holds now: no caller is compiled again for
// it, nor leaves for Go at every call (abi.ExitEnter) for want of the
// target its code was compiled with, which had made EarleyBoyer's slow
// runs. While the function has no code, a call has Go run it. A link
// outlives its entries only while code calls through it.
func TestJITSSANativeCallsSurviveEviction(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Native calls are Go's while the collector marks.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function leaf(x,o){let s=x;for(let i=0;i<2;i++)s=(s*3+o.k)|0;return s}
		function M(k){this.k=k}
		M.prototype.m=function(x){let s=x;for(let i=0;i<2;i++)s=(s+this.k)|0;return s};
		function caller(n){let t=0;const o=new M(5);for(let i=0;i<n;i++){t=(t+leaf(i,o)+o.m(i))|0;t^=t>>>3}return t}`
	want := New(Config{})
	defer want.Close()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	wv, err := want.Run(compileForTest(t, `caller(300)`))
	if err != nil {
		t.Fatal(err)
	}
	run := func(round string) {
		t.Helper()
		v, err := r.Run(compileForTest(t, `caller(300)`))
		if err != nil || v.Number() != wv.Number() {
			t.Fatalf("%s: caller(300) = %v, %v; interpreter %v", round, v, err, wv)
		}
	}
	for i := 0; i < 4; i++ {
		run("warm-up")
	}
	fnOf := func(name string) *bytecode.Function {
		return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
	}
	method := r.global.getOwn(r.atoms.intern("M")).value().Object().getOwn(r.atoms.intern("prototype")).value().Object().getOwn(r.atoms.intern("m")).value().Object().fn().closure.fn
	caller := r.jit.cache[weak.Make(fnOf("caller"))]
	if caller == nil || caller.ssa == nil || len(caller.ssaLinks) != 2 {
		t.Fatalf("caller does not call leaf and m natively: %+v", caller)
	}
	code, reopts := caller.ssa, r.jit.reoptimized
	for _, fn := range []*bytecode.Function{fnOf("leaf"), method} {
		key := weak.Make(fn)
		e := r.jit.cache[key]
		if e == nil || e.ssa == nil || !slices.Contains(caller.ssaLinks, e.link) {
			t.Fatalf("%s's link is not the one caller calls through: %+v", fn.Name, e)
		}
		l := e.link
		if !r.jit.dropEntry(key, e) {
			t.Fatalf("%s's code was not dropped", fn.Name)
		}
		if l.entry != 0 || r.jit.links[key] != l {
			t.Fatalf("%s's link after its entry was dropped: %+v", fn.Name, l)
		}
	}
	// The first run after has Go run each callee, which compiles it again.
	run("after the drop")
	for _, fn := range []*bytecode.Function{fnOf("leaf"), method} {
		e := r.jit.cache[weak.Make(fn)]
		if e == nil || e.ssa == nil || e.link.entry == 0 || !slices.Contains(caller.ssaLinks, e.link) {
			t.Fatalf("%s was not compiled again under its link: %+v", fn.Name, e)
		}
	}
	hosts := caller.ssaStats.hosts
	leafIn, mIn := r.jit.cache[weak.Make(fnOf("leaf"))].link.in, r.jit.cache[weak.Make(method)].link.in
	run("relinked")
	if caller.ssa != code || r.jit.reoptimized != reopts {
		t.Fatal("caller was compiled again for its callees' new code")
	}
	if n := caller.ssaStats.hosts - hosts; n != 0 {
		t.Fatalf("caller left native code %d times calling its callees' new code", n)
	}
	if l, m := r.jit.cache[weak.Make(fnOf("leaf"))].link.in-leafIn, r.jit.cache[weak.Make(method)].link.in-mIn; l != 300 || m != 300 {
		t.Fatalf("callees' new code called natively %d and %d times of 300", l, m)
	}
	// Once no code calls through a link and no entry owns it, it is gone.
	callerKey, leafKey := weak.Make(fnOf("caller")), weak.Make(fnOf("leaf"))
	if !r.jit.dropEntry(callerKey, caller) || !r.jit.dropEntry(leafKey, r.jit.cache[leafKey]) {
		t.Fatal("code was not dropped")
	}
	if _, ok := r.jit.links[leafKey]; ok {
		t.Fatal("leaf's link outlived its entry and its callers' code")
	}
	if l := r.jit.links[weak.Make(method)]; l == nil || l.refs != 0 || l.owner == nil {
		t.Fatalf("m's link, owned by its entry, called through by no code: %+v", l)
	}
}

// A native callee with no loop, which Go does not compile (LowerSSA) and
// only native callers call natively (LowerSSAInline), dropped by the cache,
// is compiled for them again at the first call Go then makes for them
// (abi.ExitEnter, jitEnterCallee): its callers' code would otherwise leave
// for Go at each call from then on, which had been most of EarleyBoyer's
// slow runs (sc_assq, one_way_unify1_nboyer).
func TestJITSSALooplessCalleeRecompiledAfterEviction(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	// leaf's captured binding keeps it from being inlined (jitInlinable).
	setup := `var leaf=(function(){let k=3;return function leaf(x){x.c=(x.c+k)|0;return x.c}})();
		function useLeaf(o,n){let t=0;for(let i=0;i<n;i++)t=(t+leaf(o))|0;return t}
		var lo={c:0};`
	want := New(Config{})
	defer want.Close()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	run := func(round string) {
		t.Helper()
		wv, err := want.Run(compileForTest(t, `useLeaf(lo,300)`))
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.Run(compileForTest(t, `useLeaf(lo,300)`))
		if err != nil || v.Number() != wv.Number() {
			t.Fatalf("%s: useLeaf(lo,300) = %v, %v; interpreter %v", round, v, err, wv)
		}
	}
	for i := 0; i < 4; i++ {
		run("warm-up")
	}
	fnOf := func(name string) *bytecode.Function {
		return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
	}
	key := weak.Make(fnOf("leaf"))
	e, caller := r.jit.cache[key], r.jit.cache[weak.Make(fnOf("useLeaf"))]
	if e == nil || e.ssa == nil || !e.ssaCallee || caller == nil || !slices.Contains(caller.ssaLinks, e.link) {
		t.Fatalf("leaf, with no loop, is not called natively: %+v; caller %+v", e, caller)
	}
	if !r.jit.dropEntry(key, e) {
		t.Fatal("leaf's code was not dropped")
	}
	run("after the drop")
	e = r.jit.cache[key]
	if e == nil || e.ssa == nil || !e.ssaCallee || e.link.entry == 0 || !slices.Contains(caller.ssaLinks, e.link) {
		t.Fatalf("leaf was not compiled again for its native callers: %+v", e)
	}
	hosts, in := caller.ssaStats.hosts, e.link.in
	run("relinked")
	if caller.ssaStats.hosts != hosts || e.link.in-in != 300 {
		t.Fatalf("useLeaf left native code %d times, called leaf natively %d times of 300", caller.ssaStats.hosts-hosts, e.link.in-in)
	}
}

// A function that does not compile for native callers alone is not
// compiled again at every call Go makes for them (jitEnterCallee,
// jitNativeCallee): the refusal is kept (calleeRefused), as LowerSSA's is
// by its entry.
func TestJITSSANativeCalleeRefusalKept(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function g(x){try{return x.a}catch(e){return 0}}g({a:1})`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure
	if _, err := r.jitLower(cl.fn, jitLowerInline); err == nil {
		t.Skip("g compiles for native callers now: the test needs one that does not")
	}
	if e := r.jitNativeCallee(cl); e != nil {
		t.Fatalf("g compiled for native callers: %+v", e)
	}
	e := r.jit.cache[weak.Make(cl.fn)]
	if e == nil || e.ssa != nil || !e.calleeRefused {
		t.Fatalf("g's refusal for native callers was not kept: %+v", e)
	}
}

// A native call whose callee has no code native callers may call -- one
// they stopped calling natively for leaving too often (notNative), as the
// test marks them -- is Go's, which makes it, native code going on after it
// (mir's call): a call, a method call with its receiver,
// constructions whose callee returns nothing or a primitive, and a callee
// that throws. One whose operands Go's arguments cannot hold has Go run
// the callee from its start, and goes on natively after the call
// (abi.ExitEnter), its own frame never written.
func TestJITSSAEntersCalleesWithoutCode(t *testing.T) {
	// Native calls are Go's while the collector marks: it does not, so that
	// what the test counts is the calls'.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function h(x){return String(x).length+x}
		function M(k){this.k=k}
		M.prototype.m=function(x){return String(x).length+this.k}
		function C(x){this.v=String(x)}
		function P(x){this.v=String(x);return 7}
		function thrower(x){throw new Error("bad "+x)}
		function bad(x){if(x===250)thrower(x);return String(x).length}
		function calls(n){let s=0;for(let i=0;i<n;i++)s=(s+h(i))|0;return s}
		function methods(o,n){let s=0;for(let i=0;i<n;i++)s=(s+o.m(i))|0;return s}
		function constructs(n){let s=0;for(let i=0;i<n;i++){s=(s+new C(i).v.length+new P(i).v.length)|0}return s}
		function throws(n){let s=0;for(let i=0;i<n;i++)s=(s+bad(i))|0;return s}
		function wide(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p){return String(a).length+b+c+d+e+f+g+h+i+j+k+l+m+n+o+p}
		function wides(n){let s=0;for(let i=0;i<n;i++)s=(s+wide(i,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15))|0;return s}
		var o=new M(3)`
	for _, tc := range []struct {
		name, run string
		caller    string
		entered   bool
	}{
		{"call", `[calls(300),calls(300)]`, "calls", false},
		{"method", `[methods(o,300),methods(o,300)]`, "methods", false},
		{"construct", `[constructs(300),constructs(300)]`, "constructs", false},
		{"throw", `var r=[];for(let k=0;k<3;k++){try{r.push(throws(300))}catch(e){r.push(e.message)}}r`, "throws", false},
		{"wide", `[wides(300),wides(300)]`, "wides", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := setup + ";" + tc.run + ".map(String).join()"
			want := New(Config{})
			defer func() { want.Close(); want.ReleaseClosed() }()
			if _, err := want.Run(compileForTest(t, setup)); err != nil {
				t.Fatal(err)
			}
			wv, err := want.Run(compileForTest(t, tc.run+".map(String).join()"))
			if err != nil {
				t.Fatal(err)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			// The callees the caller calls natively: native callers stop.
			cl := r.global.getOwn(r.atoms.intern(tc.caller)).value().Object().fn().closure
			e := r.jit.hint(cl)
			if e == nil || len(e.nativeCalls) == 0 {
				t.Fatalf("%s calls nothing natively: %+v", tc.caller, e)
			}
			// Its callees left native code at every call so far, which may
			// have made Go stop entering it (jitSSAProfit): it does again.
			e.entrySlow, e.ssaStats = false, jitSSAStats{}
			for _, x := range e.nativeCalls {
				if ce := r.jit.cache[weak.Make(x.cl.fn)]; ce != nil {
					ce.notNative, ce.link.entry, ce.nativeRetry = true, 0, 0
				}
			}
			entered, goCalls := r.jit.entered, r.jit.goCalls
			gv, err := r.Run(compileForTest(t, tc.run+".map(String).join()"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := gv.String().Go(), wv.String().Go(); got != want {
				t.Fatalf("got %s, interpreter %s", got, want)
			}
			switch {
			case tc.entered && r.jit.entered == entered:
				t.Fatalf("no native call ran its callee from its start: %+v", r.JITStats())
			case !tc.entered && (r.jit.goCalls == goCalls || r.jit.entered != entered):
				t.Fatalf("Go made %d of the calls, %d ran from their callees' start: %+v", r.jit.goCalls-goCalls, r.jit.entered-entered, r.JITStats())
			}
		})
	}
}

// A native caller whose code is dropped from the cache while Go finishes a
// callee that left native code -- one it called natively, or one it inlined
// -- Go running what dropped it, goes on after the call from the frame the
// call wrote, with its operands, not in its code, which is gone: resuming
// there, which the code being the entry's still allowed, ran the call again
// over that frame, and the frame's operands were taken as those at the last
// entry. The cache does not drop code to make room while a run of it may go
// on so (inUse).
func TestJITSSACallerDroppedWhileItsCalleeLeaves(t *testing.T) {
	// Native calls are Go's while the collector marks.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	for _, tc := range []struct{ name, setup, run, last string }{
		// The callee leaves at an addition of an object, whose conversion
		// Go makes, and the interpreter, finishing it, calls host, which
		// drops the caller's code.
		{"call", `var H={};function leave(x){let y=x;for(let j=0;j<1;j++)y=y*3+1;if(x===200){y+=(H+"").length;host()}return y}
			function caller(n){let s=0;for(let i=0;i<n;i++)s=(s+leave(i)*2)|0;return s}`,
			`[caller(300),caller(300)].join()`, `[caller(300),caller(300)].join()`},
		// The inlined callee leaves at its read of v, which meets a getter
		// in b, which drops the caller's code.
		{"inlined", `function O(k){this.k=k}O.prototype.leave=function(x){return x.v*3+this.k};var o=new O(1);
			function caller(a,n){let s=0;for(let i=0;i<n;i++)s=(s+o.leave(a[i])*2)|0;return s}
			var a=[];for(let i=0;i<300;i++)a.push({v:i});var b=a.slice();b[200]={get v(){host();return 200}}`,
			`[caller(a,300),caller(a,300)].join()`, `[caller(b,300),caller(a,300)].join()`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := New(Config{})
			defer func() { want.Close(); want.ReleaseClosed() }()
			want.global.setOwnRaw(want.atoms.intern("host"), want.NewFunction("host", 0,
				func(*Runtime, Value, []Value) (Value, error) { return Undefined, nil }), propDefault)
			r := jitRuntimeForTest(t, Config{JIT: true})
			var drop, dropped, inUse bool
			r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 0,
				func(rt *Runtime, _ Value, _ []Value) (Value, error) {
					if !drop {
						return Undefined, nil
					}
					drop = false
					cl := rt.global.getOwn(rt.atoms.intern("caller")).value().Object().fn().closure
					e := rt.jit.hint(cl)
					inUse = e != nil && rt.jit.inUse(e)
					dropped = e != nil && rt.jit.dropEntry(weak.Make(cl.fn), e)
					return Undefined, nil
				}), propDefault)
			for _, rt := range []*Runtime{want, r} {
				if _, err := rt.Run(compileForTest(t, tc.setup)); err != nil {
					t.Fatal(err)
				}
			}
			wv, err := want.Run(compileForTest(t, tc.last))
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := r.Run(compileForTest(t, tc.run)); err != nil {
					t.Fatal(err)
				}
			}
			cl := r.global.getOwn(r.atoms.intern("caller")).value().Object().fn().closure
			e := r.jit.hint(cl)
			if e == nil || e.ssa == nil {
				t.Fatalf("caller was not compiled: %+v", e)
			}
			if tc.name == "call" && len(e.nativeCalls) == 0 || tc.name == "inlined" && len(e.ssaInlined) == 0 {
				t.Fatalf("caller does not make the call as %s: %+v", tc.name, e)
			}
			drop = true
			unwound := r.jit.unwound
			gv, err := r.Run(compileForTest(t, tc.last))
			if err != nil {
				t.Fatal(err)
			}
			if !dropped || r.jit.unwound == unwound {
				t.Fatalf("caller's code was not dropped (%v) while Go finished its callee: %+v", dropped, r.JITStats())
			}
			if !inUse {
				t.Fatal("the cache would drop the code of a run Go is inside")
			}
			if got, want := gv.String().Go(), wv.String().Go(); got != want {
				t.Fatalf("got %s, interpreter %s", got, want)
			}
		})
	}
}

// What a native call's callee ran with -- its receiver, its closure, its
// result, its keep cells, here m's, which keeps what g returns -- is not
// kept once Go's run of the caller ends, nor the caller's own receiver and
// captured bindings: the contexts held them, frames gone once they
// returned, and kept alive whatever they reached for as long as the
// runtime lived.
func TestJITSSACalleeContextsKeepNothing(t *testing.T) {
	// Native calls are Go's while the collector marks.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function M(k){this.k=k;this.q={k:k}}
		M.prototype.g=function(){let r=null;for(let j=0;j<2;j++)r=this.q;return r};
		M.prototype.m=function(x){let t=0;for(let j=0;j<2;j++)t+=x+this.g().k;return t};
		M.prototype.sum=(function(){let c=1;return function(n){let s=0;for(let i=0;i<n;i++)s=(s+this.m(i)+c)|0;return s}})();
		var sum=M.prototype.sum,o=new M(3)`
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.Run(compileForTest(t, `o.sum(300)`)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("sum")).value().Object().fn().closure
	if e := r.jit.hint(cl); e == nil || e.ssa == nil || len(e.nativeCalls) == 0 || e.ssaStats.entries == 0 {
		t.Fatalf("sum calls nothing natively: %+v", e)
	}
	o := weak.Make(r.global.getOwn(r.atoms.intern("o")).value().Object())
	if _, err := r.Run(compileForTest(t, `o=undefined`)); err != nil {
		t.Fatal(err)
	}
	for i := range r.jit.ssaCtxs {
		if c := r.jit.ssaCtxs[i]; c.Closure != nil || c.Upvalues != nil || c.This.Ref != nil || c.RetValue.Ref != nil {
			t.Fatalf("context %d keeps what code ran with: %+v %+v %v %v", i, c.This, c.RetValue, c.Closure, c.Upvalues)
		}
		for k, x := range r.jit.ssaCtxs[i].Keep {
			if x.Ref != nil {
				t.Fatalf("context %d keeps a value in keep cell %d", i, k)
			}
		}
	}
	runtime.GC()
	runtime.GC()
	if o.Value() != nil {
		t.Fatal("the receiver of sum and of its native calls outlived them")
	}
}

// A string constant is pushed by Go (jitHost), and a throw left to the
// interpreter, which throws it, as V8's code calls its runtime to throw: a
// function with either, a message it throws on a cold path most often,
// compiles, and its loop after the constant runs natively, not in the
// interpreter. RayTrace's renderScene, which reads "5,5" before its pixel
// loops and throws if the scene came out wrong, was refused for both.
func TestJITSSAStringConstantsAndThrows(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function scale(v,n){if(v===undefined)throw "need a number, n="+n;const sep=",;";let s=0;for(let i=0;i<n;i++)s=(s+v*i)|0;return s*sep.length+n}
		function guarded(v,n){try{return scale(v,n)}catch(e){return "caught "+e}}`
	rounds := []string{`String(scale(3,300))`, `String(scale(3,300))`, `String(scale(5,300))`, `guarded(undefined,7)`, `String(guarded(2,300))`, `String(scale(7,300))`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("scale")).value().Object().fn().closure
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		interpreted := r.jit.interpreted
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 5 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
				t.Fatalf("scale did not run natively: %+v", e)
			}
			if n := r.jit.interpreted - interpreted; n != 0 {
				t.Fatalf("scale went on in the interpreter %d times", n)
			}
		}
	}
}

// A construction of a constructor that returns nothing is inlined, as V8
// inlines one: the receiver comes from the site's pool, the body runs
// inlined with it, and it is the result. An exit inside the body -- W's
// multiplication meets a string, after W wrote a -- makes W's frame, and
// the construction's result is the receiver, as the interpreter's; a pool
// that runs out is filled again at the call without counting against the
// inlining, so that steady rounds compile nothing again; and the pool is
// made again for a new prototype.
func TestJITSSAInlinedConstructions(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function W(x){this.a=x;this.b=x*2}
		W.prototype.sum=function(){return this.a+this.b};
		function build(xs,n){let s=0,w;for(let i=0;i<n;i++){w=new W(xs[i%xs.length]);s=(s+w.b)|0}return [s,w.sum(),Object.getPrototypeOf(w)===W.prototype].join()}
		var nums=[1,2,3,4],mixed=[1,2,"3",{valueOf(){return 9}}]`
	src := `build(nums,300)`
	rounds := []string{src, src, src, src, src, src, `build(mixed,300)`, src, `W.prototype={sum(){return -1}};` + src, src}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("build")).value().Object().fn().closure
	w := r.global.getOwn(r.atoms.intern("W")).value().Object()
	var compiles uint64
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if i == 4 {
			compiles = r.jit.reoptimized + r.jit.compiled
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 5 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || !slices.ContainsFunc(e.inlines, func(x jitInline) bool { return x.obj == w && x.pool != nil }) {
				t.Fatalf("build does not inline new W: %+v", e)
			}
			if n := r.jit.reoptimized + r.jit.compiled - compiles; n != 0 {
				t.Fatalf("steady rounds compiled %d times", n)
			}
		}
	}
}

// A construction of a forwarding constructor, Prototype's Class.create's
// function(){this.initialize.apply(this,arguments)}, which RayTrace's
// classes all are, is inlined as V8 inlines it: what the constructor reads
// is known from its receiver, a pool's object -- initialize on the class's
// prototype, apply the realm's -- and its call is initialize's, inlined
// with the construction's own arguments, no arguments object made. One
// wrapper's code serves both classes here, each inlined with its own
// initialize. An exit inside initialize (a string multiplied), after it
// counted the construction, makes both frames, and the construction's
// result is the receiver, counted once; initialize replaced is inlined in
// its place; one inside the constructor, its apply not the realm's once
// Function.prototype.apply is replaced, has Go make the construction over
// again, which calls the new apply with an arguments object; fewer arguments than initialize's parameters leave the rest
// undefined; an error thrown from inside initialize has the interpreter's
// stack, the constructor's frame in it. Each answer is the interpreter's.
func TestJITSSAInlinedForwardingConstructions(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var Class={create:function(){return function(){this.initialize.apply(this,arguments)}}};
		var V=Class.create();V.prototype={initialize:function(x,y){this.x=x;this.y=y},len:function(){return this.x+this.y}};
		var C=Class.create(),stats={n:0};C.prototype={initialize:function(r){stats.n=stats.n+1;this.r=r*2}};
		function build(xs,n){let s=0,v,c;for(let i=0;i<n;i++){v=new V(i,1);c=new C(xs[i%xs.length]);s=(s+v.x+v.y+c.r)|0}
			return [s,v.len(),Object.getPrototypeOf(c)===C.prototype,new V(7).y,stats.n].join()}
		var nums=[1,2,3,4],mixed=[1,"2",3,4],throws=[1,2,{valueOf(){throw new Error("bad")}},4]
		function stack(){try{return build(throws,300)}catch(e){return e.stack}}`
	src := `build(nums,300)`
	rounds := []string{src, src, src, src, src, src, `stack()`, src,
		`V.prototype.initialize=function(x){this.x=-x;this.y=0};` + src, src,
		`var apply=Function.prototype.apply;Function.prototype.apply=function(t,a){stats.n+=100;return apply.call(this,t,a)};` + src,
		`build(mixed,300)`, src}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("build")).value().Object().fn().closure
	v := r.global.getOwn(r.atoms.intern("V")).value().Object()
	c := r.global.getOwn(r.atoms.intern("C")).value().Object()
	var hosts uint64
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		e := r.jit.cache[weak.Make(cl.fn)]
		if e != nil {
			hosts = e.ssaStats.hosts
		}
		if strings.Contains(src, "Function.prototype.apply=") && (e == nil || e.entrySlow || len(e.notInline) != 0) {
			// The round whose exits in the constructor make it over again
			// runs natively, its constructions inlined.
			t.Fatalf("round %d: build no longer inlines its constructions natively: %+v", i, e)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 5 {
			e = r.jit.cache[weak.Make(cl.fn)]
			for _, o := range []*Object{v, c} {
				if e == nil || e.ssa == nil || !slices.ContainsFunc(e.inlines, func(x jitInline) bool { return x.obj == o && x.pool != nil }) {
					t.Fatalf("build does not inline its constructions: %+v", e)
				}
			}
			// Only to fill the pools again, and the last construction's.
			if n := e.ssaStats.hosts - hosts; n > 2*300/abi.PoolSize+4 {
				t.Fatalf("build left native code %d times for 600 constructions", n)
			}
		}
	}
}

// Math.sqrt(x) and Math.abs(x) are computed by native code, as V8 reduces
// them: the call checks it calls the realm's function and that x is a
// number, and computes it; mag, which calls one, is inlined in its caller
// with it. A string argument has Go make the call, and Math.sqrt replaced
// is called; -0, a negative's root and NaN are as the interpreter's.
func TestJITSSAMathIntrinsics(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function mag(v){return Math.sqrt(v.x*v.x+v.y*v.y)}
		function sum(n,vs){let s=0,z=0;for(let i=0;i<n;i++){const v=vs[i%vs.length];s+=mag(v)+Math.abs(v.x-3);z=Math.abs(-0*v.y)}return [s,1/z,Math.sqrt(-1)].join()}
		var vs=[{x:3,y:4},{x:-1,y:2},{x:0,y:0},{x:-0,y:-0}],odd=[{x:3,y:4},{x:"9",y:0},{x:-4,y:3}]`
	src := `sum(300,vs)`
	rounds := []string{src, src, src, src, src, `sum(300,odd)`, src, `Math.sqrt=function(x){return 7};` + src}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("sum")).value().Object().fn().closure
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts, entries uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts, entries = e.ssaStats.hosts, e.ssaStats.entries
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 4 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.ssaStats.entries == entries || len(e.ssaInlined) == 0 {
				t.Fatalf("sum did not run natively with mag inlined: %+v", e)
			}
			// After its loop, Go makes the array and calls join: none
			// in the loop's 300 iterations.
			if n := e.ssaStats.hosts - hosts; n > 3 {
				t.Fatalf("sum left native code %d times", n)
			}
		}
	}
}

// + with a string is made by Go, called from native code (ssa.GoAdd,
// jit's callGo), which stays in it, as V8's code calls its StringAdd: a
// loop concatenating strings, numbers, booleans, null and undefined, and
// Splay's GeneratePayloadTree, recursive, its leaves' strings so made, run
// natively without leaving, under a collector running all the while. An
// object's conversion runs its valueOf, and a symbol throws, both made by Go
// after the code leaves -- from the frame as it was at the call, though Go
// has taken every register (jitGoCallWork churns them, and collects
// garbage now and then); a string too long throws its RangeError. Each
// answer is the interpreter's.
func TestJITSSAStringAddCallsGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	calls, collect := 0, true
	jitGoCallWork = func() {
		if calls++; churnRegisters(calls) == 0.25 {
			t.Log("churned to a quarter")
		}
		if collect && calls%64 == 0 {
			runtime.GC()
		}
	}
	defer func() { jitGoCallWork = nil }()
	setup := `function cat(n){let s='',t=0;for(let i=0;i<n;i++){const p='k'+i+':'+(i%3===0)+null+undefined+(i*0.5);t=(t+p.length)|0;if(i%50===0)s=s+p}return s.length+'/'+t+'/'+s.slice(0,40)}
		function tree(d,tag){if(d==0)return {array:[0,1,2,3],string:'String for key '+tag+' in leaf node'};return {left:tree(d-1,tag),right:tree(d-1,tag)}}
		var tags=[];for(let i=0;i<40;i++)tags.push(String(i/7));
		function walk(n){let c=0;for(let i=0;i<n;i++){const x=tree(4,tags[i%40]);c=(c+x.left.right.left.left.string.length)|0}return c}
		var obj={valueOf(){return 7}},sy=Symbol('s');
		function odd(n,o){let r='',c=0;for(let i=0;i<n;i++){r='a'+(i%7?i:o);c=(c+r.length)|0}return r+c}
		function syms(n,x){let r='';for(let i=0;i<n;i++){r='a'+(i<n-1?'b':x)}return r}
		function sym(){try{return syms(300,sy)}catch(e){return e.name}}
		function big(){let s='x';try{for(let i=0;i<40;i++)s=s+s}catch(e){return e.name+s.length}return 'no'}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cat := r.global.getOwn(r.atoms.intern("cat")).value().Object().fn().closure
	tree := r.global.getOwn(r.atoms.intern("tree")).value().Object().fn().closure
	odd := r.global.getOwn(r.atoms.intern("odd")).value().Object().fn().closure
	syms := r.global.getOwn(r.atoms.intern("syms")).value().Object().fn().closure
	rounds := []string{`cat(300)`, `cat(300)`, `cat(300)`, `cat(300)`, `String(walk(40))`, `String(walk(40))`, `String(walk(40))`,
		`String(walk(40))`, `odd(300,obj)`, `odd(300,obj)`, `odd(300,obj)`, `[sym(),sym(),sym()].join()`, `big()`, `big()`, `cat(300)`}
	for i, src := range rounds {
		// walk's rounds collect only as they need to: a native call
		// leaves for Go while the collector marks, which it would do much
		// of the time, and tree be called natively seldom.
		collect = i < 4 || i > 7
		if collect {
			debug.SetGCPercent(1)
		} else {
			debug.SetGCPercent(100)
		}
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts, out, in uint64
		if e := r.jit.cache[weak.Make(cat.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		if e := r.jit.cache[weak.Make(tree.fn)]; e != nil {
			out, in = uint64(e.nativeOut), uint64(e.link.in)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		switch i {
		case 3:
			e := r.jit.cache[weak.Make(cat.fn)]
			if e == nil || e.ssa == nil {
				t.Fatalf("cat did not run natively: %+v", e)
			}
			// After its loop, which leaves none of 300 times: two, for
			// s.slice, a built-in Go calls, among them.
			if n := e.ssaStats.hosts - hosts; n > 2 {
				t.Fatalf("cat left native code %d times", n)
			}
		case 7:
			// tree, called natively, does not leave for its strings, which
			// are Go's, made without leaving -- one a leaf, before: only
			// when its literals' pools run out, which Go fills.
			e := r.jit.cache[weak.Make(tree.fn)]
			if e == nil {
				t.Fatal("tree has no entry")
			}
			calls := uint64(e.link.in) - in
			if e.ssa == nil || e.notNative || calls < 1000 {
				t.Fatalf("tree is not called natively (%d calls): %+v", calls, e)
			}
			if n := uint64(e.nativeOut) - out; n*50 > calls {
				t.Fatalf("tree left native code %d times of %d", n, calls)
			}
		case 11:
			// Their objects and symbols left: Go made those.
			for name, cl := range map[string]*closure{"odd": odd, "syms": syms} {
				if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || e.ssaStats.hosts == 0 {
					t.Fatalf("%s did not run natively, leaving for Go: %+v", name, e)
				}
			}
		}
	}
}

// A pool that runs out is filled again by a call of Go from native code
// (ssa.GoRefill), and the construction or literal is made again from its
// start, as V8's code calls its runtime when its space runs out: a loop
// constructing (inlined and called natively) and making object and array
// literals, far more of each than a pool holds, never leaves native code;
// it did at every refill. A constructor whose prototype is replaced gets
// objects of the new one. The collector runs at each call of Go, which
// churns every register. Each answer is the interpreter's.
func TestJITSSAPoolsRefilledByGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	// The collector runs only when the calls of Go run it, and has done
	// marking when they return: one that marked through a round, as it
	// may with two CPUs, had its constructions and literals leave.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	calls := 0
	jitGoCallWork = func() {
		if calls++; churnRegisters(calls) == 0.25 {
			t.Log("churned to a quarter")
		}
		if calls%8 == 0 {
			runtime.GC()
		}
	}
	defer func() { jitGoCallWork = nil }()
	setup := `function P(x){this.x=x;this.y=x*2} P.prototype.z=1;
		function Q(x){for(let k=0;k<1;k++)this.v=x+k}
		function mk(n){let s=0;for(let i=0;i<n;i++){const p=new P(i),q=new Q(i);const o={a:i,b:p};const a=[i,i+1,q];s=(s+o.b.y+a[1]+a[2].v+(p.z|0))|0}return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	mk := r.global.getOwn(r.atoms.intern("mk")).value().Object().fn().closure
	rounds := []string{`String(mk(2000))`, `String(mk(2000))`, `String(mk(2000))`, `String(mk(2000))`, `String(mk(2000))`,
		`P.prototype={z:5};String(mk(2000))`, `String(mk(2000))`, `String(mk(2000))`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var exits uint64
		if e := r.jit.cache[weak.Make(mk.fn)]; e != nil {
			exits = e.ssaStats.hosts + e.ssaStats.guards
		}
		before := calls
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 4 || i == 7 {
			e := r.jit.cache[weak.Make(mk.fn)]
			if e == nil || e.ssa == nil {
				t.Fatalf("mk did not run natively: %+v", e)
			}
			// 2,000 of each, pools of at most 128: the refills were Go's,
			// called from native code, which left only where the collector
			// marked, a few times, not at some 60 refills.
			if n := e.ssaStats.hosts + e.ssaStats.guards - exits; n > 8 {
				t.Fatalf("round %d: mk left native code %d times", i, n)
			}
			if calls-before < 4*2000/abi.PoolCapacity {
				t.Fatalf("round %d: %d calls of Go", i, calls-before)
			}
		}
	}
}

// A store that meets objects of several shapes is compiled for each, as
// V8's polymorphic stores keep a map and a transition for each: it notes,
// after Go made it at an exit, what the site's cache found (an own
// property, or the transition the property was added along), and the code
// is compiled again with them (jitPolyWriteSeen). adds puts s on objects
// of four shapes, as DeltaBlue's Constraint puts strength on its
// subclasses' receivers; writes stores p5 into objects of two shapes, past
// the eight properties native code searches a table for, as RayTrace's
// testIntersection writes hitCount; deep adds strength to objects of four
// constructors whose prototype chains are two to five deep, past the two a
// cache holds, as DeltaBlue's constraints are (jitDeepAddOf). Each had
// left native code at every object of a shape the site was not compiled
// for. Each answer is the interpreter's.
func TestJITSSAPolymorphicStores(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// The collector off after one collection: native code adds no property while the collector marks, leaving the
	// add to Go: a marking that spans the counted round, as under checkptr at
	// GOMAXPROCS=1, would count as the adds' exits.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function mk(k,i){if(k==0)return {a:i};if(k==1)return {b:i,a:i};if(k==2)return {c:i};return {d:i,e:i}}
		function adds(n){let t=0;for(let i=0;i<n;i++){const o=mk(i&3,i);o.s=i;t=(t+o.s)|0}return t}
		function big(k){const o={p0:0,p1:1,p2:2,p3:3,p4:4,p5:5,p6:6,p7:7,p8:8};if(k)o.q=1;return o}
		var bigs=[big(0),big(1),big(0),big(1)];
		function writes(n){let t=0;for(let i=0;i<n;i++){const o=bigs[i&3];o.p5=i;t=(t+o.p5+o.p8)|0}return t+':'+bigs[1].p5}
		function K0(){} function K1(){} K1.prototype=Object.create(K0.prototype);
		function K2(){} K2.prototype=Object.create(K1.prototype); function K3(){} K3.prototype=Object.create(K2.prototype);
		var ks=[K1,K2,K3,K0];
		function deep(n){let t=0;for(let i=0;i<n;i++){const o=new ks[i&3]();o.strength=i;t=(t+o.strength)|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	exits := func(name string) uint64 {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			return e.ssaStats.hosts + e.ssaStats.guards
		}
		return 0
	}
	src := `adds(400)+'/'+writes(400)+'/'+deep(400)`
	for i := 0; i < 8; i++ {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		a, w, d := exits("adds"), exits("writes"), exits("deep")
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 7 {
			// 400 iterations each, three of four objects of shapes the
			// first compile did not know of, or two of four.
			if n := exits("adds") - a; n > 2 {
				t.Fatalf("adds left native code %d times", n)
			}
			if n := exits("writes") - w; n > 2 {
				t.Fatalf("writes left native code %d times", n)
			}
			if n := exits("deep") - d; n > 2 {
				t.Fatalf("deep left native code %d times", n)
			}
			for _, name := range []string{"adds", "writes", "deep"} {
				cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
				if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || e.entrySlow {
					t.Fatalf("%s did not run natively: %+v", name, e)
				}
			}
		}
	}
	// A setter up K3's and K2's chains, on K1's prototype: their adds are
	// the setter's calls now, which native code, finding the prototype's
	// shape changed, leaves to Go.
	for i, src := range []string{`Object.defineProperty(K1.prototype,'strength',{set(v){this.s2=v*2},get(){return this.s2|0}});deep(400)`, `deep(400)`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("setter round %d: got %v, interpreter %v", i, got, want)
		}
	}
}

// TestJITSSAInlineBudgetCallsNatively: a function with more small callees
// than a compile inlines (ssa's maxInlines) calls the rest natively, as
// V8's does, instead of leaving each such call to Go at every iteration --
// RayTrace's rayTrace had some 20 such calls.
func TestJITSSAInlineBudgetCallsNatively(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, native calls leave for Go (they take no
	// pointer then): a marking that spans the counted round, as at
	// GOMAXPROCS=1, would count as the calls' exits.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function f0(x){return x+1} function f1(x){return x*2} function f2(x){return x-3} function f3(x){return x^5}
		function f4(x){return x+7} function f5(x){return x*3} function f6(x){return x-11} function f7(x){return x^13}
		function f8(x){return x+17} function f9(x){return x*5} function f10(x){return x-19} function f11(x){return x^23}
		function wide(n){let t=0;for(let i=0;i<n;i++){t=(f0(t)+f1(i)+f2(t)+f3(i)+f4(t)+f5(i)+f6(t)+f7(i)+f8(t)+f9(i)+f10(t)+f11(i))|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	e := func() *jitEntry {
		cl := r.global.getOwn(r.atoms.intern("wide")).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	exits := func() uint64 {
		if e := e(); e != nil {
			return e.ssaStats.hosts + e.ssaStats.guards
		}
		return 0
	}
	for i := 0; i < 8; i++ {
		wv, err := want.Run(compileForTest(t, `wide(400)`))
		if err != nil {
			t.Fatal(err)
		}
		before := exits()
		gv, err := r.Run(compileForTest(t, `wide(400)`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d: got %v, interpreter %v", i, got, want)
		}
		if i == 7 {
			if n := exits() - before; n > 2 {
				t.Fatalf("wide left native code %d times in 400 iterations", n)
			}
			if e := e(); e == nil || e.ssa == nil || e.entrySlow {
				t.Fatalf("wide did not run natively: %+v", e)
			}
		}
	}
}

// TestJITSSAGoCalls: a call native code makes nothing of its own for --
// a function that reads arguments, a forwarding constructor no compile
// inlined, a bound function, a built-in -- Go makes from native code
// (ssa.GoCall), which goes on with its result, as V8's code calls a
// function it has no code for, rather than leaving. Each answer is the
// interpreter's: a callee assigning the caller's captured binding; one
// that throws, its message and stack the interpreter's, the call made
// once; one that runs the caller again, for objects of shapes that have it
// compiled again while the outer call is suspended in its old code, which
// stays until that returns (closeSSA); and the collector running at each
// call.
func TestJITSSAGoCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	calls := 0
	jitGoCallWork = func() {
		if calls++; calls%16 == 0 {
			runtime.GC()
		}
	}
	defer func() { jitGoCallWork = nil }()
	setup := `function h(a){a.n=arguments.length;return a.length}
		var Class={create:function(){return function(){this.initialize.apply(this,arguments)}}};
		function Q(x){this.y=arguments.length+x}
		var P=Class.create();P.prototype={initialize:function(x){this.x=x;this.c=new Q(x)}};
		var bound=function(a,b){return a*10+b}.bind(null,7);
		var made=0;
		function thrower(i){made++;if(i===777)throw new TypeError('boom '+arguments.length);return i&7}
		function mk(){let c=0;const inc=function(){c+=2;return arguments.length};
			return function loop(n){let s=0;const o={length:3};
				for(let i=0;i<n;i++){let w=i;for(let k=0;k<30;k++)w=(w*31+k)^(w>>>3);s=(s+w+h(o)+new P(i).c.y+bound(i&3)+[i,1].at(1)+inc()+c+thrower(i))|0}return s+':'+o.n}}
		var loop=mk();
		function run(n){try{return loop(n)}catch(e){return e.message+'|'+e.stack+'|'+made}}
		function rec(o,d){let s=0;for(let i=0;i<30;i++){s=(s+o.v)|0;if(d>0&&i==15)s=(s+deeper(d))|0}return s}
		function deeper(d){var k=arguments.length,o={};o['w'+d]=d;o.v=d+k;return rec(o,d-1)}
		function held(o,n){let s=0;for(let i=0;i<40;i++){s=(s+o.v*i)|0;if(i==20)s=(s+away(n))|0}return s}
		function away(n){if(arguments.length&&n)drop(n);return n}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	// drop(1) drops held's code, as the cache does to make room; drop(2)
	// has it compiled again: while a call of Go is suspended in it.
	dropped, closed := 0, 0
	for _, rt := range []*Runtime{want, r} {
		rt.global.setOwnRaw(rt.atoms.intern("drop"), rt.NewFunction("drop", 1, func(rt *Runtime, _ Value, args []Value) (Value, error) {
			if rt != r {
				return Undefined, nil
			}
			cl := r.global.getOwn(r.atoms.intern("held")).value().Object().fn().closure
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || len(r.jit.goPinned) == 0 {
				return Undefined, nil
			}
			dropped++
			code := e.ssa
			if args[0].Number() == 1 {
				r.jit.dropEntry(weak.Make(cl.fn), e)
			} else {
				e.reopt = true
				r.jitReoptimize(cl, e)
			}
			if code.Size() == 0 {
				// Native code returns there.
				closed++
			}
			return Undefined, nil
		}), propDefault)
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range []string{`run(300)`, `run(300)`, `run(300)`, `run(300)`, `run(1000)`, `made`,
		`rec({v:1},0)`, `rec({v:1},0)`, `rec({v:1},0)`, `[rec({v:1},6),rec({a:1,v:2},6)].join()`, `rec({v:1},12)`,
		`held({v:3},0)`, `held({v:3},0)`, `held({v:3},0)`, `held({v:3},2)`, `held({v:3},0)`, `held({v:3},0)`, `held({v:3},1)`,
		`held({v:3},0)`, `held({v:3},0)`, `held({v:3},0)`, `held({v:3},2)`} {
		wv, err := want.Run(compileForTest(t, `String(`+src+`)`))
		if err != nil {
			t.Fatal(err)
		}
		goCalls, hosts := r.jit.goCalls, r.jit.hosts
		gv, err := r.Run(compileForTest(t, `String(`+src+`)`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d, %s: got\n%s\ninterpreter\n%s", i, src, got, want)
		}
		if i == 3 {
			// 300 iterations, each with six calls Go makes.
			if e := entry("loop"); e == nil || e.ssa == nil || e.entrySlow {
				t.Fatalf("loop did not run natively: %+v", e)
			}
			if n := r.jit.goCalls - goCalls; n < 6*300 {
				t.Fatalf("Go made %d calls from native code, want %d", n, 6*300)
			}
			if n := r.jit.hosts - hosts; n > 2 {
				t.Fatalf("loop left native code %d times", n)
			}
		}
	}
	if dropped < 2 || closed != 0 {
		t.Fatalf("held's code was dropped or compiled again under a call of Go %d times, closed under it %d times", dropped, closed)
	}
	if len(r.jit.goPinned) != 0 || len(r.jit.retired) != 0 || r.jit.goPending.set {
		t.Fatalf("calls of Go left %d pinned, %d retired, pending %v", len(r.jit.goPinned), len(r.jit.retired), r.jit.goPending.set)
	}
}

// A call Go makes from native code (ssa.GoCall) leaves the frame's operand
// slots alone: code entered after an exit -- at its first call, under the
// threshold stress, Infinity's read leaves -- reads t, a value it never
// loaded, from the slot it was entered with, which an exit's way of making
// the call writes the call's operands over (the fuzzer's eb178ef9ec054b76
// and 0ea58c8f6fea0c66). Each answer is the interpreter's.
func TestJITSSAGoCallsLeaveFrameSlots(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := `function h(s,i){if(i===6)throw new RangeError('h'+s);return (s+i)|0}
		function f(n){let s=1,t='3';for(let i=0;i<n;i++){t=(i % Infinity);s=h(s,i);}return s+':'+t}
		function g(n){try{return f(n)}catch(e){return e.message}}
		[g(1),g(3),g(7)].join()`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	wv, err := want.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitStress = jitStressConfig{threshold: true}
	gv, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gv.String().Go(), wv.String().Go(); got != want {
		t.Fatalf("got %s, interpreter %s", got, want)
	}
	if r.jit.goCalls == 0 {
		t.Fatal("Go made no call from native code")
	}
}

// A cache full of entries drops one that holds no code -- a refusal
// remembered for a script a host runs once -- before code, which costs a
// compile to make again: a hot function's code stays through 400 scripts
// that call it, each alive, each an entry, compiled once.
func TestJITCacheKeepsCodeOverRefusals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function hot(n){let s=0;for(let i=0;i<n;i++)s=(s*31+i)|0;return s}`)); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := r.Run(compileForTest(t, `hot(5000)`)); err != nil {
			t.Fatal(err)
		}
	}
	fn := r.global.getOwn(r.atoms.intern("hot")).value().Object().fn().closure.fn
	if e := r.jit.cache[weak.Make(fn)]; e == nil || !e.holdsCode() {
		t.Fatalf("hot has no code: %+v", e)
	}
	var scripts []*bytecode.Function
	compiled := r.jit.compiled
	for i := range 400 {
		p := compileForTest(t, fmt.Sprintf(`hot(%d)`, i))
		scripts = append(scripts, p)
		if _, err := r.Run(p); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.jit.compiled - compiled; n != 0 {
		t.Fatalf("hot's code was dropped for scripts' entries, and compiled again %d times", n)
	}
	runtime.KeepAlive(scripts)
}

// The values a function keeps in registers across its calls take spill
// slots only while they live: a slot one has done with is another's, as
// the spill slots are reused, so a long function with many calls, each a
// value or two live across it -- Crypto's bnpDivRemTo, which went to the
// old pipeline for "more than 256 spill slots" -- compiles. Each answer is
// the interpreter's.
func TestJITSSACallSavesReuseSlots(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var body strings.Builder
	for k := range 130 {
		fmt.Fprintf(&body, "a=(i*%[1]d+s)|0;b=(s^%[1]d)|0;s=(s+h(i))|0;s=(s*3+a-b)|0;", k)
	}
	src := `function h(i){return arguments.length+i}
		function many(n){let s=0,a=0,b=0;for(let i=0;i<n;i++){` + body.String() + `}return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 4 {
		wv, err := want.Run(compileForTest(t, `many(20)`))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, `many(20)`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d: got %v, interpreter %v", i, got, want)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("many")).value().Object().fn().closure
	if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil {
		t.Fatalf("many was not compiled by the new pipeline: %+v", e)
	}
}

// A construction of a global function is decided whenever the code is
// compiled -- the function found where the global object has it, as
// native code's read of the global finds it -- though the read's cache is
// empty: native code reads globals without it, so a read only native code
// ran, or none ran, never fills it. f is compiled before its branch has
// run, and its construction there decided then: once the branch runs, f
// is compiled again only for the branch's property reads, which knew no
// shape -- not for the construction, learned at its call, a compile of
// its own (jitCalleeAt; every construction of DeltaBlue's had cost one).
// Each answer is the interpreter's.
func TestJITSSASeedsGlobalsNativeCodeRead(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := `function V(a){this.a=a;this.b=a*2}
		var o={w:3};
		function f(n,k){let s=0;for(let i=0;i<n;i++){s=(s*3+i)|0;if(i>=k){const v=new V(i);s=(s+v.a+v.b+o.w)|0}}return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
		// V's code, which the construction inlines, warm.
		if _, err := rt.Run(compileForTest(t, `for(var j=0;j<20;j++)new V(j)`)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	var compiles, later uint64
	for i, call := range []string{`f(300,1e9)`, `f(300,1e9)`, `f(300,1e9)`, `f(300,0)`, `f(300,0)`, `f(300,0)`, `f(300,0)`} {
		wv, err := want.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		before := r.jit.reoptimized
		gv, err := r.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d: got %v, interpreter %v", i, got, want)
		}
		switch {
		case i == 3:
			compiles += r.jit.reoptimized - before
		case i > 3:
			later += r.jit.reoptimized - before
		}
	}
	if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil {
		t.Fatalf("f did not run natively: %+v", e)
	}
	// The branch's reads, of v and o, which knew nothing when f was
	// compiled, deoptimize as their maps' checks do, and f is compiled
	// again for them -- not for the construction, decided before.
	if compiles > 3 || later != 0 {
		t.Fatalf("f was compiled again %d times once its branch ran, %d after", compiles, later)
	}
}

// Code compiled before some of its sites had run -- a branch closed when
// its loop was compiled -- is compiled again for them once: at its next
// call, or once the call it runs in has done its budget's work, by when
// they have all run -- not at each such site's first exit in turn, before
// the others have (feedReopt). Each answer is the interpreter's.
func TestJITSSAUnfedSitesCompiledAtNextCall(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := `function P(){}P.prototype={a:1,b:2,c:3,d:4,e:5};var o=new P;
		function f(n,open){let s=0;for(let i=0;i<n;i++){s=(s*3+i)|0;if(open){s=(s+o.a+o.b+o.c+o.d+o.e)|0}}return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	calls := []string{`f(300,false)`, `f(300,false)`, `f(300,false)`, `f(300,true)`, `f(300,true)`, `f(300,true)`, `f(300,true)`}
	var compiles []uint64
	for i, call := range calls {
		wv, err := want.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		before := r.jit.reoptimized
		gv, err := r.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d: got %v, interpreter %v", i, got, want)
		}
		compiles = append(compiles, r.jit.reoptimized-before)
	}
	if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || e.entrySlow {
		t.Fatalf("f did not run natively: %+v", e)
	}
	// Opened in the fourth call, its five reads without feedback.
	var n uint64
	for _, c := range compiles[3:] {
		n += c
	}
	if n != 1 {
		t.Fatalf("compiled again %v times in the calls after its branch opened", compiles[3:])
	}
}

// A method call whose receivers are of three classes, whose methods give
// them a field, is compiled for the three shapes they have since -- not
// for the shapes they had before, which took the read's cases, so that the
// code left native code at every read of the live ones and was compiled
// again for the same stale ones until its compiles ran out (jitAddReadCase).
// Its method read first runs after the code is compiled, which is compiled
// again once for it (feedReopt). Each answer is the interpreter's.
func TestJITSSAReadsDropSupersededShapes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, native calls leave for Go: a marking that
	// spans the last round, as under checkptr at GOMAXPROCS=1, would count
	// as its reads' exits.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	src := `function A(){this.v=1}A.prototype.m=function(i){this.n=i;return this.v+i};
		function B(){this.v=2;this.w=0}B.prototype.m=function(i){this.n=i;return this.v*i};
		function C(){this.u=0;this.v=3}C.prototype.m=function(i){this.n=i;return this.v^i};
		var os=[new A,new B,new C];
		function g(n){let t=0;for(let i=0;i<n;i++){t=(t+os[i%3].m(i))|0;t^=t>>>3}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure
	var hosts uint64
	for i := range 6 {
		wv, err := want.Run(compileForTest(t, `g(3000)`))
		if err != nil {
			t.Fatal(err)
		}
		hosts = r.jit.hosts
		gv, err := r.Run(compileForTest(t, `g(3000)`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d: got %v, interpreter %v", i, got, want)
		}
	}
	e := r.jit.cache[weak.Make(cl.fn)]
	if e == nil || e.ssa == nil || e.entrySlow {
		t.Fatalf("g did not run natively: %+v", e)
	}
	if e.reopts > 1 || r.jit.hosts != hosts {
		t.Fatalf("g was compiled again %d times for what its reads met, and left native code %d times in its last round", e.reopts, r.jit.hosts-hosts)
	}
}

// churnRegisters keeps many integers and floats live at once, so that the
// registers native code had before a call of Go hold something else after.
//
//go:noinline
func churnRegisters(n int) float64 {
	a, b, c, d, e, f, g, h := float64(n), 1.5, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5
	i0, i1, i2, i3, i4, i5, i6 := n, n*3, n*5, n*7, n*11, n*13, n*17
	for k := 0; k < 8; k++ {
		a, b, c, d, e, f, g, h = b+h, c*a, d-b, e+c, f*0.5, g+e, h-f, a*1.0001
		i0, i1, i2, i3, i4, i5, i6 = i1^i6, i2+i0, i3*3, i4-i1, i5^i2, i6+i3, i0*5
	}
	return a + b + c + d + e + f + g + h + float64(i0+i1+i2+i3+i4+i5+i6)
}

// Where native code answers Math.pow itself (ssa.ExactPow), its answer is
// the VM's, bit for bit: bases from -40 to 40 and some fractions, -0, NaN
// and the infinities, to powers from -3 to 70, halves among them.
func TestJITExactPowIsVMs(t *testing.T) {
	xs := []float64{math.Copysign(0, -1), 0.5, 0.3, 2.5, -1.5, math.NaN(), math.Inf(1), math.Inf(-1), 1 << 52, -(1 << 53)}
	for x := -40; x <= 40; x++ {
		xs = append(xs, float64(x))
	}
	answered := 0
	for _, x := range xs {
		for y2 := -6; y2 <= 140; y2++ {
			y := float64(y2) / 2
			got, ok := ssa.ExactPow(x, y)
			if !ok {
				continue
			}
			answered++
			if want := jsPow(x, y); math.Float64bits(got) != math.Float64bits(want) {
				t.Fatalf("Math.pow(%v, %v): native %v, VM %v", x, y, got, want)
			}
		}
	}
	if answered < 1000 {
		t.Fatalf("native code answers only %d", answered)
	}
}

// Math.round, Math.sign, Math.max and Math.min as native code computes
// them, and ssa folds them (ssa.Round, ssa.Sign, ssa.Extremum), are the
// VM's, to the bit.
func TestJITMathFoldsAreVMs(t *testing.T) {
	xs := []float64{0, math.Copysign(0, -1), 0.5, -0.5, 1.5, -1.5, 2.5, -2.5, 0.49999999999999994, -0.49999999999999994,
		1<<52 - 0.5, -(1<<52 - 0.5), 1 << 52, 1<<53 + 2, 5e-324, -5e-324, math.MaxFloat64, -math.MaxFloat64,
		math.Inf(1), math.Inf(-1), math.NaN(), 0.7, -0.7, 0.99999999999999989, -0.99999999999999989}
	for i := -1000; i <= 1000; i++ {
		xs = append(xs, float64(i)/8, float64(i)*1e13+0.5)
	}
	same := func(a, b float64) bool {
		return math.Float64bits(a) == math.Float64bits(b) || a != a && b != b
	}
	for _, x := range xs {
		if got, want := ssa.Round(x), jsRound(x); !same(got, want) {
			t.Fatalf("Math.round(%v): native %v, VM %v", x, got, want)
		}
		if got, want := ssa.Sign(x), jsSign(x); !same(got, want) {
			t.Fatalf("Math.sign(%v): native %v, VM %v", x, got, want)
		}
	}
	for _, x := range xs[:40] {
		for _, y := range xs[:40] {
			for _, max := range []bool{true, false} {
				if got, want := ssa.Extremum(x, y, max), extremum2(x, y, max); !same(got, want) {
					t.Fatalf("extremum(%v, %v, %v): native %v, VM %v", x, y, max, got, want)
				}
			}
		}
	}
}

// Math.pow(x, y) is computed by native code where its answer is exact
// (ssa.ExactPow): y 2, and an integer to a power from 0 to 64 up to 2**53,
// RayTrace's Math.pow(10, gloss+1), which had its rayTrace leave native
// code at every call and be demoted. Past 2**53, for a fraction, a negative
// or fractional power, NaN or a string, Go makes the call; Math.max, of two
// arguments too, is not taken for it, nor a function replacing it. Each
// answer is the interpreter's.
func TestJITSSAMathPow(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function pw(n){let s=0;for(let i=0;i<n;i++){const k=i%9;s+=Math.pow(10,k)+Math.pow(-3,k)+Math.pow(i%7-3.5,2)+Math.pow(k,0)+Math.pow(2,53+k%2-1)/1e15+Math.abs(k-4)}return s}
		function odd(n){let r;for(let i=0;i<n;i++){r=[Math.pow(10,23),Math.pow(17,13),Math.pow(0.3,3),Math.pow(2,-1),Math.pow(NaN,0),Math.pow(1,Infinity),1/Math.pow(-0,3),Math.pow(-2,2.5),Math.pow('3',2),Math.max(2,3),Math.pow(i%3,0),Math.pow(-2,63)]}return r.join()}
		function one(x,y,n){let r=0;for(let i=0;i<n;i++)r=Math.pow(x,y);return r}
		function all(){return [one(10,23,50),one(17,13,50),one(0.3,3,50),one(2,-1,50),one(NaN,0,50),one(1,Infinity,50),1/one(-0,3,50),
			one(-2,2.5,50),one('3',2,50),one(-2,63,50),one(2.5,2,50),one(-7,0,50),one(-3,33,50),one(0,-1,50),one(-1,-3,50)].join()}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("pw")).value().Object().fn().closure
	odd := r.global.getOwn(r.atoms.intern("odd")).value().Object().fn().closure
	one := r.global.getOwn(r.atoms.intern("one")).value().Object().fn().closure
	rounds := []string{`String(pw(300))`, `String(pw(300))`, `String(pw(300))`, `String(pw(300))`, `String(pw(300))`,
		`odd(300)`, `odd(300)`, `odd(300)`, `String(one(10,3,300))`, `String(one(10,3,300))`, `String(one(10,3,300))`, `all()`, `all()`,
		`Math.pow=function(x,y){return x+y};String(pw(300))+all()`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 4 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil {
				t.Fatalf("pw did not run natively: %+v", e)
			}
			if n := e.ssaStats.hosts - hosts; n != 0 {
				t.Fatalf("pw left native code %d times", n)
			}
		}
		if i == 7 {
			if e := r.jit.cache[weak.Make(odd.fn)]; e == nil || e.ssa == nil {
				t.Fatalf("odd did not run natively: %+v", e)
			}
		}
		if i == 10 {
			if e := r.jit.cache[weak.Make(one.fn)]; e == nil || e.ssa == nil || e.ssaStats.hosts != 0 {
				t.Fatalf("one did not run natively: %+v", e)
			}
		}
	}
}

// A method read whose cache has not filled -- its loop not reached when
// the function was compiled -- is compiled for the object the instruction
// before it gives, where that is known: a captured binding's object now,
// or a RegExp like those a regular expression literal makes, its method
// found on the prototype as a filled cache would have it. RegExp's
// runBlock0, compiled in the first of its two dozen loops, had each later
// loop's re.exec leave native code until it had feedback, and the function
// compiled again for it, once a loop: fourteen compiles a first run. Here
// blocks compiles once, and leaves native code at none of its reads -- exec
// and a prototype's k; a binding later given an object of another shape,
// or one with k of its own, misses the guess, which the read's checks
// catch. Each answer is the interpreter's.
func TestJITSSAKnownReceivers(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function P(){} P.prototype.k=7;
		function Q(){this.z=1} Q.prototype.k=11;
		function mk(){var re=/b+/,obj=new P();
			return {set:function(o,r){obj=o;if(r)re=r},blocks:function(n){let s=0;
				for(let i=0;i<n;i++)s=(s*3+i)|0;
				for(let i=0;i<n;i++)s=(s+re.exec('abbc'+i)[0].length)|0;
				for(let i=0;i<n;i++)s=(s+/c+/.exec('acc'+i)[0].length)|0;
				for(let i=0;i<n;i++)s=(s+obj.k)|0;
				return s}}}
		var m=mk();`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func() *jitEntry {
		o := r.global.getOwn(r.atoms.intern("m")).value().Object()
		cl := o.getOwn(r.atoms.intern("blocks")).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	rounds := []string{`m.blocks(400)`, `m.blocks(400)`, `m.blocks(400)`,
		`(m.set(new Q(),/(b)/),m.blocks(400))`, `m.blocks(400)`, `(m.set({k:5}),m.blocks(400))`, `m.blocks(400)`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, `String(`+src+`)`))
		if err != nil {
			t.Fatal(err)
		}
		reopts, hosts := r.jit.reoptimized, r.jit.hosts
		gv, err := r.Run(compileForTest(t, `String(`+src+`)`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d, %s: got %s, interpreter %s", i, src, got, want)
		}
		if i < 3 {
			e := entry()
			if e == nil || e.ssa == nil {
				t.Fatalf("round %d: blocks did not run natively: %+v", i, e)
			}
			if n := r.jit.reoptimized - reopts; n != 0 || len(e.poly) != 0 {
				t.Fatalf("round %d: blocks compiled again %d times, reads learned from exits %v", i, n, e.poly)
			}
			if n := r.jit.hosts - hosts; i > 0 && n != 0 {
				t.Fatalf("round %d: blocks left native code %d times", i, n)
			}
		}
	}
}

// A compile reads its sites' stack depths (jitKnownReceiver, chainSite,
// Intrinsic) from the program it compiles, not from the function lowered
// again for inlining: blocks, whose method reads have no feedback yet, is
// lowered once, as a function on its own. Lowered for inlining too, a
// function kept both programs as long as its code lived: RegExp's run
// blocks, 1.4 MB of them, and the lowering at each compile.
func TestJITSSACompileLowersOnce(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function mk(){var re=/b+/;return function blocks(n){let s=0;
			for(let i=0;i<n;i++)s=(s*3+i)|0;
			for(let i=0;i<n;i++)s=(s+re.exec('abbc'+i)[0].length)|0;
			return s}}
		var blocks=mk();`
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(compileForTest(t, `blocks(400)`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("blocks")).value().Object().fn().closure
	e := r.jit.cache[weak.Make(cl.fn)]
	if e == nil || e.ssa == nil {
		t.Fatalf("blocks did not compile: %+v", e)
	}
	l := r.jit.lowered[weak.Make(cl.fn)]
	if l == nil || !l.done[jitLowerSSA] || l.done[jitLowerInline] {
		t.Fatalf("blocks lowered: %+v", l)
	}
}

// % of numbers that are not integers native code divides -- fractions, a
// zero divisor, the infinities, NaN, magnitudes past 2**63 -- is Go's,
// called from native code (ssa.GoMod), as V8's code calls its fmod: wrap,
// RayTrace's Chessboard wrapUp, left native code at every call, was no
// longer called natively by its callers (notNative), and each of their
// calls left native code in turn. Each answer is the interpreter's, -0's
// sign included.
func TestJITSSARemainder(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function wrap(t){t=t%2.0;if(t<-1)t+=2.0;if(t>=1)t-=2.0;return t}
		function col(n){let s=0;for(let i=0;i<n;i++){s+=wrap(i*0.37-40)*wrap(i*0.11)}return s}
		function sh(x){return Object.is(x,-0)?'-0':String(x)}
		function md(x,y,n){let r=0;for(let i=0;i<n;i++)r=x%y;return r}
		function odd(){return [md(5.5,-2,50),md(-0.5,1,50),md(-4,2,50),md(-0,5,50),md(3,0,50),md(-3,0,50),md(7,Infinity,50),md(-7,-Infinity,50),
			md(Infinity,2,50),md(NaN,1,50),md(1,NaN,50),md(1e300,3,50),md(-(2**63),7,50),md(2**64+4096,2**63,50),md(5e-324,1e-323,50),md(-1e-300,1,50),
			md(7,-1,50),md(-(2**53),-1,50),md(13,5,50)].map(sh).join()}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	col := r.global.getOwn(r.atoms.intern("col")).value().Object().fn().closure
	wrap := r.global.getOwn(r.atoms.intern("wrap")).value().Object().fn().closure
	md := r.global.getOwn(r.atoms.intern("md")).value().Object().fn().closure
	rounds := []string{`sh(col(400))`, `sh(col(400))`, `sh(col(400))`, `sh(col(400))`, `sh(col(400))`, `odd()`, `odd()`, `odd()`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(col.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 4 {
			e := r.jit.cache[weak.Make(col.fn)]
			if e == nil || e.ssa == nil {
				t.Fatalf("col did not run natively: %+v", e)
			}
			if n := e.ssaStats.hosts - hosts; n != 0 {
				t.Fatalf("col left native code %d times", n)
			}
			if w := r.jit.cache[weak.Make(wrap.fn)]; w != nil && (w.notNative || w.nativeOut != 0) {
				t.Fatalf("wrap left native code when called natively: %+v", w)
			}
		}
		if i == 7 {
			if e := r.jit.cache[weak.Make(md.fn)]; e == nil || e.ssa == nil || e.ssaStats.hosts != 0 {
				t.Fatalf("md did not run natively: %+v", e)
			}
		}
	}
}

// A property read that lowering made a number's, for a use on a branch
// not taken, and that meets a value that is not one -- RayTrace's setPixel
// reads options.pixelWidth, the string "5", and multiplies it only when it
// draws to a canvas -- is read as a value once it has (jitNumberMissed):
// sp, called natively, left native code at every call, nothing a cache
// learned changing that, and its callers stopped calling it natively.
// Taken, the branch's multiplication unboxes what the read gave. Each
// answer is the interpreter's.
func TestJITSSAReadsWhatIsNotANumber(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function Eng(){this.options={w:"5",h:3};this.canvas=null}
		Eng.prototype.sp=function(x,y){var pxW=this.options.w;var pxH=this.options.h;if(this.canvas){return x*pxW+y*pxH}return x+y+pxH};
		var eng=new Eng();
		function run(n){var t=0;for(var i=0;i<n;i++)t+=eng.sp(i,1);return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	sp := r.global.getOwn(r.atoms.intern("eng")).value().Object().proto.getOwn(r.atoms.intern("sp")).value().Object().fn().closure
	rounds := []string{`String(run(500))`, `String(run(500))`, `String(run(500))`, `String(run(500))`, `String(run(500))`,
		`eng.canvas={};String(run(500))`, `String(run(500))`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var out uint64
		if e := r.jit.cache[weak.Make(sp.fn)]; e != nil {
			out = e.nativeOut
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 4 {
			e := r.jit.cache[weak.Make(sp.fn)]
			if e == nil || e.ssa == nil {
				t.Fatalf("sp has no code: %+v", e)
			}
			if e.notNative || e.nativeOut != out {
				t.Fatalf("sp left native code %d times when called natively (notNative %v, failed %v)", e.nativeOut-out, e.notNative, e.failed)
			}
		}
	}
}

// Code a native call runs has Go make the calls it has no native code for
// (jitGoCallNested), as code at a run's own level does, and goes on: inner,
// called natively by mid, called natively by top, calls probe, a Go
// function, and String; it had left native code at every such call, and
// Go had finished each level. While a call runs, each level has a frame,
// as the interpreter's has: a stack trace shows every one, at its call.
// A throw goes through the levels to outer's catch; a call that compiles
// a level's function again, or drops outer's code, has the levels go on
// where they can. Each answer, and each stack, is the interpreter's.
func TestJITSSAGoCallsFromNativeCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var c={n:0,mode:0};
		function inner(x){let s=x;for(let j=0;j<1;j++)s=s*2+1;if(c.n++%50===0)s+=probe(x);return s+escape(x).length}
		function mid(x){let s=0;for(let j=0;j<1;j++)s=inner(x)+1;return s}
		function top(x){let s=0;for(let j=0;j<40;j++)s=(s*3+j)|0;return (s+mid(x)*2)|0}
		function outer(n){let t=0;for(let i=0;i<n;i++){try{t=(t+top(i))|0}catch(e){t=(t+e.message.length)|0}t^=t>>>3}return t}
		function sum(n){let t=0;for(let i=0;i<n;i++)t=(t+top(i&7))|0;return t}
		function fib(n){if(n<2)return n;let a=fib(n-1);let b=fib(n-2);for(let i=0;i<1;i++)a=a|0;return a+b}`
	var stacks [2][]string
	var dropCaller bool
	install := func(rt *Runtime, k int) {
		rt.global.setOwnRaw(rt.atoms.intern("probe"), rt.NewFunction("probe", 1,
			func(rt *Runtime, _ Value, args []Value) (Value, error) {
				var names []string
				for _, e := range rt.captureStack() {
					names = append(names, fmt.Sprintf("%s:%d:%d", e.Function, e.Line, e.Column))
				}
				stacks[k] = names
				switch rt.global.getOwn(rt.atoms.intern("c")).value().Object().getOwn(rt.atoms.intern("mode")).value().num {
				case 1:
					return Undefined, rt.throwTypeError("probe %d", len(names))
				case 2:
					if k == 1 && dropCaller && rt.jit != nil {
						cl := rt.global.getOwn(rt.atoms.intern("outer")).value().Object().fn().closure
						if e := rt.jit.hint(cl); e != nil {
							rt.jit.dropEntry(weak.Make(cl.fn), e)
						}
					}
				}
				return Int(len(names)), nil
			}), propDefault)
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	install(want, 0)
	install(r, 1)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	rounds := []string{`String(outer(400))`, `String(outer(400))`, `String(outer(400))`, `String(outer(400))`,
		`c.mode=1;String(outer(400))`, `c.mode=2;String(outer(400))`, `String([sum(200),fib(12)])`}
	for i, src := range rounds {
		dropCaller = i == 5
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			// A budget that does not run out: the interrupt check, a loop's,
			// is not a call leaving.
			r.backEdges = 1 << 30
		}
		hosts, goCalls := r.jit.hosts, r.jit.goCalls
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if got, want := strings.Join(stacks[1], " "), strings.Join(stacks[0], " "); got != want {
			t.Fatalf("round %d: stack in probe %s, interpreter's %s", i, got, want)
		}
		if i == 3 {
			e := entry("inner")
			if e == nil || e.ssa == nil || e.notNative || e.ssaStats.goCalls == 0 {
				t.Fatalf("inner did not call Go natively: %+v", e)
			}
			for _, name := range []string{"mid", "top"} {
				if m := entry(name); m == nil || m.ssa == nil || len(m.nativeCalls) == 0 {
					t.Fatalf("%s does not call natively: %+v", name, m)
				}
			}
			if n := r.jit.hosts - hosts; n != 0 {
				t.Fatalf("native code left for Go %d times", n)
			}
			if n := r.jit.goCalls - goCalls; n < 400 {
				t.Fatalf("Go made %d calls for native code, of 400 or more", n)
			}
		}
	}
}

// A built-in a native callee calls through Go may call the script back:
// String.prototype.replace, called by one and two, called natively by
// box.mid, called natively by outer, calls cb, whose Error's stack shows
// every level at its call, as the interpreter's does, cb and replace above
// them: each with its receiver, which none of them reads -- undefined for
// one and two, box for mid ("Box.mid") -- where the frame Go made for a
// level had shown whatever its context held last ("Number.one"). Native
// code writes a callee's receiver into its context whether or not the
// callee reads it. hook, a Go function one and two call, asks for the
// caller's code to be compiled again while the call is suspended in it,
// two levels down: it is not, while the call is in it, which returns
// there and whose frame Go reads by that code -- it stays due, compiled
// again once nothing is in it. Each answer, and each stack, is the
// interpreter's.
func TestJITSSAGoCallsFromNativeCalleesCallBack(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var c={n:0,last:''};
		function cb(m){c.last=new Error('cb '+m).stack;return m.toUpperCase()}
		function one(s){let r='';for(let j=0;j<1;j++)r=s.replace('b',cb);if(c.n++%25===0)r+=hook('one');return r.length}
		function two(s){let r='';for(let j=0;j<1;j++)r=s.replace(/c/,cb);if(c.n++%25===0)r+=hook('two');return r.length+1}
		function Box(){}
		Box.prototype.mid=function mid(s,i){let t=0;for(let j=0;j<1;j++)t=(i&1?one(s):two(s))+1;return t};
		var box=new Box();
		function outer(n){let t=0;for(let i=0;i<n;i++)t=(t*3+box.mid('abc'+(i&3),i))|0;return t+':'+c.last}`
	recompiled, kept := 0, 0
	install := func(rt *Runtime) {
		rt.global.setOwnRaw(rt.atoms.intern("hook"), rt.NewFunction("hook", 1,
			func(rt *Runtime, _ Value, args []Value) (Value, error) {
				if rt.jit == nil {
					return Str(NewString("")), nil
				}
				cl := rt.global.getOwn(rt.atoms.intern(args[0].String().Go())).value().Object().fn().closure
				e := rt.jit.cache[weak.Make(cl.fn)]
				if e == nil || e.ssa == nil || len(rt.jit.goPinned) == 0 {
					return Str(NewString("")), nil
				}
				// The level's code, which its call of Go returns to.
				code := e.ssa
				e.reopt = true
				rt.jitReoptimize(cl, e)
				switch {
				case e.ssa != code:
					recompiled++
				case e.reopt:
					kept++
				}
				return Str(NewString("")), nil
			}), propDefault)
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	install(want)
	install(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	for i := range 6 {
		src := `outer(300)`
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		goCalls := r.jit.goCalls
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got\n%s\ninterpreter's\n%s", i, got, want)
		}
		if i == 5 {
			for _, name := range []string{"one", "two"} {
				if e := entry(name); e == nil || e.ssa == nil || e.ssaStats.goCalls == 0 {
					t.Fatalf("%s did not call Go natively: %+v", name, e)
				}
			}
			if n := r.jit.goCalls - goCalls; n < 300 {
				t.Fatalf("Go made %d calls for native code, of 300 or more", n)
			}
		}
	}
	if recompiled != 0 || kept == 0 {
		t.Fatalf("a level's code compiled again under its call of Go %d times, kept due %d", recompiled, kept)
	}
	if len(r.jit.goPinned) != 0 || len(r.jit.retired) != 0 {
		t.Fatalf("calls of Go left %d pinned, %d retired", len(r.jit.goPinned), len(r.jit.retired))
	}
}

// A call of Go from a native callee takes the frames the last such call
// made for the same levels, as they are, but for their PCs (jitLevels): in
// one native stretch of outer's, fa is called again after a call of Go at
// outer's own level made the built-in's frame where fa's was; fb follows fa
// at the same level, with the same receiver; the one method is called with
// receivers of two classes, P's and Q's, one after the other. Every stack
// probe takes there -- probe, a Go function, at each of those calls and
// between them -- and every Error's stack a callback of map's takes from
// m ("at P.m", "at Q.m") is the interpreter's. (Each callee works around its
// calls, so that it pays: jitCalleeProfit.)
func TestJITSSAGoCallsReuseLevelFrames(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function work(i){let w=i;for(let j=0;j<40;j++)w=(w*31+j)^(w>>>3);return w}
		function fa(i){let s=work(i);for(let j=0;j<1;j++)s=(s+probe(i)+probe(i+1))|0;return s}
		function fb(i){let s=work(i);for(let j=0;j<1;j++)s=(s+probe(i)*2)|0;return s}
		var log=[];function capture(x){if(log.length<3000)log.push(new Error('c').stack);return x}
		function m(i){let s=work(i);for(let j=0;j<1;j++)s=(s+probe(i)+[1].map(capture)[0])|0;return s}
		function P(){} P.prototype.m=m; function Q(){} Q.prototype.m=m;
		var p=new P(), q=new Q();
		function outer(n){let t=0;for(let i=0;i<n;i++){t=(t+fa(i))|0;t=(t+fb(i))|0;t=(t+p.m(i))|0;t=(t+q.m(i))|0;
			t=(t+fa(i))|0;t=(t+probe(-i))|0;t=(t+fa(i))|0}return t}`
	var stacks [2][]string
	// The levels' frames, as the calls of Go see them: the run that made
	// them lets them go once it ends.
	levels := 0
	install := func(rt *Runtime, k int) {
		rt.global.setOwnRaw(rt.atoms.intern("probe"), rt.NewFunction("probe", 1,
			func(rt *Runtime, _ Value, args []Value) (Value, error) {
				if rt.jit != nil {
					levels = max(levels, rt.jit.levels.n)
				}
				var names []string
				for _, e := range rt.captureStack() {
					names = append(names, fmt.Sprintf("%s:%d:%d", e.Function, e.Line, e.Column))
				}
				if len(stacks[k]) < 4000 {
					stacks[k] = append(stacks[k], strings.Join(names, " "))
				}
				return Int(len(names)), nil
			}), propDefault)
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	install(want, 0)
	install(r, 1)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 4 {
		stacks = [2][]string{}
		src := `log=[];String(outer(300))+log.join('|')`
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		for j := range stacks[0] {
			if j >= len(stacks[1]) || stacks[1][j] != stacks[0][j] {
				t.Fatalf("round %d, probe %d: stack %q, interpreter's %q", i, j, stacks[1][j], stacks[0][j])
			}
		}
	}
	if levels == 0 {
		t.Fatal("no call of Go from a native callee made frames for its levels")
	}
}

// Calls of Go from native callees seven levels deep take the frames the
// last call made for the levels they share with it, however many
// (jitLevels): a call at level 7 after one at level 6, one at level 6
// after one at level 7, and one from another function at level 7, e7
// after d7, whose level 6 is shared. Every stack probe takes there, and
// every Error's stack a callback of map's takes at level 4, is the
// interpreter's.
func TestJITSSAGoCallsReuseDeepLevelFrames(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function work(i){let w=i;for(let j=0;j<40;j++)w=(w*31+j)^(w>>>3);return w}
		function d7(i){let s=work(i);for(let j=0;j<1;j++)s=(s+probe(i))|0;return s}
		function e7(i){let s=work(i);for(let j=0;j<1;j++)s=(s+probe(i)*3)|0;return s}
		function d6(i){let s=work(i);for(let j=0;j<1;j++)s=(s+d7(i)+probe(i+1)+e7(i)+d7(i+2))|0;return s}
		function d5(i){let s=work(i);for(let j=0;j<1;j++)s=(s+d6(i))|0;return s}
		var log=[];function capture(x){if(log.length<3000)log.push(new Error('c').stack);return x}
		function d4(i){let s=work(i);for(let j=0;j<1;j++)s=(s+d5(i)+[1].map(capture)[0]+d5(i+1))|0;return s}
		function d3(i){let s=work(i);for(let j=0;j<1;j++)s=(s+d4(i))|0;return s}
		function d2(i){let s=work(i);for(let j=0;j<1;j++)s=(s+d3(i))|0;return s}
		function d1(i){let s=work(i);for(let j=0;j<1;j++)s=(s+d2(i)+probe(-i))|0;return s}
		function outer(n){let t=0;for(let i=0;i<n;i++)t=(t+d1(i))|0;return t}`
	var stacks [2][]string
	// The levels' frames, as the calls of Go see them: the run that made
	// them lets them go once it ends.
	deepest := 0
	install := func(rt *Runtime, k int) {
		rt.global.setOwnRaw(rt.atoms.intern("probe"), rt.NewFunction("probe", 1,
			func(rt *Runtime, _ Value, args []Value) (Value, error) {
				if rt.jit != nil {
					deepest = max(deepest, len(rt.jit.levels.at))
				}
				var names []string
				for _, e := range rt.captureStack() {
					names = append(names, fmt.Sprintf("%s:%d:%d", e.Function, e.Line, e.Column))
				}
				if len(stacks[k]) < 6000 {
					stacks[k] = append(stacks[k], strings.Join(names, " "))
				}
				return Int(len(names)), nil
			}), propDefault)
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	install(want, 0)
	install(r, 1)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 4 {
		stacks = [2][]string{}
		src := `log=[];String(outer(200))+log.join('|')`
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if len(stacks[1]) != len(stacks[0]) {
			t.Fatalf("round %d: %d probes, interpreter's %d", i, len(stacks[1]), len(stacks[0]))
		}
		for j := range stacks[0] {
			if stacks[1][j] != stacks[0][j] {
				t.Fatalf("round %d, probe %d: stack %q, interpreter's %q", i, j, stacks[1][j], stacks[0][j])
			}
		}
	}
	if deepest <= 4 {
		t.Fatalf("calls of Go made frames for at most %d levels", deepest)
	}
}

// A built-in a call of Go calls is called as callDirect calls one
// (jitCallMade), without its other cases first: its frame for a stack
// trace -- a RangeError's from String.prototype.repeat, through a native
// callee -- the memory meter charged for its result as callDirect charges
// it, and a bound function, Math.max and Function.prototype.call still
// made the ordinary way. Each answer, each stack and the meter's count are
// the interpreter's.
func TestJITSSABuiltinCallsAsCallDirect(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var bi=String.prototype.indexOf.bind('zzc');
		function body(i,k){let w=i;for(let j=0;j<30;j++)w=(w*31+j)^(w>>>3);
			return (w+'x'.repeat(70000).length+Math.max(i,3)+String.prototype.indexOf.call('abc','c')+bi('c')+'a'.repeat(k).length)|0}
		function loop(n,bad){let s=0;for(let i=0;i<n;i++)s=(s+body(i,i===bad?-1:2))|0;return s}
		function run(n,bad){try{return loop(n,bad)}catch(e){return e.name+':'+e.message+'|'+e.stack}}`
	runs := func(jit bool) ([]string, int64) {
		r := New(Config{JIT: jit, MemoryLimit: 1 << 40})
		if jit {
			r = jitRuntimeForTest(t, Config{JIT: true, MemoryLimit: 1 << 40})
		}
		defer func() { r.Close(); r.ReleaseClosed() }()
		if _, err := r.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
		var out []string
		var charged int64
		for i, src := range []string{`run(300,-1)`, `run(300,-1)`, `run(300,-1)`, `run(300,-1)`, `run(300,250)`} {
			live := r.meter.live
			v, err := r.Run(compileForTest(t, `String(`+src+`)`))
			if err != nil {
				t.Fatal(err)
			}
			if i == 3 {
				charged = r.meter.live - live
				if jit {
					cl := r.global.getOwn(r.atoms.intern("body")).value().Object().fn().closure
					if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || e.ssaStats.goCalls == 0 {
						t.Fatalf("body did not call Go natively: %+v", e)
					}
				}
			}
			out = append(out, v.String().Go())
		}
		return out, charged
	}
	want, wantCharged := runs(false)
	got, gotCharged := runs(true)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round %d: got\n%s\ninterpreter\n%s", i, got[i], want[i])
		}
	}
	if gotCharged != wantCharged || wantCharged == 0 {
		t.Fatalf("results charged %d bytes, the interpreter %d", gotCharged, wantCharged)
	}
}

// Keep cells are given by when each is live (mir's colorKeeps): many,
// a loop of 300 calls of Go whose results it drops, every 25th added to a
// sum, calls Go natively at each -- past the 256th, a call had been an exit
// to Go, for want of a cell -- in a handful of cells. live's 60 results,
// strings of 60 lengths, are all live until their lengths are summed, each
// in a cell of its own. Each answer is the interpreter's.
func TestJITSSAKeepCellsReused(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var many, live strings.Builder
	many.WriteString("var str='abcdefgh';function many(n){let t=0;for(let i=0;i<n;i++){let w=i;for(let j=0;j<40;j++)w=(w*31+j)^(w>>>3);t=(t+w)|0;")
	live.WriteString("function live(n){let t=0;for(let i=0;i<n;i++){")
	for k := range 300 {
		if k%25 == 0 {
			fmt.Fprintf(&many, "t=(t+str.codePointAt('%c'))|0;", 'a'+k%8)
		} else {
			fmt.Fprintf(&many, "str.codePointAt('%c');", 'a'+k%8)
		}
	}
	for k := range 60 {
		fmt.Fprintf(&live, "let a%d='x'.repeat(%d+i);", k, k)
	}
	many.WriteString("}return t}")
	live.WriteString("t=(t")
	for k := range 60 {
		fmt.Fprintf(&live, "+a%d.length", k)
	}
	live.WriteString(")|0;for(let j=0;j<4000;j++)t=(t*31+j)^(t>>>3)}return t}")
	setup := many.String() + live.String()
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	for i := range 4 {
		src := `String([many(20),live(3)])`
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i < 2 {
			continue
		}
		e := entry("many")
		if e == nil || e.ssa == nil || e.entrySlow {
			t.Fatalf("many did not run natively: %+v", e)
		}
		if e.ssaStats.hosts != 0 || e.ssaKeeps > 8 || e.ssaStats.goCalls < 300 {
			t.Fatalf("many: %d exits, %d keep cells, %d calls of Go", e.ssaStats.hosts, e.ssaKeeps, e.ssaStats.goCalls)
		}
		if l := entry("live"); l == nil || l.ssa == nil || l.ssaKeeps < 60 || l.ssaKeeps > abi.MaxKeeps || l.ssaStats.goCalls == 0 {
			t.Fatalf("live: %d keep cells, %+v", l.ssaKeeps, l)
		}
	}
}

// A call Go makes from native code has no entry after it (ssa's plan): when
// it leaves -- loop's call of again has loop compiled again, or throws --
// the interpreter runs the rest of that iteration and native code goes on
// at the loop's header, in the new code; a throw goes to the caller's
// catch. Each answer is the interpreter's, and native code runs the
// iterations after a leave. (loop does work around its calls, so that it
// pays: jitSSAProfit.)
func TestJITSSAGoCallLeavesWithoutEntry(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var k={mode:0};
		function loop(n){let s=0;for(let i=0;i<n;i++){let w=i;for(let j=0;j<30;j++)w=(w*31+j)^(w>>>3);s=(s+w+'abc'.codePointAt('c')+again(i,k.mode))|0;s=(s*3+i)|0}return s}
		function run(n){try{return loop(n)}catch(e){return e.message}}`
	recompiled := 0
	install := func(rt *Runtime) {
		rt.global.setOwnRaw(rt.atoms.intern("again"), rt.NewFunction("again", 2,
			func(rt *Runtime, _ Value, args []Value) (Value, error) {
				i, mode := args[0].Number(), args[1].Number()
				if mode == 2 && i == 150 {
					return Undefined, rt.throwTypeError("again %d", int(i))
				}
				if mode != 1 || i != 100 || rt.jit == nil {
					return Int(1), nil
				}
				cl := rt.global.getOwn(rt.atoms.intern("loop")).value().Object().fn().closure
				if e := rt.jit.cache[weak.Make(cl.fn)]; e != nil && e.ssa != nil {
					code := e.ssa
					e.reopt = true
					rt.jitReoptimize(cl, e)
					if e.ssa != code {
						recompiled++
					}
				}
				return Int(1), nil
			}), propDefault)
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	install(want)
	install(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range []string{`String(run(400))`, `String(run(400))`, `String(run(400))`,
		`k.mode=1;String(run(400))`, `k.mode=2;String(run(400))`, `k.mode=0;String(run(400))`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		goCalls, interpreted := r.jit.goCalls, r.jit.interpreted
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			// Compiled again under the call at 100: the interpreter ran the
			// rest of that iteration once, native code the other 299.
			if n := r.jit.interpreted - interpreted; recompiled != 1 || n != 1 {
				t.Fatalf("loop compiled again %d times, the interpreter went on %d times", recompiled, n)
			}
			if n := r.jit.goCalls - goCalls; n < 2*390 {
				t.Fatalf("Go made %d calls for native code after the leave", n)
			}
		}
	}
}

// A level's code is pinned by the code it runs, not by its entry alone: in
// one native stretch of root's, leaf, called natively, calls Go (its code
// pinned), root's own call of Go has leaf compiled again, and root calls
// leaf again, now in its new code, which calls Go, its new code pinned --
// under which leaf is not compiled again, a native call being in it
// (otherwiseInUse). The code each call returns to -- pinned -- stays open
// until root's native code leaves; code leaf was compiled again for in
// between, which nothing ran since, is closed when replaced. Each answer
// is the interpreter's.
func TestJITSSAGoCallPinsALevelsNewCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var d={k:0};
		function leaf(s){let r=0;for(let j=0;j<1;j++)r=s.length;if(d.k)r+=again(d.k===2).length;return r}
		function root(n){let t=0;for(let i=0;i<n;i++){const m=i%10;d.k=m===1?1:m===3?2:0;t=(t*3+leaf('ab'+i))|0;if(m===2)t=(t+again(true).length)|0}return t}`
	recompiled, open := 0, 0
	install := func(rt *Runtime) {
		rt.global.setOwnRaw(rt.atoms.intern("again"), rt.NewFunction("again", 1,
			func(rt *Runtime, _ Value, args []Value) (Value, error) {
				if rt.jit == nil || !args[0].Truthy() {
					return Str(NewString("")), nil
				}
				cl := rt.global.getOwn(rt.atoms.intern("leaf")).value().Object().fn().closure
				e := rt.jit.cache[weak.Make(cl.fn)]
				if e == nil || e.ssa == nil || len(rt.jit.goPinned) == 0 {
					return Str(NewString("")), nil
				}
				code := e.ssa
				pinned := slices.Contains(rt.jit.goPinned, code)
				e.reopt = true
				rt.jitReoptimize(cl, e)
				if e.ssa != code && pinned {
					recompiled++
					if code.Size() != 0 {
						open++
					}
				}
				return Str(NewString("")), nil
			}), propDefault)
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	install(want)
	install(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 6 {
		src := `String(root(200))`
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter's %s", i, got, want)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("leaf")).value().Object().fn().closure
	// A level's pin: leaf called Go as root's native callee.
	if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || e.pinSerial == 0 {
		t.Fatalf("leaf did not call Go as a native callee: %+v", e)
	}
	if recompiled == 0 || open != recompiled {
		t.Fatalf("leaf's pinned code compiled again %d times under calls of Go, left open %d", recompiled, open)
	}
	if len(r.jit.goPinned) != 0 || len(r.jit.retired) != 0 {
		t.Fatalf("calls of Go left %d pinned, %d retired", len(r.jit.goPinned), len(r.jit.retired))
	}
}

// A loop whose back-edge budget runs out has Go give it another and check
// for an interrupt (ssa.GoPoll), as V8's code calls its StackGuard, and
// goes on natively: leaf, called natively by mid, ran its budget out at
// every 1,024 or so iterations, and Go finished it and resumed mid, every
// level unwound -- Crypto's am3 some 700 times a run. An interrupt due --
// the context cancelled by a Go function mid calls -- still stops the
// script there, within a budget. Each answer is the interpreter's.
func TestJITSSAPollsAsCallsOfGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var c={n:0};
		function leaf(n){let s=0;for(let i=0;i<n;i++){s=(s*3+i)|0;c.n++}return s}
		function mid(n){let t=0;for(let j=0;j<8;j++){if(j===5&&n<0)cancel();t=(t+leaf(n<0?-n:n))|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var at float64
	for _, rt := range []*Runtime{want, r} {
		rt.global.setOwnRaw(rt.atoms.intern("cancel"), rt.NewFunction("cancel", 0,
			func(rt *Runtime, _ Value, _ []Value) (Value, error) {
				at = rt.global.getOwn(rt.atoms.intern("c")).value().Object().getOwn(rt.atoms.intern("n")).value().num
				cancel()
				return Undefined, nil
			}), propDefault)
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.cache[weak.Make(cl.fn)]
	}
	for i := range 4 {
		wv, err := want.Run(compileForTest(t, `String(mid(5000))`))
		if err != nil {
			t.Fatal(err)
		}
		unwound, budgets, hosts := uint64(0), uint64(0), uint64(0)
		if r.jit != nil {
			unwound, budgets, hosts = r.jit.unwound, r.jit.budgets, r.jit.hosts
		}
		gv, err := r.Run(compileForTest(t, `String(mid(5000))`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			if e := entry("leaf"); e == nil || e.ssa == nil || e.link.in == 0 {
				t.Fatalf("leaf was not called natively: %+v", e)
			}
			if r.jit.budgets == budgets {
				t.Fatal("no loop ran its budget out")
			}
			if r.jit.unwound != unwound || r.jit.hosts != hosts {
				t.Fatalf("native code left for Go: %d unwound, %d exits", r.jit.unwound-unwound, r.jit.hosts-hosts)
			}
		}
	}
	r.SetContext(ctx)
	_, err := r.Run(compileForTest(t, `String(mid(-5000))`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled script ended with %v", err)
	}
	n := r.global.getOwn(r.atoms.intern("c")).value().Object().getOwn(r.atoms.intern("n")).value().num
	if n-at > 2*backEdgeCheckInterval {
		t.Fatalf("leaf ran %v iterations past its interrupt", n-at)
	}
}

// A store of a number to an element native code does not make -- into a
// hole, or at the array's length -- is Go's, called from native code
// (ssa.GoElemWrite), as V8's code calls its grow stub, where it runs no
// script and cannot throw (jitStoreElem): Crypto's BigInteger fills its
// arrays from their ends down and extends them at their lengths, and its
// code had left for Go at each such store. Where something could take the
// store -- an array that may not grow, an element setter up the chain --
// Go refuses it, and the code leaves for Go as before. Each answer is the
// interpreter's.
func TestJITSSAElementStoresGrow(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function fill(a,n){let i=n;while(--i>=0)a[i]=i*2;for(i=0;i<n;i++)a[n+i]=i+0.5;return a}
		function sum(a){let s=0;for(let i=0;i<a.length;i++)s+=a[i];return s}
		function run(k){let t=0;for(let j=0;j<k;j++)t+=sum(fill([],20+(j&3)));return t}
		function sealed(k){let a=Object.preventExtensions([1,2,3]),t=0;for(let j=0;j<k;j++){try{fill(a,2)}catch(e){t++}}return t+':'+a.join()}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	rounds := []string{`String(run(200))`, `String(run(200))`, `String(run(200))`, `String(run(200))`,
		`sealed(50)`, `var seen=0;Object.defineProperty(Array.prototype,'21',{set(v){seen++},configurable:true});String(run(40))+':'+seen`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		hosts, unwound := uint64(0), uint64(0)
		if r.jit != nil {
			hosts, unwound = r.jit.hosts, r.jit.unwound
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			cl := r.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
			if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil {
				t.Fatalf("fill has no code: %+v", e)
			}
			if r.jit.hosts != hosts || r.jit.unwound != unwound {
				t.Fatalf("native code left for Go: %d exits, %d unwound", r.jit.hosts-hosts, r.jit.unwound-unwound)
			}
		}
	}
}

// s.charCodeAt(i) is String.prototype.charCodeAt's intrinsic in any
// function the new pipeline compiles, a helper with no loop too, and the
// intrinsic is in every context a run's native calls run in (jitShared):
// it was only in functions with a loop, and only in the run's own context,
// so Crypto's intAt left native code at every call, until bnpFromString
// stopped calling it natively and Go ran each of its 3,000 calls a run.
// Replaced, the function a script gave is what is called. Each answer is
// the interpreter's.
func TestJITSSACharCodeAtInNativeCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var T=[];for(var k=48;k<58;k++)T[k]=k-48;
		function intAt(s,i){var c=T[s.charCodeAt(i)];return (c==null)?-1:c}
		function parse(s){let t=0;for(let i=0;i<s.length;i++){t=(t*7+intAt(s,i))|0}return t}
		function codes(s,i){let t=0;for(let k=0;k<2;k++)t+=s.charCodeAt(i+k);return t}
		function pairs(s){let t=0;for(let i=0;i+1<s.length;i++)t=(t*3+codes(s,i))|0;return t}
		var str='31415926535897932384626433832795';`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	src := `(function(){let u=0;for(let j=0;j<40;j++)u=(u+parse(str)+pairs(str))|0;return String(u)})()`
	rounds := []string{src, src, src, src, `String.prototype.charCodeAt=function(i){return 50};` + src}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		hosts, unwound := uint64(0), uint64(0)
		if r.jit != nil {
			hosts, unwound = r.jit.hosts, r.jit.unwound
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			// Called natively, or inlined, as a helper with no operation
			// left to Go may be.
			cl := r.global.getOwn(r.atoms.intern("intAt")).value().Object().fn().closure
			if e := r.jit.cache[weak.Make(cl.fn)]; e != nil && e.notNative {
				t.Fatalf("intAt is not called natively: %+v", e)
			}
			pl := r.global.getOwn(r.atoms.intern("parse")).value().Object().fn().closure
			if e := r.jit.cache[weak.Make(pl.fn)]; e == nil || e.ssa == nil {
				t.Fatalf("parse has no code: %+v", e)
			}
			// codes, with a loop, is called natively, in a context of its own.
			cc := r.global.getOwn(r.atoms.intern("codes")).value().Object().fn().closure
			if e := r.jit.cache[weak.Make(cc.fn)]; e == nil || e.ssa == nil || e.notNative || e.link.in == 0 {
				pe := r.jit.cache[weak.Make(r.global.getOwn(r.atoms.intern("pairs")).value().Object().fn().closure.fn)]
				t.Fatalf("codes was not called natively: notNative %v nativeIn %d entries %d; pairs %v", e.notNative, e.link.in, e.ssaStats.entries, func() string {
					if pe == nil {
						return "no entry"
					}
					return fmt.Sprintf("code %v nativeCalls %d inlines %d callSites %v goCalls %d hosts %d entries %d", pe.ssa != nil, len(pe.nativeCalls), len(pe.inlines), pe.callSites, pe.ssaStats.goCalls, pe.ssaStats.hosts, pe.ssaStats.entries)
				}())
			}
			if r.jit.hosts != hosts || r.jit.unwound != unwound {
				t.Fatalf("native code left for Go: %d exits, %d unwound", r.jit.hosts-hosts, r.jit.unwound-unwound)
			}
		}
	}
}

// An element read as a reference that native code does not find -- past
// the array's length, a hole -- is undefined, Go says, where nothing up
// the chain has an element or an index (ssa.GoElemCell): the read is then
// of a cell that is undefined, and native code goes on. A table read past
// what it holds, T[s.charCodeAt(i)], had left native code at each such
// read. Where something could answer -- an element of Array.prototype, a
// getter for an index on Object.prototype -- Go refuses, and the code
// leaves for Go as before. Each answer is the interpreter's.
func TestJITSSAAbsentElementsAreUndefined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var T=[];for(var k=48;k<58;k++)T[k]=k-48;
		function look(s,i){let c=0;for(let j=0;j<1;j++)c=T[s.charCodeAt(i)];return (c==null)?-1:c}
		function scan(s){let t=0;for(let i=0;i<s.length;i++){t=(t*7+look(s,i))|0;t=(t+(T[i+70]===undefined?1:2))|0}return t}
		var str='3141 59x26/53';`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	src := `(function(){let u=0;for(let j=0;j<40;j++)u=(u+scan(str))|0;return String(u)})()`
	rounds := []string{src, src, src, src,
		`Array.prototype[32]=5;` + src,
		`delete Array.prototype[32];Object.defineProperty(Object.prototype,'75',{get(){return 9},configurable:true});` + src}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		hosts, unwound := uint64(0), uint64(0)
		if r.jit != nil {
			hosts, unwound = r.jit.hosts, r.jit.unwound
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			for _, name := range []string{"look", "scan"} {
				cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
				if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || e.notNative {
					t.Fatalf("%s does not run natively: %+v", name, e)
				}
			}
			if r.jit.hosts != hosts || r.jit.unwound != unwound {
				t.Fatalf("native code left for Go: %d exits, %d unwound", r.jit.hosts-hosts, r.jit.unwound-unwound)
			}
		}
	}
}

// A string's charCodeAt is read where String.prototype has it now, its
// shape checked, and the call compares it with the intrinsic: a call native
// code makes may replace it, in the middle of a loop, and the next call is
// then the script's function's. The method had been the context's snapshot,
// taken by Go when it entered the code, and compared with itself: the
// intrinsic went on being called after a callback replaced it. Each answer
// is the interpreter's.
func TestJITSSACharCodeAtReplacedWhileNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, tc := range []struct{ name, setup, body, check string }{
		{"callback", `function cb(i){if(i===20)String.prototype.charCodeAt=function(){return 88}}`, `a[i]=s.charCodeAt(i);cb(i);`, `a[19]===84&&a[20]===85&&a[21]===88`},
		{"getter", `function cb(i){if(i===20)Object.defineProperty(String.prototype,'charCodeAt',{get(){return function(){return 7}},configurable:true})}`, `a[i]=s.charCodeAt(i);cb(i);`, `a[20]===85&&a[21]===7`},
		{"callee", `function code(s,i){let t=0;for(let k=0;k<1;k++)t=s.charCodeAt(i);return t}function cb(i){if(i===20)String.prototype.charCodeAt=function(){return 99}}`, `a[i]=code(s,i);cb(i);`, `a[20]===85&&a[21]===99`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := jitRuntimeForTest(t, Config{JIT: true})
			source := tc.setup + `let s='',a=new Array(64).fill(0);for(let i=0;i<64;i++)s+=String.fromCharCode(65+i);function pack(s,a){for(let i=0;i<64;i++){` + tc.body + `}return a}pack(s,a);` + tc.check
			v, err := r.Run(compileForTest(t, source))
			if err != nil || !v.IsBool() || !v.Truthy() {
				t.Fatalf("%s: %v, %v", tc.name, v.IsBool() && v.Truthy(), err)
			}
			if r.JITStats().SSAEntries == 0 {
				t.Fatal("pack never ran natively")
			}
		})
	}
}

// A call native code makes may release the runtime's JIT (releaseJIT),
// dropping every entry: the run it is suspended in goes on with the state
// its contexts and pins are in, which is released only once no native run
// is under way. It had been dropped at once, and the run's return to Go
// read a nil state.
func TestJITSSAReleasedWhileNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	released := 0
	r.global.setOwnRaw(r.atoms.intern("host"), r.NewFunction("host", 0,
		func(rt *Runtime, _ Value, _ []Value) (Value, error) {
			rt.releaseJIT()
			released++
			runtime.GC()
			return Undefined, nil
		}), propDefault)
	src := `function f(o,a,n){let s=0;for(let i=0;i<n;i++){o.x=7;if(i===n-3)host();a[0]+=1;s+=o.x+a[0]}return s}
		let o={x:3},a=[0];f(o,a,200);f(o,a,200);let t=f(o,a,200);t+':'+a[0]`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	want.global.setOwnRaw(want.atoms.intern("host"), want.NewFunction("host", 0,
		func(*Runtime, Value, []Value) (Value, error) { return Undefined, nil }), propDefault)
	wv, err := want.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.Run(compileForTest(t, src))
	if err != nil || v.String().Go() != wv.String().Go() || released != 3 {
		t.Fatalf("released while native: %v, %v, %d releases; interpreter %s", v, err, released, wv.String().Go())
	}
	if r.JITStats().SSAEntries == 0 {
		t.Fatal("f never ran natively")
	}
}

// TestJITSSAMathFunctions: every Math function of numbers runs natively,
// as V8's code makes them -- an instruction of its own (a rounding, sign,
// fround, max, min) or Go called from native code, the VM's very function
// -- with every answer the interpreter's, to the bit: zeros' signs, the
// ties Math.round breaks upward, numbers past 2**52, subnormals, the
// infinities and NaN, of each function the VM's Math has. Without SSE4.1
// (abi.Encoding's Round), Go rounds. A Math function a script replaced is
// what is called.
func TestJITSSAMathFunctions(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	t.Run("native", testJITSSAMathFunctions)
	if jitEncoding.Round {
		t.Run("noround", func(t *testing.T) {
			jitEncoding.Round = false
			defer func() { jitEncoding.Round = true }()
			testJITSSAMathFunctions(t)
		})
	}
}

func testJITSSAMathFunctions(t *testing.T) {
	var src strings.Builder
	src.WriteString(`var xs=[0,-0,0.5,-0.5,1.5,-1.5,2.5,-2.5,0.49999999999999994,-0.49999999999999994,0.7,-0.7,3.7,-3.7,
		4503599627370495.5,-4503599627370495.5,4503599627370497,2**53,2**52-0.5,1e300,-1e300,5e-324,-5e-324,1/3,Math.PI,-Math.E,
		Infinity,-Infinity,NaN,123456789.987,1e-7,0.99999999999999989,-0.99999999999999989,0.25,-1,1,3.4028235677973366e38,1e39];
		var fa=new Float64Array(1),ua=new Uint32Array(fa.buffer);
		function bits(o){return o.map(function(v){fa[0]=v;return ua[1].toString(16)+':'+ua[0].toString(16)}).join()}
		var L=xs.length;`)
	calls := map[string]string{}
	for _, name := range unaryMathNames {
		calls["u_"+name] = "Math." + name + "(x)"
	}
	for name, call := range map[string]string{"atan2": "Math.atan2(x,y)", "hypot": "Math.hypot(x,y)", "pow": "Math.pow(x,y)",
		"max2": "Math.max(x,y)", "min2": "Math.min(x,y)", "max1": "Math.max(x)", "min3": "Math.min(x,y,z)", "max4": "Math.max(x,y,z,y)",
		"minneg": "Math.min(x,-x)", "maxneg": "Math.max(-x,x)"} {
		calls["b_"+name] = call
	}
	names := slices.Sorted(maps.Keys(calls))
	for _, name := range names {
		fmt.Fprintf(&src, "var o_%[1]s=xs.slice();function %[1]s(n){const o=o_%[1]s;for(let i=0;i<n;i++){const x=xs[i%%L],y=xs[(i*7+3)%%L],z=xs[(i*5+1)%%L];o[i%%L]=%[2]s}return o}\n", name, calls[name])
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src.String())); err != nil {
			t.Fatal(err)
		}
	}
	exits := func(name string) (uint64, *jitEntry) {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		e := r.jit.cache[weak.Make(cl.fn)]
		if e == nil {
			return 0, nil
		}
		return e.ssaStats.hosts + e.ssaStats.guards, e
	}
	for round := 0; round < 7; round++ {
		if round == 6 {
			// Replaced: each call is the script's function's.
			for _, rt := range []*Runtime{want, r} {
				if _, err := rt.Run(compileForTest(t, `Math.floor=function(x){return x+1};Math.max=function(){return arguments.length};
					Math.sin=Math.cos;Math.pow=Math.atan2;Math.round=Math.trunc`)); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, name := range names {
			call := "bits(" + name + "(" + strconv.Itoa(97*3) + "))"
			wv, err := want.Run(compileForTest(t, call))
			if err != nil {
				t.Fatal(err)
			}
			before, _ := exits(name)
			gv, err := r.Run(compileForTest(t, call))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := gv.String().Go(), wv.String().Go(); got != want {
				t.Fatalf("round %d, %s: got\n%s\ninterpreter\n%s", round, calls[name], got, want)
			}
			if round == 5 {
				after, e := exits(name)
				if e == nil || e.ssa == nil || e.entrySlow {
					t.Fatalf("%s did not run natively: %+v", calls[name], e)
				}
				if after != before {
					t.Fatalf("%s left native code %d times", calls[name], after-before)
				}
			}
		}
	}
}

// A forwarding constructor's initialize that constructs another class,
// RayTrace's IntersectionInfo making a Color, is inlined with that
// construction inlined in it: the constructors do not count toward how
// deep calls are inlined, and the method's construction is decided for it
// when it has no code of its own. The inner pool running out leaves at the
// inner construction, inside the inlined frames, for Go to fill it again:
// that does not count against inlining the outer, which stays inlined.
// Each answer is the interpreter's.
func TestJITSSAInlinedNestedForwardingConstructions(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var Class={create:function(){return function(){this.initialize.apply(this,arguments)}}};
		var NS={};NS.B=Class.create();NS.B.prototype={initialize:function(x,y){this.x=x;this.y=y}};
		NS.A=Class.create();NS.A.prototype={hit:false,initialize:function(){this.c=new NS.B(1,2)}};
		function f(n){let s=0;for(let i=0;i<n;i++){const a=new NS.A();a.hit=true;s=(s+a.c.x+a.c.y)|0}return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	// Rounds enough for f to settle: what its inlined callees' sites met
	// it is compiled again for once a round has spent its budget
	// (jitTierUp).
	for i := range 9 {
		wv, err := want.Run(compileForTest(t, `String(f(300))`))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, `String(f(300))`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i >= 6 {
			e := r.jit.cache[weak.Make(cl.fn)]
			// Both constructors and both initialize methods.
			if e == nil || e.ssa == nil || e.entrySlow || len(e.notInline) != 0 || len(e.ssaInlined) < 4 {
				t.Fatalf("round %d: f does not inline its nested constructions: %+v", i, e)
			}
			// Only to fill the two pools again.
			if n := e.ssaStats.hosts - hosts; n > 2*300/abi.PoolSize+4 {
				t.Fatalf("round %d: f left native code %d times for 600 constructions", i, n)
			}
		}
	}
}

// An object literal is made natively: its object from the site's pool
// (jitFeedback.Literal), made as new_object makes it, its fields defined
// along the shapes' transitions (Define), its prototypes not looked at --
// a setter on Object.prototype is not called. In a function called
// natively, in a loop, nested. A literal with anything else done to its
// object -- a spread, an accessor, a computed key, __proto__, a key twice
// -- has Go define its fields. Splay's GeneratePayloadTree, which makes a
// literal at each node, ran in the tree tier (new_object refused the
// function). Each answer is the interpreter's.
func TestJITSSAObjectLiterals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function mk(x,y){return {a:x,b:y,c:x+y}}
		function run(n){let s=0;for(let i=0;i<n;i++){const o=mk(i,1),p={q:{r:i},t:o.c};s=(s+o.a+o.b+o.c+p.q.r+p.t)|0}return s}
		function odd(n){let s=[];for(let i=0;i<n;i++){const k='k'+(i&1),src={z:i};
			const os=[{...src,a:i},{get g(){return i},a:1},{[k]:i},{__proto__:null,a:i},{a:1,a:i}];
			s.push(os.map(o=>JSON.stringify(Object.getOwnPropertyNames(o))+':'+o.a).join())}return s.join('|')}
		function shapes(n){let s=[];for(let i=0;i<n;i++){const o={a:i,b:2};
			s.push(Object.keys(o).join()+(o.a===i)+JSON.stringify(o)+Object.getOwnPropertyDescriptor(o,'b').value+o.hasOwnProperty('a'))}return s.join()}
		function last(n){let o;for(let i=0;i<n;i++)o={a:i,b:2};return o}
		function late(n){let s=0,o;for(let i=0;i<n;i++){if(i>=n-50){o={a:i,b:i+1};s=(s+o.a+o.b)|0}else s=(s+i)|0}return s}
		function look(o){return JSON.stringify(o)+Object.getOwnPropertyNames(o)+o.hasOwnProperty('b')+('a' in o)}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	late := r.global.getOwn(r.atoms.intern("late")).value().Object().fn().closure
	rounds := []string{`String(run(300))`, `String(run(300))`, `String(run(300))`, `String(run(300))`, `odd(40)`, `odd(40)`,
		`shapes(100)`, `shapes(100)`, `look(last(300))`, `look(last(300))`, `look(last(300))`,
		`String(late(400))`, `String(late(400))`, `String(late(400))`, `String(late(400))`,
		`Object.defineProperty(Object.prototype,'a',{set(v){throw new Error('set')},configurable:true});[run(300),shapes(5)].join()`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts, lateHosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		if e := r.jit.cache[weak.Make(late.fn)]; e != nil {
			lateHosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if src == `String(late(400))` && i == 14 {
			// Its literal first met after the code was compiled: compiled
			// again once Go made the fields' transitions (jitDefineSeen).
			e := r.jit.cache[weak.Make(late.fn)]
			if e == nil || e.ssa == nil || e.entrySlow || e.ssaStats.hosts-lateHosts > 2 {
				t.Fatalf("late leaves native code at its literal: %+v", e)
			}
		}
		if i == 3 {
			e := r.jit.cache[weak.Make(cl.fn)]
			// Its own: an inlined callee's, which it makes from pools of
			// its own too, aside.
			if e == nil || e.ssa == nil || e.entrySlow || len(slices.DeleteFunc(slices.Clone(e.literals), func(l jitLiteral) bool { return l.fn != cl.fn })) != 2 {
				t.Fatalf("run does not make its literals natively: %+v", e)
			}
			// Only for the pools to be filled again.
			if n := e.ssaStats.hosts - hosts; n > 2*300/abi.PoolCapacity+4 {
				t.Fatalf("run left native code %d times", n)
			}
		}
	}
}

// A string constant is read natively, from a cell the code keeps it in
// (jitFeedback.StringCell), its pointer word there: compared, stored in a
// property and an array literal, passed to a native call, returned. Go had
// pushed each one (jitHost), leaving native code at every push. Each
// answer is the interpreter's.
func TestJITSSAStringConstants(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var o={k:''};function same(a,b){return a===b?1:0}
		function run(xs,n){let s=0,last;for(let i=0;i<n;i++){const x=xs[i%xs.length];
			s=(s+(x==='abc'?1:0)+same(x,'xyz')*10)|0;o.k='set';last=['p','q',x]}return [s,o.k,last.join(''),'end'].join()}
		var xs=['abc','xyz','abc','',1]`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	for i := range 5 {
		wv, err := want.Run(compileForTest(t, `run(xs,300)`))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, `run(xs,300)`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 4 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.entrySlow || len(e.ssaCells) == 0 {
				t.Fatalf("run does not read its string constants natively: %+v", e)
			}
			// The loop's array literal's pool refilled, and the join after it.
			if n := e.ssaStats.hosts - hosts; n > 300/abi.PoolCapacity+4 {
				t.Fatalf("run left native code %d times", n)
			}
		}
	}
}

// A captured binding is read and assigned natively, in its cell each time
// (ir.UpvalueRead, ir.UpvalueWrite), and a closure that has some is called
// natively, its context given its cells: the V8 suite's Math.random,
// seed = ... in a closure, which every suite calls, refused for
// set_upvalue. A read after a native call that assigned the binding sees
// the assignment; a binding read before its initialization, or a constant
// assigned, is the interpreter's error. Each answer is the interpreter's.
func TestJITSSACapturedBindings(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var rnd=(function(){var seed=49734321;return function(){
			seed=((seed+0x7ed55d16)+(seed<<12))&0xffffffff;seed=((seed^0xc761c23c)^(seed>>>19))&0xffffffff;
			return (seed&0xfffffff)/0x10000000}})();
		function run(n){let s=0;for(let i=0;i<n;i++)s=(s+(rnd()*1000|0))|0;return s}
		var pair=(function(){let c=0;return {inc(){c=c+1},get(){return c}}})();
		function both(n){let s=0;for(let i=0;i<n;i++){pair.inc();s=(s+pair.get()*3)|0}return s}
		function tdz(){try{const f=()=>x;f();let x=1;return 'no'}catch(e){return e.name}}
		function cst(){const k=1;const g=()=>{k=2};try{g();return 'no'}catch(e){return e.name}}
		var step=(function(){let v=0;function bump(){v=v+2}return function(n){let s=0;for(let i=0;i<n;i++){v=v+1;bump();s=(s+v)|0}return s}})();
		var grow=(function(){let v=1,w=3;function bump(){v=v<250?v+1:'x'+v}return function(n){v=1;let s=0;for(let i=0;i<n;i++){s=(s*3+v+w)%65521;bump()}return s+'|'+v}})();
		var keep=(function(){let o={k:1};function swap(){o=o.k===1?{k:2}:{k:1}}return function(n){let s=0;for(let i=0;i<n;i++){const w=o;swap();s=(s*3+w.k*10+o.k)|0}return s}})();`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	rnd := r.global.getOwn(r.atoms.intern("rnd")).value().Object().fn().closure
	step := r.global.getOwn(r.atoms.intern("step")).value().Object().fn().closure
	grow := r.global.getOwn(r.atoms.intern("grow")).value().Object().fn().closure
	keep := r.global.getOwn(r.atoms.intern("keep")).value().Object().fn().closure
	rounds := []string{`String(run(300))`, `String(run(300))`, `String(run(300))`, `String(run(300))`,
		`String(both(300))`, `String(both(300))`, `String(both(300))`, `[tdz(),tdz(),cst(),cst()].join()`,
		`String(step(300))`, `String(step(300))`, `String(step(300))`, `String(step(300))`,
		`grow(200)`, `grow(200)`, `grow(300)`, `grow(300)`, `grow(300)`, `grow(200)`,
		`String(keep(301))`, `String(keep(301))`, `String(keep(301))`, `String(keep(301))`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.entrySlow || len(e.nativeCalls) == 0 {
				t.Fatalf("run does not call rnd natively: %+v", e)
			}
			if n := e.ssaStats.hosts - hosts; n > 1 {
				t.Fatalf("run left native code %d times", n)
			}
			if re := r.jit.cache[weak.Make(rnd.fn)]; re == nil || re.ssa == nil || re.notNative {
				t.Fatalf("rnd has no native code: %+v", re)
			}
		}
		if i == len(rounds)-1 {
			// step reads v again after bump, called natively, assigned it;
			// grow, after bump made v a string, which its code took for a
			// number; keep, o's old object, w, after swap replaced it.
			for name, cl := range map[string]*closure{"step": step, "grow": grow, "keep": keep} {
				if e := r.jit.cache[weak.Make(cl.fn)]; e == nil || e.ssa == nil || len(e.nativeCalls) == 0 {
					t.Fatalf("%s makes no native calls: %+v", name, e)
				}
			}
		}
	}
}

// An addition one operand of which is a string constant, or such an
// addition's result, is Go's (compile's stringConcats), not a speculation on
// numbers: RayTrace's Vector.prototype.subtract, if(!w||!v) throw
// 'Vectors must be defined [' + v + ',' + w + ']', had its code take v and w
// for numbers, at its entry too, once its constants were read natively,
// and leave at every call. And a function only native callers call learns
// a speculation that failed in it, as one Go enters does: it was never
// compiled again. Each answer is the interpreter's.
func TestJITSSAStringConcatsAndCalleeDeopts(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function sub(v,w){if(!w||!v)throw 'Vectors ['+v+','+w+']';return v.x-w.x}
		function run(n){let s=0;const a={x:5},b={x:2};for(let i=0;i<n;i++)s=(s+sub(a,b)+i)|0;return s}
		function bad(){try{return sub({x:1},undefined)}catch(e){return e}}
		function dbl(x){let r=0;for(let k=0;k<2;k++)r=r+x*1;return r}
		function mix(xs,n){let s=0;for(let i=0;i<n;i++){const r=dbl(xs[i%xs.length]);s=(s+(typeof r==='number'?r:1))|0}return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	fnOf := func(name string) *bytecode.Function {
		return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
	}
	rounds := []string{`String(run(300))`, `String(run(300))`, `String(run(300))`, `String(run(300))`, `bad()`,
		`String(mix([1,2,3],300))`, `String(mix([1,2,3],300))`, `String(mix([1,'a',3],300))`, `String(mix([1,'a',3],300))`,
		`String(mix([1,'a',3],300))`, `String(mix([1,'a',3],300))`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var guards uint64
		if e := r.jit.cache[weak.Make(fnOf("sub"))]; e != nil {
			guards = e.ssaStats.guards
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			e := r.jit.cache[weak.Make(fnOf("run"))]
			if e == nil || e.ssa == nil || e.entrySlow {
				t.Fatalf("run did not run natively: %+v", e)
			}
			if s := r.jit.cache[weak.Make(fnOf("sub"))]; s == nil || s.ssa == nil || s.notNative || s.ssaStats.guards != guards ||
				len(s.failed) != 0 || len(s.failedEntries) != 0 {
				t.Fatalf("sub's speculation failed: %+v", s)
			}
		}
		if i == 7 {
			// At its first failure, not once Go enters it for leaving too
			// often (notNative), which a callee native code calls never is
			// until then.
			e := r.jit.cache[weak.Make(fnOf("dbl"))]
			if e == nil || e.ssa == nil || e.reopts == 0 || len(e.failed) == 0 {
				t.Fatalf("dbl was not compiled again for what failed in it: %+v", e)
			}
		}
	}
}

// An array literal of up to ir.MaxArrayLiteral elements is made natively:
// its array from the site's pool, with room for the elements and none yet,
// the operands written in, numbers and references alike, and its length
// set -- no setter on Array.prototype is asked. In a natively called
// function, in a loop, nested, empty; one of more elements, Go's. Splay's
// GeneratePayloadTree leaves its leaves at one. Each answer is the
// interpreter's.
func TestJITSSAArrayLiterals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var o={k:1},str='s';
		function mk(x){return [x,x+1,str,o,[x],[],null,undefined,true,1.5]}
		function run(n){let s=0;for(let i=0;i<n;i++){const a=mk(i),b=[i,2];s=(s+a[0]+a[1]+a.length+a[4][0]+a[5].length+b[1]+(a[3]===o?1:0))|0}return s}
		function last(n){let a;for(let i=0;i<n;i++)a=[i,'x'+i,{v:i},[i,i]];return a}
		function big(n){let s=0;for(let i=0;i<n;i++){const a=[i,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16];s=(s+a[16]+a.length)|0}return s}
		function look(a){a.push(7);return JSON.stringify(a)+a.length+Array.isArray(a)+(Object.getPrototypeOf(a)===Array.prototype)}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	rounds := []string{`String(run(300))`, `String(run(300))`, `String(run(300))`, `String(run(300))`,
		`look(last(300))`, `look(last(300))`, `look(last(300))`, `String(big(100))`, `String(big(100))`,
		`Object.defineProperty(Array.prototype,'1',{set(v){throw new Error('set')},configurable:true});[run(300),look(last(10))].join()`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.entrySlow {
				t.Fatalf("run does not run natively: %+v", e)
			}
			// Its own, and mk's, inlined, from pools of its own too.
			if own := len(slices.DeleteFunc(slices.Clone(e.literals), func(l jitLiteral) bool { return l.fn != cl.fn })); own != 1 || len(e.literals) == own {
				t.Fatalf("run does not make its array literals, and mk's, natively: %+v", e)
			}
			if n := e.ssaStats.hosts - hosts; n > 2*300/abi.PoolCapacity+4 {
				t.Fatalf("run left native code %d times", n)
			}
		}
	}
}

// A read compiled for the shapes it met (up to jitPropertyCases), one on a
// prototype among them, searches the own table of a receiver of any other
// shape rather than leave, as V8's megamorphic loads look further: objects
// given their properties in more orders than the cases hold, some on their
// prototype -- RayTrace's IntersectionInfo -- had their readers leave at
// every one, and demoted. A property found on another prototype, or
// missing, still leaves. Each answer is the interpreter's.
func TestJITSSAMegamorphicOwnReads(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var P={x:50},objs=[Object.create(P),{x:1},{y:2,x:3},{z:4,x:5},{w:6,x:7},{v:8,x:9},{u:10,x:11},{t:12,s:13,x:14}];
		function sum(a,n){let s=0;for(let i=0;i<n;i++){s=(s+a[i%a.length].x)|0}return s}
		var odd=objs.concat([Object.create({x:100}),{q:1}])`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("sum")).value().Object().fn().closure
	rounds := []string{`String(sum(objs,700))`, `String(sum(objs,700))`, `String(sum(objs,700))`, `String(sum(objs,700))`,
		`String(sum(odd,900))`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.entrySlow {
				t.Fatalf("sum did not run natively: %+v", e)
			}
			if n := e.ssaStats.hosts - hosts; n > 1 {
				t.Fatalf("sum left native code %d times for eight shapes", n)
			}
		}
	}
}

// Looking at a construction's pools when native code leaves there makes
// nothing: it had concatenated the entry's two lists at every exit, which
// came to a sixth of RayTrace's allocations with construction on.
func TestJITPoolLookupsAllocateNothing(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function P(){} function f(){return new P()} f()`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	ctor := r.global.getOwn(r.atoms.intern("P")).value().Object()
	pc := slices.IndexFunc(cl.fn.Code, func(in bytecode.Instr) bool { return in.Op == bytecode.OpNew })
	if pc < 0 || r.jit == nil {
		t.Fatal("no construction")
	}
	pool := new(abi.ObjectPool)
	e := &jitEntry{inlines: []jitInline{{pc: int32(pc), cl: ctor.fn().closure, obj: ctor, pool: pool}},
		nativeCalls: []jitInline{{pc: int32(pc) + 1}}, link: &jitLink{}}
	// In place of the entry f's call compiled, as the cache replaces one.
	key := weak.Make(cl.fn)
	if old := r.jit.cache[key]; old != nil {
		r.jit.forget(key, old)
	}
	r.jit.remember(key, e)
	l := &jitNativeLevel{cl: cl, pc: uint64(pc), kind: abi.ExitHost}
	if !r.jitPoolEmptied(l) {
		t.Fatal("the empty pool is not found")
	}
	if n := testing.AllocsPerRun(100, func() { r.jitPoolEmptied(l) }); n != 0 {
		t.Fatalf("%v allocations a look", n)
	}
}

// A read of a function's own property -- its prototype, which the VM's
// caches never learn -- is made natively by searching the function's
// table, as RayTrace calls methods through Flog.RayTracer.Vector.prototype:
// the read had left native code at every call, and its functions were
// demoted; and the method so read is called natively from the first
// compile, found by the reads that name it. One the VM makes on demand, a
// prototype not read before, a length or a name, Go reads. Each answer is
// the interpreter's.
func TestJITSSAReadsFunctionProperties(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function V(x){this.x=x} V.prototype={add(a,b){return (a+b)|0}};
		function W(){} var NS={V:V,W:W};
		function run(n){let s=0;for(let i=0;i<n;i++){s=NS.V.prototype.add(s,i)}return s}
		var NS2={R:{V:V}};
		function run2(n){let s=0;for(let i=0;i<n;i++){s=NS2.R.V.prototype.add(s,i)}return s}
		function odd(n){let s=0;for(let i=0;i<n;i++){s=(s+NS.W.length+NS.V.name.length+(NS.W.prototype.constructor===W?1:0))|0}return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	cl2 := r.global.getOwn(r.atoms.intern("run2")).value().Object().fn().closure
	rounds := []string{`String(run(300))+run2(300)`, `String(run(300))+run2(300)`, `String(run(300))+run2(300)`, `String(run(300))+run2(300)`,
		`String(odd(300))`, `String(odd(300))`, `V.prototype={add(a,b){return (a-b)|0}};String(run(300))+run2(300)`, `String(run(300))+run2(300)`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 3 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.entrySlow {
				t.Fatalf("run did not run natively: %+v", e)
			}
			if n := e.ssaStats.hosts - hosts; n > 1 {
				t.Fatalf("run left native code %d times", n)
			}
			// The method found by the reads naming it as its first code
			// was compiled (jitCalleeAt), not learned at an exit after:
			// five reads for run2, RayTrace's
			// Flog.RayTracer.Vector.prototype.subtract's.
			for name, c := range map[string]*closure{"run": cl, "run2": cl2} {
				if e := r.jit.cache[weak.Make(c.fn)]; e == nil || e.ssa == nil || e.inlineReopts != 0 {
					t.Fatalf("%s was compiled again for its calls: %+v", name, e)
				}
			}
		}
	}
}

// A forwarding constructor's read of its method, this.initialize, is
// compiled for an object as the construction's pool holds them, found on
// the constructor's prototype -- also when the pool has run out as the
// caller is compiled, as RayTrace's are: compiled knowing nothing, the
// read left native code at every construction.
func TestJITForwardPropertyEmptyPool(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	setup := `var Class={create:function(){return function(){this.initialize.apply(this,arguments)}}};
		var Ray=Class.create();Ray.prototype={initialize:function(pos,dir){this.position=pos;this.direction=dir}}`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	ctor := r.global.getOwn(r.atoms.intern("Ray")).value().Object()
	proto := ctor.getOwnVisible(atomPrototype).value().Object()
	cl := ctor.fn().closure
	if cl.fn.Code[1].Op != bytecode.OpGetProp {
		t.Fatalf("the forwarding constructor reads its method with %v", cl.fn.Code[1].Op)
	}
	for _, full := range []bool{true, false} {
		pool := new(abi.ObjectPool)
		if full {
			r.jitFillPool(pool, ctor)
		}
		fb := &jitFeedback{r: r, fn: cl.fn, cl: cl, root: &jitFeedback{r: r}, fwd: &jitForwardFrame{ctor: ctor, pool: pool, argc: 2}}
		site, ok := fb.Property(1)
		if !ok || site.Shape == 0 || site.Holders[0].Object != uintptr(unsafe.Pointer(proto)) {
			t.Fatalf("pool filled %v: the read is compiled for %+v", full, site)
		}
	}
}

// A callee inlined before its property sites' caches knew anything --
// RayTrace's Ray, a forwarding constructor read as NS.Ray, compiled into
// its caller before initialize's stores had run in Go -- leaves at each
// such site, and its cache learns the shape there. As V8 optimizes again
// once it has the feedback it lacked, the caller is compiled again for it
// (jitFinishExit), and does not count those exits against inlining the
// callee (jitShapeMiss): before, sixteen of them stopped it, and every
// construction then left native code. Each answer is the interpreter's.
func TestJITSSAInlinedCalleeLearns(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var Class={create:function(){return function(){this.initialize.apply(this,arguments)}}};
		var Ray=Class.create();Ray.prototype={position:null,direction:null,initialize:function(pos,dir){this.position=pos;this.direction=dir}};
		var NS={Ray:Ray},d={x:2};
		function run(n){let s=0;for(let i=0;i<n;i++){const r=new NS.Ray(i,d);s=(s+r.position+r.direction.x)|0}return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	start := r.jit.reoptimized
	for i := range 5 {
		wv, err := want.Run(compileForTest(t, `String(run(300))`))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		reoptimized := r.jit.reoptimized
		gv, err := r.Run(compileForTest(t, `String(run(300))`))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i >= 2 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.entrySlow || len(e.notInline) != 0 {
				t.Fatalf("round %d: run does not inline the construction: %+v", i, e)
			}
			// Only to fill the pool again.
			if n := e.ssaStats.hosts - hosts; n > 300/abi.PoolSize+2 {
				t.Fatalf("round %d: run left native code %d times for 300 constructions", i, n)
			}
			if r.jit.reoptimized != reoptimized {
				t.Fatalf("round %d: run compiled again", i)
			}
		}
	}
	// Compiled again for what it learned in its first round, once for each
	// of its and its callee's five property sites that knew nothing, at
	// most, and never after.
	if n := r.jit.reoptimized - start; n == 0 || n > 5 {
		t.Fatalf("run compiled again %d times", n)
	}
}

// Code called natively that leaves on too many of its calls is compiled
// again for what its exits taught, its native callers calling it still, as
// V8 optimizes again after a deopt; only after jitUnwindReopts such
// compiles do they stop calling it (notNative). A callee that left while
// it learned -- RayTrace's Sphere.intersect, before its constructions were
// decided -- had been made notNative at once, with a backoff that kept it
// so for the rest of the run.
func TestJITSSAUnwoundCompilesAgainFirst(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function g(n){let s=0;for(let i=0;i<n;i++)s=(s+i)|0;return s}g(300);g(300)`)); err != nil {
		t.Fatal(err)
	}
	g := r.jit.cache[weak.Make(r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure.fn)]
	if g == nil || g.ssa == nil {
		t.Fatal("g has no code")
	}
	// An entry of its own, with code, as a callee's.
	e := &jitEntry{ssa: g.ssa, link: &jitLink{}}
	leave := func() {
		// One call in two leaves, its exits having learned a call to
		// inline (jitLearning; TestJITSSAUnwoundCompiledAgainOnlyForWhatItLearned
		// has one that learns nothing).
		e.inlinePending = true
		for range jitUnwindProbe {
			e.link.in += 2
			r.jitUnwound(e)
		}
	}
	for i := range jitUnwindReopts {
		leave()
		if !e.unwindReopt || e.notNative || int(e.unwindReopts) != i+1 {
			t.Fatalf("leaving too often %d times: compiled again %v, notNative %v", i+1, e.unwindReopt, e.notNative)
		}
		e.unwindReopt = false
	}
	leave()
	if !e.notNative || e.nativeBackoff != 1 {
		t.Fatalf("leaving too often after its compiles: notNative %v, backoff %d", e.notNative, e.nativeBackoff)
	}
}

// instanceof is answered by native code where its constructor is known, as
// V8 lowers it: a walk along the value's prototypes, after checks that the
// constructor is the one compiled for, that its Symbol.hasInstance is still
// the realm's, and its prototype property an object -- EarleyBoyer's
// sc_isPair, p instanceof sc_Pair, kept every function that asked in the
// tree tier. isP, which asks, is inlined in count. Primitives, null, an
// object made with Object.create, another class's; P.prototype replaced;
// Symbol.hasInstance defined on Q; a proxy, whose getPrototypeOf trap Go
// runs: each answer the interpreter's, and no exit in the loop while
// nothing changes.
func TestJITSSAInstanceOf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function P(a){this.a=a}function Q(){}
		function isP(x){return x instanceof P}
		function count(xs,n){let c=0;for(let i=0;i<n;i++){const x=xs[i%xs.length];if(isP(x))c++;if(x instanceof Q)c+=10}return c}
		var xs=[new P(1),null,3,"s",{},Object.create(P.prototype),new Q(),[1],undefined,P.prototype]`
	src := `String(count(xs,300))`
	rounds := []string{src, src, src, src, src, src,
		`P.prototype={};xs.push(new P(2));` + src,
		`Object.defineProperty(Q,Symbol.hasInstance,{value:function(v){return typeof v==="number"}});` + src,
		`xs.push(new Proxy({},{getPrototypeOf(){return P.prototype}}));` + src}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("count")).value().Object().fn().closure
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 5 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.entrySlow || len(e.ssaInlined) == 0 {
				t.Fatalf("count did not run natively with isP inlined: %+v", e)
			}
			if n := e.ssaStats.hosts - hosts; n > 1 {
				t.Fatalf("count left native code %d times", n)
			}
		}
	}
}

// typeof compared with a string constant is told natively, as V8 folds it
// into a check of the value: number, string, boolean, undefined (an
// [[IsHTMLDDA]] object's too) and function, as a value and as a branch,
// equal and not; isNum, which asks, is inlined in kinds; an [[IsHTMLDDA]]
// object natively too. A proxy, whose
// callability its target says, has Go tell it, at the typeof, and the
// invocation goes on in the interpreter; typeof alone is Go's. EarleyBoyer's
// sc_isNumber, typeof n === "number", kept every function that called it out
// of native code. Each answer is the interpreter's.
func TestJITSSATypeOf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `function isNum(x){return typeof x==="number"}
		function kinds(xs,n){let c=0;for(let i=0;i<n;i++){const x=xs[i%xs.length];if(isNum(x))c+=1;if(typeof x==="string")c+=10;
			if(typeof x!=="boolean")c+=100;if(typeof x=="undefined")c+=1000;c+=(typeof x==="function")?10000:0}return [c,typeof xs[2]].join()}
		var plain=[1,NaN,-0,"s","",true,false,undefined,null,{},[],function(){},Object,class C{},Symbol("q"),10n],
			withDDA=plain.concat([dda]),odd=plain.concat([new Proxy({},{}),new Proxy(function(){},{})])`
	rounds := []string{`kinds(plain,300)`, `kinds(plain,300)`, `kinds(plain,300)`, `kinds(plain,300)`, `kinds(plain,300)`,
		`kinds(withDDA,300)`, `kinds(odd,300)`, `kinds(plain,300)`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		dda := newObject(rt.proto.object, ClassObject)
		dda.flags |= objHTMLDDA
		rt.global.setOwnRaw(rt.atoms.intern("dda"), Obj(dda), propDefault)
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("kinds")).value().Object().fn().closure
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(cl.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		if i == 4 {
			e := r.jit.cache[weak.Make(cl.fn)]
			if e == nil || e.ssa == nil || e.entrySlow || len(e.ssaInlined) == 0 {
				t.Fatalf("kinds did not run natively with isNum inlined: %+v", e)
			}
			// Only after its loop, a few: typeof alone, the array, join;
			// none in its 300 iterations.
			if n := e.ssaStats.hosts - hosts; n > 5 {
				t.Fatalf("kinds left native code %d times", n)
			}
		}
	}
}

// A guard's exit Go has no entry to go on natively after leaves the rest
// of its loop's iteration to the interpreter, which goes on natively at the
// header, in the same run (jitGoOn): one read of a primitive in a
// thousand iterations leaves once, and the loop's stretches go on. Code
// that leaves so at every other iteration does the same each time, and is
// never compiled with an entry where Go went on: as V8's code, it is
// entered at its start and its loop's header alone. Each answer is the
// interpreter's.
func TestJITSSAGoesOnNatively(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var obj={x:2};
		function once(n){let t=0;for(let i=0;i<n;i++){const o=i===n>>1?1:obj;t=(t+(o.x|0))|0}return t}
		function every(n){let t=0;for(let i=0;i<n;i++){const o=i&1?1:obj;t=(t+(o.x|0))|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	closure := func(name string) *closure {
		return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	}
	checked := 0
	for i := 0; i < 6; i++ {
		for _, src := range []string{`once(1000)`, `every(1000)`} {
			wv, err := want.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			cl := closure(src[:len(src)-6])
			var before jitSSAStats
			var serial uint32
			valid := false
			if e := r.jit.hint(cl); e != nil {
				before, serial, valid = e.ssaStats, e.ssaSerial, e.ssa != nil && !e.invalid
			}
			interpreted, wentOn := r.jit.interpreted, r.jit.wentOn
			gv, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := gv.Number(), wv.Number(); got != want {
				t.Fatalf("round %d, %s: got %v, interpreter %v", i, src, got, want)
			}
			e := r.jit.hint(cl)
			if e == nil || e.ssa == nil || before.entries == 0 || !valid || e.ssaSerial != serial || e.invalid {
				// Compiled before the reads met a primitive, which
				// deoptimized, made them megamorphic and invalidated the
				// code; or compiled again for it during the round.
				continue
			}
			checked++
			if pcs, want := jitEntryPCs(e, cl.fn), jitLoopHeadersOf(cl.fn); !slices.Equal(pcs, want) {
				t.Fatalf("round %d: %s entered at %v; want its start and loop header, %v", i, src, pcs, want)
			}
			// The read of 1 left once (or at every other iteration), the
			// iteration went on in the interpreter, and the loop natively:
			// a stretch of its own each time.
			leaves := uint64(1)
			if src == `every(1000)` {
				leaves = 500
			}
			if n := e.ssaStats.hosts - before.hosts; n != leaves {
				t.Errorf("round %d: %s left native code %d times, want %d", i, src, n, leaves)
			}
			if n := e.ssaStats.entries - before.entries; n < leaves+1 {
				t.Errorf("round %d: %s ran %d stretches; the loop did not go on natively", i, src, n)
			}
			if n, on := r.jit.interpreted-interpreted, r.jit.wentOn-wentOn; n != leaves || on != leaves {
				t.Errorf("round %d: %s: %d runs of the interpreter, %d gone on from natively in the same run, want %d", i, src, n, on, leaves)
			}
		}
	}
	if checked < 4 {
		t.Fatalf("checked %d runs of compiled code", checked)
	}
}

// jitEntryPCs is where e's code may be entered, by fn's PCs.
func jitEntryPCs(e *jitEntry, fn *bytecode.Function) []int {
	var pcs []int
	for pc := range fn.Code {
		if e.ssa.HasEntry(pc) {
			pcs = append(pcs, pc)
		}
	}
	return pcs
}

// jitLoopHeadersOf is fn's start and its loops' headers, the targets of its
// backward jumps, in order.
func jitLoopHeadersOf(fn *bytecode.Function) []int {
	pcs := []int{0}
	for pc, in := range fn.Code {
		switch in.Op {
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse:
			if int(in.A) <= pc && !slices.Contains(pcs, int(in.A)) {
				pcs = append(pcs, int(in.A))
			}
		}
	}
	slices.Sort(pcs)
	return pcs
}

// concat (a string chain's, or a template's) is made by Go, called from
// native code (ssa.GoConcat), which stays in it, as + with a string does:
// a loop joining strings, numbers, booleans and null runs natively without
// leaving. Parts whose conversion runs code -- an object's toString,
// through a chain's addition or a template's own conversion -- are Go's
// after the code leaves, in the order the interpreter runs them. Each
// answer is the interpreter's.
func TestJITSSAConcatCallsGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function chain(n){let t=0,s='';for(let i=0;i<n;i++){s='k'+i+':'+(i*0.5)+'/'+(i%2===0)+null;t=(t+s.length)|0}return t+'|'+s}
		var o={n:0,toString(){this.n++;return 'O'+this.n}};
		function objs(n,x){let s='';for(let i=0;i<n;i++){s='a'+i+'b'+x+'c'}return s+'|'+o.n}
		function tmpl(n,x){let s='';for(let i=0;i<n;i++){s=` + "`<${i}|${x}>`" + `}return s+'|'+o.n}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	chain := r.global.getOwn(r.atoms.intern("chain")).value().Object().fn().closure
	rounds := []string{`chain(300)`, `chain(300)`, `chain(300)`, `chain(300)`, `objs(300,o)`, `objs(300,o)`, `objs(300,7)`,
		`tmpl(300,o)`, `tmpl(300,o)`, `tmpl(300,true)`, `chain(300)`}
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		var hosts uint64
		if e := r.jit.cache[weak.Make(chain.fn)]; e != nil {
			hosts = e.ssaStats.hosts
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d (%s): got %s, interpreter %s", i, src, got, want)
		}
		if i == 3 {
			e := r.jit.cache[weak.Make(chain.fn)]
			if e == nil || e.ssa == nil {
				t.Fatalf("chain did not run natively: %+v", e)
			}
			if n := e.ssaStats.hosts - hosts; n > 2 {
				t.Fatalf("chain left native code %d times", n)
			}
		}
	}
}

// A for...of step a call of Go made, while its code was due to be compiled
// again, Go applies at the step's exit (jitCallResult): a step that found
// the cursor done goes on at the loop's end, any other with the value past
// the step. The done marker is the uninitialized value, a NaN-boxed tag,
// which == never finds equal to itself: the loop had gone on with it as
// its next value, and a call of it threw "uninitialized is not a function".
func TestJITCallResultStepDone(t *testing.T) {
	fn := jitFunctionForTest(t, `function f(a) { let s = 0; for (const x of a) s += x; return s }`)
	pc := slices.IndexFunc(fn.Code, func(in bytecode.Instr) bool { return in.Op == bytecode.OpIterNextOrJump })
	if pc < 0 {
		t.Fatal("no step")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	f := &frame{cl: &closure{fn: fn}}
	if sp, ok := r.jitCallResult(f, pc, 1, uninitialized, nil); !ok || sp != 1 || f.pc != fn.Code[pc].A {
		t.Fatalf("done: sp %d, ok %v, pc %d, want the loop's end, %d", sp, ok, f.pc, fn.Code[pc].A)
	}
	if sp, ok := r.jitCallResult(f, pc, 1, Float(7), nil); !ok || sp != 2 || f.pc != uint32(pc+1) || r.stack[1].Number() != 7 {
		t.Fatalf("a value: sp %d, ok %v, pc %d, value %v", sp, ok, f.pc, r.stack[1])
	}
}
