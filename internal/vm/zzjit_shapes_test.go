//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// shapesSetup's loops read an object's properties before and after
// something that may change its shape -- a delete by a key the loop is
// given, through the object or another name for it; a call, which may make
// a property a getter; a keyed write, which may add one -- compiled while
// it changes nothing (a key the object does not have, a function that does
// nothing, a key it has), then run where it does; and sums, which only
// reads; readd deletes a property and adds it again between two reads of
// it.
const shapesSetup = `
function rd(o, k, n) { let s = 0; for (let i = 0; i < n; i++) { s += o.a; delete o[k]; s += o.c * 2 + o.a } return s }
function alias(a, b, k, n) { let s = 0; for (let i = 0; i < n; i++) { s += a.a; delete b[k]; s += a.c * 2 + a.a } return s }
function viaCall(o, f, n) { let s = 0; for (let i = 0; i < n; i++) { s += o.a; f(o); s += o.c * 2 + o.a } return s }
function adds(o, k, n) { let s = 0; for (let i = 0; i < n; i++) { s += o.a; o[k] = i; s += o.c * 2 + o.a } return s + ':' + Object.keys(o).join() }
function readd(o, k, k2, n) { let s = 0; for (let i = 0; i < n; i++) { s += o.a; delete o[k]; o[k2] = 7; s += o.a * 2 + o.c } return s + ':' + Object.keys(o).join() }
function mk() { return { a: 1, b: 2, c: 3, d: 4 } }
function noop(o) {}
function toGetter(o) { Object.defineProperty(o, 'c', { get() { return 100 }, configurable: true }) }
function sums(p, n) { let s = 0; for (let i = 0; i < n; i++) s += p.x * p.y + p.x - p.y + p.z + p.x; return s }
var o1 = mk()
`

// A property's read of the shape a read before it checked, of the same
// object with nothing between them that may change a shape, checks
// nothing (ssa's knownShapes); anything that may -- a delete, a call, a
// keyed write -- has the reads after it check again. Each loop is compiled
// where nothing changes, then given what changes its object's shape at
// once; each answer is Node's (v26.10.0) and the interpreter's.
func TestJITShapesKnown(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, want := jitAnswersForTest(t, shapesSetup)
	for range 20 {
		want(`rd(mk(), 'zz', 300)`, "2400")
		want(`alias(o1, o1, 'zz', 300)`, "2400")
		want(`viaCall(mk(), noop, 300)`, "2400")
		want(`adds(mk(), 'a', 300)`, "91202:a,b,c,d")
		want(`sums({ x: 2, y: 3, z: 4 }, 300)`, "3300")
		want(`readd(mk(), 'zz', 'b', 300)`, "1800:a,b,c,d")
	}
	for _, name := range []string{"rd", "alias", "viaCall", "adds", "sums", "readd"} {
		jitNativeForTest(t, r, name)
	}
	want(`rd(mk(), 'b', 300)`, "2400")
	want(`rd(mk(), 'a', 300)`, "NaN")
	want(`alias(o1, o1, 'b', 300)`, "2400")
	want(`viaCall(mk(), toGetter, 300)`, "60600")
	want(`adds(mk(), 'q', 300)`, "2400:a,b,c,d,q")
	// Deleted and added again, a property is at another place of its
	// object's table: read where it had been, it would be the hole left.
	want(`readd(mk(), 'a', 'a', 300)`, "7194:b,c,d,a")
}
