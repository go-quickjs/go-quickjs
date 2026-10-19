//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// arrowPoolSetup's functions make closures that capture none of their
// locals, in code whose captured locals are in its frame (ir.Program's
// Cells false): an arrow reading the this of a method called natively, as
// AWFY Havlak's findSet does; the same function called plainly, sloppy and
// strict, its this the global object and undefined; an arrow capturing
// its maker's own captured binding.
const arrowPoolSetup = `
	class Node { constructor(v) { this.v = v; this.parent = this } link(p) { this.parent = p } find(list) { list.forEach((x) => x.link(this.parent)); return this.v } }
	function each(a, f) { for (let i = 0; i < a.length; i++) f(a[i]) }
	var nodes = []; for (let i = 0; i < 8; i++) nodes.push(new Node(i))
	function finds(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + nodes[i & 7].find([nodes[(i + 1) & 7]])) | 0; return s }
	function sloppy() { return (() => this === globalThis)() }
	function strict() { 'use strict'; return (() => this === undefined)() }
	function sloppies(n) { let s = 0; for (let i = 0; i < n; i++) s += sloppy() ? 1 : 0; return s }
	function stricts(n) { let s = 0; for (let i = 0; i < n; i++) s += strict() ? 1 : 0; return s }
	function outer(k) { return function inner(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + (() => k * 2)() + i) | 0; return s } }
	var inner = outer(5)
`

// A closure that captures none of its maker's locals is taken from its
// pool natively (mir's closureTake) in code whose captured locals are in
// its frame as well, not only in code with cells of its own; an arrow's
// this is the context's This, which Go's entry writes for such code
// (jitEntry's lexThis). Each answer is the interpreter's, and the pools
// grow, as only native code's taking makes them.
func TestJITPooledArrowsWithoutCells(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{}
	for range 8 {
		rounds = append(rounds, `finds(200)`, `sloppies(100)`, `stricts(100)`, `inner(100)`)
	}
	r := jitAgreesForTest(t, arrowPoolSetup, rounds...)
	for _, name := range []string{"find", "inner", "sloppy", "strict"} {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.ssa == nil || e.ssaProg == nil || e.ssaProg.Cells {
			t.Errorf("%s has no code with its captured locals in its frame: %+v", name, e)
			continue
		}
		// A pool grows past its first size only as native code takes
		// from it (jitRefillPool).
		pools := 0
		for _, l := range e.literals {
			if l.tmpl.template() == nil {
				continue
			}
			pools++
			if l.pool.Size <= abi.PoolSize {
				t.Errorf("%s's closures at %d were not taken from their pool: its size is %d", name, l.pc, l.pool.Size)
			}
		}
		if pools == 0 {
			t.Errorf("%s makes no closure from a pool", name)
		}
	}
}
