//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
	"time"
)

// regDroppedSrc is test262's TypedArray.prototype.map test of what the
// callback returns, its harness's loop over the constructors and the
// argument factories cut down: outer's code inlines map's callback, which
// it does not make (ForEachSite's Made), and leaves at the call of map,
// where Go makes it from the frame's template.
const regDroppedSrc = `
function isPrimitive(v) { return v === null || (typeof v !== "object" && typeof v !== "function") }
function makePassthrough(TA, x) { return x }
function makeArray(TA, x) { return isPrimitive(x) ? x : Array.from(x) }
function makeArrayLike(TA, x) {
	var arr = makeArray(TA, x)
	var obj = { length: arr.length }
	for (var i = 0; i < obj.length; i++) obj[i] = arr[i]
	return obj
}
function makeIterable(TA, x) {
	var src = makeArray(TA, x)
	var obj = {}
	obj[Symbol.iterator] = function () { return src[Symbol.iterator]() }
	return obj
}
function makeArrayBuffer(TA, x) { return new TA(makeArray(TA, x)).buffer }
var factories = [makePassthrough, makeArray, makeArrayLike, makeIterable, makeArrayBuffer]
var ctors = [BigInt64Array, BigUint64Array]
function each(f) {
	for (var k = 0; k < factories.length; ++k) {
		for (var i = 0; i < ctors.length; ++i) {
			try {
				f(ctors[i], factories[k].bind(undefined, ctors[i]))
			} catch (e) {
				e.message += " (" + ctors[i].name + ", " + factories[k].name + ")"
				throw e
			}
		}
	}
}
var out = []
for (var round = 0; round < 3; round++) {
	each(function outer(TA, makeCtorArg) {
		var sample = new TA(makeCtorArg([1n, 2n, 4n]))
		var result = sample.map(function (v) { return v * 3n })
		out.push(result.join())
	})
}
out.length + ":" + out[0] + ";" + out[out.length - 1]
`

// TestJITRegDroppedCodeExits: code the cache let go of during a run of it
// -- replaced by its function's compile installed at a check the run's
// calls made -- is closed only once Go has made of the run's exit what it
// needs (jitRegStretch's hold on it). Closed as the run ended, before
// jitRegLeft, the exit at the call of map found the code's feedback gone,
// its made closures with it, and handed map the callback's template in
// place of the closure: "TypeError: map requires a function" under CI's
// stress step on the Go table (QJS_NONATIVE=1,
// QJS_JIT_STRESS=threshold,budget=3,deopt=7,background=3), in test262's
// TypedArray/prototype/map/BigInt/callbackfn-return-affects-returned-object.
func TestJITRegDroppedCodeExits(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const want = "30:3,6,12;3,6,12"
	plain := newTestRuntime(t, Config{})
	defer plain.Close()
	if got, _ := jitRunWithin(t, plain, regDroppedSrc, 10*time.Second); got != want {
		t.Fatalf("interpreter: %q, want %q", got, want)
	}
	for _, native := range []bool{false, true} {
		for k := uint16(0); k <= 8; k++ {
			t.Run(fmt.Sprintf("native=%v/background=%d", native, k), func(t *testing.T) {
				was := SetRegNative(native)
				t.Cleanup(func() { SetRegNative(was) })
				r := jitBackgroundRuntime(t)
				regStressForTest(r, jitStressConfig{budget: 3, deopt: 7, background: true, checks: k})
				if got, _ := jitRunWithin(t, r, regDroppedSrc, 10*time.Second); got != want {
					t.Errorf("%q, want %q", got, want)
				}
			})
		}
	}
}
