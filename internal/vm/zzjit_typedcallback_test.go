//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// typedCallbackScript's methods, called from code the JIT compiles, round
// after round, answer what Node's do; and a hot loop of a typed array's
// forEach, map and reduce answers what the interpreter's does.
func TestJITTypedArrayCallbacksAgreeWithNode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := "function run() { return " + typedCallbackScript + "; }\n" + `
		function hot(n) {
			var ta = new Float64Array(64), s = 0;
			for (var i = 0; i < ta.length; i++) ta[i] = i / 4;
			for (var j = 0; j < n; j++) {
				ta.forEach(function (x, i) { s += x * i; });
				s += ta.map((x) => x + 1).reduce((a, b) => a + b, 0);
				if (ta.some((x) => x > 100)) s = -1;
			}
			return s;
		}`
	r := jitAgreesForTest(t, setup, "run()", "run()", "run()", "hot(300)", "hot(300)", "hot(300)")
	for i := range 2 {
		v, err := r.Run(compileForTest(t, "run()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != typedCallbackWant {
			diffLinesForTest(t, i, got, typedCallbackWant)
		}
	}
}
