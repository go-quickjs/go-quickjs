//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// Native code reads a global's binding at its position in the global
// object's table while the runtime's count of changes to the bindings
// other than appends, and of script-level lexical bindings declared, is
// what it was compiled with (shapeTree's globalEpoch, ssa.GlobalSite's
// Epoch): a value assigned is read as it is; a binding deleted and added
// again, one turned into an accessor, one a later script's let shadows,
// and globals added meanwhile, are each read as the interpreter reads
// them, with the collector idle and marking.
func TestJITGlobalEpoch(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	const setup = `
		// k configurable, as an assignment makes it, which a delete takes and
		// a later script's let may shadow.
		globalThis.k = 3; var step = 1;
		function add(x) { return x + step; }
		function hot(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + add(i) * k) | 0; return s; }
		function addGlobals() { for (var g = 0; g < 50; g++) globalThis['extra' + g] = g; }`
	rounds := []string{
		`hot(300)`, `hot(300)`, `hot(300)`,
		`k = 5, hot(300)`,
		`step = 2, add = function (x) { return x - step; }, hot(300)`,
		`addGlobals(), hot(300)`,
		`delete globalThis.k, globalThis.k = 7, hot(300)`,
		`Object.defineProperty(globalThis, 'step', { get() { return 4; }, configurable: true }), hot(300)`,
		`Object.defineProperty(globalThis, 'step', { value: 6, writable: true, configurable: true }), hot(300)`,
		`hot(300)`,
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprint(marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			both := jitBothForTest(t, setup)
			for range 3 {
				both.agree(t, `hot(300)`)
			}
			r := both.jit
			e := r.jit.hint(r.global.getOwn(r.atoms.intern("hot")).value().Object().fn().closure)
			if e == nil || e.ssa == nil {
				t.Fatalf("hot is not native: %+v", e)
			}
			both.agree(t, rounds...)
			// A later script's lexical binding of a name the code reads as a
			// global, in a runtime whose code Go still enters natively: the code
			// reads the let's.
			lex := jitBothForTest(t, setup)
			for range 3 {
				lex.agree(t, `hot(300)`)
			}
			if e := lex.jit.jit.hint(lex.jit.global.getOwn(lex.jit.atoms.intern("hot")).value().Object().fn().closure); e == nil || e.ssa == nil || e.entrySlow {
				t.Fatalf("hot is not entered natively: %+v", e)
			}
			lex.script(t, `let k = 11;`)
			lex.agree(t, `hot(300)`, `hot(300)`, `k = 13, hot(300)`)
		})
	}
}

// Every way a binding of the global object can change, other than by an
// append, has the count of changes move on (layoutChanged, declareGlobalLex),
// and native code compiled for the binding at its position then reads what
// the interpreter reads: a var an indirect eval declared, configurable, and
// deleted; an eval's function replacing one; an accessor installed by
// __defineGetter__, Reflect.defineProperty and Object.defineProperties and
// a data property again; an attribute changed alone; a binding deleted
// whose name the global object's new prototype has; the global object
// sealed, made non-extensible and frozen; a later script's const and class
// over a global. Each with the collector idle and marking.
func TestJITGlobalEpochPaths(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	const setup = `
		globalThis.k = 3; var step = 1;
		(0, eval)('var ev = 4; function fe(x) { return x + 1 }');
		function hot(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + fe(i) * k + ev + step) | 0; return s; }`
	rounds := []string{
		`ev = 5, hot(300)`,
		`delete globalThis.ev, hot(300)`,
		`(0, eval)('var ev = 6'), hot(300)`,
		`(0, eval)('function fe(x) { return x * 2 }'), hot(300)`,
		`globalThis.__defineGetter__('ev', function () { return 8 }), hot(300)`,
		`Reflect.defineProperty(globalThis, 'ev', { value: 9, writable: true, configurable: true }), hot(300)`,
		`Object.defineProperties(globalThis, { k: { get() { return 10 }, configurable: true } }), hot(300)`,
		`Object.defineProperty(globalThis, 'k', { value: 11, writable: true, enumerable: true, configurable: true }), hot(300)`,
		`Object.defineProperty(globalThis, 'k', { writable: false }), k = 12, hot(300)`,
		`Object.setPrototypeOf(globalThis, Object.create(Object.getPrototypeOf(globalThis), { ev: { value: 13, writable: true } })), delete globalThis.ev, hot(300)`,
	}
	// After the scripts' lexical bindings, which may not shadow a binding
	// that cannot be removed.
	sealed := []string{
		`globalThis.k2 = 14, Object.seal(globalThis), hot(300)`,
		`Object.preventExtensions(globalThis), k = 15, hot(300)`,
		`Object.freeze(globalThis), hot(300)`,
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprint(marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			both := jitBoth{want: New(Config{}), jit: jitRuntimeForTest(t, Config{JIT: true})}
			t.Cleanup(func() { both.want.Close(); both.want.ReleaseClosed() })
			for _, rt := range []*Runtime{both.want, both.jit} {
				rt.SetEvaluator(func(source string, req EvalRequest) (*bytecode.Function, error) {
					prog, err := parser.Parse(source, parser.Options{})
					if err != nil {
						return nil, err
					}
					return compiler.Compile(prog, compiler.Options{Source: "<eval>", Text: source, EvalConfigurable: true, EvalOwnVarScope: true})
				})
			}
			both.script(t, setup)
			native := func() {
				t.Helper()
				for range 3 {
					both.agree(t, `hot(300)`)
				}
				r := both.jit
				if e := r.jit.hint(r.global.getOwn(r.atoms.intern("hot")).value().Object().fn().closure); e == nil || e.ssa == nil {
					t.Fatalf("hot is not native: %+v", e)
				}
			}
			native()
			for _, round := range rounds {
				both.agree(t, round)
				native()
			}
			both.script(t, `const ev = 20;`)
			both.agree(t, `hot(300)`, `hot(300)`)
			both.script(t, `class fe {}`)
			both.agree(t, `hot(300)`, `hot(300)`)
			both.script(t, `fe = function (x) { return x - 1 }`)
			native()
			for _, round := range sealed {
				both.agree(t, round)
				native()
			}
		})
	}
}

// The count of the global object's changes is compared whole: one 2**32
// changes past what the code was compiled with, its low half the same, is
// not taken for it.
func TestJITGlobalEpochWhole(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	both := jitBothForTest(t, `
		globalThis.k = 3;
		function hot(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + i * k) | 0; return s; }`)
	for range 3 {
		both.agree(t, `hot(300)`)
	}
	r := both.jit
	if e := r.jit.hint(r.global.getOwn(r.atoms.intern("hot")).value().Object().fn().closure); e == nil || e.ssa == nil {
		t.Fatalf("hot is not native: %+v", e)
	}
	compiled := r.shapes.globalEpoch
	both.script(t, `delete globalThis.k; globalThis.k = 7;`)
	r.shapes.globalEpoch = compiled + 1<<32
	both.agree(t, `hot(300)`, `hot(300)`)
}

// A realm's global object counts its changes in the runtime's count as
// the first realm's does: native code reading another realm's globals
// reads what the interpreter reads after a binding of either realm is
// deleted, made an accessor or shadowed.
func TestJITGlobalEpochRealms(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	const setup = `
		globalThis.k = 3;
		function hot(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + i * k) | 0; return s; }`
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	rts := []*Runtime{want, r}
	realms := []*Realm{want.NewRealm(), r.NewRealm()}
	agree := func(inRealm bool, srcs ...string) {
		t.Helper()
		for _, src := range srcs {
			var got [2]string
			for i, rt := range rts {
				fn := compileForTest(t, "''+("+src+")")
				var v Value
				var err error
				if inRealm {
					v, err = rt.RunIn(realms[i], fn)
				} else {
					v, err = rt.Run(fn)
				}
				if err != nil {
					got[i] = "error " + err.Error()
				} else {
					got[i] = v.String().Go()
				}
			}
			if got[0] != got[1] {
				t.Fatalf("%s (realm %v): got %s, interpreter %s", src, inRealm, got[1], got[0])
			}
		}
	}
	for i, rt := range rts {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RunIn(realms[i], compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	native := func() {
		t.Helper()
		for range 3 {
			agree(false, `hot(300)`)
			agree(true, `hot(300)`)
		}
		for _, g := range []*Object{r.global, realms[1].global} {
			if e := r.jit.hint(g.getOwn(r.atoms.intern("hot")).value().Object().fn().closure); e == nil || e.ssa == nil {
				t.Fatalf("hot is not native: %+v", e)
			}
		}
	}
	native()
	agree(true, `delete globalThis.k, globalThis.k = 5, hot(300)`)
	agree(false, `hot(300)`)
	native()
	agree(false, `delete globalThis.k, globalThis.k = 7, hot(300)`)
	agree(true, `hot(300)`)
	native()
	agree(true, `Object.defineProperty(globalThis, 'k', { get() { return 9 }, configurable: true }), hot(300)`)
	agree(false, `hot(300)`)
	native()
	for i, rt := range rts {
		if _, err := rt.RunIn(realms[i], compileForTest(t, `let k = 11;`)); err != nil {
			t.Fatal(err)
		}
	}
	agree(true, `hot(300)`, `hot(300)`)
	agree(false, `hot(300)`, `hot(300)`)
}
