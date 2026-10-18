//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// builderDifferential runs src in the interpreter and natively, the JIT
// compiling each function at its first call and at its tenth, and wants
// the same answer from each. jitDifferential's runtimes have a memory
// limit, where appends are refused (a meter counts each string): these
// have none.
func builderDifferential(t *testing.T, src string) {
	t.Helper()
	run := func(jit bool, threshold uint16) string {
		r := New(Config{JIT: jit})
		defer func() { r.Close(); r.ReleaseClosed() }()
		if jit && r.jitCallThreshold <= 1 && threshold > 1 {
			// QJS_JIT_STRESS: the runtime compiles eagerly already.
			threshold = 1
		}
		if jit {
			r.jitCallThreshold, r.jitICThreshold = threshold, threshold
		}
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			return "uncaught: " + err.Error()
		}
		s, err := r.ToString(v)
		if err != nil {
			return "uncaught: " + err.Error()
		}
		return s.Go()
	}
	want := run(false, 0)
	for _, threshold := range []uint16{1, 10} {
		if got := run(true, threshold); got != want {
			t.Errorf("threshold %d: got\n%.400s\nwant\n%.400s", threshold, got, want)
		}
	}
}

// A loop that builds a string, s += x at each iteration, appends to one
// string natively (ssa.GoAppend, jitAppend), which no one sees grow: a
// string the loop has built and gone on from, one it stored or used
// otherwise, one it left with for Go, keeps what it had.
func TestJITStringBuilderAnswers(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	cases := map[string]string{
		"plain": `
			function build(n) { let s = ''; for (let i = 0; i < n; i++) s += String.fromCharCode(97 + i % 26); return s }
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k * 7))
			r.join(',')`,
		"kept": `
			// Each call's string stays as it was, the next building another.
			function build(n, c) { let s = ''; for (let i = 0; i < n; i++) s += c; return s }
			const kept = []
			for (let k = 0; k < 30; k++) kept.push(build(k, String.fromCharCode(65 + k % 26)))
			kept.join('|')`,
		"prefix": `
			// A string the loop starts from, which others hold, is its left side.
			function build(s, n) { for (let i = 0; i < n; i++) s += i; return s }
			const base = 'base:'
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(base, k), base)
			r.join(',')`,
		"conditional": `
			function build(n) {
				let s = ''
				for (let i = 0; i < n; i++) {
					if (i % 3 === 0) s += 'a'
					else if (i % 3 === 1) s += i
					else s += true
				}
				return s
			}
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',')`,
		"self": `
			// s += s is no append: its right operand is the builder.
			function build(n) { let s = 'x'; for (let i = 0; i < n; i++) { s += 'y'; if (i % 4 === 0) s += s } return s }
			let r = []
			for (let k = 0; k < 20; k++) r.push(build(k).length + build(k).slice(-9))
			r.join(',')`,
		"used": `
			// A loop that reads the string as it builds it builds none.
			function build(n) { let s = '', t = 0; for (let i = 0; i < n; i++) { s += 'ab'; t += s.length } return s + t }
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',')`,
		"before": `
			// The string before an append, which a break leaves with.
			function build(n) {
				let s = '', t
				for (let i = 0; ; i++) { t = s + i; if (i >= n) break; s = t }
				return s + '/' + t
			}
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',')`,
		"unused": `
			// An append whose result the next iteration drops, to a string
			// the loop builds.
			function build(n) { let s = 'q', t; for (let i = 0; i < n; i++) { s += 'a'; t = s + i } return s + t }
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',')`,
		"two": `
			// Two appends to one string, on one path, from the same one.
			function build(n) {
				let s = ''
				for (let i = 0; i < n; i++) { const a = s + 'a', b = s + 'b'; s = i % 2 ? a : b }
				return s
			}
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',')`,
		"sequential": `
			// A loop builds on what the one before it built, which is kept.
			function build(n) {
				let s = ''
				for (let i = 0; i < n; i++) s += 'a'
				const t = s
				for (let i = 0; i < n; i++) s += 'b'
				return t + '|' + s
			}
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',')`,
		"handoff": `
			// A function that builds on the string the loop built, which the
			// caller uses again, after: frozen before the first.
			function more(s) { for (let i = 0; i < 5; i++) s += 'g'; return s }
			function build(n) {
				let s = ''
				for (let i = 0; i < n; i++) s += 'f'
				const t = more(s)
				return s + '|' + t + '|' + more(s)
			}
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',')`,
		"handoff-outer": `
			// The same where the string goes round an outer loop, handed off
			// in the outer loop's last block, whose edge to the outer header
			// it leaves the inner loop by too.
			function more(s) { for (let i = 0; i < 5; i++) s += 'g'; return s }
			const out = []
			function build(n) {
				let s = ''
				for (let k = 0; k < 3; k++) {
					for (let i = 0; i < n; i++) s += 'f'
					out.push(more(s))
				}
				return s
			}
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',') + out.join(',')`,
		"nested": `
			function build(n) {
				let s = ''
				for (let i = 0; i < n; i++) { s += '['; for (let j = 0; j < i; j++) s += j; s += ']' }
				return s
			}
			function rows(n) {
				const out = []
				for (let i = 0; i < n; i++) { let s = ''; for (let j = 0; j < i; j++) s += j % 10; out.push(s) }
				return out.join('/')
			}
			let r = []
			for (let k = 0; k < 20; k++) r.push(build(k), rows(k))
			r.join(',')`,
		"exits": `
			// Appends Go refuses, an object's conversion, leave native code,
			// which goes on from the string as it was.
			function build(xs) { let s = ''; for (let i = 0; i < xs.length; i++) s += xs[i]; return s }
			const o = { valueOf() { return 7 }, toString() { return 'O' } }
			let r = []
			for (let k = 0; k < 30; k++) {
				const xs = []
				for (let i = 0; i < k; i++) xs.push(i % 5 === 4 ? o : i % 7 === 6 ? 1.5 : 'c' + i)
				r.push(build(xs))
			}
			r.join(',')`,
		"surrogates": `
			// A high surrogate at the end meeting a low one makes one code point.
			function build(xs) { let s = ''; for (let i = 0; i < xs.length; i++) s += xs[i]; return s }
			const xs = ['a', '\uD83D', '\uDE00', 'b', '\uD83D', '\uD83D', '\uDE01', '\uDE02', 'é', '\uD800']
			let r = []
			for (let k = 0; k < 30; k++) {
				const s = build(xs.slice(0, k % 11))
				r.push(s.length + ':' + s.codePointAt(1) + ':' + encodeURIComponent(s.replace(/[\uD800-\uDFFF]/g, '?')) + ':' + (s === xs.slice(0, k % 11).join('')))
			}
			r.join(',')`,
		"captured": `
			// A closure that reads the string sees it as the loop has it.
			function build(n) {
				let s = ''
				const peek = () => s.length
				let seen = 0
				for (let i = 0; i < n; i++) { s += 'z'; if (i % 5 === 0) seen += peek() }
				return s + seen
			}
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',')`,
		"number": `
			// A sum that becomes a string.
			function build(n) { let s = 0; for (let i = 0; i < n; i++) s += i === 3 ? 'x' : i; return s }
			let r = []
			for (let k = 0; k < 30; k++) r.push(build(k))
			r.join(',')`,
		"compare": `
			// Strings built so compare, hash and slice as any others.
			function build(n) { let s = ''; for (let i = 0; i < n; i++) s += String.fromCharCode(48 + i % 10); return s }
			const m = new Map()
			for (let k = 0; k < 40; k++) { const s = build(k % 20 + 50); m.set(s, (m.get(s) || 0) + 1) }
			const a = build(70), b = build(70)
			;[...m.values()].join() + (a === b) + (a < build(71)) + a.slice(60) + a.indexOf('789')`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) { builderDifferential(t, src) })
	}
}

// A string built a character at a time is made once, not once for each
// character: each character after the first is appended in place, to a
// buffer that doubles as it grows.
func TestJITStringBuilderAppendsInPlace(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		function build(n) { let s = ''; for (let i = 0; i < n; i++) s += String.fromCharCode(97 + i % 26); return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `build(10000).length`)); err != nil {
			t.Fatal(err)
		}
	}
	jitNativeForTest(t, r, "build")
	code := compileForTest(t, `(() => { let t = 0; for (let k = 0; k < 20; k++) t += build(10000).length; return t })()`)
	appended := r.jit.appended
	v, err := r.Run(code)
	if err != nil || v.Number() != 20*10000 {
		t.Fatalf("= %v, %v", v, err)
	}
	// The count, not the bytes allocated: under checkptr every call of Go
	// native code makes allocates, its pointers' conversions escaping.
	if n := r.jit.appended - appended; n < 20*(10000-1) {
		t.Errorf("20 strings of 10,000 characters had %d appends in place", n)
	}
}
