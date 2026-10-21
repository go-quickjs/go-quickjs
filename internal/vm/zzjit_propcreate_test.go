//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strings"
	"testing"
)

// propCreateScript's additions, made in a function the JIT compiles --
// its keyed writes, of names and of indices, Go's from native code
// (jitKeyedWrite), its named ones along transitions natively -- answer
// what Node's do, round after round; and a hot loop's keyed writes to
// fresh objects, run natively, fill them as the interpreter does.
func TestJITPropertyCreationAgreesWithNode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const last = `out.join("\n");`
	if !strings.HasSuffix(strings.TrimSpace(propCreateScript), last) {
		t.Fatal("propCreateScript does not end as it did")
	}
	body := strings.TrimSuffix(strings.TrimSpace(propCreateScript), last)
	setup := "function run() {\n" + body + "\nreturn " + last + "\n}\n" + `
		function fill(n) {
			var objs = [];
			for (var j = 0; j < n; j++) {
				var o = {};
				o.a = j; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10;
				for (var i = 0; i < 10; i++) o[i] = i * j;
				o["s" + (j % 3)] = j;
				objs.push(o);
			}
			return objs;
		}
		function sum(objs) {
			var s = 0;
			for (var j = 0; j < objs.length; j++) { var ks = Object.keys(objs[j]); s += ks.length * 1000 + objs[j][9] + objs[j].a; }
			return s + ":" + Object.keys(objs[objs.length - 1]).join();
		}`
	r := jitAgreesForTest(t, setup, "run()", "run()", "run()", "run()",
		"sum(fill(500))", "sum(fill(500))", "sum(fill(500))")
	for i := range 2 {
		v, err := r.Run(compileForTest(t, "run()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != propCreateWant {
			t.Errorf("round %d: the JIT's answer differs from Node's:\n%s", i, got)
		}
	}
	// Node v26.10.0's answer.
	v, err := r.Run(compileForTest(t, "sum(fill(500))"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "11747500:0,1,2,3,4,5,6,7,8,9,a,b,c,d,e,f,g,h,i,j,s1"; got != want {
		t.Errorf("sum(fill(500)) = %s, want %s", got, want)
	}
	if e := jitNativeForTest(t, r, "fill"); len(e.keyedWrites) == 0 {
		t.Error("fill's keyed writes are not Go's from native code")
	}
}
