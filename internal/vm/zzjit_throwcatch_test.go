//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

const throwCatchSetup = `
	var log = []
	function primitives(n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			try { if (i % 3 == 0) throw i; s += 1 } catch (e) { s = (s + e) | 0 }
		}
		return s
	}
	function values(n) {
		let out = ''
		for (let i = 0; i < n; i++) {
			try {
				switch (i % 4) {
				case 0: throw 'str' + i
				case 1: throw { k: i }
				case 2: throw new RangeError('r' + i)
				default: throw undefined
				}
			} catch (e) {
				out = e === undefined ? 'u' : typeof e == 'string' ? e : e instanceof Error ? e.name + ':' + e.message + ':' + (typeof e.stack) : 'k' + e.k
			}
		}
		return out
	}
	function nested(n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			try {
				try { throw i } catch (e) { s += e; if (i % 5 == 0) throw -1 }
			} catch (e) { s = s * 2 + e }
			s |= 0
		}
		return s
	}
	function noBinding(n) { let c = 0; for (let i = 0; i < n; i++) { try { throw i } catch { c++ } } return c }
	function finallyToo(n) { let s = 0; for (let i = 0; i < n; i++) { try { try { throw i } finally { s++ } } catch (e) { s += e } } return s }
	function rethrows(n) { try { for (let i = 0; i < n; i++) { try { throw i } catch (e) { if (i == n - 1) throw 'last ' + e } } } catch (e) { return e } }
	function uncaught(i) { try { throw i } catch (e) { throw e + 1 } }
	function caller(n) { let s = 0; for (let i = 0; i < n; i++) { try { uncaught(i) } catch (e) { s += e } } return s }
`

// A throw a catch clause of the same function catches, its value the one
// operand above the handler's depth, is a jump to the handler in native
// code, as the tree tier makes it: no exception made, no exit. Each answer
// is the interpreter's -- primitives, strings, objects, an Error with its
// stack, undefined; nested handlers, the inner rethrowing to the outer; a
// catch with no binding; a finally clause between, which Go still throws
// to; a rethrow past every handler; a throw out of a callee -- and the
// loops of the first leave native code at no throw.
func TestJITThrowsCaughtNatively(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	rounds := []string{}
	for range 3 {
		rounds = append(rounds, `primitives(300)`, `values(300)`, `nested(300)`, `noBinding(300)`, `finallyToo(300)`, `rethrows(300)`, `caller(300)`)
	}
	r := jitAgreesForTest(t, throwCatchSetup, rounds...)
	for _, src := range []string{`primitives(3000)`, `nested(3000)`, `noBinding(3000)`} {
		hosts, entries := r.jit.hosts, r.jit.ssaEntries
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
		if r.jit.ssaEntries == entries {
			t.Fatalf("%s ran no native code", src)
		}
		if n := r.jit.hosts - hosts; n > 2 {
			t.Errorf("%s left native code %d times", src, n)
		}
	}
}
