//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regTailClosSetup's closures reach the loop that calls them through calls
// in tail position (tail_call_w, tail_call_method_w), as SOM's Set and
// IdentitySet give theirs (AWFY's Havlak): Bag.contains gives its arrow to
// its own hasSome by a tail call, which gives it to the Vec's hasSome by
// another; Link.relink's arrow is a call in tail position itself, its
// concise body. Each tail call is inlined as the ordinary call it is while
// the stack is shallow, and the closure, then given to nothing else, is
// inlined in the loop and never made. Holes, throws and stack traces leave
// native code inside the inlined levels, where Go makes their frames, each
// the function its caller's tail call called -- viaTail's boom, a sloppy
// function, its receiver the global object in the trace; deep in the stack
// the tail call gives its frame up, which native code leaves to the
// interpreter. The runtimes start with the contexts every runtime has: the
// first exit at an inlined frame's check of the contexts past it, too few,
// has more made, and the calls stay inlined.
const regTailClosSetup = `
	class Vec {
		constructor(n) { this.storage = []; this.firstIdx = 0; this.lastIdx = 0; for (let i = 0; i < n; i++) this.append(i) }
		append(e) { this.storage[this.lastIdx] = e; this.lastIdx += 1 }
		forEach(fn) { for (let i = this.firstIdx; i < this.lastIdx; i += 1) { fn(this.storage[i]) } }
		hasSome(fn) { for (let i = this.firstIdx; i < this.lastIdx; i += 1) { if (fn(this.storage[i])) { return true } } return false }
	}
	class Bag {
		constructor(n) { this.items = new Vec(n) }
		hasSome(fn) { return this.items.hasSome(fn) }
		contains(x) { return this.hasSome((e) => e === x) }
		throwsAt(x) { return this.hasSome((e) => { if (e === x) throw new RangeError('at ' + e); return false }) }
		stackAt(x) {
			let st = 'none'
			this.hasSome((e) => { if (e === x) st = new Error('h').stack.split('\n').slice(1, 5).join('|'); return false })
			return st
		}
	}
	class Link {
		constructor(i) { this.parent = this; this.v = i }
		union(p) { this.parent = p }
		relink(v) { v.forEach((n) => n.union(this.parent)); return this.v }
	}
	var bag = new Bag(12), holey = new Bag(12)
	delete holey.items.storage[7]
	var links = new Vec(0)
	for (let i = 0; i < 6; i++) links.append(new Link(i))
	var root = new Link(100)
	function has(b, x) { let n = 0; for (let i = 0; i < 20; i++) if (b.contains((x + i) % 15)) n++; return n }
	function relinks(n) { let s = 0; for (let i = 0; i < n; i++) s += root.relink(links); return s + ':' + links.storage[3].parent.v }
	function tryThrows(b, x) { try { return b.throwsAt(x) } catch (e) { return e.message } }
	function deep(n, b, x) { if (n === 0) return has(b, x); return 0 + deep(n - 1, b, x) }
	function deepStack(n, b, x) { if (n === 0) return b.stackAt(x); return '' + deepStack(n - 1, b, x) }
	function boom(s) { if (s === -9) return new Error('b').stack.split('\n').slice(1, 3).join('|'); return s + 1 }
	function viaTail(s) { 'use strict'; return boom(s) }
	function tails(n, at) { let r = 0; for (let i = 0; i < n; i++) r = viaTail(i % 10 === at ? -9 : i); return r }
`

// regTailClosRounds are regTailClosSetup's rounds: warm ones, whose
// closures nothing leaves, then ones with exits inside the inlined levels
// and calls deep in the stack.
func regTailClosRounds() []string {
	loop := func(call string) string {
		return "(() => { let r; for (let i = 0; i < 300; i++) r = " + call + "; return r })()"
	}
	var rounds []string
	for range 12 {
		rounds = append(rounds, loop("has(bag, i % 13)"), "relinks(300)", loop("tryThrows(bag, 99)"), loop("bag.stackAt(99)"), "tails(300, -1)")
	}
	for range 4 {
		rounds = append(rounds, loop("has(holey, i % 13)"), loop("tryThrows(bag, i % 9)"), loop("bag.stackAt(i % 9)"),
			loop("tryThrows(holey, 8)"), "deep(40, bag, 3) + ':' + deep(40, holey, 6)", "deepStack(40, bag, 4)", "deepStack(3, holey, 9)", "tails(300, 9)")
	}
	return rounds
}

// TestJITRegTailCalledClosures runs regTailClosSetup's rounds in register
// code natively and with no JIT, the collector idle and marking, and
// compares every answer; the closures given on by calls in tail position
// are inlined and not made, and making contains' answer allocates nothing;
// stackAt's, whose closure takes a stack trace, is made where it does.
func TestJITRegTailCalledClosures(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			// The closures' makers compiled as functions of their own, not
			// inlined in their callers' loops: a closure an inlined callee
			// makes is made (CalledClosure, the function's own code alone).
			jitTestSwitch(t, func(jc *jitTestConfig) {
				jc.noInline = func(fn *bytecode.Function) bool {
					return fn.Name == "contains" || fn.Name == "relink" || fn.Name == "throwsAt" || fn.Name == "stackAt" || fn.Name == "viaTail"
				}
			})
			rts := regNativeRuntimes(t, regTailClosSetup)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			rounds := regTailClosRounds()
			warm := 60
			run := func(src string) {
				prog := compileForTest(t, src)
				var got [2]string
				for k, r := range rts {
					v, err := r.Run(prog)
					got[k] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("%s: with the JIT %q, without %q", src, got[0], got[1])
				}
			}
			for i, round := range rounds {
				run(round)
				if i != warm-1 {
					continue
				}
				// What the code compiled last for each function made of
				// the closures it calls, its own or inlined in a caller's.
				called := map[string]int{}
				for key, e := range r.jit.regCache {
					if fn := key.Value(); fn != nil && e != nil && e.keep != nil {
						n := e.keep.called + e.keep.unmade
						called[fn.Name] += n
						for _, cl := range e.keep.inlined {
							if cl != nil && cl.fn != nil && cl.fn.Name != fn.Name {
								called[cl.fn.Name] += n
							}
						}
					}
				}
				for _, name := range []string{"contains", "relink", "throwsAt"} {
					if called[name] == 0 {
						t.Errorf("%s makes the closure it gives on: %v", name, called)
					}
				}
				if marking {
					continue
				}
				// contains' closure and the cell of the x it captures,
				// made at each of its 20,000 calls where it is not
				// inlined: none, what the script's own compile takes
				// apart.
				release := holdCollectorNow(t)
				prog := compileForTest(t, loop1000("has(bag, i % 13)"))
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				v, err := r.Run(prog)
				runtime.ReadMemStats(&after)
				release()
				if got := regNativeShow(r, v, err); got != "14" {
					t.Errorf("has(bag, i %% 13) = %q, want 14", got)
				}
				// Counted where the build allocates as an ordinary one does:
				// checkptr=2 moves to the heap every variable whose address
				// becomes an unsafe.Pointer, 8,076 for these calls where an
				// ordinary build makes under 2,000 (allocsOrdinary).
				if n := after.Mallocs - before.Mallocs; n > 2000 && allocsOrdinary() {
					t.Errorf("20,000 calls of contains: %d allocations", n)
				}
			}
			if r.jit.materialized == 0 {
				t.Error("Go made no closure the code did not make")
			}
		})
	}
}

// loop1000 is a script calling call a thousand times, its last answer its
// value.
func loop1000(call string) string {
	return "(() => { let r; for (let i = 0; i < 1000; i++) r = " + call + "; return r })()"
}

// TestJITRegTailCalledClosuresPolled runs regTailClosSetup's rounds with
// loops polling every 3 back edges and every 7th poll finishing the
// invocation in the interpreter, the collector idle and marking: the
// frames Go makes of the levels inlined at calls in tail position, each
// the function its caller's call called, the closures named by their
// templates made before the interpreter goes on with them.
func TestJITRegTailCalledClosuresPolled(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regTailClosSetup)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			r.jitStress = jitStressConfig{budget: 3, deopt: 7}
			for _, round := range regTailClosRounds() {
				prog := compileForTest(t, round)
				var got [2]string
				for k, r := range rts {
					v, err := r.Run(prog)
					got[k] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("%s: with the JIT %q, without %q", round, got[0], got[1])
				}
			}
			if r.jit.materialized == 0 {
				t.Error("Go made no closure the code did not make")
			}
		})
	}
}
