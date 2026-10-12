//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// jitAgreesForTest runs setup, then each round, an expression, in an
// interpreter and in a runtime that compiles at the first call, and fails at
// the first answer, as a string, that differs; it returns the JIT's runtime.
func jitAgreesForTest(t *testing.T, setup string, rounds ...string) *Runtime {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		src = "''+(" + src + ")"
		wv, werr := want.Run(compileForTest(t, src))
		gv, gerr := r.Run(compileForTest(t, src))
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("round %d (%s): error %v, interpreter's %v", i, src, gerr, werr)
		}
		if werr != nil {
			if got, want := gerr.Error(), werr.Error(); got != want {
				t.Fatalf("round %d (%s): error %q, interpreter's %q", i, src, got, want)
			}
			continue
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d (%s): got %s, interpreter %s", i, src, got, want)
		}
	}
	return r
}

// jitNativeForTest fails unless the global function name has native code
// that was entered.
func jitNativeForTest(t *testing.T, r *Runtime, name string) *jitEntry {
	t.Helper()
	cl := r.global.getOwn(r.atoms.intern(name)).value.Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
		t.Fatalf("%s never ran natively: %+v", name, e)
	}
	return e
}

// A function with try statements is compiled, its handlers known at each
// instruction rather than registered: a throw from a call of Go, from an
// operation Go makes, or from a throw statement is caught by the handler in
// force there, and finally clauses run on every completion -- normal, a
// return, a throw, a break -- with the value each leaves.
func TestJITExceptionHandlers(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function thrower(i) { if (i % 7 === 3) throw new Error('e' + i); return i }
		function caught(n) {
			let s = 0, errs = 0
			for (let i = 0; i < n; i++) {
				try { s += thrower(i) } catch (e) { errs++; s -= 1 }
			}
			return s * 1000 + errs
		}
		function withFinally(n) {
			let s = 0, f = 0
			for (let i = 0; i < n; i++) {
				try { s += i } finally { f++ }
			}
			return s * 1000 + f
		}
		function returnThrough(x) {
			let log = 0
			try { if (x > 2) return x * 2; log = 1 } finally { log += 10 }
			return log
		}
		function nested(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				try {
					try { if (i % 5 === 0) throw i; s += 1 }
					finally { s += 100 }
				} catch (e) { s += 10000 }
			}
			return s
		}
		function breaks(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				try { if (i === 7) break; if (i % 2) continue; s += i } finally { s += 1000 }
			}
			return s
		}
		function uncaught(n) {
			let s = 0
			try { for (let i = 0; i < n; i++) s += thrower(i + 3) } finally { s = -1 }
			return s
		}
		function rethrows(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				try { try { s += thrower(i) } catch (e) { throw e } } catch (e) { s += 0.5 }
			}
			return s
		}
		function operation(o, n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				try { s += o.p.q } catch (e) { s += e instanceof TypeError ? 1 : 1000 }
			}
			return s
		}
		function callsReturnThrough(n) { let s = 0; for (let i = 0; i < n; i++) s += returnThrough(i % 5); return s }
	`
	r := jitAgreesForTest(t, setup,
		`caught(50)`, `caught(50)`, `caught(200)`,
		`withFinally(100)`, `withFinally(100)`,
		`callsReturnThrough(100)`, `callsReturnThrough(100)`,
		`nested(60)`, `nested(60)`,
		`breaks(20)`, `breaks(20)`,
		`(() => { try { return uncaught(10) } catch (e) { return e.message } })()`,
		`(() => { try { return uncaught(10) } catch (e) { return e.message } })()`,
		`rethrows(50)`, `rethrows(50)`,
		`operation({p: {q: 1}}, 50)`, `operation({}, 50)`, `operation({p: {q: 2}}, 50)`,
	)
	for _, name := range []string{"caught", "withFinally", "returnThrough", "nested", "breaks", "rethrows", "operation"} {
		jitNativeForTest(t, r, name)
	}
}

// The handlers in force are Go's to know wherever native code leaves a
// frame: at an exit inside a try, the frame has the try's handlers, and
// outside it none.
func TestJITHandlersAtExits(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	fn := compileForTest(t, `function f(g, n) { let a = 0; for (let i = 0; i < n; i++) { try { a = g(); try { a += g() } finally { a++ } } catch (e) { a = -1 } } return a + g() }`)
	if _, err := r.Run(fn); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value.Object().fn().closure
	p, err := r.jitLower(cl.fn, jitLowerSSA)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Handlers) != 2 || p.HandlerAt == nil {
		t.Fatalf("handlers %+v", p.Handlers)
	}
	depths := map[int]bool{}
	f := &frame{}
	for pc := range cl.fn.Code {
		jitHandlers(f, p, pc)
		depths[len(f.handlers)] = true
		if len(f.handlers) == 2 && (f.handlers[0].isFinally || !f.handlers[1].isFinally) {
			t.Fatalf("pc %d: handlers %+v", pc, f.handlers)
		}
	}
	if !depths[0] || !depths[1] || !depths[2] {
		t.Fatalf("handler depths seen: %v", depths)
	}
}
