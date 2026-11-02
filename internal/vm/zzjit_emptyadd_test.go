//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// emptyAddSetup's loops convert values to strings by "" + x and x + "":
// strings, flat, empty, not ASCII, a rope; and, in mixed, a number of each
// kind, a boolean, undefined, null, a BigInt, objects whose valueOf,
// toString and Symbol.toPrimitive are counted, an array, a String object
// and a symbol, which throws; in str, a callee inlined in inl's loop.
const emptyAddSetup = `var calls = 0
var strs = ['plain', '', 'café', 'a'.repeat(3) + 'b'.repeat(2)]
var vals = ['s', '', 42, -0, 1.5, NaN, true, undefined, null, 10n, { valueOf() { calls++; return 7 }, toString() { calls++; return 'o' } },
  { toString() { calls++; return 'ts' } }, [1, 2], new String('w'), { [Symbol.toPrimitive](h) { calls++; return h } }, Symbol('y')]
function str(x) { return '' + x }
function strs2(n) { var s = 0, t = ''; for (var i = 0; i < n; i++) { var x = strs[i & 3]; t = ('' + x) + '|' + (x + ''); s += ('' + x).length + (x + '').length } return s + t }
function mixed(n) { var o = []; calls = 0; for (var i = 0; i < n; i++) { var x = vals[i % vals.length]; try { o.push(('' + x) + ',' + (x + '') + ',' + str(x)) } catch (e) { o.push(e.constructor.name) } } return o.join(' ') + ' calls=' + calls }
function inl(n) { var s = ''; for (var i = 0; i < n; i++) s = str(strs[i & 3]) + str(i) + s.length; return s }
`

// "" + x and x + "" in native code are x itself where it is a string, and
// Go's addition where it is anything else (ssa's emptyAdd): each answer the
// interpreter's and Node's (v26.10.0), with the collector idle and while
// it marks, and strings made with no call of Go.
func TestJITEmptyStringAdd(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 6 {
			rounds = append(rounds, "strs2(200)", "mixed(48)", "inl(200)")
		}
		r := jitAgreesForTest(t, emptyAddSetup, rounds...)
		for _, c := range []struct{ src, want string }{
			{"strs2(200)", "1400aaabb|aaabb"},
			{"mixed(48)", "s,s,s ,, 42,42,42 0,0,0 1.5,1.5,1.5 NaN,NaN,NaN true,true,true undefined,undefined,undefined null,null,null 10,10,10 7,7,7 ts,ts,ts 1,2,1,2,1,2 w,w,w default,default,default TypeError s,s,s ,, 42,42,42 0,0,0 1.5,1.5,1.5 NaN,NaN,NaN true,true,true undefined,undefined,undefined null,null,null 10,10,10 7,7,7 ts,ts,ts 1,2,1,2,1,2 w,w,w default,default,default TypeError s,s,s ,, 42,42,42 0,0,0 1.5,1.5,1.5 NaN,NaN,NaN true,true,true undefined,undefined,undefined null,null,null 10,10,10 7,7,7 ts,ts,ts 1,2,1,2,1,2 w,w,w default,default,default TypeError calls=27"},
			{"inl(200)", "aaabb1998"},
		} {
			v, err := r.Run(compileForTest(t, "''+"+c.src))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.src, got, c.want)
			}
		}
		e := jitNativeForTest(t, r, "strs2")
		fast, calls, ended := r.jit.goFast, e.ssaStats.goCalls, e.ssaStats.ended
		if _, err := r.Run(compileForTest(t, "strs2(200)")); err != nil {
			t.Fatal(err)
		}
		// The joins' calls of Go, two a turn: none of the conversions.
		if got := r.jit.goFast - fast; got > 2*200 || e.ssaStats.goCalls != calls || e.ssaStats.ended != ended {
			t.Errorf("marking %v: strs2(200) made %d calls of Go through the routine, want at most 400 (and %d others, left %d times)",
				marking, got, e.ssaStats.goCalls-calls, e.ssaStats.ended-ended)
		}
		jitNativeForTest(t, r, "mixed")
		jitNativeForTest(t, r, "inl")
		debug.SetGCPercent(gc)
	}
}
