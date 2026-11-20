//go:build amd64 || arm64

package vm

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// asmRun runs src, then the expression call, in a new runtime: by the
// native handlers or the Go table, with typed code or without, and gives
// its value and whether the export name of the global module object mod
// ran typed code.
func asmRun(t *testing.T, src, call string, native, typed bool, mod, export string) (string, bool) {
	t.Helper()
	fn := compileForTest(t, src+"\n"+call)
	wasNative, wasTyped := SetRegNative(native), SetRegTyped(typed)
	defer func() { SetRegNative(wasNative); SetRegTyped(wasTyped) }()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	v, err := r.Run(fn)
	if err != nil {
		t.Fatalf("native %v, typed %v: %v", native, typed, err)
	}
	if r.frameDepth != 0 {
		t.Fatalf("native %v, typed %v: frame depth %d left", native, typed, r.frameDepth)
	}
	ran := false
	if m, _ := r.GetProp(Obj(r.Global()), r.Intern(mod)); m.IsObject() {
		if f, _ := r.GetProp(m, r.Intern(export)); f.IsObject() && f.Object().fn() != nil {
			cl := f.Object().fn().closure
			ran = cl.regTyped != nil && cl.regTyped.typedExt().asmCl == cl
		}
	}
	return strings.ReplaceAll(v.String().Go(), "\r\n", "\n"), ran
}

// TestAsmEdges runs testdata/asm_edges.js -- asm.js modules over the edges
// of their types: int32 wrapping, division and remainder by zero and of
// MinInt32 by -1, signed and unsigned shifts and compares, -0, NaN, the
// conversions, fround, Math, the heap's every view in and out of its
// bounds, module variables, calls through tables and imports whose results
// run valueOf or throw, the stack's limit, a heap detached by an import,
// and a module that fails validation -- with typed code natively, as
// while the collector marks, and by the Go table, and without typed code:
// each what Node gives (testdata/asm_edges.want), and the module typed
// where it should be.
func TestAsmEdges(t *testing.T) {
	src, err := os.ReadFile("testdata/asm_edges.js")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/asm_edges.want")
	if err != nil {
		t.Fatal(err)
	}
	w := strings.TrimRight(strings.ReplaceAll(string(want), "\r\n", "\n"), "\n")
	for _, c := range []struct {
		native, typed, marking bool
	}{{true, true, false}, {true, true, true}, {false, true, false}, {true, false, false}, {false, false, false}} {
		nativeMarking(t, c.marking)
		got, ran := asmRun(t, string(src), "asmEdges()", c.native, c.typed, "asmEdgesModule", "wrap")
		if got != w {
			gl, wl := strings.Split(got, "\n"), strings.Split(w, "\n")
			for i := range min(len(gl), len(wl)) {
				if gl[i] != wl[i] {
					t.Errorf("%+v: line %d is %q, want %q", c, i+1, gl[i], wl[i])
					break
				}
			}
			if len(gl) != len(wl) {
				t.Errorf("%+v: %d lines, want %d", c, len(gl), len(wl))
			}
		}
		if ran != c.typed {
			t.Errorf("%+v: the module ran typed %v", c, ran)
		}
	}
	nativeMarking(t, false)
}

// TestAsmMicroKernels runs the micro-benchmarks' asm.js kernels and
// call_cross with typed code natively, by the Go table and without typed
// code: each the value Node and C QuickJS give for the same call (the
// v8bench runner's TestAsmMicrobenchmarks), and the module typed where it
// should be.
func TestAsmMicroKernels(t *testing.T) {
	src, err := os.ReadFile("../v8bench/microbench_extra.js")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"asm_adler:961802452", "asm_crc32:-1170694714", "asm_copy:480901226", "asm_fnv:-792937590",
		"asm_dot:180534272", "asm_fib:2585", "asm_call_leaf:4190208", "asm_switch:-516120",
		"asm_table:1550913536", "call_cross:33550336",
	}
	var calls []string
	for _, w := range want {
		name, _, _ := strings.Cut(w, ":")
		calls = append(calls, fmt.Sprintf("%q + ':' + (%s(8192), global_res)", name, name))
	}
	call := "[" + strings.Join(calls, ", ") + "].join(' ')"
	for _, c := range []struct{ native, typed bool }{{true, true}, {false, true}, {true, false}, {false, false}} {
		got, ran := asmRun(t, "var global_res;\n"+string(src)+"\nvar asmMicro = extra_asm_kernels();", call, c.native, c.typed, "asmMicro", "adler")
		if got != strings.Join(want, " ") {
			t.Errorf("%+v: got %s", c, got)
		}
		if ran != c.typed {
			t.Errorf("%+v: the kernels ran typed %v", c, ran)
		}
	}
}
