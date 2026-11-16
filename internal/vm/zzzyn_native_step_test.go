//go:build amd64 || arm64

package vm

import (
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regStepOps are the step-and-branches.
var regStepOps = []bytecode.ROp{
	bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI, bytecode.RStepJLeU,
}

// TestRegNativeSteps runs loops of each step-and-branch natively, with
// the collector idle and as while it marks: the Go table's answers, the
// same exits to Go marking or not, no exit by a step-and-branch where the
// counters are numbers -- a bound that is not one, and the budget's end,
// go on at the partner natively -- and an exit by one for each step of a
// counter that is not a number, rsStep's.
func TestRegNativeSteps(t *testing.T) {
	nativeCounting(t)
	const numeric = `
		function kinds(n) {
			var c = 0, i, j, k, m, a = n;
			for (i = 0; i < n; i++) c++;
			for (i = 0; i <= n; i += 3) c++;
			for (i = 0; i < 1000; i++) c++;
			for (i = 2000; i > 0; i -= 5) c++;
			j = n; while (--j >= 0) c++;
			k = n; do { c++ } while (--k > 0);
			(function () { for (m = 0; m <= a; m++) c++ })();
			for (i = 0; i < "40"; i++) c++;
			for (i = 0.5; i < 40n; i++) c++;
			return c
		}
		var s = 0; for (var r = 0; r < 20; r++) s += kinds(3000); String(s)`
	const other = `
		function others() {
			var r = [], i;
			for (i = "0"; i < 5; i++) r.push(typeof i);
			for (i = "0"; i < 50; i += 2) r.push(i);
			for (i = 0n; i < 3n; i++) r.push(typeof i);
			var o = { valueOf() { return 1 } };
			for (i = o; i < 3; i++) r.push(i);
			return r.join()
		}
		others() + others()`
	for _, c := range []struct {
		name, src string
		exits     bool
	}{{"numeric", numeric, false}, {"other", other, true}} {
		want := nativeRunSrc(t, c.src, false)
		var enters [2]int64
		for i, marking := range []bool{false, true} {
			nativeMarking(t, marking)
			before, exits := atomic.LoadInt64(&nativeEnters), nativeExitOps()
			if got := nativeRunSrc(t, c.src, true); got != want {
				t.Errorf("%s (marking %v): %s, want %s", c.name, marking, got, want)
			}
			enters[i] = atomic.LoadInt64(&nativeEnters) - before
			after := nativeExitOps()
			n := int64(0)
			for _, op := range regStepOps {
				n += after[op] - exits[op]
			}
			if c.exits && n == 0 {
				t.Errorf("%s (marking %v): no step left to Go", c.name, marking)
			} else if !c.exits && n != 0 {
				t.Errorf("%s (marking %v): %d steps left to Go, want none", c.name, marking, n)
			}
		}
		nativeMarking(t, false)
		if enters[0] != enters[1] {
			t.Errorf("%s: %d exits, %d while marking", c.name, enters[0], enters[1])
		}
	}
}
