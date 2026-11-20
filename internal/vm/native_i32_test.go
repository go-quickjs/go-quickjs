//go:build amd64 || arm64

package vm

import (
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// nativeI32Src runs mul_imm and the int32 forms over every kind of
// operand their handlers meet: int32 sums and those past 2^31, 2^53 and
// 2^63, NaN, the infinities, -0, strings, objects with valueOf, and the
// BigInts whose TypeErrors are placed at the | 0 or the operator; into
// registers that hold numbers and into ones that held a pointer.
const nativeI32Src = `
	var vals = [0, -0, 1, -1, 2147483647, -2147483648, 2147483648, 4294967296, 2 ** 53, 2 ** 63, -(2 ** 63), 1e300,
		NaN, Infinity, -Infinity, 0.5, -1.5, "12", "x", null, undefined, true, { valueOf: function () { return 9 } }];
	function show(v) { return Object.is(v, -0) ? "-0" : String(v) }
	function all() {
		var out = [];
		for (var i = 0; i < vals.length; i++) {
			var x = vals[i];
			for (var j = 0; j < vals.length; j++) {
				var y = vals[j], p = "a pointer", q = {};
				p = (x + y) | 0; q = (x - y) | 0;
				out.push(show(p), show(q), show((x + 7) | 0), show((x - 7) | 0), show((y - -32767) | 0));
			}
			out.push(show(x * 3), show(-5 * x), show(x * 0), show(x * -32768));
		}
		try { var a = 1n; out.push((a + a) | 0) } catch (e) { out.push(e.name + " " + e.stack.split("\n")[1]) }
		try { var b = 1n; out.push((b - 1) | 0) } catch (e) { out.push(e.name + " " + e.stack.split("\n")[1]) }
		try { var c = 1n; out.push(c * 3) } catch (e) { out.push(e.name + " " + e.stack.split("\n")[1]) }
		return out.join(",");
	}
	all() + "|" + all()`

// mul_imm and the int32 forms answer as the Go table's loop does, with the
// collector idle and as while it marks; and over the loops that are their
// work -- loop_int32's, loop_arith's, try_finally's -- the native code
// exits to Go for none of them, marking or not.
func TestRegNativeMulImmInt32(t *testing.T) {
	want := nativeRunSrc(t, nativeI32Src, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, nativeI32Src, true); got != want {
			t.Errorf("native (marking %v):\n%.2000s\nwant\n%.2000s", marking, got, want)
		}
	}
	nativeCounting(t)
	ops := []bytecode.ROp{bytecode.RMulI, bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32}
	for _, k := range []struct {
		name string
		n    int
	}{{"loop_int32", 20000}, {"loop_arith", 20000}, {"try_finally", 20000}} {
		want, _ := nativeRunKernel(t, false, k.name, nil, k.n)
		var enters [2]int64
		for i, marking := range []bool{false, true} {
			nativeMarking(t, marking)
			before := nativeExitOps(t)
			enters0 := atomic.LoadInt64(&nativeCountsOf(t).enters)
			if got, _ := nativeRunKernel(t, true, k.name, nil, k.n); got != want {
				t.Errorf("%s (marking %v): %s, want %s", k.name, marking, got, want)
			}
			enters[i] = atomic.LoadInt64(&nativeCountsOf(t).enters) - enters0
			after := nativeExitOps(t)
			for _, op := range ops {
				if n := after[op] - before[op]; n != 0 {
					t.Errorf("%s (marking %v): %v exited to Go %d times", k.name, marking, op, n)
				}
			}
		}
		nativeMarking(t, false)
		if enters[0] != enters[1] {
			t.Errorf("%s: %d entries, %d while marking", k.name, enters[0], enters[1])
		}
	}
}
