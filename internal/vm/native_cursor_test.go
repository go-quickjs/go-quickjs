//go:build amd64 || arm64

package vm

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// nativeCursorSrc steps and closes cursors the iter_step and iter_close
// handlers answer natively -- a destructuring's steps over an array, past
// its end and after it grew or shrank, the close of a pattern, of a for-of
// left by a break and of a for-in left by a break, a continue or a return
// -- and the cursors their closes released taken again by the next loop
// or pattern, with what they leave to Go among them: a hole read through
// the prototype, a return method on %ArrayIteratorPrototype%, a cursor of
// an iterator of script, a generator's.
const nativeCursorSrc = `
	var out = [];
	function keys(o) { var r = ""; for (var k in o) { if (k === "c") break; r += k; } return r }
	function first(o) { for (var k in o) return k; return "-" }
	function* gen() { yield 1; yield 2; yield 3 }
	var it = { i: 0, [Symbol.iterator]() { this.i = 0; return this }, next() { return { value: this.i, done: this.i++ > 2 } } };
	for (var round = 0; round < 40; round++) {
		var arr = [round, 2, 3, 4];
		var [a, b, , c, d, ...e] = arr;
		out.push(a, b, c, String(d), e.length);
		var [f, g = (arr.length = 1, 9), h] = arr;
		out.push(f, g, String(h));
		var grow = [1];
		var [p, q = (grow.push(7, 8), 0), s] = grow;
		out.push(p, q, String(s));
		for (var v of [5, 6, 7]) { if (v === 6) break; out.push(v); }
		for (var w of [5, 6]) { for (var x in { m: 1, n: 2 }) { out.push(w + x); continue; } }
		out.push(keys({ a: 1, b: 2, c: 3, d: 4 }), first({ z: 1, y: 2 }));
		var [y1, y2] = gen(); out.push(y1 + y2);
		var [i1, i2] = it; out.push(i1 + i2);
		if (round === 20) Array.prototype[1] = "P";
		var [h0, h1, h2] = [0, , 2]; out.push(h1);
		if (round === 21) delete Array.prototype[1];
		if (round === 30) Object.getPrototypeOf([][Symbol.iterator]()).return = function () { out.push("ret"); return {} };
		var [r0] = [1, 2];
		if (round === 31) delete Object.getPrototypeOf([][Symbol.iterator]()).return;
	}
	out.join()`

// TestRegNativeCursors runs nativeCursorSrc by the Go table's loop and by
// the native handlers, with the collector idle and as while it marks: the
// same answers, the same exits marking or not, and the steps and closes
// of arrays' and for-ins' cursors made natively.
func TestRegNativeCursors(t *testing.T) {
	nativeCounting(t)
	want := nativeRunSrc(t, nativeCursorSrc, false)
	if !strings.Contains(want, "P") || !strings.Contains(want, "ret") {
		t.Fatalf("the script does not reach its slow cases: %s", want)
	}
	var exits [2][2]int64
	for i, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := nativeExitOps(t)
		if got := nativeRunSrc(t, nativeCursorSrc, true); got != want {
			t.Errorf("native (marking %v):\n%s\nwant\n%s", marking, got, want)
		}
		after := nativeExitOps(t)
		exits[i] = [2]int64{after[bytecode.RIterStep] - before[bytecode.RIterStep], after[bytecode.RIterClose] - before[bytecode.RIterClose]}
	}
	if exits[0][0] == 0 || exits[0][1] == 0 {
		// The generator's and the iterator's steps and closes are Go's
		// wherever register code runs the script.
		t.Fatalf("exits %v: the script did not run as register code", exits[0])
	}
	if exits[0] != exits[1] {
		t.Errorf("exits of iter_step and iter_close %v, %v while marking", exits[0], exits[1])
	}
	// Go's, each round: the generator's and the iterator's two steps and
	// their closes, and the step that meets a hole; and the closes a return
	// method on %ArrayIteratorPrototype% leaves to it.
	if steps, closes := exits[0][0], exits[0][1]; steps > 40*5 || closes > 40*2+10 {
		t.Errorf("%d steps and %d closes left to Go, want most made natively", steps, closes)
	}
}
