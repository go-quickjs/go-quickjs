//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
)

const cellPhiSetup = `
	function cells(n) {
		let x = 0, y = 1
		const inc = () => { x++ }, get = () => x, sety = v => { y = v }
		let s = 0
		for (let i = 0; i < n; i++) {
			if (i % 3 === 0) inc(); else if (i % 3 === 1) x += 2
			s += x + get()
			if (i % 5 === 0) sety(i)
			s += y
			for (let j = 0; j < 3; j++) { if (j === i % 3) y = y + j; s += y }
		}
		return s + x * 1000 + y
	}
	function outerCell() {
		let z = 0
		function w(v) { z = v }
		function loop(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				const a = z
				if (i % 4 === 0) w(i); else if (i % 4 === 2) z = z + 1
				s += a + z
			}
			return s
		}
		function only(n) { let s = 0; for (let i = 0; i < n; i++) { if (i & 1) s += z; else s -= z } return s }
		return [loop, only, w]
	}
	var L = outerCell()
	function counter() {
		let c = 0
		function down() { c-- }
		return {
			bump(k) { for (let i = 0; i < k; i++) { if (i & 1) c += i; else down() } return c },
			read: () => c,
		}
	}
	var C = counter()
`

// A phi whose arguments are all loads of one captured binding's cell is
// the cell's load (rematerializeUpvalues): the code's stores to the
// binding, a callee's -- inlined or called natively -- and a nested loop's
// on some of its paths, each between two of its loads, are all seen. Each
// answer is the interpreter's.
func TestJITCellPhisSeeStores(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, mode := range []string{"inlined", "called"} {
		t.Run(mode, func(t *testing.T) {
			gc := debug.SetGCPercent(-1)
			t.Cleanup(func() { debug.SetGCPercent(gc) })
			if mode == "called" {
				jitNoInlineForTest(t, "")
			}
			both := jitBothForTest(t, cellPhiSetup)
			var rounds []string
			for range 6 {
				rounds = append(rounds, `cells(100)`, `L[0](100)`, `L[1](50)`, `L[2](3)`, `C.bump(40) + ',' + C.read()`)
			}
			both.agree(t, rounds...)
			jitNativeForTest(t, both.jit, "cells")
			both.agree(t, `L[2](-7), L[1](20)`, `cells(1000)`, `L[0](1000)`, `C.bump(1000) + ',' + C.read()`)
		})
	}
}
