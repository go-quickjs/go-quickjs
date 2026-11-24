//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strconv"
	"testing"
)

// literalSelfSetup builds, in a loop, values each of which holds the one
// its binding held before: an array literal's element (KI-97), an object
// literal's property, Array.of's element and a generator's argument.
// walk follows such a chain from its outermost value, by key at, and
// describes it exactly: how many levels, the first three levels' keys and
// values, a sum of the other values by level, and where an object came
// back that the chain had passed already -- an array that holds itself.
const literalSelfSetup = `
	function walk(x, at) {
		const seen = new Set()
		let n = 0, first = [], sum = 0
		for (;;) {
			if (x === null || typeof x !== 'object') return 'leaf ' + String(x) + ' at ' + n
			if (seen.has(x)) return 'cycle at ' + n + ' of ' + first.join(' ')
			seen.add(x)
			const keys = Object.keys(x)
			if (n < 3) first.push((Array.isArray(x) ? 'A' : 'O') + keys.map(k => k === String(at) ? k + ':*' : k + ':' + x[k]).join(','))
			for (const k of keys) if (k !== String(at)) sum += (n + 1) * Number(x[k])
			if (!keys.includes(String(at))) {
				if (keys.length !== 0) return 'no ' + at + ' at ' + n
				return n + ' levels, ' + first.join(' ') + ', sum ' + sum
			}
			x = x[at]
			n++
		}
	}
	function walkGen(x) {
		const seen = new Set()
		let n = 0
		while (Object.prototype.toString.call(x) === '[object Generator]') {
			if (seen.has(x)) return 'cycle at ' + n
			seen.add(x)
			x = x.next().value
			n++
		}
		return n + ' levels, innermost ' + (Array.isArray(x) ? 'array of ' + x.length : typeof x)
	}
	function* gen(x) { yield x }
	function wrap(d) { let v = []; for (let i = 0; i < d; i++) v = [v]; return v }
	function wrapIndex(d) { let v = []; for (let i = 0; i < d; i++) v = [v, i]; return v }
	function wrapOld(d) { let v = []; for (let i = 0; i < d; i++) { const old = v; v = [old] } return v }
	function wrapSecond(d) { let v = []; for (let i = 0; i < d; i++) v = [1, v]; return v }
	function wrapObject(d) { let v = {}; for (let i = 0; i < d; i++) v = {p: v}; return v }
	function wrapOf(d) { let v = []; for (let i = 0; i < d; i++) v = Array.of(v); return v }
	function wrapGen(d) { let v = []; for (let i = 0; i < d; i++) v = gen(v); return v }
	// many calls f(d) n times, and answers its first answer, walked, and
	// how many of the others differ from it.
	function many(f, at, n, d) {
		const want = walk(f(d), at)
		let bad = 0
		for (let k = 1; k < n; k++) if (walk(f(d), at) !== want) bad++
		return want + '; ' + bad + ' differ'
	}
`

// literalSelfCases are literalSelfSetup's functions, each with the key its
// chain is walked by, or "" for the generators' (walkGen).
var literalSelfCases = []struct{ name, at string }{
	{"wrap", "0"}, {"wrapIndex", "0"}, {"wrapOld", "0"}, {"wrapSecond", "1"},
	{"wrapObject", "'p'"}, {"wrapOf", "0"}, {"wrapGen", ""},
}

// literalSelfWalk is the expression that walks name's chain of d levels.
func literalSelfWalk(name, at string, d int) string {
	if at == "" {
		return "walkGen(" + name + "(" + strconv.Itoa(d) + "))"
	}
	return "walk(" + name + "(" + strconv.Itoa(d) + "), " + at + ")"
}

// literalSelfWant is what literalSelfWalk answers for name's chain of d
// levels, d at least 3: exactly as the language says, which the
// interpreter answers too.
func literalSelfWant(name string, d int) string {
	levels := strconv.Itoa(d) + " levels, "
	switch name {
	case "wrapIndex":
		// The level k from the outside holds d-1-k.
		sum := 0
		for k := range d {
			sum += (k + 1) * (d - 1 - k)
		}
		return levels + fmt.Sprintf("A0:*,1:%d A0:*,1:%d A0:*,1:%d, sum %d", d-1, d-2, d-3, sum)
	case "wrapSecond":
		return levels + fmt.Sprintf("A0:1,1:* A0:1,1:* A0:1,1:*, sum %d", d*(d+1)/2)
	case "wrapObject":
		return levels + "Op:* Op:* Op:*, sum 0"
	case "wrapGen":
		return levels + "innermost array of 0"
	}
	return levels + "A0:* A0:* A0:*, sum 0"
}

// An array literal whose element is what its binding held before -- v =
// [v] in a loop -- holds that, not itself, in native code (KI-97): each
// call's chain is as deep as its loop ran, every level a new array, the
// innermost the first. The literal's array had been written to its keep
// cell before its elements were read, and an element kept in that same
// cell, the array the literal made at the turn before, was read as the
// new array. So are an object literal's, Array.of's and a generator's,
// whose operands are kept alike. Each is compiled at its first call, by
// OSR in a long loop, and at the runtime's own thresholds, with the
// collector idle and while it marks.
func TestJITLiteralHoldsWhatItReplaces(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			t.Run("entry", func(t *testing.T) {
				var rounds []string
				for range 6 {
					for _, c := range literalSelfCases {
						rounds = append(rounds, literalSelfWalk(c.name, c.at, 3), literalSelfWalk(c.name, c.at, 50))
					}
				}
				r := jitAgreesForTest(t, literalSelfSetup, rounds...)
				for _, c := range literalSelfCases {
					jitNativeForTest(t, r, c.name)
					for _, d := range []int{3, 50} {
						if got, want := literalSelfRun(t, r, literalSelfWalk(c.name, c.at, d)), literalSelfWant(c.name, d); got != want {
							t.Errorf("%s(%d): %s, want %s", c.name, d, got, want)
						}
					}
				}
			})
			t.Run("osr", func(t *testing.T) {
				// Called once, at thresholds a call does not reach: the
				// loop's code is entered by OSR.
				r := jitRuntimeForTest(t, Config{JIT: true})
				r.jitCallThreshold, r.jitICThreshold = 1000, 1000
				if _, err := r.Run(compileForTest(t, literalSelfSetup)); err != nil {
					t.Fatal(err)
				}
				osrs := func() uint64 {
					if r.jit == nil {
						return 0
					}
					return r.jit.osrs
				}
				for _, c := range literalSelfCases {
					before := osrs()
					if got, want := literalSelfRun(t, r, literalSelfWalk(c.name, c.at, 20000)), literalSelfWant(c.name, 20000); got != want {
						t.Errorf("%s(20000): %s, want %s", c.name, got, want)
					}
					if osrs() == before {
						t.Errorf("%s(20000): its loop was not entered by OSR", c.name)
					}
				}
			})
			t.Run("defaults", func(t *testing.T) {
				// As a program meets it: 5,000 calls of a chain of 3, then
				// one of 1,000.
				r := jitRuntimeForTest(t, Config{JIT: true})
				jitDefaultsForTest(r)
				if _, err := r.Run(compileForTest(t, literalSelfSetup)); err != nil {
					t.Fatal(err)
				}
				for _, c := range literalSelfCases {
					if c.at == "" {
						continue
					}
					src := "many(" + c.name + ", " + c.at + ", 5000, 3)"
					if got, want := literalSelfRun(t, r, src), literalSelfWant(c.name, 3)+"; 0 differ"; got != want {
						t.Errorf("%s: %s, want %s", src, got, want)
					}
					if got, want := literalSelfRun(t, r, literalSelfWalk(c.name, c.at, 1000)), literalSelfWant(c.name, 1000); got != want {
						t.Errorf("%s(1000): %s, want %s", c.name, got, want)
					}
				}
				for _, run := range []struct{ d, n int }{{3, 5000}, {1000, 20}} {
					src := fmt.Sprintf("(() => { let s, bad = 0; for (let k = 0; k < %d; k++) { const w = %s; if (k === 0) s = w; else if (w !== s) bad++ } return s + '; ' + bad + ' differ' })()", run.n, literalSelfWalk("wrapGen", "", run.d))
					if got, want := literalSelfRun(t, r, src), literalSelfWant("wrapGen", run.d)+"; 0 differ"; got != want {
						t.Errorf("wrapGen(%d) %d times: %s, want %s", run.d, run.n, got, want)
					}
				}
				for _, c := range literalSelfCases {
					jitNativeForTest(t, r, c.name)
				}
			})
		})
	}
}

// literalSelfRun runs src in r and answers its value as a string.
func literalSelfRun(t *testing.T, r *Runtime, src string) string {
	t.Helper()
	v, err := r.Run(compileForTest(t, "''+("+src+")"))
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return v.String().Go()
}
