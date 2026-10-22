//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
)

// A comparison or a test that only its branch uses is compared by the
// branch itself, a compare and a conditional jump, the boolean never made
// (mir's findFused), a negation swapping the branch's targets: <, <=, >,
// >=, == and != of numbers and of BigInts, === and !== and == and != of
// any two values, with null and with undefined, ??, typeof, instanceof,
// truthiness, and whether a string's character has a cell (charAt,
// substring). Every answer is the interpreter's, for every kind of operand
// -- NaN, -0, the infinities, strings equal and not of one length, objects
// the same and not, [[IsHTMLDDA]], BigInts, functions -- as a branch, as a
// branch of a negation, and as a boolean kept for later, which is still
// made; compiled at once and with what the sites learned; and each
// function runs natively. dies is a negation whose operand's last use is
// the comparison, values emitted after it -- its block's end loads a
// captured binding for the join -- which must not take its register.
func TestJITFusedBranches(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function C(){} function D(){} D.prototype=Object.create(C.prototype);
		var o1={}, o2={}, f1=function(){};
		var vals=[0,-0,1,-1,1.5,NaN,Infinity,-Infinity,'','a','b','ab','ba','1',true,false,null,undefined,
			o1,o1,o2,[],[0],dda,new C(),new D(),f1,C,0n,1n,-1n];
		var nums=[0,-0,1,-1,1.5,NaN,Infinity,-Infinity,2**53,-(2**31),0.1,0.2];
		var bigs=[0n,1n,-1n,2n**62n,-(2n**63n),5n];
		function pairs(a){var xs=[],ys=[];for(var i=0;i<a.length;i++)for(var j=0;j<a.length;j++){xs.push(a[i]);ys.push(a[j])}return [xs,ys]}
		function rel(xs,ys){let out='';for(let i=0;i<xs.length;i++){const x=xs[i],y=ys[i];let k=0;
			if(x<y)k|=1;if(x<=y)k|=2;if(x>y)k|=4;if(x>=y)k|=8;if(x==y)k|=16;if(x!=y)k|=32;
			if(!(x<y))k|=64;if(!(x>=y))k|=128;if(!(x==y))k|=256;if(!(x!=y))k|=512;out+=k+','}return out}
		function eq(xs,ys){let out='';for(let i=0;i<xs.length;i++){const x=xs[i],y=ys[i];let k=0;
			if(x===y)k|=1;if(x!==y)k|=2;if(x==y)k|=4;if(x!=y)k|=8;if(!(x===y))k|=16;if(!(x!=y))k|=32;out+=k+','}return out}
		function nul(xs){let out='';for(let i=0;i<xs.length;i++){const x=xs[i];let k=0;
			if(x===null)k|=1;if(x!==null)k|=2;if(x===undefined)k|=4;if(x!==undefined)k|=8;if(x==null)k|=16;
			if(x!=null)k|=32;if(x==undefined)k|=64;if(!(x==null))k|=128;if((x??5)===5)k|=256;if(null===x)k|=512;out+=k+','}return out}
		function types(xs){let out='';for(let i=0;i<xs.length;i++){const x=xs[i];let k=0;
			if(typeof x==='number')k|=1;if(typeof x!=='number')k|=2;if(typeof x==='string')k|=4;if(typeof x!=='string')k|=8;
			if(typeof x==='boolean')k|=16;if(typeof x!=='boolean')k|=32;if(typeof x==='undefined')k|=64;if(typeof x!=='undefined')k|=128;
			if(typeof x==='function')k|=256;if(typeof x!=='function')k|=512;if(!(typeof x==='number'))k|=1024;out+=k+','}return out}
		function truthy(xs){let out='';for(let i=0;i<xs.length;i++){const x=xs[i];let k=0;
			if(x)k|=1;if(!x)k|=2;if(!!x)k|=4;while(x&&k<64)k+=8;out+=k+','}return out}
		function inst(xs){let out='';for(let i=0;i<xs.length;i++){const x=xs[i];let k=0;
			if(x instanceof C)k|=1;if(!(x instanceof C))k|=2;if(x instanceof D)k|=4;if(!(x instanceof D))k|=8;out+=k+','}return out}
		function big(xs,ys){let out='';for(let i=0;i<xs.length;i++){const x=xs[i],y=ys[i];let k=0;
			if(x<y)k|=1;if(x<=y)k|=2;if(x>y)k|=4;if(x>=y)k|=8;if(!(x<y))k|=16;if(!(x>y))k|=32;out+=k+','}return out}
		function kept(xs,ys){let out='';for(let i=0;i<xs.length;i++){const x=xs[i],y=ys[i];let k=0;
			const a=x===y;if(a)k|=1;const b=x<y;if(b)k|=2;const c=x==null;if(c)k|=4;
			const d=typeof x==='string';if(d)k|=8;const e=!x;if(e)k|=16;const f=x!==null;if(f)k|=32;
			out+=k+':'+a+b+c+d+e+f+','}return out}
		function shorter(x,y){let n=0;while(y!==null){if(x===null)return n;x=x.next;y=y.next;n++}return -n}
		function list(n){let l=null;for(let i=0;i<n;i++)l={next:l};return l}
		var defaults=(function(){let up={k:3,j:4};
			return function defaults(a,n=4,m=0.5){let s=0;for(let i=0;i<n;i++)s+=up.k*m+a+up.j;return s}})();
		var joins=(function(){var E={a:1,b:2};
			return function joins(o,f,r){if(f===undefined)f=0;if(r!==undefined)r=0.5;let s=E.a+(r||0);
				for(let i=0;i<o.n;i++){if(o.t===E.a)s+=f;else if(o.t===E.b)s-=f}return s}})();
		var dies=(function(){var E={a:3,b:2};
			return function dies(o,f){let g=1;if(f!==void 0)g=2;let s=E.a*g;for(let i=0;i<o.n;i++)s+=i;return s}})();
		function chars(s){let out='';for(let i=0;i<s.length;i++)out+=s.charAt(i)+s.substring(i,i+1)+s.slice(i,i+2);return out}
		var wide='ab'+String.fromCharCode(233,8364)+'c'+String.fromCodePoint(128512)+'d';
		var vp=pairs(vals), np=pairs(nums), bp=pairs(bigs);`
	rounds := []string{
		`rel(np[0],np[1])`, `rel(np[0],np[1])`, `rel(vp[0],vp[1])`, `rel(np[0],np[1])`,
		`eq(np[0],np[1])`, `eq(vp[0],vp[1])`, `eq(vp[0],vp[1])`, `eq(np[0],np[1])`,
		`nul(vals)`, `nul(vals)`, `nul(nums)`,
		`types(vals)`, `types(vals)`, `types(nums)`,
		`truthy(vals)`, `truthy(nums)`, `truthy(vals)`, `truthy(bigs)`,
		`inst(vals)`, `inst(vals)`, `inst(nums)`,
		`big(bp[0],bp[1])`, `big(bp[0],bp[1])`, `big(bp[0],bp[1])`,
		`kept(vp[0],vp[1])`, `kept(np[0],np[1])`, `kept(vp[0],vp[1])`,
		`[shorter(list(3),list(5)),shorter(list(5),list(3)),shorter(null,list(2)),shorter(list(4),null)]`,
		`[shorter(list(30),list(50)),shorter(list(50),list(30)),shorter(list(7),list(7))]`,
		`[defaults(1),defaults(1,3),defaults(2,undefined,2),defaults(1,null),defaults(1,6,undefined),defaults(0,2,1)]`,
		`chars(wide)`, `chars('xyz')`, `chars(wide)`, `chars('')`,
		`[dies({n:3}),dies({n:3},1),dies({n:30},undefined),dies({n:2},null),dies({n:300}),dies({n:300},'u')]`,
		`[joins({n:300,t:1}),joins({n:300,t:2},3),joins({n:200,t:2},undefined,2),joins({n:0,t:1},5,7),joins({n:400,t:1},6,undefined),joins({n:100,t:3},4,1)]`,
	}
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	// Compiled at once, with no feedback, and at the default thresholds,
	// with what the sites learned: BigInts held in their values among it.
	forced := jitRuntimeForTest(t, Config{JIT: true})
	learned := New(Config{JIT: true})
	t.Cleanup(func() { learned.Close(); learned.ReleaseClosed() })
	for _, rt := range []*Runtime{want, forced, learned} {
		dda := rt.NewObject()
		dda.MarkHTMLDDA()
		rt.global.setOwnRaw(rt.atoms.intern("dda"), Obj(dda), propDefault)
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []*Runtime{forced, learned} {
		passes := 3
		if r == learned {
			passes = 60
		}
		for range passes {
			for i, src := range rounds {
				src = "''+(" + src + ")"
				wv, werr := want.Run(compileForTest(t, src))
				gv, gerr := r.Run(compileForTest(t, src))
				if werr != nil || gerr != nil {
					t.Fatalf("round %d (%s): error %v, interpreter's %v", i, src, gerr, werr)
				}
				if got, want := gv.String().Go(), wv.String().Go(); got != want {
					t.Fatalf("round %d (%s): got %s, interpreter %s", i, src, got, want)
				}
			}
		}
		names := []string{"rel", "eq", "nul", "types", "truthy", "inst", "big", "kept", "shorter", "defaults", "joins", "dies", "chars"}
		if r == learned {
			// What its loops' work makes hot enough at the thresholds.
			names = []string{"rel", "eq", "truthy", "inst", "big", "kept", "shorter", "joins"}
		}
		for _, name := range names {
			jitNativeForTest(t, r, name)
		}
	}
}
