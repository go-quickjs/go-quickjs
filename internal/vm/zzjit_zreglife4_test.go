//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"strings"
	"testing"
)

// regWalkSrc is Octane Typescript's AstWalker: a recursive walk whose
// every level calls the visitors its walker holds, a walker for each pass
// with visitors of its own, and the children's walker, which calls the
// walk again, natively.
const regWalkSrc = `
function Walker(kids, pre, post) { this.kids = kids; this.pre = pre; this.post = post }
Walker.prototype.walk = function (n) {
	var p = this.pre(n, this);
	this.kids[n.t](n, this);
	return this.post(p, this);
};
var kids = [
	function (n, w) { },
	function (n, w) { w.walk(n.a) },
	function (n, w) { w.walk(n.a); w.walk(n.b) },
];
function tree(d) {
	if (d == 0) return { t: 0, v: 1 };
	if (d % 3 == 0) return { t: 1, v: d, a: tree(d - 1) };
	return { t: 2, v: d, a: tree(d - 1), b: tree(d - 2 < 0 ? 0 : d - 2) };
}
var root = tree(9);
var pres = [
	function (n, w) { return n.v }, function (n, w) { return n.v + 1 }, function (n, w) { return n.v + 2 },
	function (n, w) { return n.v + 3 }, function (n, w) { return n.v + 4 }, function (n, w) { return n.v + 5 },
	function (n, w) { return n.v + 6 }, function (n, w) { return n.v + 7 },
];
var posts = [
	function (p, w) { return p * 1 % 1000 }, function (p, w) { return p * 2 % 1000 }, function (p, w) { return p * 3 % 1000 },
	function (p, w) { return p * 4 % 1000 }, function (p, w) { return p * 5 % 1000 }, function (p, w) { return p * 6 % 1000 },
	function (p, w) { return p * 7 % 1000 }, function (p, w) { return p * 8 % 1000 },
];
function walks() {
	var s = 0;
	for (var k = 0; k < pres.length; k++) s += new Walker(kids, pres[k], posts[k]).walk(root);
	return s;
}
`

// TestJITRegRecursiveRecompiled runs a recursive walk whose levels are
// native calls of its own code, which learns at its exits the visitors
// each new walker brings, and checks that the walk is compiled again for
// them while its outer levels are in its code, as the stack form's code is
// (otherwiseInUse): its exits stop once it has learned them. Go finishes
// those levels, each in the code it holds; only a native call of it that
// has not returned, whose code Go would find by its function's, keeps it
// from being compiled (regCalled). Counted as levels instead, every level
// of the recursion Go was finishing kept it from being compiled again, and
// it left at each new visitor's call for good: Octane Typescript's
// AstWalker.walk, 285,000 times a run.
func TestJITRegRecursiveRecompiled(t *testing.T) {
	regBothMarking(t, func(t *testing.T) {
		regNoInline(t)
		rts := regNativeRuntimes(t, regWalkSrc)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(2, 2)
		var got [2]string
		for i, rt := range rts {
			fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("walks"))
			for range 60 {
				v, err := rt.Call(fn, Undefined, nil)
				got[i] = regNativeShow(rt, v, err)
			}
		}
		if got[0] != got[1] || got[0] != "492" {
			t.Fatalf("with the JIT %q, without %q, want 492", got[0], got[1])
		}
		w, _ := r.GetProp(Obj(r.Global()), r.Intern("Walker"))
		proto, _ := r.GetProp(w, r.Intern("prototype"))
		walk, _ := r.GetProp(proto, r.Intern("walk"))
		cl := walk.Object().fn().closure
		e := r.jit.regCache[r.jitRegKeyOf(cl)]
		if e == nil || e.code == nil || e.life == nil {
			t.Fatal("the walk has no register code")
		}
		hosts := e.life.ssaStats.hosts
		t.Logf("walk: %d compiles, %d host exits, %d native calls of it", e.life.ssaSerial, hosts, r.jit.regLinks[e.key].in)
		if e.life.reopt {
			t.Errorf("the walk is still to be compiled for what it learned")
		}
		// Each run has its eight walkers' visitors called at 3 of the walk's
		// instructions, at some 100 levels each: before, every one of them
		// past the first compile left the code.
		if hosts > 100 {
			t.Errorf("the walk left its code %d times; compiled again for its visitors, it leaves at a few of their first calls", hosts)
		}
	})
}

// regBusyAsyncSrc is microbench_extra's async_busy: an async function
// doing work between awaits of settled values, its loop's.
const regBusyAsyncSrc = `
var out = [];
async function busy(n) {
	var i, s = 0;
	for (i = 0; i < n; i++) {
		s = (s + i * 3) | 0;
		if ((i & 255) == 0) s = (s + await (i & 7)) | 0;
	}
	return s;
}
function drive(n) { busy(n).then(function (v) { out.push(v) }) }
var order = [];
async function tick(tag, n) {
	var s = 0;
	for (var i = 0; i < n; i++) {
		s = (s + i) | 0;
		if ((i & 1023) == 0) { order.push(tag + (i >> 10)); await s }
	}
	order.push(tag + "!");
}
function interleave() { order = []; tick("a", 5000); tick("b", 3000); Promise.resolve().then(function () { order.push("p") }) }
`

// TestJITRegSettledAwaitGoesOn runs an async function whose loop awaits a
// settled value, the job queue otherwise empty, and checks that its
// native code goes on after each such await: Go makes the await in place
// at the code's exit, as the interpreter's await would (regAwaitsInPlace),
// and no run of the code ends in the interpreter there. Before, Go left
// the await to the interpreter, which went on in place and ran the rest of
// the frame, every turn of the loop to its end.
func TestJITRegSettledAwaitGoesOn(t *testing.T) {
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, regBusyAsyncSrc)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(2, 2)
		r.SetJITReport(true)
		var got [2]string
		for i, rt := range rts {
			fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("drive"))
			for range 20 {
				if _, err := rt.Call(fn, Undefined, []Value{Float(20000)}); err != nil {
					t.Fatal(err)
				}
				if err := rt.DrainJobs(); err != nil {
					t.Fatal(err)
				}
			}
			v, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("out"))
			s, _ := rt.GetProp(v, rt.Intern("length"))
			last, _ := rt.GetProp(v, rt.Intern("19"))
			got[i] = regNativeShow(rt, s, nil) + ":" + regNativeShow(rt, last, nil)
			// Awaits with other jobs queued suspend, in the order the
			// queue resumes them.
			il, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("interleave"))
			for range 20 {
				if _, err := rt.Call(il, Undefined, nil); err != nil {
					t.Fatal(err)
				}
				if err := rt.DrainJobs(); err != nil {
					t.Fatal(err)
				}
			}
			o, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("order"))
			got[i] += " " + regNativeShow(rt, o, nil)
		}
		const want = "20:599970000 a0,b0,a1,b1,p,a2,b2,a3,b!,a4,a!"
		if got[0] != got[1] || got[0] != want {
			t.Fatalf("with the JIT %q, without %q, want %q", got[0], got[1], want)
		}
		ended := r.RegNativeStats().Ended
		for k, n := range ended {
			t.Logf("ended: %s %d", k, n)
			if strings.Contains(k, "await in busy") {
				t.Errorf("%d runs ended in the interpreter at a settled await: %s", n, k)
			}
		}
		if hosts := r.RegNativeStats().HostOps["await"]; hosts < 1000 {
			t.Errorf("native code made %d awaits; want each of the loop's after its first runs", hosts)
		}
	})
}

// regRangeSrc is microbench_extra's iterator_for_of: a for...of over an
// iterator whose next is the script's, and loops that break out of one,
// one whose next throws, and one whose next returns no object.
const regRangeSrc = `
function Range(n) { this.i = 0; this.n = n }
Range.prototype.next = function () {
	if (this.i < this.n) return { value: this.i++, done: false };
	return { value: undefined, done: true };
};
Range.prototype[Symbol.iterator] = function () { return this };
var closed = 0;
Range.prototype.return = function () { closed++; return {} };
function sum(n) {
	var s = 0;
	for (var j = 0; j < n; j++) for (var v of new Range(100)) s += v;
	return s;
}
function early(n) {
	var s = 0;
	for (var j = 0; j < n; j++) for (var v of new Range(100)) { if (v == 7) break; s += v }
	return s + ":" + closed;
}
function Bad(k, bad) { this.i = 0; this.k = k; this.bad = bad }
Bad.prototype.next = function () {
	if (this.i == this.k) { if (this.bad) return 5; throw new RangeError("at " + this.i) }
	return { value: this.i++, done: false };
};
Bad.prototype[Symbol.iterator] = function () { return this };
function throws(n) {
	var s = 0, m = "";
	for (var j = 0; j < n; j++) {
		try { for (var v of new Bad(j % 50, j % 3 == 0)) s += v } catch (e) { m = e.constructor.name + " " + e.message }
	}
	return s + ":" + m;
}
`

// TestJITRegIteratorStepMade runs for...of loops over iterators of script
// natively, and checks that each step is the stack form's call of Go
// (jitRegStepMade, jitCallMade's): the step itself, which calls the
// iterator's next, not the interpreter's instruction run whole on the
// frame (jitRegWhole) -- and that a break closes the iterator, and what
// next throws, or a result that is no object, goes to the loop's handler.
func TestJITRegIteratorStepMade(t *testing.T) {
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, regRangeSrc)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(2, 2)
		r.SetJITReport(true)
		calls := []struct{ name, want string }{
			{"sum", "990000"},
			{"early", "4200:2000"},
			{"throws", "78400:RangeError at 49"},
		}
		for _, call := range calls {
			var got [2]string
			for i, rt := range rts {
				fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern(call.name))
				for range 10 {
					v, err := rt.Call(fn, Undefined, []Value{Float(200)})
					got[i] = regNativeShow(rt, v, err)
				}
			}
			if got[0] != got[1] || got[0] != call.want {
				t.Fatalf("%s: with the JIT %q, without %q, want %q", call.name, got[0], got[1], call.want)
			}
		}
		ops := r.RegNativeStats().GoCallOps
		t.Logf("steps %d, whole %d", ops["iter_next"], ops["whole iter_next"])
		if ops["iter_next"] < 1000 {
			t.Errorf("native code made %d steps of Go; want each step of the loops", ops["iter_next"])
		}
		if n := ops["whole iter_next"]; n != 0 {
			t.Errorf("%d steps run whole on the frame", n)
		}
	})
}

// regInlinedCtorSrc constructs in a callee only ever inlined: make is
// called from run's loop alone, inlined there before its own code is
// compiled, if ever.
const regInlinedCtorSrc = `
function P(x, y) { this.x = x; this.y = y }
function make(i) { return new P(i, i + 1) }
function run(n) { var s = 0; for (var i = 0; i < n; i++) { var p = make(i); s += p.x + p.y } return s }
function Q(x, y) { this.x = -x; this.y = 2 * y }
function swap() { P = Q }
`

// TestJITRegInlinedConstructionLearned runs a loop whose callee, inlined
// in it, constructs with a global's function, and checks that no
// construction is made by Go: the construction is seeded from the
// function the global holds when the loop is compiled, in the callee
// inlined (regConstructorNow, jitRegSeedConstruct), as the stack form's
// compiles seed one (jitSeedCalls). Before, the construction was learned
// only at a call of Go from code of the callee's, or of the code inlining
// it, which then had to be compiled again for it.
func TestJITRegInlinedConstructionLearned(t *testing.T) {
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, regInlinedCtorSrc)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(2, 2)
		r.SetJITReport(true)
		var got [2]string
		for i, rt := range rts {
			fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("run"))
			for range 50 {
				v, err := rt.Call(fn, Undefined, []Value{Float(1000)})
				got[i] = regNativeShow(rt, v, err)
			}
		}
		if got[0] != got[1] || got[0] != "1000000" {
			t.Fatalf("with the JIT %q, without %q, want 1000000", got[0], got[1])
		}
		st := r.RegNativeStats()
		defer func() {
			// The global's function replaced: the construction checks it.
			for i, rt := range rts {
				sw, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("swap"))
				if _, err := rt.Call(sw, Undefined, nil); err != nil {
					t.Fatal(err)
				}
				fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("run"))
				v, err := rt.Call(fn, Undefined, []Value{Float(1000)})
				got[i] = regNativeShow(rt, v, err)
			}
			if got[0] != got[1] || got[0] != "501500" {
				t.Errorf("P replaced: with the JIT %q, without %q, want 501500", got[0], got[1])
			}
		}()
		var made uint64
		for k, n := range st.GoCallOps {
			t.Logf("call of Go %s %d", k, n)
			if strings.HasPrefix(k, "new_w") {
				made += n
			}
		}
		for k, n := range st.HostOps {
			t.Logf("host exit %s %d", k, n)
			if k == "new_w" {
				made += n
			}
		}
		if made != 0 {
			t.Errorf("Go made %d of the 50,000 constructions; want them native from the first compile", made)
		}
		if e := regCodeNamed(t, r, "make"); e != nil && e.code != nil {
			t.Logf("make has code of its own: %d native calls", r.jit.regLinks[e.key].in)
		}
	})
}
