package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestGlobalLexicalShadowing covers globals read and written by functions
// whose sites remember the global object's slot, in a runtime whose scripts
// declare lexical bindings: only a binding of the global's own name may
// change where its reads and writes go, and one declared by a later script
// must be seen at once -- its value, its dead zone, and a const's refusal --
// whether the function runs as a tree or in the interpreter.
func TestGlobalLexicalShadowing(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	evalString(t, rt, `let unrelated = 1; const other = 2;
		globalThis.g = "prop"; globalThis.h = "prop";
		function readLoop() { var r; for (var i = 0; i < 3; i++) r = g; return r }
		function read() { return h }
		function writeLoop(v) { for (var i = 0; i < 3; i++) g = v }
		function write(v) { h = v }
		for (var i = 0; i < 3; i++) { readLoop(); read(); writeLoop("p" + i); write("q" + i) }`)
	if got := evalString(t, rt, `[readLoop(), read(), globalThis.g, globalThis.h].join()`); got != "p2,q2,p2,q2" {
		t.Errorf("before the lets: got %s", got)
	}
	evalString(t, rt, `let g = "lexG"; const h = "lexH"`)
	got := evalString(t, rt, `var r = [readLoop(), read()]; writeLoop("w");
		try { write("x") } catch (e) { r.push(e.name) }
		r.push(g, h, globalThis.g, globalThis.h); r.join()`)
	if got != "lexG,lexH,TypeError,w,lexH,p2,q2" {
		t.Errorf("after the lets: got %s", got)
	}
	evalString(t, rt, `globalThis.k = "prop"; function readK() { for (var i = 0; i < 2; i++) var v = k; return v } readK(); readK()`)
	got = evalString(t, rt, `var r = []; try { readK(); let k = 1 } catch (e) { r.push("same script " + e.name) } r.join()`)
	if got != "" {
		t.Errorf("a block's let is not global: got %s", got)
	}
	if _, err := rt.Eval(`readK(); let k = "late"`); err == nil {
		t.Errorf("reading k in its dead zone did not throw")
	}
}
