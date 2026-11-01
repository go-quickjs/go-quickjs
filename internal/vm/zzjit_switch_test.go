//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
)

// jitSwitchSrc has switches of string, number and mixed literal cases, and
// strict equalities with constants, in loops over keys of every kind.
const jitSwitchSrc = `
	function strs(keys, n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			switch (keys[i % keys.length]) {
			case 'a': s += 1; break
			case 'div': s += 2; break
			case 'p': s += 3; break
			case 'span': s += 4; break
			case 'button': s += 5; break
			case 'tr': s += 6; break
			case 'td': s += 7
			case 'li': s += 8; break
			case '': s += 9; break
			case 'section': s += 10; break
			case 'a': s += 1000; break
			case 'todos/todosLoaded': s += 11; break
			case 'filters/colorFilterChanged': s += 12; break
			case 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxyyyyyyyyyyyyyyyyyyyyyyyy': s += 13; break
			default: s += 100
			}
		}
		return s
	}
	function nums(keys, n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			switch (keys[i % keys.length]) {
			case 1: s += 1; break
			case 2: s += 2
			case 0: s += 3; break
			case -0: s += 1000; break
			case 1.5: s += 4; break
			case 16: s += 5; break
			case true: s += 6; break
			case null: s += 7; break
			case undefined: s += 8; break
			default: s += 100
			}
		}
		return s
	}
	function eqs(keys, n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			const x = keys[i % keys.length]
			if (x === 'section') s += 1
			if (x !== 'a') s += 2
			if ('td' === x) s += 4
			if (x === 1.5) s += 8
			if (x === 0) s += 16
			if (x !== true) s += 32
			if (x === NaN) s += 64
			if (x === '') s += 128
		}
		return s
	}
	var plain = ['a', 'div', 'td', 'section', 'nope', '', 'todos/todosLoaded', 'filters/colorFilterChanged',
		'section?'.slice(0, 7), ['t', 'd'].join(''), 'été', 'sectioN', 'todos/todosLoadeD',
		1, 2, 0, -0, 1.5, 16, NaN, '1', true, false, null, undefined, {}, [], 1n]
	var ropes = ['x'.repeat(40) + 'y'.repeat(24), 'x'.repeat(40) + 'y'.repeat(23) + 'z', 'sec' + 'tion'.repeat(20), 'a']
`

// A switch's cases and a strict equality with a constant compare natively
// without a call of Go: a number, a boolean, null or undefined by its
// word (+0 and -0 alike, NaN never), an ASCII string constant by the
// string's length and then its bytes as immediates (mir's strictConst).
// A rope of a constant's length is Go's (ssa.GoEqual). Each answer is the
// interpreter's, the collector idle and marking.
func TestJITSwitchCompares(t *testing.T) {
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
				if _, err := rt.Run(compileForTest(t, jitSwitchSrc)); err != nil {
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
			const all = `[strs(plain, 2800), nums(plain, 2800), eqs(plain, 2800)].join()`
			for range 3 {
				run(all)
			}
			hosts, interpreted, entries := r.jit.hosts, r.jit.interpreted, r.jit.ssaEntries
			// The calls of Go a run makes are the loops' polls at their back
			// edges, a few a thousand steps, and its result's join: none of
			// the 28,000 steps' comparisons calls Go.
			calls = 0
			run(all)
			once := calls
			calls = 0
			run(`[strs(plain, 28000), nums(plain, 28000), eqs(plain, 28000)].join()`)
			if h, i := r.jit.hosts-hosts, r.jit.interpreted-interpreted; h != 0 || i != 0 || calls-once > 3*280 {
				t.Fatalf("switches and equalities: %d exits to Go, %d finished in the interpreter, %d calls of Go for ten times %d",
					h, i, calls, once)
			}
			if r.jit.ssaEntries == entries {
				t.Fatal("strs, nums and eqs never ran natively")
			}
			for range 3 {
				run(`[strs(ropes, 40), eqs(ropes, 40)].join()`)
			}
			run(all)
		})
	}
}
