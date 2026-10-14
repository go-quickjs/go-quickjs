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

// x.length: an array's, a string's and a typed array's natively -- the
// last where its view is in its buffer -- and any other receiver's Go's,
// which goes on after it once the guard has failed (ssa.OpLength), rather
// than leave the rest of each invocation to the interpreter. Each answer is
// the interpreter's, in every tier and under every stress setting: arrays,
// strings, typed arrays of each kind, a view on a detached buffer, a
// subarray, and objects with a length of their own.
func TestJITLengths(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		const views = [new Uint8Array(7), new Int32Array(13), new Float64Array(3), new Uint16Array(new ArrayBuffer(16), 2, 5)]
		const sub = new Uint8Array(20).subarray(4, 9)
		const gone = new Uint8Array(new ArrayBuffer(8))
		function total(xs, n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				const x = xs[i % xs.length]
				s = (s * 3 + x.length) | 0
			}
			return s
		}
		const log = []
		for (let r = 0; r < 25; r++) {
			log.push(total(views, 40), total([sub, 'hello', [1, 2, 3]], 30), total([{length: r}, 'ab', gone], 9))
			if (r === 15 && typeof gone.buffer.transfer === 'function') gone.buffer.transfer()
		}
		log.join()`
	jitDifferential(t, src)
}

// A native call links to its callee before the callee has code: native code
// calls it with abi.ExitEnter, Go running it from the frame native code
// made, until it is compiled, and natively from then on, the caller not
// compiled again. Not a callee whose frame is more than its arguments in
// its parameters -- an arguments object, a rest parameter, defaults,
// destructuring -- which that frame would lose: Go makes those calls. Each
// answer is the interpreter's, in every tier and under every stress
// setting, the loop compiled before any callee is called.
func TestJITCallsLinkedBeforeCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		function plain(a, b) { return a * 3 + b }
		function args() { let s = 0; for (let i = 0; i < arguments.length; i++) s += arguments[i]; return s }
		function rest(a, ...xs) { return a + xs.length * 10 + (xs[1] | 0) }
		function defaults(a, b = 7) { return a - b }
		function destructures({x, y}, [z]) { return x * y + z }
		function loop(n, late) {
			let s = 0
			for (let i = 0; i < n; i++) {
				s = (s + plain(i, 1)) | 0
				if (i >= late) {
					s = (s + args(i, 2, 3) + rest(i, 4, 5, 6) + defaults(i) + defaults(i, 1)) | 0
					s = (s + destructures({x: i, y: 2}, [3])) | 0
				}
			}
			return s
		}
		const log = []
		for (let r = 0; r < 12; r++) log.push(loop(400, r < 6 ? 1000 : 200))
		log.join()`
	jitDifferential(t, src)
}

// A call of one argument in a function that reads "charCodeAt" is made as
// any other, natively or by Go, unless its function is the method that
// read took (jitcompile's charCodeCalls): PDF.js's Type1Font_wrap's
// cff.push(field.charCodeAt(i)) had been lowered as a charCodeAt too,
// which left native code at every character, and its code was judged not
// to pay. Once compiled, wrap runs natively at every call, and leaves it
// far less often than once a character.
func TestJITCallsBesideCharCodeAt(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		const long = { a: 'abcdefghij'.repeat(50), b: '0123456789'.repeat(50) }
		const short = { a: 'abcdefghij', b: '0123456789' }
		function wrap(fields) {
			var cff = []
			for (var k in fields) { var field = fields[k]; for (var i = 0, ii = field.length; i < ii; i++) cff.push(field.charCodeAt(i)) }
			return cff
		}`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `wrap(long)`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "wrap")
	var hosts [2]uint64
	for k, c := range []struct {
		fields string
		want   float64
	}{{"long", 20 * (1000 + 'h' + '9')}, {"short", 20 * (20 + 'h' + '9')}} {
		h, entries := e.ssaStats.hosts, r.JITStats().SSAEntries
		v, err := r.Run(compileForTest(t, `(() => { let t = 0; for (let k = 0; k < 20; k++) { const c = wrap(`+c.fields+`); t += c.length + c[7] + c[c.length/2 + 9] } return t })()`))
		if err != nil || v.Number() != c.want {
			t.Fatalf("%s: = %v, %v; want %v", c.fields, v, err, c.want)
		}
		if n := r.JITStats().SSAEntries - entries; n < 20 {
			t.Errorf("%s: native code entered %d times in 20 calls", c.fields, n)
		}
		hosts[k] = e.ssaStats.hosts - h
	}
	// Its for...in's steps, and its loops' budgets: not a character's.
	if hosts[0] >= 20*1000/10 {
		t.Errorf("wrap left native code %d times for 20,000 characters (%d for 400)", hosts[0], hosts[1])
	}
}
