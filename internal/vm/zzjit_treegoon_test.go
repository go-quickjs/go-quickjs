//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// Each function's loop meets, partway through an invocation, what its code
// was not compiled for -- a string where numbers ran, a BigInt -- and
// deoptimizes there, or leaves for Go at it: the interpreter runs the frame
// to its loop's next back-edge check, and the tree, where the function has
// a plain one (ztree_goon.go), the rest from that loop's header.
const treeGoOnSetup = `
	function mixed(n, k) {
		let s = 0
		for (let i = 0; i < n; i++) {
			const x = i == k ? 'a' : i
			s = (s + (typeof x == 'string' ? x.length : x)) | 0
		}
		return s
	}
	function nested(n, k) {
		let s = 0
		outer: for (let i = 0; i < n; i++) {
			for (let j = 0; j < 30; j++) {
				if (j == 7 && i % 3 == 0) continue outer
				if (i == k && j == 2) s = s + ''
				if (j > 20 && i % 5 == 1) break
				s = (+s + i * j) | 0
			}
		}
		return s
	}
	function loops(n, k) {
		let s = 0, i = 0
		while (i < n) {
			switch (i % 4) {
			case 0: s += i; break
			case 1: s -= 1; break
			default: s = (s * 3) | 0
			}
			if (i == k) s = s + 0.5
			i++
		}
		do { s = (s ^ i) | 0; i-- } while (i > n - 50)
		return s
	}
	function captured(n, k) {
		const fs = []
		let s = 0
		for (let i = 0; i < n; i++) {
			if (i % 50 == 0) fs.push(() => i)
			s = (s + (i == k ? '1' : 1)) | 0
		}
		for (const f of fs) s += f()
		return s
	}
	function big(n) {
		let s = BigInt(0), a = BigInt(1), one = BigInt(1)
		for (let i = 0; i < n; i++) { s += a * a; a += one }
		return Number(s % BigInt(1000003))
	}
	function all(n, k) { return mixed(n, k) + nested(n, k) * 3 + loops(n, k) * 5 + captured(n, k) * 7 + big(n) * 11 }
`

// A frame native code gave back for the rest of its invocation goes on in
// its tree, as it would have run without the JIT, from its loop's header
// at the interpreter's next back-edge check there: each answer the
// interpreter's, the frames' locals, their operands, a loop's captured
// bindings and the loops left by break and continue all as the
// interpreter has them; and the tree takes frames over. At the default
// thresholds, the functions run in their trees before they are compiled.
func TestJITTreeGoesOn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var srcs []string
	for range 40 {
		srcs = append(srcs, `all(3000, 3000)`)
	}
	for _, k := range []string{"5", "700", "2999", "1500", "3000", "11"} {
		srcs = append(srcs, `all(3000, `+k+`)`)
	}
	r := jitDefaultRuns(t, treeGoOnSetup, srcs...)
	if r.jit.treeGoneOn == 0 {
		t.Fatal("no frame went on in its tree")
	}
}
