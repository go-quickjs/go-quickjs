package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestPureStoreRefusalIsNotAMiss pins that a storing pure body called where
// its caller could still give up -- so that its store is refused -- is not
// counted as missing its fast paths: called directly, where it may store, it
// is still answered without a frame. Richards's markAsSuspended, called from
// suspendCurrent, lost its frameless calls everywhere when it was.
func TestPureStoreRefusalIsNotAMiss(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function T() { this.state = 0 }
		T.prototype.mark = function () { this.state = this.state | 2 };
		function S(t) { this.cur = t }
		S.prototype.suspend = function () { this.cur.mark(); return this.cur };
		var t = new T(), s = new S(t);
		for (var i = 0; i < 200; i++) s.suspend();
		for (var i = 0; i < 10; i++) t.mark();
		T.prototype.mark`))
	if err != nil {
		t.Fatal(err)
	}
	cl := v.Object().fn().closure
	// The first call misses once, before the store's cache is filled.
	if cl.pureMiss > 2 {
		t.Errorf("mark missed %d times; a refused store should not count", cl.pureMiss)
	}
}

// TestPureRefusalIsTheCallersMiss pins whose miss a refused store is when it
// is two calls down: not the storing body's, nor that of the body between,
// which was itself called where it could not let anything store, but the
// miss of the body that made that call -- which is the one that can never
// be answered without a frame, and so the one that should stop trying.
func TestPureRefusalIsTheCallersMiss(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function T() { this.state = 0; this.list = [] }
		T.prototype.mark = function () { this.state = this.state | 2 };
		T.prototype.add = function (x) { this.list.push(x) };
		function M(t) { this.t = t }
		M.prototype.markIt = function () { this.t.mark() };
		M.prototype.addIt = function (x) { this.t.add(x) };
		function S(m) { this.m = m }
		S.prototype.both = function () { this.m.markIt(); this.m.addIt(1); return this.m };
		var t = new T(), m = new M(t), s = new S(m);
		for (var i = 0; i < 200; i++) s.both();
		for (var i = 0; i < 10; i++) { m.markIt(); m.addIt(2) }
		[T.prototype.mark, T.prototype.add, M.prototype.markIt, M.prototype.addIt, S.prototype.both]`))
	if err != nil {
		t.Fatal(err)
	}
	fns := v.Object().elems
	for i, name := range []string{"mark", "add", "markIt", "addIt"} {
		if n := fns[i].Object().fn().closure.pureMiss; n > 2 {
			t.Errorf("%s missed %d times; a refusal is not its miss", name, n)
		}
	}
	if n := fns[4].Object().fn().closure.pureMiss; n < pureMissLimit {
		t.Errorf("both missed %d times; its calls are what cannot store", n)
	}
}

// TestCompiledCallCounts pins that the count of calls the compiler keeps,
// which the tree tier's gate reads, is the count of call, call_method and
// new instructions in each function's code.
func TestCompiledCallCounts(t *testing.T) {
	fn := compileForTest(t, `
		function a(x) { return f(x) + o.m(x, 1) + new C(x) + g?.(x) + o?.m(x) + tag`+"`t${x}`"+` }
		function b(...xs) { return f(...xs) + o.m(...xs) + new C(...xs) + Math.max.apply(null, xs) }
		class D extends C { constructor() { super(1); this.x = super.m(2) } get y() { return h() } }
		var arrow = (p) => p.q(p.r(), [1, 2].map(z => z * 2));
		function c() { if (k) return f(); else { try { g() } finally { h() } } return eval("1") }`)
	var walk func(f *bytecode.Function)
	walk = func(f *bytecode.Function) {
		n := 0
		for _, in := range f.Code {
			switch in.Op {
			case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
				n++
			}
		}
		if int(f.Calls) != n {
			t.Errorf("%q: Calls is %d, the code has %d", f.Name, f.Calls, n)
		}
		for _, k := range f.Constants {
			if k.Fn != nil {
				walk(k.Fn)
			}
		}
	}
	walk(fn)
}
