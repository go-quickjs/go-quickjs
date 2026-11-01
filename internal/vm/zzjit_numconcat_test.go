//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// numConcatSetup's loop joins numbers that are no integers, or past the
// integers concatInt writes -- fractions, -0, NaN, the infinities, 1e21,
// a denormal, 2**53+2 -- to strings on either side, one not ASCII.
const numConcatSetup = `var nums = [0.1, -0, NaN, Infinity, -Infinity, 1e21, 1.5e-7, 123456789.125, -2.5, 42, 2 ** 53 + 2, 5e-324]
function join(n) { var s = '', h = 0; for (var i = 0; i < n; i++) { var x = nums[i % nums.length]; s = (x + '') + (x + 'px') + '|' + ('#' + x) + '|' + ('é' + (x * 3)); h = (h * 31 + s.length) | 0 } return h + ':' + s }
`

// A number joined to a string by native code's + (jitAddHook) is the
// number's string concatenated, as add makes it: each answer the
// interpreter's and Node's (v26.10.0), with the collector idle and while
// it marks, the joins made by Go from native code without leaving it.
func TestJITNumberConcat(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	want := "34:0.10.1px|#0.1|é0.30000000000000004 1064:00px|#0|é0 33002:NaNNaNpx|#NaN|éNaN " +
		"1023100:InfinityInfinitypx|#Infinity|éInfinity 31716142:-Infinity-Infinitypx|#-Infinity|é-Infinity " +
		"983200428:1e+211e+21px|#1e+21|é3e+21 414442226:1.5e-71.5e-7px|#1.5e-7|é4.5e-7 " +
		"-37192824:123456789.125123456789.125px|#123456789.125|é370370367.375 " +
		"-1152977522:-2.5-2.5px|#-2.5|é-7.5 -1382564799:4242px|#42|é126 " +
		"90164262:90071992547409949007199254740994px|#9007199254740994|é27021597764222984 " +
		"-1499875142:5e-3245e-324px|#5e-324|é1.5e-323"
	all := "(function () { var o = []; for (var k = 1; k <= 12; k++) o.push(join(k)); return o.join(' ') })()"
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		r := jitAgreesForTest(t, numConcatSetup, "join(200)", "join(200)", "join(200)", "join(200)", all)
		e := jitNativeForTest(t, r, "join")
		ended := e.ssaStats.ended
		v, err := r.Run(compileForTest(t, all))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("marking %v: got %s, want Node's %s", marking, got, want)
		}
		if _, err := r.Run(compileForTest(t, "join(200)")); err != nil {
			t.Fatal(err)
		}
		if e.ssaStats.ended != ended {
			t.Errorf("marking %v: join left native code %d times", marking, e.ssaStats.ended-ended)
		}
		debug.SetGCPercent(gc)
	}
}
