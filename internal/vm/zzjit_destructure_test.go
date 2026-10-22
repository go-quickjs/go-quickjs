//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strings"
	"testing"
)

// destructureSetup's loops destructure objects: sum's own properties, a
// missing one's default, and, given mixed sources, a prototype's
// property, a getter's and a string's; nested's patterns within patterns;
// named's computed keys and a string's length; guarded's sources, the
// last null or undefined, whose TypeError the interpreter's message names;
// rests', restAll's and restTracked's rest elements.
const destructureSetup = `
function sum(xs, n) { let s = 0; for (let i = 0; i < n; i++) { const { a, b, c = 10 } = xs[i % xs.length]; s += a * 2 + b + c } return s }
function nested(xs, n) { let s = ''; for (let i = 0; i < n; i++) { const { p: { q }, r: [x, y] } = xs[i % xs.length]; s = q + ':' + x + y } return s }
function named(o, n) { let t = 0; for (let i = 0; i < n; i++) { const { length, ['k' + (i & 1)]: k } = o; t += (length | 0) + k } return t }
class P { get b() { return 5 } }
P.prototype.c = 7
function mkP() { const p = new P(); p.a = 1; return p }
var plain = [{ a: 1, b: 2 }, { a: 3, b: 4, c: 5 }]
var mixed = [{ a: 1, b: 2 }, mkP(), Object.create({ a: 9, b: 9 }), 'str']
var mixed2 = [{ a: 1, b: 2 }, mkP(), Object.create({ a: 9, b: 9 }), { b: 4, a: 3, c: 1 }]
function guarded(xs) { let s = 0; try { for (let i = 0; i < xs.length; i++) { const { a } = xs[i]; s += a } } catch (e) { return s + ' ' + e.constructor.name + ': ' + e.message } return s }
function rests(xs, n) { let s = ''; for (let i = 0; i < n; i++) { const [a, , ...r] = xs[i % xs.length]; s = a + '/' + r.length + '/' + r.join('') } return s }
function* gen() { yield 1; yield 2; yield 3; yield 4 }
var log = []
var tracked = { [Symbol.iterator]() { let i = 0; return { next() { log.push('n' + i); return { value: i, done: i++ >= 3 } }, return() { log.push('ret'); return {} } } } }
function srcs() { return [[1, 2, 3, 4, 5], 'abcde', new Set([7, 8, 9, 10]), gen(), [1, , 3]] }
function restAll(n) { let s = ''; for (let i = 0; i < n; i++) { const xs = srcs(); for (const x of xs) { const [a, ...r] = x; s += a + ':' + r.join(',') + ';' } } return s }
function restTracked(n) { log.length = 0; let s = 0; for (let i = 0; i < n; i++) { const [a, ...r] = tracked; s += a + r.length } return s + '|' + log.slice(0, 6).join() }
function objs(n, last) { const xs = []; for (let i = 0; i < n; i++) xs.push({ a: i }); xs.push(last); return xs }
`

// A destructuring's check of its source (check_coercible) and an object
// pattern's reads (get_prop_under) are native code's: the check exits,
// for Go to throw, only at a null or undefined source, and a read
// searches the source's own small table, leaving for Go a property it
// does not have there. A function that destructures had been refused
// whole -- 747 of the 13,986 functions of npm's own code -- and run in the
// tree tier. Each answer is Node's (v26.10.0), but the TypeError's
// message, which is the interpreter's, and the loops run natively.
func TestJITDestructures(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, want := jitAnswersForTest(t, destructureSetup)
	for range 20 {
		want(`sum(plain, 300)`, "4350")
		want(`nested([{ p: { q: 'Q' }, r: [1, 2] }], 300)`, "Q:12")
		want(`named({ length: 3, k0: 1, k1: 2 }, 300)`, "1350")
		want(`guarded(objs(299, { a: 1 }))`, "44552")
		want(`rests([[1, 2, 3, 4, 5]], 300)`, "1/3/345")
	}
	for _, name := range []string{"sum", "nested", "named", "guarded", "rests"} {
		jitNativeForTest(t, r, name)
	}
	want(`sum(mixed, 300)`, "NaN")
	want(`sum(mixed2, 300)`, "5700")
	want(`named('ab', 4)`, "NaN")
	// Rest elements of arrays, a string, a Set, a generator and a hole,
	// and of an iterator that logs its steps and its return, which a rest
	// element, exhausting it, never calls.
	want(`restAll(2)`, strings.Repeat("1:2,3,4,5;a:b,c,d,e;7:8,9,10;1:2,3,4;1:,3;", 2))
	for range 3 {
		want(`restTracked(300)`, "600|n0,n1,n2,n3,n0,n1")
	}
	interp := New(Config{})
	defer func() { interp.Close(); interp.ReleaseClosed() }()
	if _, err := interp.Run(compileForTest(t, destructureSetup)); err != nil {
		t.Fatal(err)
	}
	for _, last := range []string{"null", "undefined"} {
		src := "guarded(objs(299, " + last + "))"
		v, err := interp.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		want(src, v.String().Go())
	}
	want(`sum(plain, 300)`, "4350")
}
