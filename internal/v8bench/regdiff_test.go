package v8bench

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// The register tier's differential tests (docs/register-interpreter.md,
// "Raw int32 registers"): the same programs run with the register tier off
// and on, each in runtimes of their own, and every result compared. The
// register code's correctness rests on its verifier for memory and on these
// for meaning: what a raw int32 register holds must be what the Value the
// stack form keeps would be, at every overflow, -0, NaN and shift.

// withRegTier runs f with the register backend and tier set to on, and puts
// back what the environment says after.
func withRegTier(on bool, f func()) {
	compiler.SetRegisterForm(on)
	vm.SetRegTier(on)
	defer func() {
		env := os.Getenv("QJS_REG") != ""
		compiler.SetRegisterForm(env)
		vm.SetRegTier(env)
	}()
	f()
}

// regDiffRun evaluates src in a new runtime and then each of calls, and
// returns each call's result as a string, or its error's.
func regDiffRun(t *testing.T, src string, calls []string) []string {
	t.Helper()
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(src); err != nil {
		t.Fatalf("eval: %v", err)
	}
	out := make([]string, len(calls))
	for i, c := range calls {
		v, err := rt.Eval(c)
		if err != nil {
			out[i] = "error: " + err.Error()
			continue
		}
		out[i] = v.String()
	}
	return out
}

// regDiff runs src and calls with the register tier off, twice, and on, and
// fails where the tier on answers otherwise. A call whose two answers with
// the tier off differ -- a clock, Math.random -- is not compared. It
// returns how many functions the tier gave register code.
func regDiff(t *testing.T, src string, calls []string) int64 {
	t.Helper()
	var off, again, on []string
	withRegTier(false, func() { off = regDiffRun(t, src, calls) })
	withRegTier(false, func() { again = regDiffRun(t, src, calls) })
	before := vm.RegsBuilt()
	withRegTier(true, func() { on = regDiffRun(t, src, calls) })
	built := vm.RegsBuilt() - before
	bad, skipped := 0, 0
	defer func() {
		t.Logf("%d calls compared, %d not (their answers vary), %d functions as register code",
			len(calls)-skipped, skipped, built)
	}()
	for i := range calls {
		if off[i] != again[i] {
			skipped++
			continue
		}
		if off[i] == on[i] {
			continue
		}
		if bad++; bad <= 20 {
			t.Errorf("%s:\n  register tier off: %.300s\n  register tier on:  %.300s", calls[i], off[i], on[i])
		}
	}
	if bad > 20 {
		t.Errorf("and %d more", bad-20)
	}
	return built
}

// regDiffResult is the expression a call is compared by: what it returns
// and what it left in global_res, as text.
func regDiffResult(call string) string {
	return fmt.Sprintf(`(function () { var r = %s; var g = global_res; try { g = JSON.stringify(g) } catch (e) { g = String(g) } return String(r) + " / " + g })()`, call)
}

func TestRegDiffMicrobenchmarks(t *testing.T) {
	if testing.Short() {
		t.Skip("runs every micro-benchmark three times")
	}
	end := strings.LastIndex(microbench, "\nif (typeof scriptArgs === \"undefined\")")
	if end < 0 {
		t.Fatal("microbench.js: no call of main to leave out")
	}
	src := `if (typeof console === "undefined") globalThis.console = { log: function () {} };
if (typeof performance === "undefined") globalThis.performance = { now: function () { return 0 } };
var global_res;
` + microbench[:end+1] + microbenchExtra
	calls := make([]string, 0, len(microWork))
	for _, w := range microWork {
		// A little of each test's work: enough for its loops to turn and
		// its functions to run as each tier runs them.
		n := max(w.n/4096, 1)
		calls = append(calls, regDiffResult(fmt.Sprintf("%s(%d)", w.name, n)))
	}
	if built := regDiff(t, src, calls); built < 100 {
		t.Errorf("only %d functions ran as register code", built)
	}
}

func TestRegDiffV8Suite(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the V8 suite three times")
	}
	dir := os.Getenv("QUICKJS_V8_DIR")
	if dir == "" {
		t.Skip("QUICKJS_V8_DIR names no checkout of the V8 suite (v8bench -fetch)")
	}
	var src strings.Builder
	for _, f := range suites["v8"].files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Skip(err)
		}
		src.Write(b)
		src.WriteString("\n")
	}
	// Each benchmark run three times, its own checks of its answers on --
	// Richards' counts, DeltaBlue's projections, Crypto's decryption,
	// NavierStokes' checksum, Splay's tree -- and what it leaves to see
	// compared: the error a check throws, if any.
	src.WriteString(`
function regDiffBench(name) {
	var out = [];
	BenchmarkSuite.suites.forEach(function (s) {
		s.benchmarks.forEach(function (b) {
			if (b.name !== name) return;
			try {
				b.Setup();
				for (var i = 0; i < 3; i++) out.push(String(b.run()));
				b.TearDown();
			} catch (e) { out.push("threw " + e) }
		});
	});
	return out.join(",");
}
var global_res;
`)
	var calls []string
	for _, name := range []string{"Richards", "DeltaBlue", "Encrypt", "Decrypt", "RayTrace", "EarleyBoyer", "RegExp", "Splay", "NavierStokes"} {
		calls = append(calls, fmt.Sprintf("regDiffBench(%q)", name))
	}
	if built := regDiff(t, src.String(), calls); built < 100 {
		t.Errorf("only %d functions ran as register code", built)
	}
}

// TestRegDiffInt32 compares generated integer arithmetic: functions of
// loops over locals that bitwise operators, shifts and | 0 keep int32s,
// and sums, differences and products that leave them -- past 2^31, to -0,
// to NaN -- with shifts by 32 and more, >>> 0, and doubles mixed in.
func TestRegDiffInt32(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	consts := []string{"0", "1", "-1", "2", "7", "31", "32", "33", "64", "255", "0x3fff", "0xfffffff",
		"0x7fffffff", "-0x80000000", "0x80000000", "0xffffffff", "4294967296", "-0", "0.5", "-1.5",
		"1e10", "NaN", "Infinity", "65535", "65536", "-65536", "46341", "-46341"}
	ops := []string{"+", "-", "*", "&", "|", "^", "<<", ">>", ">>>", "/", "%"}
	var src strings.Builder
	var calls []string
	vars := []string{"a", "b", "c", "d"}
	operand := func() string {
		if rnd.Intn(3) == 0 {
			return consts[rnd.Intn(len(consts))]
		}
		return vars[rnd.Intn(len(vars))]
	}
	var expr func(depth int) string
	expr = func(depth int) string {
		if depth == 0 || rnd.Intn(4) == 0 {
			return operand()
		}
		e := "(" + expr(depth-1) + " " + ops[rnd.Intn(len(ops))] + " " + expr(depth-1) + ")"
		switch rnd.Intn(6) {
		case 0:
			e = "(" + e + " | 0)"
		case 1:
			e = "(" + e + " >>> 0)"
		case 2:
			e = "(" + e + " & 0x3fff)"
		}
		return e
	}
	cmps := []string{"<", "<=", ">", ">=", "==", "!=", "===", "!=="}
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("g%d", i)
		fmt.Fprintf(&src, "function %s(a, b, n) {\n\tvar c = %s, d = %s, t = [], out = [];\n", name, operand(), operand())
		fmt.Fprintf(&src, "\tfor (var i = 0; i < 8; i++) t[i] = i * 3;\n")
		fmt.Fprintf(&src, "\tfor (var j = 0; j < n; j++) {\n")
		for k := 0; k < 3; k++ {
			v := vars[rnd.Intn(len(vars))]
			switch rnd.Intn(5) {
			case 0:
				fmt.Fprintf(&src, "\t\t%s = %s;\n", v, expr(3))
			case 1:
				fmt.Fprintf(&src, "\t\t%s = (%s + j) | 0;\n", v, v)
			case 2:
				fmt.Fprintf(&src, "\t\t%s = t[j & 7] + %s;\n", v, expr(2))
			case 3:
				fmt.Fprintf(&src, "\t\tif (%s %s %s) %s++; else %s--;\n", expr(2), cmps[rnd.Intn(len(cmps))], expr(1), v, v)
			default:
				fmt.Fprintf(&src, "\t\tt[(j + %d) & 7] = %s;\n", rnd.Intn(8), expr(2))
			}
		}
		fmt.Fprintf(&src, "\t\tout.push(a, b, c, d);\n\t}\n")
		fmt.Fprintf(&src, "\treturn out.map(function (x) { return Object.is(x, -0) ? \"-0\" : String(x) }).join() + \"|\" + t.join();\n}\n")
		for _, args := range [][2]string{{"1", "2"}, {"0x7ffffff0", "-0x7ffffff0"}, {"-0", "0.25"}, {"46340", "46341"}, {"NaN", "3"}} {
			calls = append(calls, fmt.Sprintf("%s(%s, %s, 6)", name, args[0], args[1]))
		}
	}
	if built := regDiff(t, src.String(), calls); built < 300 {
		t.Errorf("only %d functions ran as register code", built)
	}
}
