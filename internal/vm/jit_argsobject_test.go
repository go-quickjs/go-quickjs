//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"
)

// An arguments object read as a value -- a callee's, returned -- has its
// length and its elements read natively, as V8's loads follow their
// feedback's receivers (ssa.ArgumentsFeedback): its length its own data
// property, at the place the realm's arguments objects' shapes have it, an
// unmapped one's elements by their cells, as an Array's. Each read had
// been a call of Go, which cost native code more than the interpreter's
// read: microbench's arguments_object ran slower with the JIT than
// without. What the shapes do not describe falls back to Go -- a length
// written, defined as an accessor, deleted; an element deleted or past the
// end; a mapped object's elements, which alias its parameters -- with the
// same answers. Each answer is Node's.
func TestJITArgumentsObjectReads(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer holdCollectorNow(t)()
	r, want := jitAnswersForTest(t, `
		function args() { "use strict"; return arguments }
		function sloppy(a, b) { a = a * 2; return arguments }
		function read(n) {
			var s = 0, o;
			for (var j = 0; j < n; j++) {
				o = args(j, 2);
				s = (s + o.length + o[0]) | 0;
			}
			return s;
		}
		function readOf(f, n, k) {
			var s = "";
			for (var j = 0; j < n; j++) {
				var o = f(j);
				s = o.length + ":" + o[k];
			}
			return s;
		}
		function readSloppy(n) {
			var s = 0, o;
			for (var j = 0; j < n; j++) {
				o = sloppy(j, 3);
				s = (s + o.length + o[0] + o[1]) | 0;
			}
			return s;
		}`)
	want(`read(200000)`, "-1474536480")
	e := jitNativeForTest(t, r, "read")
	if e.ssaStats.goCalls > 2*jitSSAProbe {
		t.Fatalf("read made %d calls of Go for its arguments object's length and element", e.ssaStats.goCalls)
	}
	calls := e.ssaStats.goCalls
	want(`read(100000)`, "705182704")
	if n := e.ssaStats.goCalls - calls; n != 0 {
		t.Fatalf("read, compiled, made %d calls of Go", n)
	}
	// The same reads of arguments objects their shapes do not describe, or
	// of elements they do not hold, after the loop has run on those they
	// do.
	want(`readOf(function (j) { "use strict"; return arguments }, 20000, 0)`, "1:19999")
	want(`readOf(function (j) { "use strict"; arguments.length = "x"; return arguments }, 20000, 0)`, "x:19999")
	want(`readOf(function (j) { "use strict"; Object.defineProperty(arguments, "length", {get() { return 7 }}); return arguments }, 20000, 0)`, "7:19999")
	want(`readOf(function (j) { "use strict"; delete arguments.length; return arguments }, 20000, 0)`, "undefined:19999")
	want(`readOf(function (j) { "use strict"; delete arguments[0]; return arguments }, 20000, 0)`, "1:undefined")
	want(`readOf(function (j) { "use strict"; return arguments }, 20000, 3)`, "1:undefined")
	want(`readOf(function (j) { "use strict"; arguments[0] = "w"; return arguments }, 20000, 0)`, "1:w")
	want(`readOf(function (j) { "use strict"; Object.defineProperty(arguments, 0, {get() { return "g" }}); return arguments }, 20000, 0)`, "1:g")
	want(`readOf(function (j) { return [j, j] }, 20000, 1)`, "2:19999")
	want(`readOf(function (j) { return "ab" }, 20000, 1)`, "2:b")
	// A mapped object's length natively, its elements, which alias its
	// parameters, by Go.
	want(`readSloppy(100000)`, "1410465408")
	want(`readOf(function (j) { j = "p"; return arguments }, 20000, 0)`, "1:p")
}
