//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// forwardingSetup's classes are made as Box2DWeb makes its own: each
// constructor hands its arguments to its class's static initializer,
// A.init.apply(this, arguments), and, for an instance of the class itself
// and not of a subclass, to the prototype's, this.constructor === V &&
// this.b2Vec2.apply(this, arguments) -- arguments used for nothing else, so
// that no arguments object is made (bytecode's ForwardsArguments). count
// counts the constructions an initializer finishes, which an exit after
// one of them must not count again.
const forwardingSetup = `
	var stats = { n: 0 }
	var B = (function () {
		function A() { A.init.apply(this, arguments) }
		A.init = function (x) { this.lower = x; stats.n++ }
		function V() { V.b2Vec2.apply(this, arguments); this.constructor === V && this.b2Vec2.apply(this, arguments) }
		V.b2Vec2 = function () { this.x = 0; this.y = 0; stats.n++ }
		V.prototype.b2Vec2 = function (x, y) { this.x = x; this.y = y === undefined ? -1 : y }
		V.prototype.len = function () { return this.x + this.y }
		function W() { W.b2Vec3.apply(this, arguments); this.constructor === W && this.b2Vec3.apply(this, arguments) }
		W.prototype = Object.create(V.prototype)
		W.prototype.constructor = W
		W.b2Vec3 = function () { V.b2Vec2.apply(this, arguments); this.z = 0 }
		W.prototype.b2Vec3 = function (x, y, z) { this.x = x; this.y = y; this.z = z }
		return { A: A, V: V, W: W }
	})()
	function build(n, k) {
		let s = 0, v
		for (let i = 0; i < n; i++) {
			v = new B.V(i, k)
			const a = new B.A(i * 2)
			const w = new B.W(i, 1, 2)
			s = (s + v.x + v.y + a.lower + w.x + w.y + w.z) | 0
		}
		return [s, v.len(), new B.V(7).y, new B.V().x, new B.V(1, 2, 3).y, stats.n].join()
	}
	var odd = { valueOf() { stats.n += 1000; return 5 } }
	function throwing() { try { return new B.V(odd, Symbol()).len() } catch (e) { return e.constructor.name + ': ' + stats.n } }
`

// A construction of a constructor that only forwards its arguments, as
// Box2DWeb's do -- twice, the second time behind a test, the method found
// on the constructor itself or on the prototype -- is inlined as V8
// inlines it, each forward the call of the method, inlined with the
// construction's arguments, and no arguments object made: its frame, at an
// exit inside it, has the construction's arguments (abi.Context's
// InlineArgs), so that Go goes on after the forward the exit was past
// rather than make the construction over again. Each answer is the
// interpreter's: a construction counted once whatever leaves; fewer
// arguments than the method's parameters, and more; a subclass's
// construction, whose test fails; a method replaced; an argument a method
// converts, which runs script.
func TestJITForwardingConstructors(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	// Each class constructed once, as a program's setup does, before
	// build's loop is compiled: a forwarding constructor is inlined once
	// its code has run (jitHasRun).
	steady := []string{`(new B.V(1, 2), new B.A(1), new B.W(1, 2, 3), 0)`}
	for range 6 {
		steady = append(steady, `build(300, 1)`)
	}
	rounds := append(slices.Clone(steady),
		// A string where the methods met numbers: their code leaves after
		// the construction's first forward counted it.
		`build(300, 'x')`, `build(300, 1)`,
		`throwing()`,
		`(B.V.prototype.b2Vec2 = function (x, y) { this.x = -x; this.y = y }, build(300, 2))`,
		`(B.V.b2Vec2 = function () { this.x = 9; stats.n += 2 }, build(300, 2))`,
		`build(300, 1)`)
	jitAgreesForTest(t, forwardingSetup, rounds...)
	// Steady, native code makes the constructions itself.
	r := jitAgreesForTest(t, forwardingSetup, steady...)
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == "build" {
			e = x
		}
	}
	if e == nil || e.ssa == nil {
		t.Fatalf("build has no native code: %+v", e)
	}
	inlined := 0
	for _, x := range e.inlines {
		if x.pool != nil && x.cl != nil && x.cl.fn.ForwardsArguments {
			inlined++
		}
	}
	if inlined < 3 {
		t.Errorf("build inlines %d forwarding constructions of 3: %+v", inlined, e.inlines)
	}
	// It leaves native code only to fill the pools again, and Go makes
	// none of its constructions, nor runs a constructor native code called
	// (abi.ExitEnter).
	hosts, calls, entered, entries := e.ssaStats.hosts, e.ssaStats.goCalls, r.jit.entered, e.ssaStats.entries
	if _, err := r.Run(compileForTest(t, `build(300, 1)`)); err != nil {
		t.Fatal(err)
	}
	if e.ssaStats.entries == entries {
		t.Fatal("build ran no native code")
	}
	if n := e.ssaStats.hosts - hosts; n > 3*300/abi.PoolSize+4 {
		t.Errorf("build left native code %d times for 900 constructions", n)
	}
	if n := e.ssaStats.goCalls - calls; n > 4 {
		t.Errorf("Go made %d of build's 900 constructions", n)
	}
	if n := r.jit.entered - entered; n > 4 {
		t.Errorf("Go ran %d of build's 900 constructors", n)
	}
}
