//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// leafFastSetup's loop calls leaves of each kind -- String's native leaves
// (codePointAt, at, toUpperCase, startsWith), a Map's get, Number(x) and
// Number.isNaN -- a turn of the loop making seven; odd(n) gives at's call,
// at its eleventh turn, an object whose conversion runs script, which the
// leaf's check refuses.
const leafFastSetup = `
var conversions = 0
function leafTurns(s, m, xs, ks, n) {
	let h = 0, out = ''
	for (let i = 0; i < n; i++) {
		const x = xs[i % xs.length]
		const c = s.codePointAt(i % 7)
		out = s.at(ks[i % ks.length]) + ':' + m.get(i % 3) + ':' + Number(x) + ':' + Number.isNaN(x) + ':' + s.toUpperCase().length
		if (s.startsWith('h', i & 1)) h++
		h = (h * 31 + out.length + c) | 0
	}
	return h + '|' + out
}
var sm = new Map([[0, 'a'], [1, 'b'], [2, 'c']])
function leafs(n) { return leafTurns('héllo😀', sm, [1.5, '42', true, null, NaN, -0], [0, 3, -1, 9], n) }
function odd(n) { conversions = 0; return leafTurns('hello', sm, [2], [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, { valueOf() { conversions++; return 1 } }], n) + '|' + conversions }
`

// leafFastWants are leafFastSetup's rounds and their results, Node's
// (v26.10.0).
var leafFastWants = [][2]string{
	{"leafs(40)", "-127136937|undefined:a:0:false:7"},
	{"odd(40)", "112500713|undefined:a:2:false:5|3"},
	{"leafs(40)", "-127136937|undefined:a:0:false:7"},
}

// A leaf's call native code makes (ssa.GoLeaf) goes through the VM's
// routine for leaves (abi.Encoding's LeafFast, jitLeafGo): the call's
// description names its function (abi.GoCallState's Leaf), which native
// code passes no operand for, and Go applies the leaf with no expansion of
// the call nor any dispatch of calls of Go, as the form it made of the
// call at its first says (jitLeafForm), the result's number word back in a
// register. A leaf that refuses -- at's check, of an object -- leaves
// the call to an exit, the interpreter converting the object; each answer
// is Node's and the interpreter's.
func TestJITLeafFast(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitLeafFastForTest(t)
	e := jitNativeForTest(t, r, "leafTurns")
	leaves := 0
	for _, st := range e.ssa.GoCallStates() {
		if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf {
			if st.Leaf == 0 {
				t.Errorf("a leaf's call names no leaf: %+v", st)
			}
			leaves++
		}
	}
	if leaves < 7 {
		t.Errorf("leafTurns' code has %d leaves' calls, want the seven", leaves)
	}
	fast, refused, calls := r.jit.leafFast, r.jit.leafRefused, e.ssaStats.goCalls
	if _, err := r.Run(compileForTest(t, "leafs(40)")); err != nil {
		t.Fatal(err)
	}
	// Seven leaves a turn, each made by the routine; the Map's get read
	// once, by Go.
	if got := r.jit.leafFast - fast; got != 7*40 || r.jit.leafRefused != refused || e.ssaStats.goCalls-calls > 1 {
		t.Errorf("leafs(40) made %d leaves' calls through the routine, refused %d, %d calls of Go with frames; want 280, 0, at most 1",
			got, r.jit.leafRefused-refused, e.ssaStats.goCalls-calls)
	}
	// Each call's form made once, at its first call, and kept in its
	// description (jitLeafForm).
	for _, st := range e.ssa.GoCallStates() {
		if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf && (st.LeafForm == nil || (*jitLeafForm)(st.LeafForm).keep != st.Keep) {
			t.Errorf("a leaf's call made without its form: %+v", st)
		}
	}
	if _, err := r.Run(compileForTest(t, "odd(40)")); err != nil {
		t.Fatal(err)
	}
	if r.jit.leafRefused == refused {
		t.Error("at's call of an object was not refused")
	}
}

// jitLeafFastForTest runs leafFastSetup's rounds until leafTurns' leaves
// are calls of their own, each round's answer Node's and the
// interpreter's, and returns the runtime.
func jitLeafFastForTest(t *testing.T) *Runtime {
	t.Helper()
	var rounds []string
	for range 6 {
		rounds = append(rounds, "leafs(40)")
	}
	r := jitAgreesForTest(t, leafFastSetup, rounds...)
	jitLeafFastAgree(t, r)
	return r
}

// jitLeafFastAgree checks leafFastWants' rounds on r.
func jitLeafFastAgree(t *testing.T, r *Runtime) {
	t.Helper()
	for _, rd := range leafFastWants {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
		}
	}
}

// A leaf's call through the routine while the collector runs -- a
// collection in the middle of calls (jitGoCallWork), which scans the
// goroutine's stack through the routine's frame, and the code compiled
// for a collector that marks (its stores through Go's barrier) -- makes
// the leaves as it does otherwise: the answers Node's, the routine making
// them.
func TestJITLeafFastWhileCollecting(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		calls := 0
		jitGoCallWork = func() {
			if calls++; calls%97 == 0 {
				runtime.GC()
			}
		}
		r := jitLeafFastForTest(t)
		fast := r.jit.leafFast
		jitLeafFastAgree(t, r)
		jitGoCallWork = nil
		if r.jit.leafFast == fast || calls < 97 {
			t.Errorf("marking %v: %d leaves' calls through the routine while collections ran, %d calls of Go", marking, r.jit.leafFast-fast, calls)
		}
	}
}
