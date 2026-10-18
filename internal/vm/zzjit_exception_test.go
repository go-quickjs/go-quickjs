//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strings"
	"testing"
)

// jitAgreesForTest runs setup, then each round, an expression, in an
// interpreter and in a runtime that compiles at the first call, the jobs it
// queued run after it, and fails at the first answer, as a string, that
// differs; it returns the JIT's runtime.
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
		if werr == nil {
			werr = want.runMicrotasks()
		}
		gv, gerr := r.Run(compileForTest(t, src))
		if gerr == nil {
			gerr = r.runMicrotasks()
		}
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
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
		t.Fatalf("%s never ran natively: %+v", name, e)
	}
	return e
}

// A function with try statements is compiled, its handlers known at each
// instruction rather than registered: a throw from a call of Go, from an
// operation Go makes, or from a throw statement is caught by the handler in
// force there -- native code going on at the handler, where Go caught it --
// and finally clauses run on every completion -- normal, a return, a
// throw, a break -- with the value each leaves.
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
		function throwsOwn(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				try { if (i % 3 === 0) throw { code: i }; s += 1 } catch (e) { s += e.code }
			}
			return s
		}
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
		`throwsOwn(60)`, `throwsOwn(60)`,
	)
	for _, name := range []string{"caught", "withFinally", "returnThrough", "nested", "breaks", "rethrows", "operation", "throwsOwn"} {
		jitNativeForTest(t, r, name)
	}
	// A throw caught in the function goes on natively at its handler: a
	// run in which every turn's property read throws, or every third's
	// throw statement, goes on natively. (The first run's reads,
	// speculated for an object that has p, are compiled again for one that
	// has not: megamorphic, the read of q off undefined made by Go, which
	// throws, and that of p off the empty object, which the VM gives no
	// shape until a cache asks, by Go the first time -- the interpreter
	// going on to the loop's header, once a run. The read of p off the
	// empty object, which nothing on its chain has, is answered natively
	// once its cache remembers it by the chain's cell, and q's read off
	// undefined then deoptimizes once, and is compiled again megamorphic:
	// the runs until no code is compiled again.)
	for i := 0; i < 4; i++ {
		reoptimized := r.jit.reoptimized
		if _, err := r.Run(compileForTest(t, `operation({}, 100)`)); err != nil {
			t.Fatal(err)
		}
		if i > 0 && r.jit.reoptimized == reoptimized {
			break
		}
	}
	for _, c := range []struct {
		src         string
		interpreted uint64
	}{{`operation({}, 100)`, 1}, {`throwsOwn(100)`, 0}} {
		interpreted := r.jit.interpreted
		cl := r.global.getOwn(r.atoms.intern(c.src[:strings.IndexByte(c.src, '(')])).value().Object().fn().closure
		e := r.jit.hint(cl)
		if e == nil || e.ssa == nil {
			t.Fatalf("%s is not compiled", c.src)
		}
		entries := e.ssaStats.entries
		if _, err := r.Run(compileForTest(t, c.src)); err != nil {
			t.Fatal(err)
		}
		if n := r.jit.interpreted - interpreted; n > c.interpreted || r.jit.hint(cl) != e || e.ssaStats.entries == entries {
			t.Fatalf("%s finished in the interpreter %d times, entered %d times", c.src, n, e.ssaStats.entries-entries)
		}
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
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
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

// A generator's or an async function's body is compiled, and each
// resumption enters native code at its pc: the first after the
// parameters, or the one after the yield or the await it was suspended
// at, with the value sent in. A suspension leaves the frame as the
// interpreter's does, and what a resumption injects -- a throw, a return
// -- is unwound to the body's handlers.
func TestJITGenerators(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function* evens(n) { let s = 0; for (let i = 0; i < n; i++) { s += i; if (i % 2 === 0) yield s } return -s }
		function* echo(n) { let s = 0; for (let i = 0; i < n; i++) { const x = yield i * 2; s += x === undefined ? 1 : x } return s }
		function* guarded(n) {
			let s = 0
			try { for (let i = 0; i < n; i++) { s += i; yield s } }
			finally { log.push('finally ' + s) }
		}
		function* catches(n) {
			let s = 0
			for (let i = 0; i < n; i++) { try { s += yield i } catch (e) { s += 1000 } }
			return s
		}
		var log = []
		function sumOf(g) { let s = 0; for (const v of g) s += v; return s }
		function drive(n) { const g = echo(n); let r = g.next(), s = 0; while (!r.done) { s += r.value; r = g.next(r.value + 1) } return s * 1000 + r.value }
		function injected(n) {
			const g = catches(n); g.next(); let s = 0
			for (let i = 0; i < n; i++) { const r = i % 3 ? g.next(i) : g.throw(new Error('x')); if (!r.done) s += r.value }
			return s + ':' + g.return(7).value
		}
		function returned(n) { const g = guarded(n); let s = 0; for (let i = 0; i < n / 2; i++) s += g.next().value; g.return(); return s + ' ' + log.join() }
		async function adds(n) { let s = 0; for (let i = 0; i < n; i++) s += await i; return s }
		async function fails(n) { let s = 0; for (let i = 0; i < n; i++) { try { s += await (i % 4 ? i : Promise.reject(i)) } catch (e) { s -= e } } return s }
		var out
	`
	r := jitAgreesForTest(t, setup,
		`sumOf(evens(40))`, `sumOf(evens(40))`, `[...evens(30)].join()`,
		`drive(50)`, `drive(50)`,
		`injected(30)`, `injected(30)`,
		`returned(20)`, `returned(20)`,
		`(adds(30).then(v => out = v), 0)`, `out`, `(adds(40).then(v => out = v), 0)`, `out`,
		`(fails(30).then(v => out = v), 0)`, `out`, `(fails(30).then(v => out = v), 0)`, `out`,
	)
	for _, name := range []string{"evens", "echo", "catches", "adds", "fails"} {
		// Entered at each resumption, not once.
		if e := jitNativeForTest(t, r, name); e.ssaStats.entries < 30 {
			t.Fatalf("%s entered natively %d times", name, e.ssaStats.entries)
		}
	}
	if r.frameDepth != 0 {
		t.Fatalf("left %d frames", r.frameDepth)
	}
}

// A scope with using declarations is a try statement whose finally clause
// disposes of what it bound: its resources are Go's to add and dispose of
// at host exits, and native code goes on after each. Disposal runs on every
// way out of the scope, last resource first, an error from a disposer
// folded into the exception in flight (SuppressedError).
func TestJITUsing(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var log = []
		function res(name, fail) { return { [Symbol.dispose]: makeDispose(name, fail) } }
		function makeDispose(name, fail) { return function () { log.push(name); if (fail) throw new Error('dispose ' + name) } }
		function scoped(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				using a = res('a' + i), b = res('b' + i)
				s += i
				if (i === 5) break
			}
			return s
		}
		function early(x) { using r = res('r' + x); if (x > 1) return x * 10; return -1 }
		function callsEarly(n) { let s = 0; for (let i = 0; i < n; i++) s += early(i % 3); return s }
		function throwsIn(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				try { using r = res('t' + i); if (i % 2) throw new Error('body ' + i); s++ }
				catch (e) { s += 100 }
			}
			return s
		}
		function failing(n) {
			let msg = ''
			for (let i = 0; i < n; i++) {
				try { using r = res('f' + i, true); if (i === 1) throw new Error('body') }
				catch (e) { msg += e.constructor.name + ':' + e.message + (e.suppressed ? '/' + e.suppressed.message : '') + ';' }
			}
			return msg
		}
		function take() { const l = log.join(); log = []; return l }
	`
	r := jitAgreesForTest(t, setup,
		`scoped(10) + take()`, `scoped(10) + take()`,
		`callsEarly(12) + take()`, `callsEarly(12) + take()`,
		`throwsIn(10) + take()`, `throwsIn(10) + take()`,
		`failing(3) + take()`, `failing(3) + take()`,
	)
	for _, name := range []string{"scoped", "throwsIn", "failing"} {
		jitNativeForTest(t, r, name)
	}
	// A scope's resources are added and disposed of at exits native code
	// goes on after: scoped's runs, which throw nothing, never finish in
	// the interpreter.
	interpreted := r.jit.interpreted
	if _, err := r.Run(compileForTest(t, `scoped(5)`)); err != nil {
		t.Fatal(err)
	}
	if r.jit.interpreted != interpreted {
		t.Fatalf("scoped finished in the interpreter %d times", r.jit.interpreted-interpreted)
	}
}

// A for...of or for...in loop's cursor is Go's: native code leaves at each
// step, and goes on past it with the value, or at the loop's end. Its
// iterator is closed however the loop is left -- break, return, a throw
// caught in the function or leaving it -- as the interpreter closes it.
func TestJITForOf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var log = []
		function counter(n, failAt) {
			let i = 0
			return { [Symbol.iterator]() { return this },
				next() { if (i === failAt) throw new Error('next ' + i); return i < n ? { value: i++, done: false } : { value: undefined, done: true } },
				return() { log.push('closed@' + i); return {} } }
		}
		function sumArray(a) { let s = 0; for (const x of a) s += x; return s }
		function sumMap(m) { let s = 0; for (const [k, v] of m) s += v; return s }
		function sumKeys(o) { let s = ''; for (const k in o) s += k + o[k]; return s }
		function breaks(n) { let s = 0; for (const x of counter(n)) { if (x === 4) break; s += x } return s }
		function returns(n) { for (const x of counter(n)) { if (x === 3) return x * 100 } return -1 }
		function nested(n) { let s = 0; for (const x of counter(n)) for (const y of counter(3)) { if (y === 2) break; s += x * y } return s }
		function throwsOut(n) { for (const x of counter(n)) { if (x === 2) throw new Error('out ' + x) } }
		function caughtIn(n) { let s = 0; for (let k = 0; k < n; k++) { try { for (const x of counter(5)) { if (x === k % 4) throw x; s++ } } catch (e) { s += 100 } } return s }
		function nextFails(n) { try { let s = 0; for (const x of counter(10, n)) s += x; return s } catch (e) { return e.message } }
		function* gen(n) { for (const x of counter(n)) { yield x * 2; if (x === 3) return } }
		function take() { const l = log.join(); log = []; return l }
	`
	r := jitAgreesForTest(t, setup,
		`sumArray([1,2,3,4,5])`, `sumArray([1,2,3,4,5,6,7])`, `sumArray([1.5, 'x', 3])`,
		`sumMap(new Map([[1,10],[2,20]]))`, `sumMap(new Map([[1,10],[2,20],[3,30]]))`,
		`sumKeys({a: 1, b: 2})`, `sumKeys({a: 1, b: 2, c: 3})`,
		`breaks(10) + take()`, `breaks(10) + take()`,
		`returns(10) + take()`, `returns(10) + take()`,
		`nested(5) + take()`, `nested(5) + take()`,
		`(() => { try { throwsOut(5) } catch (e) { return e.message } })() + take()`,
		`(() => { try { throwsOut(5) } catch (e) { return e.message } })() + take()`,
		`caughtIn(12) + take()`, `caughtIn(12) + take()`,
		`nextFails(3) + take()`, `nextFails(3) + take()`,
		`[...gen(10)].join() + take()`, `[...gen(10)].join() + take()`,
	)
	for _, name := range []string{"sumArray", "sumKeys", "breaks", "returns", "nested", "caughtIn", "gen"} {
		jitNativeForTest(t, r, name)
	}
}

// yield* is compiled: its delegate's steps are Go's (iter_resume,
// iter_unpack_delegate), its suspension as yield's, and its resumption,
// which sends a value and its kind, enters native code after it. What
// the outer generator is sent, thrown or asked to return goes to the
// delegate, and the delegate's return value is yield*'s.
func TestJITYieldStar(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var log = []
		function* inner(n) { let got = 0; try { for (let i = 0; i < n; i++) got += (yield i) || 0 } finally { log.push('inner done ' + got) } return got }
		function* outer(n) { let s = 0; for (let k = 0; k < n; k++) s += yield* inner(3); return s }
		function* overArray(n) { for (let k = 0; k < n; k++) yield* [k, k * 10] }
		function sumOf(g) { let s = 0; for (const v of g) s += v; return s }
		function driven(n) { const g = outer(n); let r = g.next(), s = 0; while (!r.done) { s += r.value; r = g.next(r.value + 1) } return s + ':' + r.value }
		function thrown(n) { const g = outer(n); g.next(); g.next(); try { g.throw(new Error('x')) } catch (e) { return e.message + ' ' + log.join() } return 'no' }
		function returned(n) { const g = outer(n); g.next(); g.next(); const r = g.return(42); return r.value + ' ' + r.done + ' ' + log.join() }
		function take() { const l = log.join(); log = []; return l }
	`
	r := jitAgreesForTest(t, setup,
		`sumOf(outer(10)) + take()`, `sumOf(outer(10)) + take()`,
		`sumOf(overArray(20))`, `sumOf(overArray(20))`,
		`driven(8) + take()`, `driven(8) + take()`,
		`thrown(5) + take()`, `thrown(5) + take()`,
		`returned(5) + take()`, `returned(5) + take()`,
	)
	for _, name := range []string{"outer", "overArray"} {
		// Entered at each resumption, not once.
		if e := jitNativeForTest(t, r, name); e.ssaStats.entries < 30 {
			t.Fatalf("%s entered natively %d times", name, e.ssaStats.entries)
		}
	}
}

// An await using scope's disposal is compiled: each step is Go's, which
// runs the disposers until one leaves a value to await -- awaited natively
// as any await -- or none is left, when native code goes on where the
// disposal ends.
func TestJITAwaitUsing(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var log = []
		function res(name, fail) { return { [Symbol.asyncDispose]: makeDispose(name, fail) } }
		function syncRes(name) { return { [Symbol.dispose]: makeDispose(name) } }
		function makeDispose(name, fail) { return function () { log.push(name); if (fail) return Promise.reject(new Error('dispose ' + name)); return Promise.resolve() } }
		async function scoped(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				await using a = res('a' + i), b = syncRes('b' + i)
				s += i
			}
			return s
		}
		async function failing(n) {
			let msg = ''
			for (let i = 0; i < n; i++) {
				try { await using r = res('f' + i, i % 2 === 1); if (i === 2) throw new Error('body') }
				catch (e) { msg += e.constructor.name + ':' + e.message + ';' }
			}
			return msg
		}
		function take() { const l = log.join(); log = []; return l }
		var out
	`
	r := jitAgreesForTest(t, setup,
		`(scoped(6).then(v => out = v + take()), 0)`, `out`, `(scoped(6).then(v => out = v + take()), 0)`, `out`,
		`(failing(5).then(v => out = v + take()), 0)`, `out`, `(failing(5).then(v => out = v + take()), 0)`, `out`,
	)
	for _, name := range []string{"scoped", "failing"} {
		jitNativeForTest(t, r, name)
	}
}
