package vm

import (
	"strings"
	"testing"
)

// strMentions are the ways code can reach a local from a function or a
// class. Each declares what reads out, and read is a call of it, made
// between two appends to out in a loop: what it hands back is kept, and if
// the compiler took out's appends for its own, the string would grow under
// what was kept. localAppend rests on its scanner seeing every one.
var strMentions = []struct{ name, decl, read string }{
	{"arrow", `const r = () => out`, `r()`},
	{"function expression", `const r = function () { return out }`, `r()`},
	{"declaration", `function r() { return out }`, `r()`},
	{"nested arrow", `const r = () => () => out`, `r()()`},
	{"arrow's default", `const r = (a = out) => a`, `r()`},
	{"function's default", `function r(a = out) { return a }`, `r()`},
	{"destructured default", `const r = ({ a = out } = {}) => a`, `r()`},
	{"array destructured default", `const r = ([a = out] = []) => a`, `r()`},
	{"computed key in a parameter", `const r = ({ [out]: a = out } = {}) => a`, `r()`},
	{"rest default", `const r = (...[a = out]) => a`, `r()`},
	{"method", `const o = { m() { return out } }`, `o.m()`},
	{"getter", `const o = { get m() { return out } }`, `o.m`},
	{"setter", `let got; const o = { set m(v) { got = out } }`, `(o.m = 1, got)`},
	{"computed member", `const o = { ['m']() { return out } }`, `o.m()`},
	{"shorthand in a closure", `const r = () => ({ out }).out`, `r()`},
	{"spread in a closure", `const r = () => [...[out]][0]`, `r()`},
	{"object spread in a closure", `const r = () => ({ ...{ a: out } }).a`, `r()`},
	{"template in a closure", "const r = () => `${out}`", `r()`},
	{"tag in a closure", "const t = (s, v) => v, r = () => t`${out}`", `r()`},
	{"tagged member in a closure", "const t = { f: (s, v) => v }, r = () => t.f`${out}`", `r()`},
	{"optional call in a closure", `const f = v => v, r = () => f?.(out)`, `r()`},
	{"optional member in a closure", `const o = { }, r = () => o?.[out] ?? out`, `r()`},
	{"optional chain's end", `const o = { a: { f: v => v } }, r = () => o?.a.f(out)`, `r()`},
	{"new in a closure", `class K { constructor(v) { this.v = v } } const r = () => new K(out).v`, `r()`},
	{"conditional in a closure", `const r = () => true ? out : 0`, `r()`},
	{"logical in a closure", `const r = () => null ?? out`, `r()`},
	{"sequence in a closure", `const r = () => (0, out)`, `r()`},
	{"assignment in a closure", `let k; const r = () => k = out`, `r()`},
	{"destructuring assignment in a closure", `let k; const r = () => ([k] = [out], k)`, `r()`},
	{"unary's operand", `const r = () => { void out; return typeof out === 'string' ? out : 0 }`, `r()`},
	{"in a block", `const r = () => { { return out } }`, `r()`},
	{"in an if", `const r = () => { if (out) return out }`, `r()`},
	{"in a loop", `const r = () => { for (;;) return out }`, `r()`},
	{"for-of's head", `const r = () => { for (const v of [out]) return v }`, `r()`},
	{"for-in's head", `const r = () => { for (const v in { [0]: 0 }) return out }`, `r()`},
	{"while", `const r = () => { while (true) return out }`, `r()`},
	{"do", `const r = () => { do { return out } while (false) }`, `r()`},
	{"labeled", `const r = () => { a: for (;;) { return out } }`, `r()`},
	{"switch's test", `const r = () => { switch (0) { case 0: return out } }`, `r()`},
	{"switch's case", `const r = () => { let v; switch (1) { case (v = out, 1): } return v }`, `r()`},
	{"try", `const r = () => { try { return out } catch {} }`, `r()`},
	{"catch", `const r = () => { try { throw 1 } catch { return out } }`, `r()`},
	{"catch's pattern", `const r = () => { try { throw {} } catch ({ a = out }) { return a } }`, `r()`},
	{"finally", `const r = () => { let v; try {} finally { v = out } return v }`, `r()`},
	{"throw", `const r = () => { try { throw out } catch (e) { return e } }`, `r()`},
	{"var", `const r = () => { var v = out; return v }`, `r()`},
	{"let", `const r = () => { let v = out; return v }`, `r()`},
	{"using", `const r = () => { let v; { using d = { [Symbol.dispose]() { v = out } } } return v }`, `r()`},
	{"generator", `function* g() { yield out } const r = () => g().next().value`, `r()`},
	{"generator's delegate", `function* g() { yield* [out] } const r = () => g().next().value`, `r()`},
	{"class method", `class K { m() { return out } } const r = () => new K().m()`, `r()`},
	{"static method", `class K { static m() { return out } }`, `K.m()`},
	{"class getter", `class K { get m() { return out } }`, `new K().m`},
	{"field", `class K { f = out }`, `new K().f`},
	{"static field", `const r = () => { class K { static f = out } return K.f }`, `r()`},
	{"computed field", `const r = () => { let v; class K { [(v = out, 'f')] = 1 } return v }`, `r()`},
	{"computed method", `const r = () => { let v; class K { [(v = out, 'm')]() {} } return v }`, `r()`},
	{"private field", `class K { #f = out; get f() { return this.#f } }`, `new K().f`},
	{"private method", `class K { #m() { return out } get f() { return this.#m() } }`, `new K().f`},
	{"static block", `const r = () => { let v; class K { static { v = out } } return v }`, `r()`},
	{"extends", `const r = () => { let v; class K extends (v = out, Object) {} return v }`, `r()`},
	{"class expression", `const K = class { m() { return out } }, r = () => new K().m()`, `r()`},
	{"class in a parameter", `const r = (K = class { m() { return out } }) => new K().m()`, `r()`},
	{"super's argument", `class A { constructor(v) { this.v = v } } class B extends A { constructor() { super(out) } }`, `new B().v`},
	{"async arrow", `let v; const r = async () => { v = out }`, `(r(), v)`},
	{"async generator", `let v; async function* g() { v = out } const r = () => (g().next(), v)`, `r()`},
	{"import's argument", `let v; const r = () => { import(v = out).catch(() => {}); return v }`, `r()`},
	{"a getter on String.prototype", `Object.defineProperty(String.prototype, 'me', { configurable: true, get() { 'use strict'; return this } })`, `out.me`},
	{"a closure in with", `const r = () => { with ({}) { return out } }`, `r()`},
	{"a closure's with object", `const r = () => { with ({ a: out }) { return a } }`, `r()`},
}

// strMentionProgram reads out each way, between appends.
func strMentionProgram(decl, read string) string {
	return `
function hash(s) { let h = 7; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0; return s.length + ':' + h }
function esc(s) { return '<' + s + '>' }
function t() {
  var out = 'x'.repeat(70)
  ` + decl + `
  const seen = []
  for (let i = 0; i < 12; i++) {
    out += 'a'; out += esc('b'); out += 'c'; out += 'd'; out += 'e'
    seen.push(` + read + `)
    out += 'f'; out += esc('g'); out += 'h'; out += 'i'; out += 'j'
  }
  return seen.map(hash).join() + '|' + hash(out)
}
let last = ''
for (let round = 0; round < 40; round++) last = t()
last
`
}

func TestStrAppendMentions(t *testing.T) {
	t.Cleanup(func() { SetTreeTier(true) })
	for _, m := range strMentions {
		t.Run(strings.ReplaceAll(m.name, " ", "_"), func(t *testing.T) {
			src := strMentionProgram(m.decl, m.read)
			ref := New(Config{Debug: true})
			want := strFuzzRun(t, ref, src, true)
			ref.Close()
			if strings.HasPrefix(want, "uncaught") {
				t.Fatalf("the program itself fails: %s", want)
			}
			for _, tree := range []bool{false, true} {
				SetTreeTier(tree)
				r := New(Config{})
				got := strFuzzRun(t, r, src, false)
				r.Close()
				if got != want {
					t.Errorf("tree tier %v: %s", tree, firstLogDifference(got, want))
				}
			}
		})
	}
}
