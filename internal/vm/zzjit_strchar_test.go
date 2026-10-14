//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// A string's element, s[i], a string of one character: native code makes
// an ASCII one from the runtime's strings of one character, once the read
// has met a string (ssa.OpStringChar), and Go makes any other -- a code
// unit past ASCII, an index past the end or not an integer, a string that
// is not flat. Each answer is the interpreter's, in every tier and under
// every stress setting: ASCII and not, past the end, negative and
// fractional keys, a rope, and a site whose receivers are an array too.
func TestJITStringElements(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		const ascii = 'The quick brown fox jumps over the lazy dog 0123456789 {}[]~'
		const wide = 'café 中文 😀 ok'
		function scan(s, n) {
			let h = 0, letters = 0, out = ''
			for (let i = 0; i < n; i++) {
				const c = s[i]
				if (c >= 'a' && c <= 'z') letters++
				if (c === ' ') out += '_'
				else if (c !== undefined) out += c
				h = (h * 31 + (c === undefined ? 7 : c.length + c.charCodeAt(0))) | 0
			}
			return h + ':' + letters + ':' + out
		}
		function at(s, k) { return s[k] }
		const log = []
		for (let r = 0; r < 25; r++) {
			let rope = 'ab'
			for (let k = 0; k < r; k++) rope = rope + 'cd' + k
			log.push(scan(ascii, ascii.length + 3), scan(wide, wide.length + 1), scan(rope, 9))
			log.push(String(at(ascii, -1)), String(at(ascii, 1.5)), String(at(ascii, r)), String(at([10, 20, 30], r % 4)))
		}
		log.join()`
	jitDifferential(t, src)
}

// The reads run natively once learned: Go reads none of an ASCII string's
// characters.
func TestJITStringElementsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		const s = 'abcdefghijklmnopqrstuvwxyz0123456789'.repeat(8)
		function count(n) { let k = 0; for (let i = 0; i < n; i++) { const c = s[i % s.length]; if (c >= 'a') k++ } return k }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `count(1000)`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "count")
	if len(e.stringReads) == 0 {
		t.Fatalf("count's read not learned as a string's: %+v", e)
	}
	hosts := e.ssaStats.hosts
	v, err := r.Run(compileForTest(t, `let t = 0; for (let k = 0; k < 20; k++) t += count(1000); t`))
	if err != nil || v.Number() != 20*728 {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := e.ssaStats.hosts - hosts; n > 2 {
		t.Errorf("count left native code %d times in 20,000 reads", n)
	}
}
