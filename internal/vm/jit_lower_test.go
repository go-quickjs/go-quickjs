//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// A function's register code is lowered once for each kind and kept
// (jitLowering): asked again -- by each caller that inlines it, by each
// compile of its own -- the program is the one lowered first, checked once,
// until its code learns a hint, which the next ask lowers it anew with.
// Lowered at every ask, Typescript's compiles lowered some 0.6 GB of
// programs a run. What the lowerings hold is the JIT's to count
// (lowerBytes).
func TestJITRegLowersOnce(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rts := regNativeRuntimes(t, `function add(a,b){return a+b}
		function f(n){let s=0;for(let i=0;i<n;i++)s=add(s,i);return s}
		function g(n){let s=0;for(let i=0;i<n;i++)s=add(i,s)|0;return s}`)
	r := rts[0]
	want := map[string]float64{"f": 1999000, "g": 1999000}
	for _, name := range []string{"f", "g"} {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
		for range 8 {
			v, err := r.Call(fn, Undefined, []Value{Float(2000)})
			if err != nil || !v.IsNumber() || v.Number() != want[name] {
				t.Fatalf("%s(2000) = %v, %v; want %v", name, v, err, want[name])
			}
		}
	}
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern("add"))
	cl := fn.Object().fn().closure
	key := r.jitRegKeyOf(cl)
	s := r.jit
	l := s.regLowered[key]
	if l == nil || !l.done[jitLowerInline] || l.progs[jitLowerInline] == nil {
		t.Fatalf("add not lowered for its callers: %+v", l)
	}
	p := l.progs[jitLowerInline]
	if q, err := r.jitRegLower(cl, cl.reg.rc, jitLowerInline); err != nil || q != p {
		t.Fatalf("add lowered again: %p, %v, against %p", q, err, p)
	}
	held := func() int {
		n := 0
		for _, x := range s.lowered {
			n += x.bytes
		}
		for _, x := range s.regLowered {
			n += x.bytes
		}
		return n
	}
	if n := held(); n != s.lowerBytes {
		t.Fatalf("lowerings hold %d bytes, the JIT counts %d", n, s.lowerBytes)
	}
	// An addition that met a string, learned at an exit: lowered anew,
	// Go's.
	rpc := slices.IndexFunc(cl.reg.rc.Code, func(w bytecode.RInstr) bool { return w.Op() == bytecode.RAdd })
	if rpc < 0 {
		t.Fatal("add has no add")
	}
	learned := s.regLearnedOf(key)
	learned.regHints.StringAdds = append(learned.regHints.StringAdds, int32(rpc))
	q, err := r.jitRegLower(cl, cl.reg.rc, jitLowerInline)
	if err != nil || q == p || !slices.ContainsFunc(q.Code, func(in ir.Instruction) bool { return in.Op == ir.StringAdd }) {
		t.Fatalf("add not lowered again for its hint: %p, %v, against %p", q, err, p)
	}
	if again, _ := r.jitRegLower(cl, cl.reg.rc, jitLowerInline); again != q {
		t.Fatalf("add lowered again with the same hints: %p, against %p", again, q)
	}
	if n := held(); n != s.lowerBytes {
		t.Fatalf("lowerings hold %d bytes, the JIT counts %d", n, s.lowerBytes)
	}
}

// A deoptimization at an inlined register callee's write is learned in
// what the callee's code learned (regLearned), and the code inlining it
// compiled again reads it there: a write met at more than one shape --
// here an array's length, the arrays of two shapes -- is megamorphic in
// the callee's next inlining, as in its own code (megamorphicOf). Looked
// for in the stack form's cache, which held nothing of register code, it
// was compiled for one shape at every compile of its caller, which
// deoptimized there each time: Typescript's returnResolutionData,
// rd.actuals.length = 0, inlined, had its caller compiled twelve times.
func TestJITRegInlinedMegamorphicWrite(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rts := regNativeRuntimes(t, `function Rd(id){this.actuals=[];this.exact=[];this.id=id}
		function ret(rd){rd.actuals.length=0;rd.exact.length=0;return rd.id}
		var rds=[new Rd(1),new Rd(2)];
		function owner(n){let s=0;for(let i=0;i<n;i++){const rd=rds[i&1];rd.actuals.push(i);rd.exact.push(i,i);s+=ret(rd)}return s+rd2()}
		function rd2(){return rds[0].actuals.length+rds[1].exact.length}`)
	r := rts[0]
	guards := r.jit.guards
	for i := range 40 {
		for k, rt := range rts {
			fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("owner"))
			v, err := rt.Call(fn, Undefined, []Value{Float(500)})
			if err != nil || !v.IsNumber() || v.Number() != 750 {
				t.Fatalf("round %d, runtime %d: owner(500) = %v, %v; want 750", i, k, v, err)
			}
		}
	}
	// Two writes, each deoptimizing at its second shape, then at the shape
	// it met before once the code is compiled again (jitDeoptSeenOp).
	if n := r.jit.guards - guards; n > 4 {
		t.Errorf("%d guards failed in 40 rounds: the inlined writes deoptimized at every compile", n)
	}
}
