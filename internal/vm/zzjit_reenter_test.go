//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
)

// jitLongRunForTest runs setup, warms what it defines by running warm ten
// times, and runs src once, returning its result as a string and what the
// JIT counted meanwhile: Guards, the deoptimizations, and Hosts, the exits
// Go ran an instruction at.
func jitLongRunForTest(t *testing.T, setup, warm, src string) (string, JITStats) {
	t.Helper()
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, warm)); err != nil {
			t.Fatal(err)
		}
	}
	code := compileForTest(t, src)
	before := r.JITStats()
	v, err := r.Run(code)
	if err != nil {
		t.Fatal(err)
	}
	after := r.JITStats()
	s, err := r.ToString(v)
	if err != nil {
		t.Fatal(err)
	}
	return s.Go(), JITStats{Guards: after.Guards - before.Guards, Hosts: after.Hosts - before.Hosts}
}

// A long invocation whose loop fails a guard its code was compiled with
// goes on in code compiled again for what failed, its old code invalidated,
// entered at a loop's back edge once the interpreter has spent the budget
// (jitTierUp), as V8 enters code optimized again by on-stack replacement:
// the run that left is no run of the code while the interpreter goes on
// with its frame. Counted one, the code had been entered again as it was,
// failing again at every back edge the interpreter offered it, each
// failure one run deeper.
func TestJITDeoptRecompilesWithinTheInvocation(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	got, st := jitLongRunForTest(t, `
		function f(a, n) {
			let s = 0
			for (let i = 0; i < n; i++) s += a[i % a.length]
			return typeof s + ':' + (typeof s === 'string' ? s.length : s)
		}`, `f([1, 2, 3], 3000)`, `f(['ab', 'c'], 200000)`)
	if got != "string:300001" {
		t.Fatalf("= %s", got)
	}
	// Compiled again a few times for what failed, an addition's operands
	// and its result, each failure once, as V8's feedback widens.
	if st.Guards > 8 {
		t.Errorf("200,000 iterations of one invocation deoptimized %d times", st.Guards)
	}
}

// An element read compiled before it ran, as a number for an addition
// with no feedback yet, that meets a string learns at the deoptimization
// that it reads a string's character (jitIndexSeen), as a host exit does,
// and is compiled so next, where the compiles that made it generic a guard
// at a time had taken one each -- an array's element as a number, then as
// it is, then a string's. PDF.js's font decryption builds a string in one
// loop, compiled at its back edge, and tokenizes it in the next, which
// the code was compiled for before it ran.
func TestJITStringReadLearnedAtDeopt(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	got, st := jitLongRunForTest(t, `
		function tokens(bytes) {
			let s = ''
			for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i])
			let token = '', n = 0
			for (let i = 0; i < s.length; i++) {
				const c = s[i]
				if (c === ' ') { n += token.length; token = '' } else token += c
			}
			return n + token.length
		}
		const bytes = []
		for (let i = 0; i < 100000; i++) bytes.push(i % 7 ? 160 + i % 90 : 32)`, `0`, `tokens(bytes)`)
	if got != "85714" {
		t.Fatalf("= %s", got)
	}
	// Read natively once learned; read by Go, paced by the code's profit,
	// the loop had run mostly in the interpreter.
	if st.Guards > 4 || st.Hosts > 8 {
		t.Errorf("tokenizing 100,000 characters deoptimized %d times and left for Go %d", st.Guards, st.Hosts)
	}
}

// A unary operator -- x | 0, -x, ~x -- or an update, x++, whose operand
// was not a number once is Go's from the next compile on, at a host exit,
// as a binary operator's is: guarded again at each compile, it had failed
// at every call, compiled again for nothing -- s[i] | 0 of a string's
// character, after the read was learned as a string's.
func TestJITUnaryGenericOnceFailed(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, c := range []struct{ name, body, want string }{
		{"int32", `t += a[i % a.length] | 0`, "300000"},
		{"neg", `t -= -a[i % a.length]`, "300000"},
		{"bitnot", `t += ~a[i % a.length]`, "-500000"},
		{"update", `let x = a[i % a.length]; x++; t += x`, "500000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			setup := `function u(a, n) { let t = 0; for (let i = 0; i < n; i++) { ` + c.body + ` } return t }`
			// Compiled for numbers, then given strings, call after call.
			got, st := jitLongRunForTest(t, setup, `u([1, 2, 3], 3000)`,
				`let r; for (let k = 0; k < 20; k++) r = u(['1', '2'], 10000); r * 20`)
			if got != c.want {
				t.Fatalf("= %s, want %s", got, c.want)
			}
			if st.Guards > 8 {
				t.Errorf("20 calls deoptimized %d times", st.Guards)
			}
		})
	}
}

// A function reading a table of strings at several sites, adding what each
// reads, learns each read's string with the additions of it, at one
// failed guard (jitcompile.Hints' StringReads): learned one at a time, a
// read and then each addition, its compiles for what failed had run out
// before its last site -- PDF.js's btoa -- which then left native code at
// every call.
func TestJITStringTableReadsSettle(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		const table = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'.split('')
		function enc(data) {
			let r = ''
			for (let i = 0; i < data.length; i++) {
				const b = data[i]
				r += table[b >> 2]
				if (b & 1) r += table[b & 63] + '='
				else r += table[(b >> 1) & 63] + table[(b >> 2) & 63]
				r += table[(b * 7) & 63]
			}
			return r.length
		}
		const data = []
		for (let i = 0; i < 2000; i++) data.push(i * 37 & 255)`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	code := compileForTest(t, `enc(data)`)
	for range 20 {
		if v, err := r.Run(code); err != nil || v.Number() != 8000 {
			t.Fatalf("= %v, %v", v, err)
		}
	}
	jitNativeForTest(t, r, "enc")
	before := r.JITStats()
	for range 10 {
		if v, err := r.Run(code); err != nil || v.Number() != 8000 {
			t.Fatalf("= %v, %v", v, err)
		}
	}
	if n := r.JITStats().Guards - before.Guards; n != 0 {
		t.Errorf("10 calls after 20 deoptimized %d times", n)
	}
}

// Operators whose guards have failed, made generic -- an oddball's number
// natively (ssa.OpOddballF64), a relational comparison by Go
// (ssa.GoCompare), anything else Go's at a host exit -- answer as the
// interpreter does for every kind of operand, -0, NaN and equality's
// undefined == null among them, compiled for numbers first.
func TestJITGenericOperatorsAnswers(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// A function an operator, each site of which fails once: a function's
	// compiles for what failed are few.
	jitDifferential(t, `
		const show = v => Object.is(v, -0) ? '-0' : typeof v + ':' + String(v)
		const unary = [x => x | 0, x => -x, x => +x, x => ~x, x => x >> 1, x => x * 2, x => x - 1, x => x % 3,
			x => { let a = x; a++; return a }, x => { let a = x; return a++ }, x => { let a = x; return --a }]
		const binary = [(x, y) => x < y, (x, y) => x <= y, (x, y) => x > y, (x, y) => x >= y, (x, y) => x == y,
			(x, y) => x != y, (x, y) => x + y, (x, y) => x * y, (x, y) => x - y,
			(x, y) => { let n = 0; if (x < y) n++; return n }, (x, y) => { let n = 0; if (x >= y) n++; return n }]
		const nums = [1, -0, 2.5, NaN, -3, 0x7fffffff]
		const vals = nums.concat([undefined, null, true, false, '', '0', '12', 'abc', 'é', ' 3 '])
		const out = []
		for (let r = 0; r < 20; r++) for (const x of nums) {
			for (const f of unary) f(x)
			for (const y of nums) for (const f of binary) f(x, y)
		}
		for (let r = 0; r < 3; r++) for (const x of vals) {
			for (const f of unary) out.push(show(f(x)))
			for (const y of vals) for (const f of binary) out.push(show(f(x, y)))
		}
		out.join()`)
}

// A run whose frame Go goes on with in the interpreter (jitGoOn) -- code
// judged not to pay (entrySlow), the rest of the invocation interpreted --
// is no run of its code meanwhile (jitLeaveCode), as a deoptimized one is
// not: it goes on natively, if at all, at an entry of the code as it is
// then, and the code may be compiled again for a run that begins
// meanwhile. Counted, a run of PDF.js's font parsers had blocked their
// compiles again for the rest of each call.
func TestJITRunOutOfCodeInInterpreter(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	var e *jitEntry
	var runs []int32
	r.global.setOwnRaw(r.atoms.intern("slow"), r.NewFunction("slow", 1, func(_ *Runtime, _ Value, args []Value) (Value, error) {
		if e != nil && args[0].Truthy() {
			e.entrySlow = true
		}
		return Undefined, nil
	}), propDefault)
	r.global.setOwnRaw(r.atoms.intern("probe"), r.NewFunction("probe", 1, func(_ *Runtime, _ Value, args []Value) (Value, error) {
		if e != nil && args[0].Truthy() {
			runs = append(runs, e.ssaRuns)
		}
		return Undefined, nil
	}), propDefault)
	const setup = `
		function f(n, o, go) {
			let t = 0
			for (let i = 0; i < n; i++) {
				if (i === 50) slow(go)
				if (i === 100) t += o.x | 0
				if (i === 200) probe(go)
				t += i
			}
			return t
		}`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `f(3000, 1, false)`)); err != nil {
			t.Fatal(err)
		}
	}
	e = jitNativeForTest(t, r, "f")
	entries := e.ssaStats.entries
	v, err := r.Run(compileForTest(t, `f(3000, 1, true)`))
	if err != nil || v.Number() != 3000*2999/2 {
		t.Fatalf("= %v, %v", v, err)
	}
	if e.ssaStats.entries == entries {
		t.Fatal("f's code never ran")
	}
	if len(runs) != 1 || runs[0] != 0 {
		t.Errorf("runs of f's code while the interpreter went on with its frame: %v", runs)
	}
	if e.ssaRuns != 0 {
		t.Errorf("f's code has %d runs after it returned", e.ssaRuns)
	}
}

// An addition the tree tier has seen add a string is compiled as one from
// the JIT's first compile (treeSeen, jitcompile.Hints' StringAdds): the
// code made at a loop's back edge, after the tree tier ran the loop's
// first turns, does not leave at it. Compiled blind, it had, at every such
// addition once, and been compiled again for it -- 21 of the 24 of PDF.js's
// additions that deoptimized.
func TestJITTreeSeenStringAdds(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	if !treeTier.Load() {
		t.Skip("the tree tier is off (QJS_NOTREE)")
	}
	got, st := jitLongRunForTest(t, `
		function cat(a, n) {
			let s = '', k = 0
			for (let i = 0; i < n; i++) {
				s = s + a[i % a.length]
				if (s.length > 100) { k += s.length; s = '' }
			}
			return k + s.length
		}
		const strs = ['x', 'yy', 'zzz']`, `0`, `cat(strs, 30000)`)
	if got != "60000" {
		t.Fatalf("= %s", got)
	}
	if st.Guards != 0 {
		t.Errorf("a loop over strings, compiled after the tree tier ran it, deoptimized %d times", st.Guards)
	}
}

// The marks are the function's tree's, which every runtime running its
// bytecode shares: runtimes that mark and lower it at once agree, and race
// on nothing (go test -race).
func TestJITTreeSeenSharedAcrossRuntimes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	if !treeTier.Load() {
		t.Skip("the tree tier is off (QJS_NOTREE)")
	}
	code := compileForTest(t, `
		function cat(a, n) {
			let s = '', k = 0
			for (let i = 0; i < n; i++) {
				s = s + a[i % a.length]
				if (s.length > 100) { k += s.length; s = '' }
			}
			return k + s.length
		}
		;(() => { let t = 0; for (let r = 0; r < 5; r++) t += cat(['x', 'yy', 'zzz'], 3000); return t })()`)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := New(Config{JIT: true})
			defer func() { r.Close(); r.ReleaseClosed() }()
			for range 4 {
				if v, err := r.Run(code); err != nil || v.Number() != 30000 {
					t.Errorf("= %v, %v; want 30000", v, err)
				}
			}
		}()
	}
	wg.Wait()
}

// A frame native code made -- a native call's callee -- whose code Go
// finishes after it left at a PC with no entry (jitFinishExit) goes on in
// the interpreter only to its loop's next header, and natively from there
// (jitGoOn), as a frame Go entered the code with does: not in the
// interpreter for the rest of its invocation, until the next back-edge
// budget ran out. callee leaves at its one read of a primitive in one of
// every five of caller's native calls of it -- leaving at more than one in
// four, it would not be called natively (jitUnwound) -- and goes on
// natively at its header each time. Each answer is the interpreter's.
func TestJITNativeCalleeGoesOnAtLoops(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// The collector off, so that what the test counts is the calls': when
	// native calls were Go's while it marked, a collection during a round
	// had made one of its calls Go's, and the round not counted -- three
	// of four on a CI runner.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var obj={x:2};
		function callee(n,k){let t=0;for(let i=0;i<n;i++){const o=i===k?1:obj;t=(t+(o.x|0))|0}return t}
		function caller(m){let s=0;for(let j=0;j<m;j++){s=(s+callee(100,j%5===0?j:-1))|0}return s}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("callee")).value().Object().fn().closure
	checked := 0
	for round := 0; round < 8; round++ {
		wv, err := want.Run(compileForTest(t, `caller(50)`))
		if err != nil {
			t.Fatal(err)
		}
		var in uint64
		if e := r.jit.entryFor(cl.fn); e != nil && e.link != nil {
			in = e.link.in
		}
		wentOn, unwound, interpreted := r.jit.wentOn, r.jit.unwound, r.jit.interpreted
		gv, err := r.Run(compileForTest(t, `caller(50)`))
		if err != nil || gv.Number() != wv.Number() {
			t.Fatalf("round %d: got %v, %v; interpreter %v", round, gv, err, wv)
		}
		if e := r.jit.entryFor(cl.fn); e == nil || e.link == nil || e.link.in-in != 50 || r.jit.unwound-unwound != 10 {
			// Not yet called natively at each call, each leaving.
			continue
		}
		checked++
		if n, i := r.jit.wentOn-wentOn, r.jit.interpreted-interpreted; n != 10 || i != 10 {
			t.Errorf("round %d: callee went on natively at its loop %d times in %d runs of the interpreter, want 10", round, n, i)
		}
	}
	if checked < 4 {
		t.Errorf("callee was called natively at each call, leaving, in %d rounds", checked)
	}
}
