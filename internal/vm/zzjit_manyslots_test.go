//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// manySlotsSource is a function of n locals and a loop over them: numbers,
// strings and objects that change kind each turn, so that an exit writes a
// primitive over a reference and a reference over a primitive; a call of
// a function it passes locals to, whose state names every local written;
// a call of 40 arguments, for a deep operand stack; and a throw caught every
// 50th turn.
func manySlotsSource(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "function %s(rounds, seed) {\n", name)
	for i := range n {
		switch {
		case i%7 == 3:
			fmt.Fprintf(&b, "let v%d = {k: %d};\n", i, i)
		case i%11 == 5:
			fmt.Fprintf(&b, "let v%d = 's%d';\n", i, i)
		case i%13 == 2:
			fmt.Fprintf(&b, "let v%d = seed + %d;\n", i, i)
		default:
			fmt.Fprintf(&b, "let v%d = %d;\n", i, i)
		}
	}
	last, obj, str := n-1, 3, 5
	fmt.Fprintf(&b, "let s = 0;\nfor (let i = 0; i < rounds; i++) {\n")
	fmt.Fprintf(&b, "v0 += i; v%d = v%d + 2 * i;\n", last, last)
	fmt.Fprintf(&b, "v%d = (i & 1) ? 'x' + i : {k: i};\n", obj)
	fmt.Fprintf(&b, "v%d = (i & 2) ? i : 'y' + i;\n", str)
	fmt.Fprintf(&b, "v%d = v%d - 1;\n", n/2, n/2)
	fmt.Fprintf(&b, "s += helper(v%d, i, v%d);\n", n-2, obj)
	b.WriteString("s += wide(")
	for k := range 40 {
		if k > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "v%d", (k*n/40)%n)
	}
	b.WriteString(");\n")
	fmt.Fprintf(&b, "if (i %% 50 === 49) { try { thrower(i) } catch (e) { s += e.n } }\n")
	fmt.Fprintf(&b, "s += typeof v%d === 'string' ? v%d.length : v%d.k;\n", obj, obj, obj)
	fmt.Fprintf(&b, "s += typeof v%d === 'string' ? 1 : v%d;\n", str, str)
	b.WriteString("}\n")
	fmt.Fprintf(&b, "return [s, v0, v%d, v%d, String(v%d), String(v%d.k), v%d].join();\n}\n", last, n/2, str, obj+7, n-3)
	return b.String()
}

const manySlotsHelpers = `
	function helper(x, i, o) { return (typeof x === 'number' ? x & 7 : 3) + (typeof o === 'string' ? 1 : 2) + (i & 1) }
	function wide() { let t = 0; for (let k = 0; k < arguments.length; k++) { const a = arguments[k]; t += typeof a === 'number' ? a & 3 : 1 } return t }
	function thrower(i) { const e = new Error('e'); e.n = i & 15; throw e }
`

// Functions of more slots than 256 -- 300, 1,000 and 5,000 locals, and an
// operand stack past 256 -- are compiled and run natively, with the
// collector idle and marking, under small budgets and deoptimizations, and
// answer as the interpreter does.
func TestJITManySlots(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	sizes := []int{300, 1000, 5000}
	if testing.Short() {
		sizes = sizes[:2]
	}
	for _, n := range sizes {
		name := fmt.Sprintf("big%d", n)
		setup := manySlotsHelpers + manySlotsSource(name, n) + deepStackSource
		want := New(Config{})
		if _, err := want.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
		var answers []string
		for _, round := range []string{name + "(120, 1)", name + "(37, 2.5)", "deep(60)"} {
			v, err := want.Run(compileForTest(t, "''+("+round+")"))
			if err != nil {
				t.Fatal(err)
			}
			answers = append(answers, v.String().Go())
		}
		want.Close()
		want.ReleaseClosed()
		for _, stress := range []jitStressConfig{{}, {threshold: true, budget: 3, deopt: 7}} {
			for _, marking := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/stress=%v/marking=%v", n, stress != jitStressConfig{}, marking), func(t *testing.T) {
					if marking {
						jitMarkingForTest(t)
					}
					r := jitRuntimeForTest(t, Config{JIT: true})
					r.jitStress = stress
					// The compile of a function of 5,000 slots is past the
					// default budget's estimate (jitCompileBytes).
					r.jitMemoryLimit = 1 << 30
					if _, err := r.Run(compileForTest(t, setup)); err != nil {
						t.Fatal(err)
					}
					for range 2 {
						for i, round := range []string{name + "(120, 1)", name + "(37, 2.5)", "deep(60)"} {
							v, err := r.Run(compileForTest(t, "''+("+round+")"))
							if err != nil {
								t.Fatal(err)
							}
							if got := v.String().Go(); got != answers[i] {
								t.Fatalf("%s: got %s, interpreter %s", round, got, answers[i])
							}
						}
					}
					if stress == (jitStressConfig{}) {
						jitNativeForTest(t, r, name)
						jitNativeForTest(t, r, "deep")
					}
				})
			}
		}
	}
}

// deepStackSource is a function whose operand stack is past 256 deep: an
// array literal of 300 elements, every tenth a sum, in a loop.
var deepStackSource = func() string {
	var b strings.Builder
	b.WriteString("function deep(n) { let s = 0; for (let i = 0; i < n; i++) { const a = [")
	for k := range 300 {
		if k > 0 {
			b.WriteString(", ")
		}
		if k%10 == 0 {
			fmt.Fprintf(&b, "i + %d", k)
		} else {
			fmt.Fprintf(&b, "%d", k)
		}
	}
	b.WriteString("]; s += a[i % 300] + a.length } return s }\n")
	return b.String()
}()

// TestJITManySlotsCompileTime reports what compiling the functions of many
// slots takes, natively, the first call's run included.
func TestJITManySlotsCompileTime(t *testing.T) {
	if !jitSSABackend || testing.Short() {
		t.Skip("no SSA backend on this architecture, or short")
	}
	for _, n := range []int{300, 1000, 5000} {
		name := fmt.Sprintf("big%d", n)
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.jitMemoryLimit = 1 << 30
		if _, err := r.Run(compileForTest(t, manySlotsHelpers+manySlotsSource(name, n))); err != nil {
			t.Fatal(err)
		}
		call := compileForTest(t, name+"(1, 1)")
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		if _, err := r.Run(call); err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		runtime.ReadMemStats(&after)
		e := jitNativeForTest(t, r, name)
		t.Logf("%d locals: %d instructions, first call %v, %d MB allocated, estimate %d MB, compiled %d bytes", n, len(e.ssaProg.Code), took,
			(after.TotalAlloc-before.TotalAlloc)>>20, jitCompileBytes(jitFnForTest(t, r, name))>>20, e.compiledBytes())
	}
}
