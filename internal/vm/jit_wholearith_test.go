//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// jitWholeArithSetup has loops of arithmetic on an object, whose
// conversion (ToPrimitive) runs its valueOf: an addition to a number and
// to a string, a Date's difference, a product by an immediate, a
// remainder and a bitwise or, each over turns turns, each turn with work
// of numbers beside it (work) as a program's has. log records the
// conversions in their order, at the turns order asks for.
const jitWholeArithSetup = `
	let log = ""
	const o = { valueOf() { return 1 } }
	const p = { valueOf() { log += "p"; return 2 }, toString() { return "P" } }
	const q = { valueOf() { log += "q"; return 3 } }
	const d = new Date(5000)
	function work(i) { let t = 0; for (let j = 0; j < 8; j++) t = (t * 31 + i + j) | 0; return t & 1 }
	function add(turns) { let s = 0; for (let i = 0; i < turns; i++) s += o + work(i); return s }
	function append(turns) { let s = ""; for (let i = 0; i < turns; i++) { s += o; s += work(i) } return s.length }
	function sub(turns) { let s = 0; for (let i = 0; i < turns; i++) s += d - o + work(i); return s }
	function mul(turns) { let s = 0; for (let i = 0; i < turns; i++) s += o * 2 + work(i); return s }
	function mod(turns) { let s = 0; for (let i = 0; i < turns; i++) s += (i + 7) % q + work(i); return s }
	function or(turns) { let s = 0; for (let i = 0; i < turns; i++) s += (o | 4) + work(i); return s }
	function order(turns) {
		let s = 0
		for (let i = 0; i < turns; i++) s += (i % 100 === 0 ? p : 1) - (i % 100 === 0 ? q : 1) + work(i)
		return s + ":" + log.length + ":" + log.slice(0, 6)
	}`

// Arithmetic on an object -- s += o, d - o, o * 2 -- whose conversion runs
// its valueOf, is a call of Go that runs the operator whole on the
// register frame once an exit has shown the object (Hints' WholeOps), as
// V8's code calls its Add, Subtract or Multiply builtin for feedback any,
// native code going on after it. Each had left native code at every turn:
// 64 exits, the loop finishing 66 runs in the interpreter, until the code
// was entered no more. Each answer is the interpreter's, the conversions
// in their order, with the collector marking too.
func TestJITArithmeticOnObjects(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		for _, fn := range []string{"add", "append", "sub", "mul", "mod", "or", "order"} {
			t.Run(fmt.Sprintf("%s/marking=%v", fn, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				holdCollector(t)
				r, run := jitTierRuntimeForTest(t, jitInvocationsForFeedback, jitWholeArithSetup)
				src := fmt.Sprintf("%s(8000)", fn)
				for range 3 {
					run(src)
				}
				for i := range 3 {
					st, gcs := r.RegNativeStats(), jitCollectionsForTest(r)
					run(src)
					after := r.RegNativeStats()
					if hosts, ended := after.Hosts-st.Hosts, after.Interpreted-st.Interpreted; (hosts != 0 || ended != 0) && gcs == jitCollectionsForTest(r) {
						t.Errorf("warm call %d of %s: %d exits to Go (%v), %d invocations finished in the interpreter", i, src, hosts, after.HostOps, ended)
					}
					if after.GoCalls == st.GoCalls {
						// The conversions are Go's, from native code.
						t.Errorf("warm call %d of %s made no call of Go: not native", i, src)
					}
				}
			})
		}
	}
}

// A valueOf that throws, met by arithmetic Go runs whole from native code
// (Hints' WholeOps), throws from there as from the interpreter: its error,
// caught by the loop's try at the turn it threw, the turns before it
// counted; and one that throws uncaught throws out of the function.
func TestJITArithmeticOnObjectsThrows(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		let bad = -1
		const o = { valueOf() { if (bad-- === 0) throw new RangeError("valueOf " + bad); return 1 } }
		function caught(turns, at) {
			bad = at
			let s = 0, err = ""
			for (let i = 0; i < turns; i++) {
				try { s += o * 3 } catch (e) { err = e.name + ":" + e.message + "@" + i }
			}
			return s + " " + err
		}
		function uncaught(turns, at) {
			bad = at
			let s = 0
			for (let i = 0; i < turns; i++) s += o - 1
			return s
		}`
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			_, run := jitTierRuntimeForTest(t, jitInvocationsForFeedback, setup)
			for range 3 {
				run(`caught(3000, -1)`)
				run(`uncaught(3000, -1)`)
			}
			run(`caught(3000, 2500)`)
			run(`caught(3000, 10)`)
			for _, at := range []int{2500, 10} {
				// Caught by a caller the interpreter runs: its error, message
				// and stack the interpreter's.
				run(fmt.Sprintf("(function(){try{return uncaught(3000, %d)}catch(e){return 'threw '+e.name+': '+e.message+'|'+e.stack}})()", at))
			}
			run(`uncaught(3000, -1)`)
		})
	}
}
