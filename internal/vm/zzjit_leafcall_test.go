//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// leafCallSetup's loop calls leaves of each kind -- a string's method, a
// Map's get, Number(x) and String(x) plain, Number.isNaN -- and the loop
// again on a receiver whose get is a function of the script's, and with
// Number.isNaN replaced.
const leafCallSetup = `
function leafLoop(s, m, xs, n) { let h = 0, out = ''; for (let i = 0; i < n; i++) { const x = xs[i % xs.length]; out = s.at(i % 5) + ':' + m.get(i % 3) + ':' + Number(x); out += ':' + String(x) + ':' + Number.isNaN(x); h = (h * 31 + out.length) | 0 } return h + '|' + out }
function leaves(n) { return leafLoop('hello', new Map([[0, 'a'], [1, 'b'], [2, 'c']]), [1.5, '42', true, null, undefined, NaN, -0, '0x10'], n) }
function otherGet(n) { return leafLoop('hello', { get(i) { return 'g' + i } }, [1, 2], n) }
function replaced(n) { const save = Number.isNaN; Number.isNaN = x => 'R'; try { return leaves(n) } finally { Number.isNaN = save } }
`

// A call native code has made without a frame, a leaf's (jitLeafSeen), is
// in the code compiled next a call of Go of its own (ssa.GoLeaf), checked
// to call that function, of which Go finds out nothing: each answer is
// Node's (v26.10.0) and the interpreter's, the loop's code has such calls,
// and, warm, it makes one call of Go with a frame at most in a round -- the
// Map's get read once in each invocation, by Go -- and never leaves its
// code. Another function at the site, or the built-in replaced, fails the
// check, and Go calls it.
func TestJITLeafCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var rounds []string
	for range 6 {
		rounds = append(rounds, "leaves(30)")
	}
	r := jitAgreesForTest(t, leafCallSetup, rounds...)
	e := jitNativeForTest(t, r, "leafLoop")
	leaves := 0
	for _, st := range e.ssa.GoCallStates() {
		if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf {
			leaves++
		}
	}
	if leaves < 5 {
		t.Errorf("leafLoop's code has %d leaves' calls, want the five", leaves)
	}
	calls, hosts, entries := e.ssaStats.goCalls, e.ssaStats.hosts, e.ssaStats.entries
	if _, err := r.Run(compileForTest(t, "leaves(30)")); err != nil {
		t.Fatal(err)
	}
	if calls, hosts, entries = e.ssaStats.goCalls-calls, e.ssaStats.hosts-hosts, e.ssaStats.entries-entries; entries == 0 || calls > 1 || hosts != 0 {
		t.Errorf("leafLoop ran natively %d times, made %d calls of Go with frames and left its code %d times", entries, calls, hosts)
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	if _, err := want.Run(compileForTest(t, leafCallSetup)); err != nil {
		t.Fatal(err)
	}
	for _, rd := range [][2]string{
		{"leaves(30)", "1103408584|o:c:NaN:NaN:true"},
		{"otherGet(30)", "1022700096|o:g2:2:2:false"},
		{"replaced(30)", "1961254988|o:c:NaN:NaN:R"},
		{"leaves(30)", "1103408584|o:c:NaN:NaN:true"},
	} {
		for _, rt := range []*Runtime{want, r} {
			v, err := rt.Run(compileForTest(t, "''+("+rd[0]+")"))
			if err != nil {
				t.Fatalf("%s: %v", rd[0], err)
			}
			if got := v.String().Go(); got != rd[1] {
				t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
			}
		}
	}
}
