//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// forInSetup's loops walk objects every way a for...in's cursor can: the
// keys of an object's own layout, which native code steps; a key deleted
// during the loop, which is not visited, and one added, which is not
// either; keys inherited from a prototype; an array's indices, then its
// other keys; a dictionary's; an object with no keys, null and undefined;
// a loop left by break, and loops within loops; a proxy's.
const forInSetup = `
	function P() { this.p = 1 } P.prototype.inherited = 2
	var shared = [{a: 1, b: 2, c: 3}, {a: 4, b: 5, c: 6}]
	function own(n) { var s = ""; for (var j = 0; j < n; j++) for (var k in shared[j & 1]) s += k; return s.length }
	function keys(o) { var s = []; for (var k in o) s.push(k); return s.join() }
	function deleting() { var o = {a: 1, b: 2, c: 3, d: 4}, s = ""; for (var k in o) { s += k; if (k === "a") delete o.c } return s }
	function adding() { var o = {a: 1, b: 2}, s = ""; for (var k in o) { s += k; o["x" + k] = 1 } return s }
	function dict() { var o = {z: 0, a: 1, b: 2}; delete o.z; return keys(o) }
	function early(o) { for (var k in o) { if (k === "b") return k } return "none" }
	function nested(o) { var s = ""; for (var k in o) for (var m in o) s += k + m; return s }
	var proxy = new Proxy({a: 1, b: 2}, {})
	function all() {
		return [keys({a: 1, b: 2, c: 3}), keys(new P()), keys([5, 6, 7]), keys(Object.assign([1, 2], {x: 3})),
			dict(), keys({}), keys(null), keys(undefined), keys("ab"), early({a: 1, b: 2, c: 3}), nested({a: 1, b: 2}),
			keys(proxy), deleting(), adding()].join("|")
	}`

func forInRounds() []string {
	var rounds []string
	for range 40 {
		rounds = append(rounds, `all()`, `own(200)`)
	}
	return rounds
}

// A for...in's steps over the keys of its object's layout are native
// code's (mir's forInStep), and its cursor a call of Go, not an exit: a
// loop over objects of a shared layout makes about one call of Go a loop,
// where it made one a key. Each answer is the interpreter's, for every
// kind of cursor, the ones native code leaves to Go among them.
func TestJITForIn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitForInChecked(t)
}

// TestJITForInMarking is TestJITForIn while the collector marks: the steps
// store no pointer, so they are the same.
func TestJITForInMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	jitForInChecked(t)
}

func jitForInChecked(t *testing.T) {
	t.Helper()
	r := jitAgreesForTest(t, forInSetup, forInRounds()...)
	before := r.jit.goCalls
	v, err := r.Run(compileForTest(t, `''+own(1000)`))
	if err != nil || v.String().Go() != "3000" {
		t.Fatalf("own(1000) = %v, %v", v, err)
	}
	// A thousand loops of three keys each: their cursors Go makes, their
	// steps native code.
	if n := r.jit.goCalls - before; n < 900 || n > 1500 {
		t.Errorf("own(1000) made %d calls of Go; want about one a loop, 1000, not one a key and one a loop, 4000", n)
	}
}
