//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// cellsCloseSetup's loops make closures capturing their iteration's
// bindings -- consts of a number and of an object, a let a closure assigns
// -- beside a let of the function's that every iteration's closures share,
// as AWFY Havlak's findLoops does: in a function Go enters (loops), in a
// native callee (callee, from drive's loop), and in one whose loop leaves
// native code past its first iterations (exits: a read of an object of
// another shape), the interpreter going on with the cells native code
// made; and a loop's own let, an object, which each iteration's binding
// starts with (links).
const cellsCloseSetup = `
	function loops(n) {
		const fs = []
		let total = 0
		for (let i = 0; i < n; i++) {
			const k = i * 2
			let m = i
			const o = { v: i }
			fs.push(() => { total += k + o.v; m++; return m + k })
			if (i % 3 === 0) fs[fs.length - 1]()
		}
		let s = 0
		for (let j = 0; j < fs.length; j++) s += fs[j]()
		return s + ':' + total
	}
	function callee(n) {
		const fs = []
		let total = 0
		for (let i = 0; i < n; i++) {
			const k = i + 1
			fs.push(() => { total += k; return k })
		}
		let s = 0
		for (let j = 0; j < fs.length; j++) s += fs[j]()
		return s + total
	}
	var head = null
	for (let i = 0; i < 300; i++) head = { v: i, next: head }
	function links(n) {
		const fs = []
		let total = 0
		let j = 0
		for (let p = head; p && j < n; p = p.next, j++) fs.push(() => { total += p.v; return p.v })
		let s = 0
		for (let i = 0; i < fs.length; i++) s += fs[i]()
		return s + ':' + total
	}
	function drive(n) { let s = 0; for (let j = 0; j < 8; j++) s += callee(n); return s }
	var shapes = []
	for (let i = 0; i < 300; i++) shapes.push({ a: i })
	function exits(n, odd) {
		const fs = []
		let total = 0
		for (let i = 0; i < n; i++) {
			const k = i * 3
			fs.push(() => { total += k; return k })
			total += (i === n - 5 && odd ? { b: 1, a: 7 } : shapes[i]).a
		}
		let s = 0
		for (let j = 0; j < fs.length; j++) s += fs[j]()
		return s + ':' + total
	}
`

// A scope's end in code whose captured locals are in cells of their own
// gives its locals fresh cells natively (mir's cellsClose), from the
// function's pool, as V8's code allocates a block's context at each
// iteration, where Go had made each -- on the frame Go entered the code
// with too, whose cells Go opens on its slots again once native code
// leaves it (jitReopenRunCells). Each answer is the interpreter's, and
// loops' pool grew, as only native code's taking makes it.
func TestJITCellsClosed(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTest(t, cellsCloseSetup, cellsCloseRounds()...)
	jitCellsClosedFor(t, r)
}

// TestJITCellsClosedMarking is TestJITCellsClosed while the collector
// marks (mir's cellsCloseMarking), every register native code may keep
// given junk across each store (jit.StoreRefClobber).
func TestJITCellsClosedMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	r := jitAgreesForTest(t, cellsCloseSetup, cellsCloseRounds()...)
	jitCellsClosedFor(t, r)
}

func cellsCloseRounds() []string {
	rounds := []string{}
	for i := range 8 {
		odd := "false"
		if i >= 6 {
			odd = "true"
		}
		rounds = append(rounds, `loops(200)`, `drive(50)`, `exits(200, `+odd+`)`, `links(200)`)
	}
	return rounds
}

func jitCellsClosedFor(t *testing.T, r *Runtime) {
	t.Helper()
	for _, name := range []string{"loops", "callee", "exits", "links"} {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.ssa == nil || e.ssaProg == nil || !e.ssaProg.Cells {
			t.Errorf("%s has no code with cells of its own", name)
			continue
		}
		if (name == "loops" || name == "links") && (e.cellsPool == nil || e.cellsPool.Size <= abi.PoolSize) {
			t.Errorf("%s's cells were not taken from their pool: %+v", name, e.cellsPool)
		}
	}
}
