//go:build amd64 || arm64

package vm

import (
	"sync/atomic"
	"testing"
)

func TestRegNativeBinaryLeaf(t *testing.T) {
	nativeCounting(t)
	const src = `
	function add(a, b) { return a + b }
	function sub(a, b) { return a - b }
	function mul(a, b) { return a * b }
	function make(x) { return [function (y) { return x + y }, function (y) { return y - x }, function (y) { return x * y }] }
	function show(x) { return Object.is(x, -0) ? "-0" : String(x) }
	var out = [], vals = [0, -0, 1, -1, NaN, Infinity, -Infinity, 0.5, 1e300, "2", null, undefined, true,
		{ valueOf() { return 7 } }, 3n];
	for (var round = 0; round < 3; round++) for (var i = 0; i < vals.length; i++) {
		var x = vals[i], fs = make(x);
		for (var j = 0; j < vals.length; j++) {
			var y = vals[j];
			for (var f of [add, sub, mul]) { try { out.push(show(f(x, y))) } catch (e) { out.push(e.name) } }
			for (var f of fs) { try { out.push(show(f(y))) } catch (e) { out.push(e.name) } }
		}
	}
	out.push(show(add(1)), show(sub()), show(mul(2)));
	var log = [], a = { valueOf() { log.push("a"); return 2 } }, b = { valueOf() { log.push("b"); return 5 } };
	out.push(add(a, b), sub(a, b), mul(a, b), log.join(""));
	out.join("|")`
	want := nativeRunSrc(t, src, false)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("marking %v: native binary leaves differ from the Go table", marking)
		}
	}
}

// Arithmetic leaves read their operands directly, even when each call uses
// another closure of the same template, rather than entering its frame.
func TestRegNativeBinaryLeafFrameless(t *testing.T) {
	nativeCounting(t)
	const src = `
	function add(a, b) { return a + b }
	function run() {
	var t = 0;
	for (var i = 0; i < 1000; i++) {
		t = add(t, i);
		var f = function (x) { return x + i };
		t = f(t);
	}
	return t;
	}
	String(run())`
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := atomic.LoadInt64(&nativeCalls)
		if got := nativeRunSrc(t, src, true); got != "999000" {
			t.Fatalf("marking %v: got %s, want 999000", marking, got)
		}
		if n := atomic.LoadInt64(&nativeCalls) - before; n > 4 {
			t.Fatalf("marking %v: arithmetic leaves entered %d native frames", marking, n)
		}
	}
}
