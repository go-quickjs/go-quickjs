//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regGenStepSrc steps register generators every way a program does: by
// for...of, by next with values sent in, with return and throw injected
// into a suspended one, through yield*, across try/finally, with captures,
// a method's this and a named expression's own name, and with a throw out
// of its body.
const regGenStepSrc = `
function* count(n) { for (let i = 0; i < n; i++) yield i; }
function forOf(n) {
	let s = 0;
	for (let k = 0; k < 4; k++) for (const v of count(n)) s += v;
	return s;
}
function* acc() {
	let t = 0;
	for (;;) {
		const x = yield t;
		if (x === undefined) return t;
		t += x;
	}
}
function nexts(n) {
	let out = '';
	for (let k = 0; k < 3; k++) {
		const g = acc();
		g.next();
		let r;
		for (let i = 1; i <= n; i++) r = g.next(i);
		const end = g.next();
		out += r.value + ',' + r.done + ',' + end.value + ',' + end.done + ';';
	}
	return out;
}
function* fin(log) {
	let x = 0;
	try {
		x = 5;
		yield 1;
		x = 6;
		yield 2;
	} finally {
		log.push('f' + x);
	}
	return 9;
}
function* catcher() {
	let caught = 0;
	for (;;) {
		try {
			yield caught;
		} catch (e) {
			caught += e;
		}
	}
}
function injected(n) {
	let s = '';
	for (let i = 0; i < n; i++) {
		const log = [];
		const g = fin(log);
		g.next();
		const r = g.return(7);
		const a = fin(log);
		for (const v of a) {}
		const c = catcher();
		c.next();
		c.throw(2);
		const t = c.throw(3);
		if (i === n - 1) s = r.value + ',' + r.done + ',' + log.join('') + ',' + t.value + ',' + g.next().done;
	}
	return s;
}
function* outer(n) { yield* count(n); yield -1; }
function delegate(n) {
	let s = 0;
	for (let k = 0; k < 3; k++) for (const v of outer(n)) s += v;
	return s;
}
function* tf(n) {
	let k = 0;
	for (let i = 0; i < n; i++) {
		try { yield i; k++; } finally { k += 2; }
	}
	return k;
}
function finallies(n) {
	let s = 0, last;
	for (let j = 0; j < 3; j++) {
		const g = tf(n);
		for (let r = g.next(); ; r = g.next()) {
			if (r.done) { last = r.value; break; }
			s += r.value;
		}
	}
	return s + ':' + last;
}
function* captures(n) {
	let s = 0;
	const add = v => { s += v; };
	for (let i = 0; i < n; i++) { add(i); yield s; }
}
function scaled(k) { return function* (n) { for (let i = 0; i < n; i++) yield i * k; }; }
const by3 = scaled(3);
function captured(n) {
	let s = 0;
	for (let k = 0; k < 3; k++) {
		for (const v of captures(n)) s += v;
		for (const v of by3(n)) s += v;
	}
	return s;
}
const holder = { k: 3, *items(n) { for (let i = 0; i < n; i++) yield this.k * i; } };
const named = function* self(n) { for (let i = 0; i < n; i++) yield typeof self === 'function' ? i : -1000; };
function receivers(n) {
	let s = 0;
	for (let k = 0; k < 3; k++) {
		for (const v of holder.items(n)) s += v;
		for (const v of named(n)) s += v;
	}
	return s;
}
function* thrower(n) {
	for (let i = 0; i < n; i++) {
		if (i === n - 1) throw new Error('boom ' + i);
		yield i;
	}
}
function throws(n) {
	let s = 0, m = '';
	for (let k = 0; k < 3; k++) {
		try {
			for (const v of thrower(n)) s += v;
		} catch (e) {
			m = e.message;
		}
	}
	const [a, b, c] = count(5);
	return s + ',' + m + ',' + a + b + c;
}
let out = 0;
function* dead(n) {
	let x = 0;
	for (let i = 0; i < n; i++) {
		try { x = i * 2 + 1; yield i; x = -1; } finally { out = x; }
	}
}
function returned(n) {
	let s = '';
	for (let k = 0; k < 3; k++) {
		const g = dead(1e9);
		let t = 0;
		for (let i = 0; i < n; i++) t += g.next().value;
		const r = g.return(5);
		s = t + ',' + out + ',' + r.value + ',' + r.done;
	}
	return s;
}
function* own(n) { for (let i = 0; i < n; i++) yield i; }
function cursors(n) {
	let s = 0;
	for (let k = 0; k < 3; k++) {
		for (const v of count(n)) s += v;
		const g = count(n);
		for (const v of g) { s += v; if (v === 2) break; }
		for (const v of g) s += 1000 * v;
		const h = own(3);
		h[Symbol.iterator] = function* () { yield 7; };
		for (const v of h) s += v;
		for (const v of [1, 2, 3]) s += v;
		for (const v of 'ab') s += v.length;
		for (const v of new Map([[1, 2]])) s += v[1];
	}
	return s;
}
function* mixedBody(n) {
	let s = 0, t = '';
	for (let i = 0; i < n; i++) {
		s += i;
		if (i % 50 === 49) t = t + String.fromCharCode(97 + i % 26) + JSON.stringify({ i });
		if (i === n - 3) s = 'x' + s;
		const sent = yield s + ':' + t.length;
		if (sent !== undefined) s += sent;
	}
	return t.length;
}
function bodies(n) {
	let out = '';
	for (let k = 0; k < 3; k++) {
		const g = mixedBody(n);
		let c = 0, last, r;
		for (r = g.next(); !r.done; r = g.next(k)) { c++; last = r.value; }
		let f = 0;
		for (const v of mixedBody(n)) f += typeof v === 'string' ? v.length : 0;
		out += c + '|' + last + '|' + r.value + '|' + f + ';';
	}
	return out;
}
function* pair(a, b) { yield a; yield b; }
function pairs(n) {
	let s = 0;
	for (let j = 0; j < n; j++) for (const v of pair(j, 1)) s += v;
	return s;
}
`

// TestJITRegGenSteps runs register generators stepped by native code --
// a loop's step and a call of next resuming the generator's code at its
// generator entry, its yields suspending it natively (ssa.CallSite's
// Gens, mir's windowGenEntry and windowYieldExit) -- pinned, equal without
// the JIT, the collector idle and marking.
func TestJITRegGenSteps(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	cases := []struct {
		name string
		n    float64
		want string
	}{
		{"forOf", 500, "499000"},
		{"nexts", 300, "45150,false,45150,true;45150,false,45150,true;45150,false,45150,true;"},
		{"injected", 200, "7,true,f5f6,5,true"},
		{"delegate", 300, "134547"},
		{"finallies", 200, "59700:600"},
		{"captured", 200, "4179000"},
		{"receivers", 200, "238800"},
		{"throws", 200, "59103,boom 199,012"},
		{"pairs", 3000, "4501500"},
		{"returned", 3000, "4498500,5999,5,true"},
		{"cursors", 300, "134610"},
		// Its body leaves native code -- calls of Go, a deoptimization --
		// where Go makes a window of the generator's values, which native
		// code going on runs on and a yield writes back (jitRegGenLevel).
		{"bodies", 200, "200|x1950301980199:38|38|1309;200|x1970011981199:38|38|1309;200|x1989721982199:38|38|1309;"},
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regGenStepSrc)
			rts[0].SetJITReport(true)
			// Counted: what stress makes of the compiles' order is not.
			rts[0].jitStress = jitStressConfig{}
			for _, tc := range cases {
				for range 12 {
					var got [2]string
					for i, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(tc.name))
						v, err := r.Call(fn, Undefined, []Value{Float(tc.n)})
						got[i] = regNativeShow(r, v, err)
					}
					if got[0] != got[1] {
						t.Fatalf("%s: with the JIT %q, without %q", tc.name, got[0], got[1])
					}
					if got[1] != tc.want {
						t.Fatalf("%s: %q, want %q", tc.name, got[1], tc.want)
					}
				}
			}
			st := rts[0].RegNativeStats()
			if n := st.GoCallOps["whole iter_next"]; n > 20000 {
				t.Errorf("%d steps of generators by calls of Go", n)
			}
			if n := st.GoCallOps["whole for_of_start"]; n > 20000 {
				t.Errorf("%d for...of cursors made by calls of Go", n)
			}
			if n := st.GoCallOps["call_method_w->next"]; n > 5000 {
				t.Errorf("%d calls of next by calls of Go", n)
			}
			t.Logf("calls of Go: %v; hosts: %v", st.GoCallOps, st.HostOps)
		})
	}
}

// A for...of loop over a generator, the frames run by the Go handlers
// (QJS_NONATIVE, the fallback dispatcher): the generator's code, leaving
// native code at each yield, had made the next back edge's check due at
// once (jitRegLeaveTo) -- the consumer's loop's, a check that is no
// sample -- so that every back edge of forOf's spent its budget for
// nothing, and forOf, sampled never, was never compiled.
func TestJITRegGenConsumerSampled(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	nativeForTest(t, false)
	rts := regNativeRuntimes(t, regGenStepSrc)
	r := rts[0]
	var got [2]string
	for i, rt := range rts {
		fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("forOf"))
		for range 12 {
			v, err := rt.Call(fn, Undefined, []Value{Float(500)})
			got[i] = regNativeShow(rt, v, err)
		}
	}
	if got[0] != got[1] {
		t.Errorf("forOf: with the JIT %q, without %q", got[0], got[1])
	}
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern("forOf"))
	cl := fn.Object().fn().closure
	if e := r.jit.regCache[r.jitRegKeyOf(cl)]; e == nil || e.code == nil {
		t.Error("forOf not compiled: its loop's back edges were not sampled")
	}
}

// An async function whose loop awaits at every turn: each turn is a
// resumption Go makes of its register code, and the loops' back edges in
// all are too few for a sample. Its resumptions are its invocations, each
// spending its length of its budget (jitRegResumed): counted only at every
// jitHotWork of them, as code refused for host operations alone is, af was
// never compiled, and nothing of the program ran natively.
func TestJITRegAsyncResumptionsCompile(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `var out = 0;
async function af(n) { let total = 0; const add = v => { total += v }; for (let i = 0; i < n; i++) add(await i); return total }
async function main(m) { for (let k = 0; k < m; k++) out += await af(k) }`
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, src)
			// Counted: what stress makes of the compiles' order is not.
			rts[0].jitStress = jitStressConfig{}
			rts[0].SetJITWarmUp(8, 8)
			var got [2]string
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("main"))
				// 435 turns of af's loop and 30 of main's: no sample.
				if _, err := r.Call(fn, Undefined, []Value{Float(30)}); err != nil {
					t.Fatal(err)
				}
				if err := r.DrainJobs(); err != nil {
					t.Fatal(err)
				}
				v, err := r.GetProp(Obj(r.Global()), r.Intern("out"))
				got[i] = regNativeShow(r, v, err)
			}
			if got[0] != got[1] || got[1] != "4060" {
				t.Fatalf("with the JIT %q, without %q, want 4060", got[0], got[1])
			}
			r := rts[0]
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("af"))
			cl := fn.Object().fn().closure
			if e := r.jit.regCache[r.jitRegKeyOf(cl)]; e == nil || e.code == nil {
				t.Fatal("af not compiled: its resumptions were not counted")
			}
			if st := r.JITStats(); st.SSAEntries == 0 {
				t.Errorf("nothing ran natively: %+v", st)
			}
		})
	}
}

// TestJITRegGenStepsCompileForARM64 compiles again, with arm64's backend,
// on any host, the register code regGenStepSrc's generators were compiled
// to, their generator entries and native yields (mir's windowGenEntry,
// windowYieldExit), and their callers' native steps, calls of next and
// for...of cursors taken from a generator's room (mir's genForOf).
func TestJITRegGenStepsCompileForARM64(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	recordJITCompilesForTest(t)
	rts := regNativeRuntimes(t, regGenStepSrc)
	r := rts[0]
	for _, name := range []string{"forOf", "nexts", "finallies", "pairs", "cursors"} {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
		for range 12 {
			r.Call(fn, Undefined, []Value{Float(500)})
		}
	}
	fns, _, _ := regCompileARM64(t, r)
	if fns == 0 {
		t.Error("nothing compiled for arm64")
	}
	jitCompileLog.Lock()
	seen := jitCompileLog.seen[r]
	jitCompileLog.Unlock()
	gens, steps, cursors := 0, 0, 0
	for _, cl := range seen {
		e := r.jit.regCache[r.jitKeyOf(cl.fn)]
		if e == nil || e.p == nil || e.code == nil {
			continue
		}
		f, err := ssa.BuildWith(e.p, r.jitRegFeedbackFor(cl, e.p))
		if err != nil {
			continue
		}
		jitRegShape(f, e.p, cl, cl.reg)
		ssa.Optimize(f)
		if f.GenEntry {
			gens++
		}
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op == ssa.OpCall && len(v.Calls()) != 0 && len(v.Calls()[0].Gens) != 0 {
					steps++
				}
				if v.Op == ssa.OpCall && len(v.Calls()) != 0 && v.Calls()[0].ForOf && v.Calls()[0].GenIter != 0 {
					cursors++
				}
			}
		}
		if _, err := mir.CompileARM64(f, jitEncoding); err != nil {
			t.Errorf("%s: %v", cl.fn.Name, err)
		}
	}
	if gens < 3 || steps < 3 || cursors < 2 {
		t.Errorf("%d generator entries, %d native steps, %d native cursors compiled for arm64", gens, steps, cursors)
	}
}
