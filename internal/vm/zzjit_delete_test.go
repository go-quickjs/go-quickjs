//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"
	"testing"
)

// The delete operator is compiled (ir.DeleteProperty): Go makes it from
// native code (ssa.GoDelete) where that runs nothing and throws nothing,
// and the code leaves for Go to make the rest -- a proxy's deleteProperty
// trap, a key converted by a method of its own, a base that is no object,
// a property strict code cannot delete, which throws. Every answer, every
// trap and conversion called and every error is the interpreter's: own
// properties deleted, the first added and the last, then read again --
// undefined, or the prototype's -- with the object's other properties,
// its getters and its keys after; absent and inherited properties;
// non-configurable and frozen ones in sloppy code (false) and in strict
// code (the TypeError); arrays' elements (holes, the length unchanged),
// past the length, the length itself; string, number, symbol, boolean,
// null, undefined and BigInt keys; a proxy and an object whose prototype
// is one; functions' synthesized name and length; the global object's
// properties; typed arrays' elements and string wrappers' characters,
// which cannot be deleted, and a mapped arguments object's parameters,
// which a deletion unmaps; an object deleted from and added to in a loop;
// values read from properties before their deletion, used after it;
// receivers of many shapes; and in an inlined callee, sloppy and strict, a
// proxy's, an object key's and a primitive's leaving it for Go there, the
// TypeError caught by its caller's handler.
func TestJITDelete(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var sym=Symbol('s'), log=[], conv=0, gets=0, big=1n
		function del(o,ks){let s='';for(let i=0;i<ks.length;i++)s+=(delete o[ks[i]])?1:0;return s}
		function sdel(o,ks){'use strict';let s='';for(let i=0;i<ks.length;i++){try{s+=(delete o[ks[i]])?1:0}catch(e){s+=e.name+':'+e.message}s+=';'}return s}
		function tryDel(os,k){let s='';for(let i=0;i<os.length;i++){try{s+=delete os[i][k]}catch(e){s+=e.name+':'+e.message}s+=';'}return s}
		function own(n){let s='';for(let i=0;i<n;i++){let o={a:i,b:2,c:3};s+=delete o.a;s+=o.a;s+=('a' in o);s+=o.b+o.c;s+=delete o.c;s+=o.c;s+=o.b;s+=Object.keys(o).join()+';'}return s}
		function inherited(o,k,n){let s='';for(let i=0;i<n;i++){s+=delete o[k];s+=o[k];s+=(k in o)+';'}return s}
		function arr(a,ks){let s='';for(let i=0;i<ks.length;i++)s+=(delete a[ks[i]])?1:0;return s+':'+a.length+':'+JSON.stringify(a)+':'+(1 in a)+':'+Object.keys(a).join()}
		function holes(n){let a=[];for(let i=0;i<n;i++)a.push(i);for(let i=0;i<n;i+=2)delete a[i];let s=0;for(let i=0;i<n;i++)s+=(a[i]===undefined)?100:a[i];return s+':'+a.length+':'+(0 in a)+(1 in a)}
		function keyed(){let o={a:1,[sym]:2,2:3,'1.5':4,true:5,null:6,undefined:7,'0':8,'1n':9,1:10};let ks=[sym,2,1.5,true,null,undefined,-0,big,'missing',Symbol('fresh'),'1'];let s='';for(let i=0;i<ks.length;i++)s+=(delete o[ks[i]])?1:0;return s+':'+Reflect.ownKeys(o).map(String).join()}
		function churn(n){let o={x:0,y:1,z:2};let s=0;for(let i=0;i<n;i++){delete o.x;s+=o.y;o.x=i;s+=o.x+o.z;delete o.z;s+=(o.z===undefined)?1:0;o.z=2}return s+':'+Object.keys(o).join()}
		function getters(n){let s='';for(let i=0;i<n;i++){let o={get g(){gets++;return 7},b:2,c:3};s+=delete o.b;s+=o.g;s+=o.c;s+=delete o.g;s+=o.g;s+=Object.keys(o).join()+';'}return s}
		function fns(n){let s='';for(let i=0;i<n;i++){let f=function nm(a,b){};s+=delete f.name;s+=f.name+'|';s+=delete f.length;s+=f.length;s+=delete f.prototype;s+=typeof f.prototype+';'}return s}
		var declared=1
		function globals(n){let s='';for(let i=0;i<n;i++){globalThis.gx=i;s+=typeof gx;s+=delete globalThis.gx;s+=typeof gx;s+=delete globalThis.declared;s+=declared+';'}return s}
		function triples(n){let os=[];for(let i=0;i<n;i++)os.push({p:{v:i},q:{v:-i},r:{v:2*i}});return os}
		function kept(os){let out=[];for(let i=0;i<os.length;i++){let o=os[i];let p=o.p,q=o.q,r=o.r;delete o.p;delete o.r;delete o.q;out[3*i]=p;out[3*i+1]=q;out[3*i+2]=r}return out.map(x=>x.v).join()}
		function mega(os){let s=0;for(let i=0;i<os.length;i++){let o=os[i];s+=o.a;delete o.a;s+=(o.a===undefined)?1:0;o.a=i;s+=o.b}return s}
		function shapes(){let os=[];for(let i=0;i<12;i++){let o={};o['k'+i]=i;o.a=1;o.b=2;os.push(o)}return os}
		var frozen=Object.freeze({a:1,b:2}), frozenArr=Object.freeze([1,2])
		var nc={y:1}; Object.defineProperty(nc,'x',{value:1})
		var proto={p:1}, child=Object.create(proto), shadow=Object.create(proto)
		var px=new Proxy({a:1,b:2},{deleteProperty(t,k){log.push(typeof k==='symbol'?k.toString():String(k));return delete t[k]}})
		var pxNo=new Proxy({a:1},{deleteProperty(t,k){log.push('no:'+String(k));return false}})
		var pchild=Object.create(px); pchild.own=1
		var okey={toString(){conv++;return 'a'}}
		var pkey={[Symbol.toPrimitive](h){conv++;return sym}}
		var tkey={toString(){conv++;throw new RangeError('no key')}}
		function dropKey(o,k){return delete o[k]}
		function sdropKey(o,k){'use strict';return delete o[k]}
		function viaCallee(os,k){let s='';for(let i=0;i<os.length;i++){try{s+=dropKey(os[i],k)?1:0}catch(e){s+=e.name}try{s+=sdropKey(os[i],k)?1:0}catch(e){s+=e.name}s+=';'}return s}`
	rounds := []string{
		`own(6)`,
		`own(6)`,
		`(shadow.p=2,inherited(child,'p',3)+inherited(shadow,'p',3)+inherited({},'zz',2))`,
		`del(nc,['x','y','x','z'])+':'+nc.x+':'+('y' in nc)`,
		`sdel(nc,['x','z'])+':'+nc.x`,
		`del(frozen,['a','b','c'])+del(frozenArr,[0,1,2,'length'])+':'+frozen.a+frozenArr`,
		`sdel(frozen,['a','c'])+sdel(frozenArr,[0,5])`,
		`arr([1,2,3,4,5],[1,'3',4,10,'length',-1,1.5,'x'])`,
		`arr([1,,3],[0,1,2])`,
		`sdel([1,2],['length',0,1,2])`,
		`holes(9)`,
		`keyed()`,
		`keyed()`,
		`churn(8)`,
		`(gets=0,getters(4)+':'+gets)`,
		`fns(3)`,
		`globals(3)+':'+typeof gx+':'+declared`,
		`kept(triples(8))`,
		`kept(triples(8))`,
		`mega(shapes())`,
		`mega(shapes())+mega(shapes().concat([{a:1,b:1},{b:2,a:2}]))`,
		`(log=[],del(px,['a','z',sym,'b'])+':'+log.join()+':'+Reflect.ownKeys(px).join())`,
		`(log=[],sdel(pxNo,['a'])+del(pxNo,['a'])+':'+log.join())`,
		`(log=[],del(pchild,['own','a','own'])+':'+log.join()+':'+pchild.a)`,
		`(conv=0,del({a:1,[sym]:2,b:3},[okey,pkey,'b',okey])+':'+conv)`,
		`(conv=0,del({a:1},[tkey]))`,
		`(conv=0,tryDel([{a:1},5,null,undefined,'s',{b:1},true,1n],okey)+':'+conv)`,
		`tryDel([{a:1},5,null,undefined,'s',Symbol(),1n],'a')+tryDel(['str',[1]],'length')+tryDel(['str'],0)`,
		`viaCallee([{a:1},{b:1},frozen,[1],Object.create({a:1})],'a')`,
		`viaCallee([{a:1},{b:1},frozen,[1],Object.create({a:1})],'a')`,
		`(log=[],conv=0,viaCallee([{a:1},px,5,null,frozen,{a:2}],okey)+':'+log.join()+':'+conv)`,
		`viaCallee([[1,2],{0:1},frozenArr],0)`,
		`del(new Uint8Array(2),[0,1,2,'-0','1.5','x','length'])+sdel(new Uint8Array(2),[0,2])`,
		`del(new String('ab'),[0,1,2,'length','x'])+sdel(new String('ab'),[1,'length',3])`,
		`(function(a,b){let args=arguments;let r=del(args,[0,5]);return r+':'+a+':'+args[0]+':'+args[1]+':'+args.length+':'+(a=3,args[0])+(b=4,args[1])})(1,2)`,
		`own(3)+churn(5)+arr([1,2,3],[0,2])+keyed()`,
	}
	r := jitAgreesForTest(t, setup, rounds...)
	e := jitNativeForTest(t, r, "viaCallee")
	// Its deletes are dropKey's and sdropKey's, inlined.
	for _, callee := range []string{"dropKey", "sdropKey"} {
		if !slices.Contains(e.ssaInlined, jitGlobalClosureForTest(r, callee)) {
			t.Errorf("viaCallee does not inline %s: %v", callee, e.ssaInlined)
		}
	}
	// Eight times over: code compiled at a function's first call
	// deoptimizes once at each property site its first runs had not met,
	// and the deletes after them run natively only once every site has.
	var all []string
	for range 8 {
		all = append(all, rounds...)
	}
	r = jitAgreesForTest(t, setup, all...)
	for _, name := range []string{"del", "sdel", "tryDel", "own", "inherited", "arr", "holes", "keyed", "churn", "getters", "fns", "globals", "kept", "mega", "viaCallee"} {
		jitNativeForTest(t, r, name)
	}
}

// Go makes each delete from native code, one call of Go apiece, the code
// never leaving it, for ordinary objects and arrays, in sloppy code and in
// strict, a property that cannot be deleted included in sloppy code; it
// leaves for Go at a proxy's, whose trap Go calls after the exit, at an
// object key's, which Go converts there, and in strict code at a property
// that cannot be deleted, whose TypeError Go throws there.
func TestJITDeleteGoCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var sym=Symbol('s'), traps=0, conv=0
		var px=new Proxy({},{deleteProperty(t,k){traps++;return true}})
		var okey={toString(){conv++;return 'a'}}
		var nc={}; Object.defineProperty(nc,'x',{value:1})
		var keys=['a','b',sym,3,'3',0,1,'x']
		function fresh(){return [{a:1,[sym]:2,3:0,b:4},[1,2,3],Object.create({a:1}),nc]}
		function count(os,ks){let c=0;for(let j=0;j<os.length;j++)for(let i=0;i<ks.length;i++)if(delete os[j][ks[i]])c++;return c}
		function scount(os,ks){'use strict';let c=0;for(let j=0;j<os.length;j++)for(let i=0;i<ks.length;i++){try{if(delete os[j][ks[i]])c++}catch(e){c+=100}}return c}
		function triples(n){let os=[];for(let i=0;i<n;i++)os.push({p:{v:i},q:{v:-i},r:{v:2*i}});return os}
		function kept(os,out){for(let i=0;i<os.length;i++){let o=os[i];let p=o.p,q=o.q,r=o.r;delete o.p;delete o.r;delete o.q;out[3*i]=p;out[3*i+1]=q;out[3*i+2]=r}let s=0;for(let i=0;i<out.length;i++)s+=out[i].v;return s}`
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, warm string
		want       float64
		deletes    int
	}{
		// Every key of the three objects is gone after, and nc's x stays.
		{"count", `count(fresh(),keys)`, 8 + 8 + 8 + 7, 4 * 8},
		{"scount", `scount(fresh().slice(0,3),keys)`, 8 + 8 + 8, 3 * 8},
		// The values read from the properties deleted, used after: kept
		// first (ssa's allocOnly), as a call's are. Their sum, 2 times
		// the sum of 0 to 7.
		{"kept", `kept(triples(8),new Array(24).fill(0))`, 56, 3 * 8},
	} {
		for i := 0; i < 8; i++ {
			if _, err := r.Run(compileForTest(t, c.warm)); err != nil {
				t.Fatal(err)
			}
		}
		e := jitNativeForTest(t, r, c.name)
		calls := 0
		jitGoCallWork = func() { calls++ }
		before := e.ssaStats
		v, err := r.Run(compileForTest(t, c.warm))
		jitGoCallWork = nil
		if err != nil || v.Number() != c.want {
			t.Fatal(c.name, v, err)
		}
		if left := e.ssaStats.hosts - before.hosts; left != 0 || r.jit.hint(jitGlobalClosureForTest(r, c.name)) != e {
			t.Errorf("%s left native code %d times", c.name, left)
		}
		if calls != c.deletes {
			t.Errorf("%s: %d calls of Go, want %d, one for each delete", c.name, calls, c.deletes)
		}
	}
	// A proxy's trap, an object key's toString and strict code's TypeError
	// are Go's after an exit at each, called as often as the interpreter
	// calls them.
	e := jitNativeForTest(t, r, "count")
	s := jitNativeForTest(t, r, "scount")
	for _, c := range []struct {
		e         *jitEntry
		src, want string
		exits     uint64
	}{
		{e, `traps=0,count([px],keys)+':'+traps`, "8:8", 8},
		{e, `conv=0,count([{a:1}],[okey,'a',okey])+':'+conv`, "3:2", 2},
		{s, `''+scount([nc,{x:1}],['x','y','x'])`, "204", 2},
	} {
		before := c.e.ssaStats
		v, err := r.Run(compileForTest(t, c.src))
		if err != nil || v.String().Go() != c.want {
			t.Fatalf("%s: %v, %v, want %s", c.src, v, err, c.want)
		}
		if left := c.e.ssaStats.hosts - before.hosts; left != c.exits {
			t.Errorf("%s: left native code %d times, want %d", c.src, left, c.exits)
		}
	}
}
