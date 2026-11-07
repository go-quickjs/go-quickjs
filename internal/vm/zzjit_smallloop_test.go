//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"testing"
)

// A small callee whose loop calls nothing is inlined (ssa's
// maxInlineLoop): EarleyBoyer's sc_assq, a search of a short list, in
// the caller's code, which then makes no call of it. Its returns from
// inside the loop and after it, and a list it walks to the end, are each
// the interpreter's, with the collector idle and marking.
func TestJITInlinesSmallLoop(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	inlined := jitInlinedForTest(t)
	const setup = `
		function P(car, cdr) { this.car = car; this.cdr = cdr; }
		function assq(o, al) { while (al !== null) { if (al.car.car === o) return al.car; al = al.cdr; } return false; }
		var al = null;
		for (var k = 0; k < 6; k++) al = new P(new P('k' + k, k * 10), al);
		function unify(n) {
			var s = 0;
			for (var i = 0; i < n; i++) {
				var hit = assq('k' + (i % 8), al);
				s = (s + (hit === false ? -1 : hit.cdr)) | 0;
			}
			return s;
		}`
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		both := jitBothForTest(t, setup)
		for range 6 {
			both.agree(t, `unify(400)`, `assq('k5', al).cdr`, `assq('none', al)`, `assq('k0', null)`)
		}
		if !jitInlinedFnForTest(t, both.jit, inlined, "unify", "assq") {
			t.Fatalf("unify's code does not inline assq: %x", inlined["unify"])
		}
	}
}

// jitInlinedFnForTest reports whether the last compile of the function
// named caller has inlined, of what each compile inlined (inlined, from
// jitInlinedForTest), the function the global expression callee gives in
// r -- or, for callee "", anything.
func jitInlinedFnForTest(t *testing.T, r *Runtime, inlined map[string][]uintptr, caller, callee string) bool {
	t.Helper()
	if callee == "" {
		return len(inlined[caller]) != 0
	}
	return slices.Contains(inlined[caller], jitClosureForTest(t, r, callee))
}

// A function that gives a closure it makes to a callee whose loop calls it
// is built so that the closure is inlined there and not made
// (ssa.InlineCallsGiven); its other calls are inlined by the same rule
// in that build as in any other, a small loop that calls nothing
// included. That build had refused find, which every other inlined, and
// the call was made natively. Each answer is the interpreter's.
func TestJITInlinesSmallLoopBesideGiven(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	inlined := jitInlinedForTest(t)
	const setup = `
		function N(v, next) { this.v = v; this.next = next; }
		var list = new N(0, new N(1, new N(2, new N(3, null)))), arr = [1, 2, 3];
		function each(a, f) { for (var i = 0; i < a.length; i++) f(a[i]); }
		function find(l, x) { while (l !== null) { if (l.v === x) return l.v; l = l.next; } return -1; }
		function caller(a, l, k) {
			var t = 0;
			for (var i = 0; i < k; i++) { each(a, function (v) { t += v; }); t += find(l, i % 6); }
			return t;
		}`
	both := jitBothForTest(t, setup)
	for range 8 {
		both.agree(t, `caller(arr, list, 300)`)
	}
	r := both.jit
	for _, callee := range []string{"each", "find"} {
		if !jitInlinedFnForTest(t, r, inlined, "caller", callee) {
			t.Errorf("caller's code does not inline %s: %x", callee, inlined["caller"])
		}
	}
	if e := r.jit.hint(jitGlobalClosureForTest(r, "caller")); e == nil || e.ssaCalled != 1 || len(e.ssaInlinedAt) != 2 {
		t.Fatalf("caller's code does not inline both calls, its closure not made: %+v", e)
	}
	hosts := r.jit.hosts
	both.agree(t, `caller(arr, list, 300)`, `caller([4], new N(5, list), 50)`)
	if n := r.jit.hosts - hosts; n != 0 {
		t.Errorf("caller left native code %d times", n)
	}
}

// smallLoopFramesSetup has a small callee with a loop called as each kind
// of frame the inliner makes: a call, a getter and a setter, a
// construction, a forwarding constructor's method (InlineSite's Restart
// and Forward) and one that does more (Arguments), a callee that reads its
// arguments by index (ReadsArguments), a closure its caller makes
// (Captured) and an array's callback. Each walks a list.
const smallLoopFramesSetup = `
	function N(v, next) { this.v = v; this.next = next; }
	function mk(n, v) { var l = null; for (var k = n - 1; k >= 0; k--) l = new N(v === undefined ? k : v, l); return l; }
	var list = mk(6);
	function find(l, x) { while (l !== null) { if (l.v === x) return l.v; l = l.next; } return -1; }
	function finds(l, k) { var t = 0; for (var i = 0; i < k; i++) t += find(l, i % 8); return t; }
	function total(l) { var s = 0; while (l !== null) { s += l.v; l = l.next; } return s; }
	function totals(l, k) { var t = 0; for (var i = 0; i < k; i++) t += total(l); return t; }
	class Box {
		constructor(items) { this.items = items; }
		get sum() { var s = 0, l = this.items; while (l !== null) { s += l.v; l = l.next; } return s; }
		set add(x) { var l = this.items; while (l !== null) { l.w = l.v + x; l = l.next; } }
	}
	function getter(b, k) { var s = 0; for (var i = 0; i < k; i++) s += b.sum; return s; }
	function setter(b, k) { var s = 0; for (var i = 0; i < k; i++) { b.add = i; s += b.items.w; } return s; }
	var out = { n: 0 };
	function V(l, o) { this.s = 0; while (l !== null) { o.n++; this.s += l.v; l = l.next; } }
	function ctor(l, k) { var t = 0; for (var i = 0; i < k; i++) t += new V(l, out).s; return t + ':' + out.n; }
	function F() { this.init.apply(this, arguments); }
	F.prototype.init = V;
	function fwd(l, k) { var t = 0; for (var i = 0; i < k; i++) t += new F(l, out).s; return t + ':' + out.n; }
	function G() { this.k = 1; this.init.apply(this, arguments); }
	G.prototype.init = V;
	function fwdArgs(l, k) { var t = 0; for (var i = 0; i < k; i++) t += new G(l, out).s + new G(l, out).k; return t + ':' + out.n; }
	function pick() { var s = 0, l = arguments[0]; while (l !== null) { s += l.v; l = l.next; } return s; }
	function args(l, k) { var t = 0; for (var i = 0; i < k; i++) t += pick(l, i); return t; }
	function made(l, k) {
		var base = 3;
		var f = function (m) { while (m !== null) { base += m.v; m = m.next; } return base; };
		var t = 0;
		for (var j = 0; j < k; j++) t = (t + f(l)) | 0;
		return t + base;
	}
	function each(ls, k) {
		var t = 0;
		for (var i = 0; i < k; i++) ls.forEach(function (l) { while (l !== null) { t += l.v; l = l.next; } });
		return t;
	}
	function trace(f) { try { return f(); } catch (e) { return e.constructor.name + ': ' + e.stack; } }
	var odd = { v: 7, next: list }, big = mk(4, 0x7ffffff0), str = mk(3, 'x');
	var broken = new N(1, new N(2, undefined));`

// smallLoopFrames are the callers of smallLoopFramesSetup, each with the
// global expression of the callee it inlines, if it can be named, and how
// it is called on a list l k times.
var smallLoopFrames = []struct{ caller, callee, call string }{
	{"finds", "find", "finds(%s, %d)"},
	{"totals", "total", "totals(%s, %d)"},
	{"getter", "Object.getOwnPropertyDescriptor(Box.prototype, 'sum').get", "getter(new Box(%s), %d)"},
	{"setter", "Object.getOwnPropertyDescriptor(Box.prototype, 'add').set", "setter(new Box(%s), %d)"},
	{"ctor", "V", "ctor(%s, %d)"},
	{"fwd", "V", "fwd(%s, %d)"},
	{"fwdArgs", "V", "fwdArgs(%s, %d)"},
	{"args", "pick", "args(%s, %d)"},
	{"made", "", "made(%s, %d)"},
	{"each", "", "each([%s, %[1]s], %d)"},
}

// InlineCalls decides for every kind of frame the inliner makes, not only
// a plain call's, so a small loop is inlined in each, and runs there
// without leaving native code. In each the loop deoptimizes -- at a node
// of another shape, a sum past int32, a string -- and throws, a node's
// next undefined, each answer and error the interpreter's, stack trace
// included, and what the loop did to another object before it left done
// once, with the collector idle and marking.
func TestJITInlinedSmallLoopFrames(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	inlined := jitInlinedForTest(t)
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		both := jitBothForTest(t, smallLoopFramesSetup)
		for range 6 {
			for _, c := range smallLoopFrames {
				both.agree(t, fmt.Sprintf(c.call, "list", 300))
			}
		}
		r := both.jit
		for _, c := range smallLoopFrames {
			if !jitInlinedFnForTest(t, r, inlined, c.caller, c.callee) {
				t.Errorf("%s's code does not inline %s: %x", c.caller, c.callee, inlined[c.caller])
			}
			hosts := r.jit.hosts
			both.agree(t, fmt.Sprintf(c.call, "list", 300))
			if n := r.jit.hosts - hosts; n != 0 {
				t.Errorf("%s left native code %d times", c.caller, n)
			}
		}
		for _, c := range smallLoopFrames {
			for _, l := range []string{"odd", "big", "str", "list"} {
				both.agree(t, fmt.Sprintf(c.call, l, 40))
			}
			both.agree(t, "trace(() => "+fmt.Sprintf(c.call, "broken", 40)+")")
			both.agree(t, fmt.Sprintf(c.call, "list", 300))
		}
	}
}

// A loop inlined in its caller still polls, in every kind of frame the
// inliner makes: its back edge's budget runs out, Go gives it another and
// native code goes on, leaving for nothing; and a context cancelled
// before the call stops the script within a budget of the inlined loop's
// turns, as it stops the loop out of line (TestJITSSAPollsAsCallsOfGo,
// whose callee is not inlined). Each answer is the interpreter's.
func TestJITInlinedSmallLoopPolls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	inlined := jitInlinedForTest(t)
	// A callee of each kind runs a loop of n turns, each counted in c.n.
	const setup = `var c = { n: 0 };
		function leaf(c, n) { var s = 0; for (var i = 0; i < n; i++) { s = (s * 3 + i) | 0; c.n++; } return s; }
		class Box { constructor(c, n) { this.c = c; this.n = n; }
			get spin() { var s = 0, c = this.c; for (var i = 0; i < this.n; i++) { s = (s * 3 + i) | 0; c.n++; } return s; } }
		function V(c, n) { this.s = 0; for (var i = 0; i < n; i++) { this.s = (this.s * 3 + i) | 0; c.n++; } }
		function F() { this.init.apply(this, arguments); }
		F.prototype.init = V;
		function pick() { var c = arguments[0], i = arguments[1]; while (i > 0) { c.n++; i--; } return c.n & 0xff; }
		`
	kinds := []struct{ caller, callee, src string }{
		{"plain", "leaf", `function plain(n) { var t = 0, m = n < 0 ? -n : n;
				for (var j = 0; j < 8; j++) { if (j === 5 && n < 0) cancel(); t = (t + leaf(c, m)) | 0; } return t; }`},
		{"getter", "Object.getOwnPropertyDescriptor(Box.prototype, 'spin').get",
			`function getter(n) { var t = 0, m = n < 0 ? -n : n, b = new Box(c, m);
				for (var j = 0; j < 8; j++) { if (j === 5 && n < 0) cancel(); t = (t + b.spin) | 0; } return t; }`},
		{"ctor", "V", `function ctor(n) { var t = 0, m = n < 0 ? -n : n;
				for (var j = 0; j < 8; j++) { if (j === 5 && n < 0) cancel(); t = (t + new V(c, m).s) | 0; } return t; }`},
		{"fwd", "V", `function fwd(n) { var t = 0, m = n < 0 ? -n : n;
				for (var j = 0; j < 8; j++) { if (j === 5 && n < 0) cancel(); t = (t + new F(c, m).s) | 0; } return t; }`},
		{"args", "pick", `function args(n) { var t = 0, m = n < 0 ? -n : n;
				for (var j = 0; j < 8; j++) { if (j === 5 && n < 0) cancel(); t = (t + pick(c, m)) | 0; } return t; }`},
		{"made", "", `function made(n) { var t = 0, m = n < 0 ? -n : n, d = c;
				var f = function (k) { var s = 0; for (var i = 0; i < k; i++) { s = (s * 3 + i) | 0; d.n++; } return s; };
				for (var j = 0; j < 8; j++) { if (j === 5 && n < 0) cancel(); t = (t + f(m)) | 0; } return t; }`},
		{"each", "", `function each(n) { var t = 0, m = n < 0 ? -n : n, cs = [c];
				for (var j = 0; j < 8; j++) { if (j === 5 && n < 0) cancel();
					cs.forEach(function (d) { var s = 0; for (var i = 0; i < m; i++) { s = (s * 3 + i) | 0; d.n++; } t = (t + s) | 0; }); }
				return t; }`},
	}
	for _, k := range kinds {
		t.Run(k.caller, func(t *testing.T) {
			want := New(Config{})
			t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
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
				if _, err := rt.Run(compileForTest(t, setup+k.src)); err != nil {
					t.Fatal(err)
				}
			}
			call := "String(" + k.caller + "(%d))"
			for i := range 4 {
				wv, err := want.Run(compileForTest(t, fmt.Sprintf(call, 5000)))
				if err != nil {
					t.Fatal(err)
				}
				budgets, hosts := r.jit.budgets, r.jit.hosts
				gv, err := r.Run(compileForTest(t, fmt.Sprintf(call, 5000)))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := gv.String().Go(), wv.String().Go(); got != want {
					t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
				}
				if i == 3 {
					if r.jit.budgets == budgets {
						t.Fatal("no loop ran its budget out")
					}
					if r.jit.hosts != hosts {
						t.Fatalf("native code left for Go %d times", r.jit.hosts-hosts)
					}
				}
			}
			if !jitInlinedFnForTest(t, r, inlined, k.caller, k.callee) {
				t.Fatalf("%s's code does not inline its loop's callee: %x", k.caller, inlined[k.caller])
			}
			r.SetContext(ctx)
			_, err := r.Run(compileForTest(t, fmt.Sprintf(call, -5000)))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled script ended with %v", err)
			}
			n := r.global.getOwn(r.atoms.intern("c")).value().Object().getOwn(r.atoms.intern("n")).value().num
			if n-at > 2*backEdgeCheckInterval {
				t.Fatalf("the inlined loop ran %v iterations past its interrupt", n-at)
			}
		})
	}
}
