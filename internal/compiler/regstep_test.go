package compiler

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// regStepOps is each step-and-branch the code holds, as "op k form" by
// pc order, and the partner after each.
func regStepOps(t *testing.T, f *bytecode.Function) []string {
	t.Helper()
	rc := f.RegCode()
	if rc == nil {
		t.Fatalf("%s has no register code", f.Name)
	}
	var out []string
	for pc, w := range rc.Code {
		partner, ok := w.Op().StepPartner()
		if !ok {
			continue
		}
		if !bytecode.StepPaired(rc.Code, pc) || rc.Code[pc+1].Op() != partner {
			t.Errorf("%s: %s at %d without its partner:\n%s", f.Name, w.Op(), pc, rc)
		}
		out = append(out, rc.Format(pc))
	}
	return out
}

// TestRegStepFused pins the peephole that fuses a step and the
// compare-and-branch after it into a step-and-branch: each shape a counted
// loop's bottom test takes -- a for loop's ++ and += k, a while's and a
// do-while's -- and --, each of the six compares, a jump to the step from
// continue, and what is left alone: a compare of the counter with itself,
// a step past X's 7 bits, an add of a negative literal and a subtraction
// of one, a compare that is not one of the six (if (++i < n) is a jnlt),
// a for loop compiled for a debugger, whose test's debug_stmt stands
// between its update and its test, and a function whose jumps are far. A
// while's or a do-while's --n >= 0 compiled for a debugger is one
// expression, with no place to stop between its step and its compare:
// fused as it is without one.
func TestRegStepFused(t *testing.T) {
	fns := regFunctions(t, `
		function up(n) { var s = 0; for (let i = 0; i < n; i++) s += i; return s }
		function upk(n) { var s = 0; for (var i = 0; i < n; i += 63) s += i; return s }
		function downk(n) { var s = 0; for (var i = n; i > 0; i -= 64) s += i; return s }
		function whileDec(n) { var c = 0; while (--n >= 0) c++; return c }
		function doDec(j) { var c = 0; do { c++ } while (--j > 0); return c }
		function le(n) { var c = 0; for (var i = 0; i <= n; i++) c++; return c }
		function imm() { var c = 0; for (var i = 0; i < 5; i++) c++; return c }
		function bound(a) { return function inner() { var c = 0; for (var u = 0; u <= a; u++) c++; return c } }
		function cont(n) { var c = 0; for (var i = 0; i < n; i++) { if (i & 1) continue; c++ } return c }
		function self() { for (var i = 0; i < i; i++) {} }
		function wide(n) { for (var i = 0; i < n; i += 64) {} for (var j = 0; j < n; j += 100) {} }
		function neg(n) { for (var i = 0; i < n; i += -1) {} for (var j = 0; j < n; j -= -1) {} }
		function cond(i, n) { if (++i < n) return 1; return 0 }`)
	want := map[string][]string{
		"up":       {"step_jlt r4 += 1 (update), r2 -> 3"},
		"upk":      {"step_jlt r4 += 63 (add), r2 -> 3"},
		"downk":    {"step_jgt_imm r4 += -64 (sub), 0 -> 3"},
		"whileDec": {"step_jge_imm r2 += -1 (update), 0 -> 2"},
		"doDec":    {"step_jgt_imm r2 += -1 (update), 0 -> 1"},
		"le":       {"step_jle r4 += 1 (update), r2 -> 3"},
		"imm":      {"step_jlt_imm r3 += 1 (update), 5 -> 3"},
		"inner":    {"step_jle_upval r3 += 1 (update), u0 -> 3"},
		"cont":     {"step_jlt r4 += 1 (update), r2 -> 3"},
		"self":     nil,
		"wide":     nil,
		"neg":      nil,
		"cond":     nil,
	}
	for name, w := range want {
		f := fns[name]
		if f == nil {
			t.Fatalf("no function %s", name)
		}
		got := regStepOps(t, f)
		if strings.Join(got, "\n") != strings.Join(w, "\n") {
			t.Errorf("%s: %q, want %q\n%s", name, got, w, f.RegCode())
		}
	}

	// Compiled for a debugger: a for loop's test has a debug_stmt of its
	// own, after the update; a while's test is one expression.
	src := `function up(n) { var s = 0; for (let i = 0; i < n; i++) s += i; return s }
		function whileDec(n) { var c = 0; while (--n >= 0) c++; return c }
		function doDec(j) { var c = 0; do { c++ } while (--j > 0); return c }`
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	top, err := Compile(prog, Options{Debug: true})
	if err != nil {
		t.Fatal(err)
	}
	debugWant := map[string]string{
		"up":       "",
		"whileDec": "step_jge_imm r2 += -1 (update), 0 -> 4",
		"doDec":    "step_jgt_imm r2 += -1 (update), 0 -> 3",
	}
	n := 0
	for _, c := range top.Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil || c.Fn.RegCode() == nil {
			continue
		}
		n++
		if got := strings.Join(regStepOps(t, c.Fn), "\n"); got != debugWant[c.Fn.Name] {
			t.Errorf("%s compiled for a debugger fuses %q, want %q:\n%s", c.Fn.Name, got, debugWant[c.Fn.Name], c.Fn.RegCode())
		}
	}
	if n != 3 {
		t.Errorf("%d functions compiled for a debugger have register code, want 3", n)
	}

	// A function whose jumps are far: each compare-and-branch is a branch
	// over a jmp, so that no step is followed by its compare's jump.
	var sb strings.Builder
	sb.WriteString("function far(n) { var c = 0; for (var i = 0; i < n; i++) {")
	for range 40000 {
		sb.WriteString(" c = c + 1;")
	}
	sb.WriteString(" } return c }")
	far := regFunctions(t, sb.String())["far"]
	if far == nil || far.RegCode() == nil {
		t.Fatal("far has no register code")
	}
	if got := regStepOps(t, far); len(got) != 0 {
		t.Errorf("far fuses %q", got)
	}
	if !strings.Contains(far.RegCode().String(), "inc ") {
		t.Errorf("far has no inc")
	}
}
