//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

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
	jitFewCallsForTest(r)
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

// A call of String.prototype.charCodeAt is made natively by its target,
// as V8 reduces one, whatever its method's name (ssa's CharCodeFeedback):
// under another name, and in a callee inlined into the loop calling it. Once compiled, each runs natively at every call, and
// neither leaves native code nor has Go make a call nearly once a
// character; and each
// answer is the interpreter's, in every tier and stress setting, a
// replaced charCodeAt and receivers that are not strings included.
func TestJITCharCodeAtByTarget(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, c := range []struct{ name, setup string }{
		{"renamed", `String.prototype.code = String.prototype.charCodeAt
			function sum(s) { let t = 0; for (let i = 0; i < s.length; i++) t += s.code(i); return t }`},
		{"inlined", `function at(s, i) { return s.charCodeAt(i) }
			function sum(s) { let t = 0; for (let i = 0; i < s.length; i++) t += at(s, i); return t }`},
	} {
		t.Run(c.name, func(t *testing.T) {
			jitDifferential(t, c.setup+`
				const s = 'abcdefghij'.repeat(30)
				let r = 0
				for (let k = 0; k < 5; k++) r += sum(s)
				r += sum('\u1234x') + sum('')
				const saved = String.prototype.charCodeAt
				String.prototype.charCodeAt = function (i) { return 1 }
				if (String.prototype.code) String.prototype.code = String.prototype.charCodeAt
				r += sum(s)
				String.prototype.charCodeAt = saved
				r`)
			r := New(Config{JIT: true})
			t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
			if r.jitCallThreshold <= 1 {
				t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
			}
			if _, err := r.Run(compileForTest(t, c.setup+`; const s = 'abcdefghij'.repeat(100)`)); err != nil {
				t.Fatal(err)
			}
			for range 10 {
				if _, err := r.Run(compileForTest(t, `sum(s)`)); err != nil {
					t.Fatal(err)
				}
			}
			e := jitNativeForTest(t, r, "sum")
			hosts, goCalls, entries := e.ssaStats.hosts, e.ssaStats.goCalls, r.JITStats().SSAEntries
			v, err := r.Run(compileForTest(t, `(() => { let t = 0; for (let k = 0; k < 20; k++) t += sum(s); return t })()`))
			if want := 20 * 100 * float64('a'+'b'+'c'+'d'+'e'+'f'+'g'+'h'+'i'+'j'); err != nil || v.Number() != want {
				t.Fatalf("= %v, %v; want %v", v, err, want)
			}
			if n := r.JITStats().SSAEntries - entries; n < 20 {
				t.Errorf("native code entered %d times in 20 calls", n)
			}
			if n := e.ssaStats.hosts - hosts; n >= 20*1000/10 {
				t.Errorf("sum left native code %d times for 20,000 characters", n)
			}
			if n := e.ssaStats.goCalls - goCalls; n >= 20*1000/10 {
				t.Errorf("Go made %d calls for 20,000 characters", n)
			}
		})
	}
}

// An element of a typed array a local holds, read by a local key
// (get_local_index) as it is, for ==, is learned at its exit as a[i]'s is
// (jitIndexOperands): read as a typed array's number from the next compile
// on. It had left native code at every invocation, the rest of each run by
// the interpreter: PDF.js's Huffman tables, lengths[val] == len. Once
// compiled, 20 calls neither leave native code nor finish in the
// interpreter more than twice; each answer is the interpreter's in every
// tier and stress setting, a plain array's included.
func TestJITLocalTypedReadsLearned(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function count(lengths, len) {
			let n = lengths.length, c = 0
			for (let val = 0; val < n; ++val) if (lengths[val] == len) c++
			return c
		}
		const lengths = new Uint8Array(4096).map((_, i) => i % 7)`
	jitDifferential(t, setup+`
		let r = 0
		for (let k = 0; k < 6; k++) r += count(lengths, k % 7) + count(lengths.subarray(5, 900), 3)
		r + count([1, 3, 3, '3', null], 3)`)
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitFewCallsForTest(r)
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `count(lengths, 3)`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "count")
	hosts, interpreted := e.ssaStats.hosts, r.JITStats().Interpreted
	v, err := r.Run(compileForTest(t, `(() => { let t = 0; for (let k = 0; k < 20; k++) t += count(lengths, 3); return t })()`))
	if err != nil || v.Number() != 20*585 {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := e.ssaStats.hosts - hosts; n > 2 {
		t.Errorf("count left native code %d times in 20 calls", n)
	}
	if n := r.JITStats().Interpreted - interpreted; n > 2 {
		t.Errorf("%d of 20 calls finished in the interpreter", n)
	}
}

// An object's property read by its name's string, obj[name], is made by Go
// from native code once the read has met one (ssa.GoIndex, jitKeyedRead):
// where reading it runs nothing -- a data property, own or inherited, found
// by the runtime's shared property cache -- and otherwise after an exit,
// native code going on after it: a getter, a proxy, a key that is an
// index, a missing property, a number key of an array. It had left native
// code at every invocation, the rest of each by the interpreter: PDF.js's
// Type1 fonts' map[command]. Each answer is the interpreter's in every
// tier and stress setting; and, once compiled, 20 calls of plain keys
// finish in the interpreter at most twice.
func TestJITKeyedReadsByGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		let hits = 0
		function count(map, keys) {
			let c = 0, s = ''
			for (let i = 0; i < keys.length; i++) {
				const key = keys[i], cmd = map[key]
				if (cmd === 1) c++
				else s += typeof cmd
			}
			return c + s
		}
		const proto = { inherited: 1 }
		const map = Object.create(proto)
		map.a = 1; map.b = 2; map.c = 1; map[0] = 1
		const plain = ['a', 'b', 'c', 'inherited', 'a', 'c']`
	jitDifferential(t, setup+`
		Object.defineProperty(map, 'g', { get() { hits++; return 1 }, configurable: true })
		const px = new Proxy(map, { get(t, k) { hits++; return t[k] } })
		let r = ''
		for (let k = 0; k < 6; k++) r += count(map, plain) + ';'
		r += count(map, ['a', 'missing', '0', 'g', 'inherited', 'toString']) + ';'
		r += count(px, plain) + ';'
		r += count([1, 2, 1], [0, 1, 2, 'length', 1.5]) + ';'
		proto.inherited = 2
		delete map.c
		r += count(map, plain) + ';'
		r + hits`)
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitFewCallsForTest(r)
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	if _, err := r.Run(compileForTest(t, setup+`; const many = []; for (let i = 0; i < 1000; i++) many.push(plain[i % 6])`)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `count(map, many)`)); err != nil {
			t.Fatal(err)
		}
	}
	jitNativeForTest(t, r, "count")
	interpreted := r.JITStats().Interpreted
	v, err := r.Run(compileForTest(t, `(() => { let t = ''; for (let k = 0; k < 20; k++) t = count(map, many); return t })()`))
	if want := "833" + strings.Repeat("number", 167); err != nil || v.String().Go() != want {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := r.JITStats().Interpreted - interpreted; n > 2 {
		t.Errorf("%d of 20 calls finished in the interpreter", n)
	}
}

// An addition speculated to add numbers that deoptimizes on a string is
// learned as one that met a string at a host exit (jitStringAddSeen), so
// that the lowering makes it Go's with every addition of its result after
// it: a chain, tag + s(a) + s(b) + s(c), is learned at its first failure,
// not one failure, and one compile, an addition -- PDF.js's
// createTableEntry had run out of compiles so and deoptimized at every
// invocation. Its answers are the interpreter's in every tier and stress
// setting; once warm, it fails at most two guards in all, and none in its
// 20 calls after.
func TestJITStringAddChainsLearnedAtOnce(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function s(n) { return String.fromCharCode(65 + (n & 15)) }
		function entry(file, tag, a, b, c) {
			let w = 0
			for (let i = 0; i < 40; i++) w = (w * 31 + i) | 0
			const e = tag + s(a) + s(b) + s(c)
			file.text += e
			return w
		}`
	jitDifferential(t, setup+`
		const file = { text: '' }
		for (let k = 0; k < 30; k++) entry(file, 'T' + k, k, k + 1, k + 2)
		entry(file, 7, 1, 2, 3)
		file.text`)
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitFewCallsForTest(r)
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	if _, err := r.Run(compileForTest(t, setup+`; const file = { text: '' }`)); err != nil {
		t.Fatal(err)
	}
	for k := range 40 {
		if _, err := r.Run(compileForTest(t, fmt.Sprintf(`entry(file, 'T%d', %d, 2, 3)`, k, k))); err != nil {
			t.Fatal(err)
		}
	}
	jitNativeForTest(t, r, "entry")
	guards := r.JITStats().Guards
	if guards > 2 {
		t.Errorf("entry's chain failed %d guards, learned one addition at a time", guards)
	}
	v, err := r.Run(compileForTest(t, `(() => { for (let k = 0; k < 20; k++) entry(file, 'U', k, 1, 2); return file.text.length })()`))
	if err != nil || v.Number() != 310 {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := r.JITStats().Guards - guards; n != 0 {
		t.Errorf("%d guards failed in 20 calls once warm", n)
	}
}

// A typed array's length is read natively from the view's first read, its
// storage filled in when it is made (constructTypedArray,
// allocTypedArray): before, only Go's first access to it filled it in, and
// every length of a view only ever read -- PDF.js's subarrays -- left
// native code. Once compiled, 20 calls over 50 fresh subarrays each leave
// native code at most twice.
func TestJITFreshViewLengthsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitFewCallsForTest(r)
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		const buf = new Uint8Array(1000)
		function total(views) { let t = 0; for (let i = 0; i < views.length; i++) t += views[i].length; return t }
		function views() { const vs = []; for (let i = 0; i < 50; i++) vs.push(buf.subarray(i, i + 7)); return vs }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `total(views())`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "total")
	hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
	v, err := r.Run(compileForTest(t, `(() => { let t = 0; for (let k = 0; k < 20; k++) t += total(views()); return t })()`))
	if err != nil || v.Number() != 20*50*7 {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := e.ssaStats.hosts - hosts + e.ssaStats.guards - guards; n > 2 {
		t.Errorf("total left native code %d times in 20 calls", n)
	}
}

// String.fromCharCode(c) of one integer below 256 is made natively, by its
// target (ssa.OpCodeChar, jitFeedback.FromCharCode): the runtime's own
// string of the character, from the table s[i] reads too, now of 256, with
// nothing made and no call of Go -- PDF.js's fonts build their strings so,
// a character at a time. Each answer is the interpreter's in every tier and
// stress setting: codes past 255, negative, fractional and NaN, a valueOf,
// two arguments, and String.fromCharCode replaced, which Go makes. Once
// compiled, 20 calls of 1,000 characters each make next to no calls of Go.
func TestJITFromCharCodeNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function build(b) { let s = ''; for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]); return s }
		function sum(s) { let t = 0; for (let i = 0; i < s.length; i++) t += s[i] === 'é' ? 1000 : s[i].length; return t }
		const bytes = []
		for (let i = 0; i < 1000; i++) bytes.push(i * 37 % 256)`
	jitDifferential(t, setup+`
		let r = ''
		for (let k = 0; k < 4; k++) r += build(bytes).length + ':' + sum(build(bytes)) + ';'
		r += build([65, 300, -1, 66.7, NaN, 233, 0x10000 + 67]) + ';'
		r += build([{ valueOf() { return 68 } }, 69]) + ';'
		r += String.fromCharCode(70, 71) + ';'
		const saved = String.fromCharCode
		String.fromCharCode = c => '<' + c + '>'
		r += build([72, 73]) + ';'
		String.fromCharCode = saved
		r + build([74, 233])`)
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `sum(build(bytes))`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "build")
	goCalls, hosts, entries := e.ssaStats.goCalls, e.ssaStats.hosts, e.ssaStats.entries
	v, err := r.Run(compileForTest(t, `(() => { let t = 0; for (let k = 0; k < 20; k++) t += build(bytes).length; return t })()`))
	if err != nil || v.Number() != 20*1000 {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := e.ssaStats.entries - entries; n < 20 {
		t.Errorf("build's code entered %d times in 20 calls", n)
	}
	if n := e.ssaStats.goCalls - goCalls + e.ssaStats.hosts - hosts; n > 20*1000/10 {
		t.Errorf("build made %d calls of Go and exits for 20,000 characters", n)
	}
}
