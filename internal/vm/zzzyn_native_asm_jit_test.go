//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64)) && !quickjs_nativelayout

package vm

import (
	"os"
	"strings"
	"testing"
)

// TestAsmJITRunsGeneric runs testdata/asm_edges.js in a runtime with the
// JIT: what Node gives, and no closure of
// the module given typed code, which no JIT reads -- a runtime that runs
// the JIT runs the ordinary code, which it compiles.
func TestAsmJITRunsGeneric(t *testing.T) {
	src, err := os.ReadFile("testdata/asm_edges.js")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/asm_edges.want")
	if err != nil {
		t.Fatal(err)
	}
	w := strings.TrimRight(strings.ReplaceAll(string(want), "\r\n", "\n"), "\n")
	fn := compileForTest(t, string(src)+"\nasmEdges()")
	r := New(Config{JIT: true})
	defer r.Close()
	v, err := r.Run(fn)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(v.String().Go(), "\r\n", "\n"); got != w {
		t.Error("not what Node gives")
	}
	m, _ := r.GetProp(Obj(r.Global()), r.Intern("asmEdgesModule"))
	for _, name := range []string{"add", "wrap", "fib", "heapr", "calls"} {
		f, _ := r.GetProp(m, r.Intern(name))
		if cl := f.Object().fn().closure; cl.regTyped != nil {
			t.Errorf("%s was given typed code", name)
		}
	}
}
