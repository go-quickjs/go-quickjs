//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regCallSrc's functions call one another natively from register code
// (jitRegFeedback's NativeCalls, zzjit_zregcall.go): plain calls, a
// method's, a sloppy function's this, an arrow's captured this, missing
// and extra arguments, chains of native calls whose innermost leaves
// native code -- a shape it was not compiled for, a closure made, a throw
// caught by a caller, a call in tail position -- deep recursion past the
// contexts native calls take, and callees that call Go.
const regCallSrc = `
function add(a, b) { return a + b }
function three(a, b, c) { return c === undefined ? a + b : a + b + c }
function one(a) { return a }
function P(x) { this.x = x }
P.prototype.get = function (k) { return this.x + k };
function sloppy(k) { return this === globalThis ? k + 1 : -1 }
function strict(k) { "use strict"; return this === undefined ? k : -k }
var obj = { v: 3, arrows() { var f = (k) => this.v + k; var s = 0; for (var i = 0; i < 50; i++) s += f(i); return s } };
function plain(n) {
	var s = 0, p = new P(2);
	for (var i = 0; i < n; i++) {
		s = add(s, i) + three(i, 1) + three(1, 2, 3) + one(i, i, i);
		s += p.get(i) + sloppy(i) + strict(i);
	}
	return s + obj.arrows();
}
function leaf3(o) { return o.x * 2 }
function mid3(o) { return leaf3(o) + 1 }
function top3(o) { return mid3(o) + 1 }
function chain(n) {
	var a = new P(1), b = { y: 0, x: 5 }, s = 0;
	for (var i = 0; i < n; i++) s += top3(i % 97 == 96 ? b : a);
	return s;
}
function mk(k) { var c = function () { return k }; return c() }
function midMk(k) { return mk(k) + 1 }
function makes(n) { var s = 0; for (var i = 0; i < n; i++) s += midMk(i); return s }
function thrower(i) { if (i % 50 == 49) throw new RangeError("at " + i); return i }
function midThrow(i) { return thrower(i) + 1 }
function catches(n) {
	var s = 0, m = "";
	for (var i = 0; i < n; i++) { try { s += midThrow(i) } catch (e) { m = e.message } }
	return s + m;
}
function uncaught(n) { var s = 0; for (var i = 0; i < n; i++) s += midThrow(i); return s }
function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2) }
function depth(n) { return n == 0 ? 0 : 1 + depth(n - 1) }
function deep(n) { var s = 0; for (var i = 0; i < n; i++) s += depth(150) + fib(12); return s }
function tailed(n, acc) { if (n == 0) return acc; return tailed(n - 1, acc + 1) }
function tails(n) { var s = 0; for (var i = 0; i < n; i++) s += tailed(30, i); return s }
function goes(k) { var a = [k, k + 1]; a.push(k); return JSON.stringify(a).length + String(k).length + Math.max(k, 3) }
function midGoes(k) { return goes(k) + 1 }
function callsGo(n) { var s = 0; for (var i = 0; i < n; i++) s += midGoes(i); return s }
function Q(a, b) { this.a = a; this.b = b }
function R(k) { if (k % 7 == 6) return { r: k }; this.k = k }
function T(k) { if (k % 50 == 49) throw new Error("T" + k); this.k = k; this.t = new Q(k, k) }
function constructs(n) {
	var s = 0, m = "";
	for (var i = 0; i < n; i++) {
		var q = new Q(i, 2), r = new R(i);
		s += q.a + q.b + (r.k === undefined ? r.r : r.k);
		try { s += new T(i).t.a } catch (e) { m = e.message }
	}
	return s + m + (new Q(1, 2) instanceof Q) + Object.keys(new R(3)) + Object.getPrototypeOf(new Q(0, 0)).constructor.name;
}
function counter(n) { var k = 0; var inc = function () { k++ }; for (var i = 0; i < n; i++) inc(); return k }
function loopClosures(n) { var fs = []; for (let i = 0; i < n; i++) fs.push(() => i); var s = 0; for (var j = 0; j < fs.length; j++) s += fs[j](); return s }
function mkAdder(a) { return function (b) { return a + b } }
function midCap(k) { var x = k; var g = function () { return x }; x = x + 1; var t = [1, 2].map(function (e) { return x + e }); x++; return g() + t[1] }
function closures(n) { var s = 0; for (var i = 0; i < n; i++) s += mkAdder(i)(1) + midCap(i); return s + counter(n) + loopClosures(n / 100) }
function restf(...xs) { return xs.length }
function argsf() { "use strict"; return arguments.length + (arguments[0] | 0) }
function nt() { return new.target === undefined ? 1 : 2 }
function frameOps(n) { var s = 0; for (var i = 0; i < n; i++) s += restf(i, i) + argsf(i, 2) + nt(); return s }
function stack(k) { return new Error("e" + k).stack.split("\n").length }
function midStack(k) { return stack(k) }
function stacks(n) { var s = 0; for (var i = 0; i < n; i++) s += midStack(i); return s }
`

// TestJITRegNativeCalls runs regCallSrc's callers in a runtime whose
// register code calls natively and in one with no JIT, and compares their
// answers: native calls made, their callees' exits finished by Go and their
// callers gone on natively, the collector seen idle and marking.
func TestJITRegNativeCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			// Its callees called, none inlined (jitRegFeedback's Inline):
			// the native calls are what it is about.
			regNoInline(t)
			rts := regNativeRuntimes(t, regCallSrc)
			// Compiled at their first calls: callers before their callees,
			// whose calls they learn at their calls of Go.
			rts[0].SetJITWarmUp(1, 1)
			for _, c := range []struct {
				name string
				n    float64
			}{{"plain", 400}, {"chain", 600}, {"makes", 300}, {"catches", 400}, {"uncaught", 400}, {"deep", 20}, {"tails", 100}, {"callsGo", 300}, {"stacks", 100}, {"constructs", 300}, {"closures", 3000}, {"frameOps", 3000}} {
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
					for range 8 {
						v, err := r.Call(fn, Undefined, []Value{Float(c.n)})
						got[i] = regNativeShow(r, v, err)
					}
				}
				if got[0] != got[1] {
					t.Errorf("%s: with the JIT %q, without %q", c.name, got[0], got[1])
				}
			}
			r := rts[0]
			st := r.RegNativeStats()
			native := map[string]uint64{}
			for key, l := range r.jit.regLinks {
				if fn := key.Value(); fn != nil {
					native[fn.Name] += l.in
				}
			}
			t.Logf("native calls %v; unwound %d, resumed %d, entries %d", native, st.Unwound, st.Resumed, st.Entries)
			t.Logf("register code %d bytes of %d", r.jit.regBytes(), r.jitBudget())
			for _, name := range []string{"add", "three", "leaf3", "mid3", "fib", "depth", "goes", "Q", "R", "T", "mkAdder", "midCap", "nt"} {
				if native[name] == 0 {
					t.Errorf("%s: never called natively", name)
				}
			}
			if st.Unwound == 0 || st.Resumed == 0 {
				t.Errorf("unwound %d, resumed %d: no callee left native code, or no caller went on natively", st.Unwound, st.Resumed)
			}
		})
	}
}

// regLitSrc calls RegExp literals' exec and test in loops, which register
// code makes one instruction of (regexp_lit): lowered as the stack form's
// code is (compile's regexpLit), the literal from its pool, its method
// read, called; then RegExp.prototype.exec replaced, and a getter put in
// its place, which the code was not compiled for.
const regLitSrc = `
function lits(n, s) {
	var k = 0, m = "";
	for (var i = 0; i < n; i++) {
		var r = /a(b+)?c/.exec(s + i);
		if (r) { k += r.length; m = r[1] || m }
		if (/\d$/.test(s + i)) k++;
		if (/x/.exec(s)) k += 2;
	}
	return k + m;
}
function swap() { var e = RegExp.prototype.exec; RegExp.prototype.exec = function (t) { return e.call(this, t + "c") }; return e }
function getter() { var e = RegExp.prototype.exec; Object.defineProperty(RegExp.prototype, "exec", { get() { return e }, configurable: true }) }
`

// TestJITRegExpLiteralCalls runs regLitSrc's lits natively and as ordinary
// calls and compares them: Go makes no regexp_lit whole at host exits.
func TestJITRegExpLiteralCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regLitSrc)
			rts[0].SetJITWarmUp(1, 1)
			run := func(name string, args ...Value) {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, args)
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Errorf("%s: with the JIT %q, without %q", name, got[0], got[1])
				}
			}
			for range 6 {
				run("lits", Float(3000), Str(NewString("abbc")))
			}
			// Of 54,000 made: all were left to Go before; now only where a
			// pool runs out, or before the code is compiled.
			if n := rts[0].RegNativeStats().HostOps["regexp_lit"]; n*100 > 54000 {
				t.Errorf("%d regexp_lit left to Go at host exits", n)
			}
			run("swap")
			for range 3 {
				run("lits", Float(500), Str(NewString("abb")))
			}
			run("getter")
			for range 3 {
				run("lits", Float(500), Str(NewString("abbc")))
			}
			t.Logf("%+v", rts[0].RegNativeStats())
		})
	}
}

// TestJITRegCallsCompileForARM64 compiles again, with arm64's backend, the
// register code regCallSrc's callers and callees were compiled to, on any
// host: the native calls between register functions (ssa.CallSite's
// Window), which arm64 makes as amd64 does, compile there. arm64 hosts
// run them (TestJITRegNativeCalls).
func TestJITRegCallsCompileForARM64(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	RecordJITCompiles(true)
	defer RecordJITCompiles(false)
	regNoInline(t)
	rts := regNativeRuntimes(t, regCallSrc)
	r := rts[0]
	r.SetJITWarmUp(1, 1)
	for _, name := range []string{"plain", "chain", "makes", "catches", "deep", "tails", "callsGo", "constructs", "closures", "frameOps"} {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
		for range 8 {
			r.Call(fn, Undefined, []Value{Float(300)})
		}
	}
	fns, windows, _ := regCompileARM64(t, r)
	t.Logf("%d functions compiled for arm64, %d native calls of register code", fns, windows)
	if windows == 0 {
		t.Error("no native call of register code compiled")
	}
}

// regCompileARM64 compiles again with arm64's backend, on any host, the
// register code r compiled since RecordJITCompiles, failing t where the
// backend refuses one, and returns how many functions it compiled, how
// many native calls of register code they make, and how many callees they
// inline.
func regCompileARM64(t *testing.T, r *Runtime) (fns, windows, inlined int) {
	t.Helper()
	jitParityLog.Lock()
	seen := jitParityLog.seen[r]
	jitParityLog.Unlock()
	for _, cl := range seen {
		e := r.jit.regCache[r.jitKeyOf(cl.fn)]
		if e == nil || e.p == nil || e.code == nil {
			continue
		}
		p := e.p
		f, err := ssa.BuildWith(p, r.jitRegFeedbackFor(cl, p))
		if err != nil {
			continue
		}
		// As jitRegCompile lays the register frame out.
		f.FrameLocals = p.Regs
		f.Scratch = p.Locals - (p.UpvalueBase + p.Upvalues)
		f.ThisSlot = -1
		f.Params, f.StartOnly = p.ParamBase+p.Params, false
		f.Cells = p.Cells
		f.CallEntry = true
		inlined += len(f.Inlined)
		ssa.Optimize(f)
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op == ssa.OpCall && len(v.Calls()) != 0 && v.Calls()[0].Window != 0 && v.Calls()[0].Go == 0 {
					windows++
				}
			}
		}
		if _, err := mir.CompileARM64(f, jitEncoding); err != nil {
			t.Errorf("%s: %v", cl.fn.Name, err)
			continue
		}
		fns++
	}
	return fns, windows, inlined
}

// regNoInline has no function inlined until the test ends (jitNoInline).
func regNoInline(t *testing.T) {
	saved := jitNoInline
	jitNoInline = func(*bytecode.Function) bool { return true }
	t.Cleanup(func() { jitNoInline = saved })
}

// regReuseSrc is Octane's RegExp's Exec, a match's array kept in a local
// that only its length and elements are read from, which register code
// marks for the interpreter to make again in the memory of the last
// (bytecode.RMarkReuse).
const regReuseSrc = `
function execs(n, s) {
	var re = /(\d+)-(\w+)/, sum = 0;
	for (var i = 0; i < n; i++) {
		re.lastIndex = 0;
		var array = re.exec(s + i);
		if (array) for (var j = 0; j < array.length; j++) { var p = array[j]; if (p) sum += p.length }
	}
	return sum;
}
`

// TestJITRegExecReuseLeaf runs regReuseSrc's execs natively and in a
// JIT-less runtime: its exec, marked RMarkReuse, is a leaf of the native
// code (jitRegFeedback's Leaf), which makes each array anew, as the stack
// form's native code does; the answers equal, the collector idle and
// marking, and the call neither left to Go at an exit nor made by Go.
func TestJITRegExecReuseLeaf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regReuseSrc)
			rts[0].SetJITWarmUp(1, 1)
			fn, _ := rts[0].GetProp(Obj(rts[0].Global()), rts[0].Intern("execs"))
			rc := fn.Object().fn().closure.fn.RegCode()
			reuse := false
			for _, m := range rc.Marks {
				reuse = reuse || m&bytecode.RMarkReuse != 0
			}
			if !reuse {
				t.Fatal("execs' exec is not marked RMarkReuse")
			}
			var got [2]string
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("execs"))
				for range 8 {
					v, err := r.Call(fn, Undefined, []Value{Float(2000), Str(NewString("x 12-ab "))})
					got[i] = regNativeShow(r, v, err)
				}
			}
			if got[0] != got[1] {
				t.Errorf("with the JIT %q, without %q", got[0], got[1])
			}
			st := rts[0].RegNativeStats()
			t.Logf("hosts %v, calls of Go %d", st.HostOps, st.GoCalls)
			if n := st.HostOps["call_method_w"]; n > 8 {
				t.Errorf("exec left to Go at %d exits", n)
			}
			// The leaf's, not a call of Go each turn (jitRegGoCallOn), as
			// before it was a leaf.
			if st.GoCalls > 8 {
				t.Errorf("exec made by %d calls of Go, not the leaf", st.GoCalls)
			}
		})
	}
}
