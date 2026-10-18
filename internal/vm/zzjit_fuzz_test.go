//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// jitProgram writes a JavaScript program from a byte string, as a fuzzer
// mutates it: the bytes choose among the operations the JIT compiles, and
// the values at their edges. Every effect a callback has -- a getter, a
// setter, valueOf, a helper call -- goes into a log, and the program's value
// is that log with what each call returned and left behind, so two engines
// that differ in anything observable give different strings.
type jitProgram struct {
	data []byte
	pos  int
	b    strings.Builder
}

func (g *jitProgram) next() int {
	if g.pos >= len(g.data) {
		return 0
	}
	g.pos++
	return int(g.data[g.pos-1])
}

func (g *jitProgram) pick(n int) int { return g.next() % n }

var jitFuzzNumbers = []string{"0", "-0", "1", "-1", "0.5", "3", "7", "2147483647", "2147483648", "4294967297",
	"-2147483649", "NaN", "Infinity", "-Infinity", "1e300", "5e-324", "9007199254740993", "-1.5"}

// number is a numeric literal, parenthesized when negative so that it can
// follow any operator.
func (g *jitProgram) number() string {
	n := jitFuzzNumbers[g.pick(len(jitFuzzNumbers))]
	if strings.HasPrefix(n, "-") {
		return "(" + n + ")"
	}
	return n
}

// value is an element or property value: mostly numbers, sometimes not.
func (g *jitProgram) value() string {
	switch g.pick(16) {
	case 0:
		return "'3'"
	case 1:
		return "undefined"
	case 2:
		return "null"
	case 3:
		return "true"
	case 4:
		return "{valueOf(){log.push('v');return 2}}"
	case 5:
		return "5n"
	}
	return g.number()
}

func (g *jitProgram) index() string {
	return []string{"i", "i", "i+1", "i-1", "(i*3)&7", "n-i", "i>>1", "0"}[g.pick(8)]
}

func (g *jitProgram) expr(depth int) string {
	k := g.pick(10)
	if depth >= 2 && k > 5 {
		k %= 6
	}
	switch k {
	case 0:
		return "s"
	case 1:
		return "t"
	case 2:
		return "i"
	case 3:
		return g.number()
	case 4:
		return "a[" + g.index() + "]"
	case 5:
		return "o.x"
	case 6:
		ops := []string{"+", "-", "*", "/", "%", "&", "|", "^", "<<", ">>", ">>>"}
		return "(" + g.expr(depth+1) + " " + ops[g.pick(len(ops))] + " " + g.expr(depth+1) + ")"
	case 7:
		return []string{"-", "~", "+", "!"}[g.pick(4)] + "(" + g.expr(depth+1) + ")"
	case 8:
		cmp := []string{"<", "<=", ">", ">=", "==", "===", "!=", "!=="}
		return "(" + g.expr(depth+1) + " " + cmp[g.pick(len(cmp))] + " " + g.expr(depth+1) + " ? " + g.expr(depth+1) + " : " + g.expr(depth+1) + ")"
	default:
		return "a.length"
	}
}

func (g *jitProgram) statement(depth int) string {
	switch g.pick(9) {
	case 0:
		ops := []string{"+", "-", "*", "|", "^", "&", ">>>"}
		return "s=s " + ops[g.pick(len(ops))] + " " + g.expr(0) + ";"
	case 1:
		return "t=" + g.expr(0) + ";"
	case 2:
		return "a[" + g.index() + "]=" + g.expr(0) + ";"
	case 3:
		return "s+=a[" + g.index() + "];"
	case 4:
		return "o.x=" + g.expr(0) + ";"
	case 5:
		return "s=h(s,i);"
	case 6:
		if depth < 1 {
			return "if(" + g.expr(0) + "){" + g.statement(depth+1) + "}else{" + g.statement(depth+1) + "}"
		}
		return "t=-t;"
	case 7:
		return []string{"if(i===" + fmt.Sprint(g.pick(8)) + ")continue;", "if(s>" + g.number() + ")break;"}[g.pick(2)]
	default:
		return "s+=o.x;"
	}
}

func (g *jitProgram) source() string {
	b := &g.b
	b.WriteString("var log=[];\n")
	switch g.pick(3) {
	case 0:
		b.WriteString("function h(s,i){return (s+i)|0}\n")
	case 1:
		b.WriteString("function h(s,i){log.push('h'+i);return s*2-i}\n")
	default:
		b.WriteString("function h(s,i){if(i===" + fmt.Sprint(3+g.pick(9)) + ")throw new RangeError('h'+s);return s+1}\n")
	}
	b.WriteString("function f(a,o,n){let s=" + g.number() + ",t=" + g.value() + ";for(let i=0;i<n;i++){")
	for k := 1 + g.pick(5); k > 0; k-- {
		b.WriteString(g.statement(0))
	}
	b.WriteString("}return [s,t]}\n")
	b.WriteString("function show(v){return typeof v==='number'&&Object.is(v,-0)?'-0':String(v)}\n")
	b.WriteString("function run(a,o,n){try{let r=f(a,o,n);log.push(show(r[0])+','+show(r[1]))}catch(e){log.push(e.name+':'+e.message)}" +
		"log.push(a.length+':'+Array.from(a,show).join(','),show(o.x))}\n")
	for k := 1 + g.pick(4); k > 0; k-- {
		n := 1 + g.pick(40)
		var elems []string
		for j := g.pick(9); j > 0; j-- {
			if g.pick(8) == 0 {
				elems = append(elems, "")
			} else {
				elems = append(elems, g.value())
			}
		}
		obj := "{x:" + g.value() + "}"
		if g.pick(4) == 0 {
			obj = "{_x:" + g.value() + ",get x(){log.push('g');return this._x},set x(v){log.push('s');this._x=v}}"
		}
		fmt.Fprintf(b, "run([%s],%s,%d);\n", strings.Join(elems, ","), obj, n)
	}
	b.WriteString("log.join('|')")
	return b.String()
}

// jitDifferential runs src in the interpreter, the tree tier and the JIT
// under several stress settings, and reports any difference. It returns the
// JIT's counters summed over the stress runs.
func jitDifferential(t *testing.T, src string) JITStats {
	t.Helper()
	run := func(tree, jit bool, c jitStressConfig) (string, JITStats) {
		previous := treeTier.Swap(tree)
		defer treeTier.Store(previous)
		// A generated program can grow a string without bound -- o.x=s;
		// s+=o.x doubles it -- which takes every tier seconds and gigabytes
		// to reach the engine's length limit. The memory limit stops it
		// early and keeps the fuzzer's workers responsive. Where it stops
		// it is not the same in every tier: the meter measures once the
		// process has allocated enough, the JIT's compiler included, and
		// a rope that shares itself is written out unmetered, so a run
		// over the limit may end either way (see outOfMemory).
		r := New(Config{JIT: jit, MemoryLimit: 16 << 20})
		defer func() { r.Close(); r.ReleaseClosed() }()
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		r.jitStress = c
		// Each tier compiles its own bytecode: a function's tree decision is
		// cached in it.
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			return "uncaught: " + err.Error(), r.JITStats()
		}
		s, err := r.ToString(v)
		if err != nil {
			return "uncaught: " + err.Error(), r.JITStats()
		}
		return s.Go(), r.JITStats()
	}
	want, _ := run(false, false, jitStressConfig{})
	if outOfMemory(want) {
		return JITStats{}
	}
	if got, _ := run(true, false, jitStressConfig{}); got != want && !outOfMemory(got) {
		t.Fatalf("tree tier: %q\ninterpreter: %q\n%s", got, want, src)
	}
	var total JITStats
	for _, c := range []jitStressConfig{
		{threshold: true},
		{threshold: true, budget: 1},
		{threshold: true, budget: 5, deopt: 2},
		// CI's test262 setting under stress (QJS_JIT_STRESS's
		// threshold,budget=3,deopt=7).
		{threshold: true, budget: 3, deopt: 7},
	} {
		got, st := run(true, true, c)
		if got != want && !outOfMemory(got) {
			t.Fatalf("JIT %+v: %q\ninterpreter: %q\n%s", c, got, want, src)
		}
		total.Entries += st.Entries
		total.Guards += st.Guards
		total.Interpreted += st.Interpreted
		total.SSAEntries += st.SSAEntries
		total.Entries += st.Entries
		total.Guards += st.Guards
		total.Interpreted += st.Interpreted
	}
	return total
}

// outOfMemory reports whether a run stopped at its memory limit, which
// decides nothing: whether a program over the limit is stopped, and where,
// depends on when the meter measures.
func outOfMemory(result string) bool {
	return result == "uncaught: "+ErrMemoryLimit.Error()
}

// FuzzJITDifferential compares the JIT with the interpreter on generated
// programs. Without -fuzz it runs its seeds:
//
//	go test -tags quickjs_jit ./internal/vm -run '^$' -fuzz '^FuzzJITDifferential$' -fuzztime=10m
func FuzzJITDifferential(f *testing.F) {
	for i := 0; i < 16; i++ {
		seed := make([]byte, 64)
		r := rand.New(rand.NewPCG(uint64(i), 7))
		for j := range seed {
			seed[j] = byte(r.Uint32())
		}
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		jitDifferential(t, (&jitProgram{data: data}).source())
	})
}

// TestJITDifferentialRandom runs a fixed set of generated programs in every
// ordinary test run, and checks that they reach native code, guards and
// fallbacks, so the generator cannot drift out of the JIT's subset unnoticed.
func TestJITDifferentialRandom(t *testing.T) {
	var total JITStats
	native := 0
	const programs = 300
	for i := 0; i < programs; i++ {
		data := make([]byte, 96)
		r := rand.New(rand.NewPCG(uint64(i), 99))
		for j := range data {
			data[j] = byte(r.Uint32())
		}
		st := jitDifferential(t, (&jitProgram{data: data}).source())
		if st.Entries != 0 {
			native++
		}
		total.Entries += st.Entries
		total.Guards += st.Guards
		total.Interpreted += st.Interpreted
		total.SSAEntries += st.SSAEntries
	}
	t.Logf("%d of %d programs ran natively: %+v", native, programs, total)
	if jitSSABackend && total.SSAEntries == 0 {
		t.Fatal("no generated program ran through the new pipeline")
	}
	if native < programs/3 || total.Guards == 0 || total.Interpreted == 0 {
		t.Fatalf("generated programs no longer exercise the JIT: %d of %d native, %+v", native, programs, total)
	}
}
