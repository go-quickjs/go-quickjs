//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
)

// jitTypeofSrc makes typeof's answer, kept (ssa.OpTypeofCell), and
// compares it with each type's name and a name of none (ir.TypeTest), in a
// loop over values of every kind.
const jitTypeofSrc = `
	function kinds(vals, n) {
		let c = 0, s = 0
		for (let i = 0; i < n; i++) {
			const v = vals[i % vals.length]
			const t = typeof v
			if (t === 'number') c += 1
			else if (t === 'string') c += 2
			else if (t === 'object') c += 3
			else if (t === 'function') c += 4
			else if (t === 'undefined') c += 5
			else if (t === 'boolean') c += 6
			else if (t === 'symbol') c += 7
			else if (t === 'bigint') c += 8
			if (typeof v === 'object') s += 1
			if (typeof v !== 'function') s += 2
			if (typeof v == 'symbol') s += 4
			if (typeof v === 'bigint') s += 8
			if (typeof v === 'strnig') s += 16
			if (typeof v != 'undefined') s += 32
			if (typeof v !== 'nope') s += 64
		}
		return c + ':' + s
	}
	function names(vals) {
		let s = ''
		for (let i = 0; i < vals.length; i++) s += typeof vals[i] + ','
		return s
	}
	var plain = [1, -0, NaN, 'a', '', null, undefined, true, false, {}, [], function () {}, () => 2, class {},
		Math.max, Math.max.bind(null), Symbol('s'), 1n, 2n ** 80n, -(2n ** 70n), new Date(0), /r/, new String('s'), dda]
	var proxies = [new Proxy(function () {}, {}), new Proxy({}, {}), 1, new Proxy(class {}, {})]
`

// typeof's answer is made natively, loaded from the runtime's table of
// the eight names by the value's word, its flags and its class
// (ssa.OpTypeofCell), and compared with a name it tells natively too
// (ir.TypeTest), object, symbol and bigint among them and a name of no
// type false: a loop over values of every kind, document.all's among them,
// calls no Go and leaves no native code, whether the collector marks or
// not. A proxy, a function where its target is, is Go's to tell, from
// native code. Each answer is the interpreter's.
func TestJITTypeofNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			runtime.GC()
			defer debug.SetGCPercent(debug.SetGCPercent(-1))
			want := New(Config{})
			defer func() { want.Close(); want.ReleaseClosed() }()
			r := jitRuntimeForTest(t, Config{JIT: true})
			jitDefaultsForTest(r)
			for _, rt := range []*Runtime{want, r} {
				dda := rt.NewObject()
				dda.MarkHTMLDDA()
				rt.global.setOwnRaw(rt.atoms.intern("dda"), Obj(dda), propDefault)
				if _, err := rt.Run(compileForTest(t, jitTypeofSrc)); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			run := func(call string) {
				t.Helper()
				wv, err := want.Run(compileForTest(t, call))
				if err != nil {
					t.Fatal(err)
				}
				jitGoCallWork = func() { calls++ }
				gv, err := r.Run(compileForTest(t, call))
				jitGoCallWork = nil
				if err != nil {
					t.Fatal(err)
				}
				if got, want := gv.String().Go(), wv.String().Go(); got != want {
					t.Fatalf("%s: got %q, interpreter %q", call, got, want)
				}
			}
			for range 3 {
				run(`kinds(plain, 2400)`)
				run(`names(plain)`)
			}
			hosts, interpreted, entries := r.jit.hosts, r.jit.interpreted, r.jit.ssaEntries
			// The calls of Go a run makes are its result's string's and the
			// loop's polls at its back edge (ssa.GoPoll), a few a thousand
			// steps: none of its 24,000 steps' eight typeofs and seven
			// tests calls Go.
			calls = 0
			run(`kinds(plain, 2400)`)
			once := calls
			calls = 0
			run(`kinds(plain, 24000)`)
			if h, i := r.jit.hosts-hosts, r.jit.interpreted-interpreted; h != 0 || i != 0 || calls-once > 240 {
				t.Fatalf("typeof of every kind: %d exits to Go, %d finished in the interpreter, %d calls of Go for ten times %d",
					h, i, calls, once)
			}
			if r.jit.ssaEntries == entries {
				t.Fatal("kinds and names never ran natively")
			}
			for range 3 {
				run(`names(proxies)`)
				run(`kinds(proxies, 200)`)
			}
			run(`kinds(plain, 2400)`)
		})
	}
}
