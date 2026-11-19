//go:build amd64 || arm64

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestRegNativeBigCompares runs compares of a register with an upvalue
// (jlt_upval and its kin) and === and !== of BigInts held in their
// Values natively: the int64 range's ends, past it to 2^64 either way,
// and heap BigInts, numbers, strings and null against them, which Go
// compares; each answer Node's. Callbacks of BigInt64Array's methods
// compare so, entered with a frame (regCallback).
func TestRegNativeBigCompares(t *testing.T) {
	const src = `
		var vals = [0n, 1n, -1n, 5n, 2n ** 63n - 1n, -(2n ** 63n), 2n ** 63n, 2n ** 64n - 1n, -(2n ** 64n) + 1n, 2n ** 64n, 2n ** 70n + 5n, -(2n ** 65n) - 3n, 5, "5", 1.5, null]
		function mk(b) { return [(x) => x < b, (x) => x <= b, (x) => !(x < b), (x) => !(x <= b), (x) => x === b, (x) => x !== b] }
		var out = []
		for (var b of vals) {
			var fs = mk(b), s = ""
			for (var x of vals) { for (var f of fs) s += f(x) ? "1" : "0"; s += " " }
			out.push(s.trim())
		}
		var a = BigInt64Array.from([3n, -7n, 2n ** 62n, 9n]), last = a[3]
		out.push(a.every((x) => x <= last), a.find((x) => x === last), a.findIndex((x) => x === -7n), a.some((x) => x > 2n ** 61n))
		out.join("\n")`
	const want = `011010 001101 110001 001101 001101 110001 001101 001101 110001 001101 001101 110001 001101 001101 001101 011001
110001 011010 110001 001101 001101 110001 001101 001101 110001 001101 001101 110001 001101 001101 001101 110001
001101 001101 011010 001101 001101 110001 001101 001101 110001 001101 001101 110001 001101 001101 001101 001101
110001 110001 110001 011010 001101 110001 001101 001101 110001 001101 001101 110001 011001 011001 110001 110001
110001 110001 110001 110001 011010 110001 001101 001101 110001 001101 001101 110001 110001 110001 110001 110001
001101 001101 001101 001101 001101 011010 001101 001101 110001 001101 001101 110001 001101 001101 001101 001101
110001 110001 110001 110001 110001 110001 011010 001101 110001 001101 001101 110001 110001 110001 110001 110001
110001 110001 110001 110001 110001 110001 110001 011010 110001 001101 001101 110001 110001 110001 110001 110001
001101 001101 001101 001101 001101 001101 001101 001101 011010 001101 001101 110001 001101 001101 001101 001101
110001 110001 110001 110001 110001 110001 110001 110001 110001 011010 001101 110001 110001 110001 110001 110001
110001 110001 110001 110001 110001 110001 110001 110001 110001 110001 011010 110001 110001 110001 110001 110001
001101 001101 001101 001101 001101 001101 001101 001101 001101 001101 001101 011010 001101 001101 001101 001101
110001 110001 110001 011001 001101 110001 001101 001101 110001 001101 001101 110001 011010 011001 110001 110001
110001 110001 110001 011001 001101 110001 001101 001101 110001 001101 001101 110001 011001 011010 110001 110001
110001 110001 110001 001101 001101 110001 001101 001101 110001 001101 001101 110001 001101 001101 011010 110001
011001 001101 110001 001101 001101 110001 001101 001101 110001 001101 001101 110001 001101 001101 001101 011010
false
9
1
true`
	if got := nativeRunSrc(t, src, true); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	nativeCheck(t, src, `
		function mk(b) { return [(x) => x < b, (x) => x <= b, (x) => !(x < b), (x) => !(x <= b), (x) => x === b, (x) => x !== b] }
		var vals = [0n, 1n, -1n, 5n, 2n ** 63n - 1n, -(2n ** 63n), 2n ** 64n - 1n, -(2n ** 64n) + 1n], n = 0
		for (var k = 0; k < 20; k++) for (var b of vals) { var fs = mk(b); for (var x of vals) for (var f of fs) if (f(x)) n++ }
		String(n)`, bytecode.RJLtU, bytecode.RJLeU, bytecode.RJNLtU, bytecode.RJNLeU, bytecode.RJSEq, bytecode.RJSNe)
}
