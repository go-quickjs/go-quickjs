//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64)) && !quickjs_nativelayout

package vm

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

// asmJITNative reports, for each function of the module object mod of r
// by its export's name, whether its typed code runs natively (its twin's
// code, asmTwin) or why not.
func asmJITNative(r *Runtime, mod string, names []string) map[string]string {
	out := map[string]string{}
	m, _ := r.GetProp(Obj(r.Global()), r.Intern(mod))
	for _, name := range names {
		f, _ := r.GetProp(m, r.Intern(name))
		if !f.IsObject() || f.Object().fn() == nil {
			out[name] = "no function"
			continue
		}
		cl := f.Object().fn().closure
		t := cl.regTyped
		switch {
		case t == nil || t.typedExt().asmCl != nil && t.typedExt().asmCl != cl:
			out[name] = "no typed code"
		case t.typedExt().twin == nil:
			out[name] = "no twin"
		default:
			e := t.typedExt().twin.closureCode.jitReg
			switch {
			case e == nil:
				out[name] = "not compiled"
			case e.code == nil:
				out[name] = "refused: " + e.refusal
			default:
				out[name] = "native"
			}
		}
	}
	return out
}

// TestAsmJIT runs testdata/asm_edges.js in a runtime with the JIT, as the
// JIT runs it and under its stress settings, with the collector idle and
// marking: what Node gives (testdata/asm_edges.want), the module's
// functions linked to typed code and compiled natively from it at the link
// -- before they run, with no warm-up -- by the JIT.
func TestAsmJIT(t *testing.T) {
	src, err := os.ReadFile("testdata/asm_edges.js")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/asm_edges.want")
	if err != nil {
		t.Fatal(err)
	}
	w := strings.TrimRight(strings.ReplaceAll(string(want), "\r\n", "\n"), "\n")
	for _, c := range []struct{ stress, marking bool }{{false, false}, {true, false}, {false, true}} {
		t.Run(fmt.Sprintf("stress=%v/marking=%v", c.stress, c.marking), func(t *testing.T) {
			if c.marking {
				jitMarkingForTest(t)
			}
			fn := compileForTest(t, string(src)+"\nasmEdges()")
			r := jitRuntimeForTest(t, Config{JIT: true})
			if c.stress {
				r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
			}
			v, err := r.Run(fn)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.ReplaceAll(v.String().Go(), "\r\n", "\n")
			if got != w {
				gl, wl := strings.Split(got, "\n"), strings.Split(w, "\n")
				for i := range min(len(gl), len(wl)) {
					if gl[i] != wl[i] {
						t.Errorf("line %d is %q, want %q", i+1, gl[i], wl[i])
						break
					}
				}
			}
			for name, how := range asmJITNative(r, "asmEdgesModule", []string{"wrap", "fib", "heapr", "calls"}) {
				if how != "native" {
					t.Errorf("%s: %s", name, how)
				}
			}
		})
	}
}

// TestAsmJITBackground runs testdata/asm_edges.js in a runtime that
// compiles in the background, as qjs --jit does: the link asks for each
// function's code there, from a snapshot of its feedback taken ahead
// (jitRegSnapshotOf), which asked of every operation of its program --
// typed code's raw operations among them, past the table of the
// operations' questions, which had ended at the last operation of
// ordinary code: the link's compile had panicked. Its answers are Node's
// (testdata/asm_edges.want), while the compiles run and once each is
// installed, and every function is then native.
func TestAsmJITBackground(t *testing.T) {
	src, err := os.ReadFile("testdata/asm_edges.js")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/asm_edges.want")
	if err != nil {
		t.Fatal(err)
	}
	w := strings.TrimRight(strings.ReplaceAll(string(want), "\r\n", "\n"), "\n")
	r := jitBackgroundRuntime(t)
	r.jitStress = jitStressConfig{background: true}
	for round := range 2 {
		v, err := r.Run(compileForTest(t, string(src)+"\nasmEdges()"))
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if got := strings.ReplaceAll(v.String().Go(), "\r\n", "\n"); got != w {
			t.Fatalf("round %d differs from Node:\n%s", round, firstDiff(got, w))
		}
		for _, j := range r.jit.regJobs {
			<-j.done
		}
		r.jitDrain()
	}
	for name, how := range asmJITNative(r, "asmEdgesModule", []string{"wrap", "fib", "heapr", "calls"}) {
		if how != "native" {
			t.Errorf("%s: %s", name, how)
		}
	}
}

// TestAsmJITTableCalls calls through a function table of four from typed
// code compiled at the link, from script calling it 200 times: the
// table's functions are its call's targets from that first compile
// (regTableTargets), each called natively, and no call through the table
// is Go's. Learned a call of Go at a time, each learning a compile again,
// they had been: 18 here, and most of a run of the micro-benchmarks'
// asm_table, 45 ns a call against 9 of the ordinary code's. The sum is
// exact: int32 arithmetic, as the module's.
func TestAsmJITTableCalls(t *testing.T) {
	const src = `
function TableCalls(stdlib, foreign, heap) {
    "use asm";
    var imul = stdlib.Math.imul;
    function f0(a, b) { a = a | 0; b = b | 0; return (a + b) | 0; }
    function f1(a, b) { a = a | 0; b = b | 0; return (a - b) | 0; }
    function f2(a, b) { a = a | 0; b = b | 0; return (a ^ b) | 0; }
    function f3(a, b) { a = a | 0; b = b | 0; return imul(a, b) | 0; }
    function run(n) {
        n = n | 0;
        var i = 0, s = 1;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0)
            s = ops[i & 3](s, (i + 7) | 0) | 0;
        return s | 0;
    }
    var ops = [f0, f1, f2, f3];
    return { run: run };
}
var tableCalls = TableCalls(globalThis, {}, new ArrayBuffer(65536));
`
	run := func(n int) int32 {
		s := int32(1)
		for i := int32(0); i < int32(n); i++ {
			b := i + 7
			switch i & 3 {
			case 0:
				s += b
			case 1:
				s -= b
			case 2:
				s ^= b
			case 3:
				s *= b
			}
		}
		return s
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	r.jitState().reporting = true
	v, _ := jitCountGoCalls(t, r, "var s = 0; for (var j = 0; j < 200; j++) s = (s + tableCalls.run(1024)) | 0; ''+s")
	want := int32(0)
	for range 200 {
		want += run(1024)
	}
	if got := v.String().Go(); got != fmt.Sprint(want) {
		t.Fatalf("the sum is %s, want %d", got, want)
	}
	for op, n := range r.RegNativeStats().GoCallOps {
		if strings.HasPrefix(op, "call_w") {
			t.Errorf("%d calls of Go at call_w: %s", n, op)
		}
	}
	for name, how := range asmJITNative(r, "tableCalls", []string{"run"}) {
		if how != "native" {
			t.Errorf("%s: %s", name, how)
		}
	}
}

// TestAsmJITMicroKernels runs the micro-benchmarks' asm.js kernels with
// the JIT, its stress settings and the collector marking: each the value
// Node gives (TestAsmMicroKernels), every kernel native, and callLeaf's
// add2, a typed callee nothing inside of which exits, inlined in it, as
// the ordinary code's is (ssa's typedInline): called natively, a call
// had cost twice what the ordinary code's inlined one does.
func TestAsmJITMicroKernels(t *testing.T) {
	src, err := os.ReadFile("../v8bench/microbench_extra.js")
	if err != nil {
		t.Fatal(err)
	}
	want := "asm_adler:961802452 asm_crc32:-1170694714 asm_copy:480901226 asm_fnv:-792937590 " +
		"asm_dot:180534272 asm_fib:2585 asm_call_leaf:4190208 asm_switch:-516120 asm_table:1550913536 call_cross:33550336"
	var calls []string
	for _, w := range strings.Fields(want) {
		name, _, _ := strings.Cut(w, ":")
		calls = append(calls, fmt.Sprintf("%q + ':' + (%s(8192), global_res)", name, name))
	}
	prog := "var global_res;\n" + string(src) + "\nvar asmMicro = extra_asm_kernels();\n[" + strings.Join(calls, ", ") + "].join(' ')"
	for _, c := range []struct{ stress, marking bool }{{false, false}, {true, false}, {false, true}} {
		t.Run(fmt.Sprintf("stress=%v/marking=%v", c.stress, c.marking), func(t *testing.T) {
			if c.marking {
				jitMarkingForTest(t)
			}
			inlined := jitInlinedForTest(t)
			fn := compileForTest(t, prog)
			r := jitRuntimeForTest(t, Config{JIT: true})
			if c.stress {
				r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
			}
			v, err := r.Run(fn)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != want {
				t.Errorf("got %s", got)
			}
			if n := len(inlined["callLeaf"]); n != 1 {
				t.Errorf("callLeaf inlined %d callees, want 1 (add2)", n)
			}
			for name, how := range asmJITNative(r, "asmMicro", []string{"adler", "crc", "copy", "fnv", "dot", "fib", "callLeaf", "table"}) {
				if how != "native" {
					t.Errorf("%s: %s", name, how)
				}
			}
		})
	}
}

// TestAsmJITStackPositions runs asmPosSrc with the JIT: its functions'
// native code, compiled from their typed code, gives every frame of the
// module in an error's stack where the ordinary code puts it
// (TestAsmStackPositions), and the stack overflows where it does.
func TestAsmJITStackPositions(t *testing.T) {
	want, _ := asmRun(t, asmPosSrc, "posRun()", true, false, "posModule", "outer")
	for _, stress := range []bool{false, true} {
		fn := compileForTest(t, asmPosSrc+"\nposRun()")
		r := jitRuntimeForTest(t, Config{JIT: true})
		if stress {
			r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
		}
		v, err := r.Run(fn)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.ReplaceAll(v.String().Go(), "\r\n", "\n"); got != want {
			t.Errorf("stress %v: stacks\n%s\nwant\n%s", stress, got, want)
		}
		for name, how := range asmJITNative(r, "posModule", []string{"outer", "thrower", "deep"}) {
			if how != "native" {
				t.Errorf("stress %v: %s: %s", stress, name, how)
			}
		}
	}
}

// TestAsmJITOps runs testdata/asm_ops.js -- every typed operation over
// edge operands, the heap's every view in and out of its bounds -- with
// the JIT, under its stress settings and with the collector marking, and
// compares each answer exactly with the typed interpreter's and with the
// ordinary code's, each Node's (testdata/asm_ops.want), the module
// validated and its functions native.
func TestAsmJITOps(t *testing.T) {
	src, err := os.ReadFile("testdata/asm_ops.js")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/asm_ops.want")
	if err != nil {
		t.Fatal(err)
	}
	typed, ran := asmRun(t, string(src), "asmOps()", true, true, "asmOpsModule", "iop")
	if !ran {
		t.Fatal("the module did not run typed")
	}
	if w := strings.TrimRight(strings.ReplaceAll(string(want), "\r\n", "\n"), "\n"); typed != w {
		t.Fatalf("typed code differs from Node (testdata/asm_ops.want):\n%s", firstDiff(typed, w))
	}
	plain, _ := asmRun(t, string(src), "asmOps()", true, false, "asmOpsModule", "iop")
	if typed != plain {
		t.Fatalf("typed and ordinary code differ:\n%s", firstDiff(typed, plain))
	}
	was := jitEncoding.Round
	t.Cleanup(func() { jitEncoding.Round = was })
	for _, c := range []struct{ stress, marking, noRound bool }{{false, false, false}, {true, false, false}, {false, true, false}, {false, false, true}} {
		t.Run(fmt.Sprintf("stress=%v/marking=%v/noRound=%v", c.stress, c.marking, c.noRound), func(t *testing.T) {
			if c.marking {
				jitMarkingForTest(t)
			}
			// A CPU without SSE4.1's rounding instruction, as amd64's code
			// is made for one (abi.Encoding's Round): Math.floor's code
			// SSE2's, and % an integer instruction all the same.
			jitEncoding.Round = was && !c.noRound || runtime.GOARCH == "arm64"
			fn := compileForTest(t, string(src)+"\nasmOps()")
			r := jitRuntimeForTest(t, Config{JIT: true})
			if c.stress {
				r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
			}
			v, err := r.Run(fn)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.ReplaceAll(v.String().Go(), "\r\n", "\n"); got != typed {
				t.Errorf("native code differs from the typed interpreter:\n%s", firstDiff(got, typed))
			}
			for name, how := range asmJITNative(r, "asmOpsModule", []string{"iop", "ibr", "dop", "dbr", "store", "load", "loadOff", "globals", "loop"}) {
				if how != "native" {
					t.Errorf("%s: %s", name, how)
				}
			}
		})
	}
}

// firstDiff is the first line where got and want differ, of each.
func firstDiff(got, want string) string {
	gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := range min(len(gl), len(wl)) {
		if gl[i] != wl[i] {
			g, w := gl[i], wl[i]
			for j := range min(len(g), len(w)) {
				if g[j] != w[j] {
					lo := max(j-80, 0)
					return fmt.Sprintf("line %d:\n  got  ...%.200s\n  want ...%.200s", i+1, g[lo:], w[lo:])
				}
			}
			return fmt.Sprintf("line %d: %q vs %q", i+1, g, w)
		}
	}
	return fmt.Sprintf("%d lines against %d", len(gl), len(wl))
}
