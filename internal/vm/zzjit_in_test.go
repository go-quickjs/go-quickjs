//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"
	"testing"
)

// The in operator is compiled (ir.HasProperty): Go answers it from native
// code (ssa.GoHas) where that runs nothing, and the code leaves for Go to
// make the rest -- a proxy's has trap, a key converted by a method of its
// own, a right operand that is no object, which throws. Every answer, every
// trap and conversion called and every error is the interpreter's: own
// properties present and absent, inherited ones, a getter on the prototype,
// which in never calls, arrays' elements in range, holes and past the
// length, an array's length, typed arrays', string wrappers' and functions'
// keys the classes answer themselves, string, number, symbol, boolean,
// null, undefined and BigInt keys, a property deleted and added again
// between iterations (by a callee), and #x in o beside it; and in an
// inlined callee, a proxy's, an object key's and a primitive's leaving it
// for Go there, the TypeError caught by its caller's handler.
func TestJITIn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var sym=Symbol('s'), absent=Symbol('t'), gets=0, conv=0, log=[]
		var proto={get g(){gets++;return 1},p:1}
		var child=Object.create(proto); child.own=1
		var bare=Object.create(null); bare.k=1
		var own={a:1,b:undefined,[sym]:1,2:0,1.5:1,'-0':1}
		var arr=[1,,3]; arr.length=5
		var ta=new Uint8Array(3), sw=new String('ab'), fn=function(){}
		var dict={}; for(let i=0;i<40;i++)dict['k'+i]=i; for(let i=0;i<40;i+=2)delete dict['k'+i]
		var px=new Proxy({a:1},{has(t,k){log.push(typeof k==='symbol'?k.toString():String(k));return Reflect.has(t,k)}})
		var pchild=Object.create(px)
		var okey={toString(){conv++;return 'a'}}
		var pkey={[Symbol.toPrimitive](h){conv++;return h==='string'?sym:0}}
		var tkey={toString(){conv++;throw new RangeError('no key')}}
		var objKeys=['a','b','c',sym,absent,2,'2',3,1.5,'1.5',-0,'-0',0,'toString','hasOwnProperty','__proto__','g',true,null,undefined,1n]
		var arrKeys=[0,1,2,3,4,5,'0','1','length',-1,1.5,-0,NaN,'00',sym,Symbol.iterator,'push',true,null,undefined,1n,2n,4294967295,4294967294]
		function has(o,ks){let s='';for(let i=0;i<ks.length;i++)s+=(ks[i] in o)?1:0;return s}
		function tryHas(os,k){let s='';for(let i=0;i<os.length;i++){try{s+=(k in os[i])}catch(e){s+=e.name+':'+e.message}s+=';'}return s}
		function drop(o,k){delete o[k]}
		function hasKey(o,k){return k in o}
		function viaCallee(os,k){let s='';for(let i=0;i<os.length;i++){try{s+=hasKey(os[i],k)?1:0}catch(e){s+=e.name}}return s}
		function delIn(o,n){let s='';for(let i=0;i<n;i++){s+=('a' in o)?1:0;if(i===2)drop(o,'a');if(i===4)o.a=0}return s}
		function grow(a,n){let s='';for(let i=0;i<n;i++){s+=(i in a)?1:0;a[i]=i;if(i===3)drop(a,1)}return s+(1 in a)}
		var P=class{#x=1;static has(os){let s='';for(let i=0;i<os.length;i++){s+=(#x in os[i])?1:0;s+=('y' in os[i])?1:0}return s}}
		var pobjs=[new P(),{y:1},Object.create(new P()),Object.assign(new P(),{y:2})]`
	rounds := []string{
		`has(own,objKeys)`,
		`has(own,objKeys)`,
		`(gets=0,has(child,['own','p','g','x','toString',sym])+':'+gets)`,
		`has(bare,['k','toString','__proto__'])`,
		`has(arr,arrKeys)`,
		`has(ta,[0,2,3,'1','-0','1.5','length',-1,'x','set'])`,
		`has(sw,[0,1,2,'length','1','-0','charAt'])`,
		`has(fn,['prototype','name','length','call','x','arguments'])`,
		`has(dict,['k0','k1','k38','k39','k40'])`,
		`has(own,objKeys)+has(arr,arrKeys)`,
		`(log=[],has(px,['a','b',0,sym])+':'+log.join())`,
		`(log=[],has(pchild,['a','z'])+':'+log.join())`,
		`(conv=0,has({a:1,[sym]:1},[okey,pkey,'a',okey])+':'+conv)`,
		`(conv=0,has({},[tkey]))`,
		`(conv=0,tryHas([{a:1},5,null,undefined,'s',{a:2}],okey)+':'+conv)`,
		`tryHas([{a:1},5,true,Symbol(),1n],'a')`,
		`has(5,['a'])`,
		`delIn({a:1,b:2},7)`,
		`grow([],8)`,
		`grow([0,1,2,3,4,5,6,7],8)`,
		`(Array.prototype[3]='p',has(arr,arrKeys)+':'+(delete Array.prototype[3]))`,
		`has(arr,arrKeys)`,
		`P.has(pobjs)`,
		`P.has(pobjs)`,
		`P.has([5])`,
		`viaCallee([own,child,arr,{}],'a')`,
		`viaCallee([own,child,arr,{}],'a')`,
		`(log=[],conv=0,viaCallee([own,px,5,child,null,{a:1}],okey)+':'+log.join()+':'+conv)`,
		`viaCallee([own,child,arr,{}],'p')`,
		`has(own,objKeys)+has(child,['g','p'])+has(arr,arrKeys)`,
	}
	r := jitAgreesForTest(t, setup, rounds...)
	for _, name := range []string{"has", "tryHas", "delIn", "grow", "viaCallee"} {
		jitNativeForTest(t, r, name)
	}
	{
		e := jitNativeForTest(t, r, "viaCallee")
		// Its in is hasKey's, inlined.
		if !slices.Contains(e.ssaInlined, jitGlobalClosureForTest(r, "hasKey")) {
			t.Errorf("viaCallee does not inline hasKey: %v", e.ssaInlined)
		}
	}
	p := r.global.getOwn(r.atoms.intern("P")).value().Object().getOwnVisible(r.atoms.intern("has")).value().Object().fn().closure
	if e := r.jit.hint(p); e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
		t.Fatalf("P.has never ran natively: %+v", e)
	}
}

// Go answers each in from native code, one call of Go apiece, the code never
// leaving it, for ordinary objects and arrays; it leaves for Go at a proxy's,
// whose trap Go calls after the exit, and at an object key's, which Go
// converts there.
func TestJITInGoCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var sym=Symbol('s'), traps=0, conv=0
		var o={a:1,[sym]:2,3:0}, arr=[1,,3], inherited=Object.create(o)
		var px=new Proxy({a:1},{has(t,k){traps++;return k in t}})
		var okey={toString(){conv++;return 'a'}}
		var keys=['a','b',sym,3,'3',0,1,2]
		function count(o,ks){let c=0;for(let i=0;i<ks.length;i++)if(ks[i] in o)c++;return c}`
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	warm := `count(o,keys)+count(arr,keys)+count(inherited,keys)`
	for i := 0; i < 4; i++ {
		if _, err := r.Run(compileForTest(t, warm)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "count")
	calls := 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	before := e.ssaStats
	v, err := r.Run(compileForTest(t, warm))
	if err != nil || v.Number() != 4+2+4 {
		t.Fatal(v, err)
	}
	if left := e.ssaStats.hosts - before.hosts; left != 0 || r.jit.hint(jitGlobalClosureForTest(r, "count")) != e {
		t.Errorf("count left native code %d times", left)
	}
	if want := 3 * 8; calls != want {
		t.Errorf("%d calls of Go, want %d, one for each in", calls, want)
	}
	// A proxy's trap, and an object key's toString, are Go's after an exit
	// at each, called as often as the interpreter calls them.
	for _, c := range []struct {
		src, want string
		exits     uint64
	}{
		{`traps=0,count(px,keys)+':'+traps`, "1:8", 8},
		{`conv=0,count(o,[okey,'a',okey])+':'+conv`, "3:2", 2},
	} {
		before = e.ssaStats
		v, err = r.Run(compileForTest(t, c.src))
		if err != nil || v.String().Go() != c.want {
			t.Fatalf("%s: %v, %v, want %s", c.src, v, err, c.want)
		}
		if left := e.ssaStats.hosts - before.hosts; left != c.exits {
			t.Errorf("%s: count left native code %d times, want %d", c.src, left, c.exits)
		}
	}
}

// jitGlobalClosureForTest is the closure the global name holds.
func jitGlobalClosureForTest(r *Runtime, name string) *closure {
	return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
}
