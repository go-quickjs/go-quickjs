//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"
)

// mathStubSetup's loops have Go compute Math's functions through their own
// routines (abi.GoStub): Math.pow and ** of what is no exact power, % of
// what is no integer's, sin, exp, log, cbrt and tanh, atan2 and hypot --
// NaN, the infinities, the zeros of either sign, denormals among the
// operands -- with an int32 and a float live across each, which the routine
// clobbers the registers of. Each result's bits, NaN made one marker, go to
// a Float64Array whose words are hashed, so that a result whose sign or
// last bit differs differs.
const mathStubSetup = `
const xs = [2.5, -2, 0, -0, NaN, Infinity, -Infinity, 5e-324, 7, -3.75, 1e300, 0.1]
const ys = [0.5, -1.5, 3.25, NaN, -0, 1e10, -Infinity, 0.1, 2, -3, Infinity, 1e-310, 7.5]
function hash(out) { const u = new Uint32Array(out.buffer); let h = 0; for (let i = 0; i < u.length; i++) h = (Math.imul(h, 31) + u[i]) | 0; return h }
function put(out, i, r) { out[i] = r !== r ? -12345.5 : r }
function pows(n) {
	const out = new Float64Array(n * 2); let k = 3, f = 0.25
	for (let i = 0; i < n; i++) {
		const x = xs[i % xs.length], y = ys[i % ys.length]
		put(out, 2 * i, Math.pow(x, y) + k); put(out, 2 * i + 1, (x + f) ** y)
		k ^= i & 5; f += 0.5
	}
	return hash(out) + ':' + k + ':' + f
}
function mods(n) {
	const out = new Float64Array(n); let k = 1, f = 0.75
	for (let i = 0; i < n; i++) {
		const x = xs[i % xs.length] * f, y = ys[(i * 7) % ys.length]
		put(out, i, x % y + k); k = (k * 3 + i) & 255; f += 0.125
	}
	return hash(out) + ':' + k + ':' + f
}
function unary(n) {
	const out = new Float64Array(n * 5); let k = 2, f = 1.5
	for (let i = 0; i < n; i++) {
		const x = xs[i % xs.length] + f
		put(out, 5 * i, Math.sin(x)); put(out, 5 * i + 1, Math.exp(x / 7)); put(out, 5 * i + 2, Math.log(x))
		put(out, 5 * i + 3, Math.cbrt(x) * k); put(out, 5 * i + 4, Math.tanh(x))
		k = (k + i) % 11; f -= 0.375
	}
	return hash(out) + ':' + k + ':' + f
}
function binary(n) {
	const out = new Float64Array(n * 2); let k = 5, f = -0.5
	for (let i = 0; i < n; i++) {
		const x = xs[i % xs.length], y = ys[(i * 5) % ys.length] + f
		put(out, 2 * i, Math.atan2(x, y) * k); put(out, 2 * i + 1, Math.hypot(x, y) + f)
		k = (k ^ i) & 15; f += 0.25
	}
	return hash(out) + ':' + k + ':' + f
}
`

// The Math functions Go computes are made through their own routines
// (abi.Encoding's GoStubs), with no description of the call and no other
// call of Go on the way -- pow's and %'s too, which had saved every
// register around jit's callGo -- and give the interpreter's results bit
// for bit, the values live across them kept, with collections at the calls
// (jitTestConfig's goCallWork) and with the code compiled for a collector
// that marks.
func TestJITMathStubs(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range jitMarkingModes() {
		if marking {
			jitMarkingForTest(t)
		}
		calls := 0
		jitTestSwitch(t, func(jc *jitTestConfig) {
			jc.goCallWork = func() {
				if calls++; calls%97 == 0 {
					runtime.GC()
				}
			}
		})
		var rounds []string
		for range 6 {
			rounds = append(rounds, "pows(200)", "mods(200)", "unary(100)", "binary(150)")
		}
		r := jitAgreesForTest(t, mathStubSetup, rounds...)
		for _, name := range []string{"pows", "mods", "unary", "binary"} {
			jitNativeForTest(t, r, name)
		}
		stubs, goCalls := r.jit.goStub, calls
		if _, err := r.Run(compileForTest(t, "''+pows(200)+mods(200)+unary(100)+binary(150)")); err != nil {
			t.Fatal(err)
		}
		// Of pow's 400 calls, those of an exact power are native, as are %'s
		// of integers; atan2's and hypot's 300, and sin's 100 at least, are
		// Go's.
		got := r.jit.goStub - stubs
		t.Logf("marking %v: %d calls through the routines", marking, got)
		if got < 200+2*150+100 {
			t.Errorf("marking %v: %d calls of Math's functions through their routines", marking, got)
		}
		if calls == goCalls {
			t.Errorf("marking %v: the routines did not do the test's work at a call of Go", marking)
		}
	}
}

// stringStubSetup's loops have Go make a string's + through the routines of
// their own (abi.GoStubAppend, GoStubAdd, GoStubConcat): a builder's
// s += x of strings, numbers, booleans and null, a prepend, a template of
// four parts and a concatenation of three, with an int32 and a float live
// across each; withSymbol's + of a symbol, which Go refuses, leaves for
// the exit, which throws the interpreter's TypeError.
const stringStubSetup = `
const parts = ['ab', 7, 'c', true, null, -0, 1.5, 'xyz', 12345678901234567890n]
function build(n) {
	let s = '', p = '', t = '', k = 1, f = 0.5
	for (let i = 0; i < n; i++) {
		const x = parts[i % parts.length]
		s += x; p = (i % 10) + p.slice(0, 20)
		t = ` + "`" + `${x}-${k}:${f}|${i}` + "`" + ` + t.slice(0, 30)
		k = (k * 5 + i) & 1023; f += 0.25
	}
	return s.length + ':' + s.slice(-40) + ':' + p + ':' + t + ':' + k + ':' + f
}
function withSymbol(n) {
	let s = ''
	try { for (let i = 0; i < n; i++) s += (i == n - 3 ? Symbol('q') : 'z') } catch (e) { return e.constructor.name + ':' + s.length }
	return s
}
`

// A string's + Go makes -- a builder's append, any other +, a template's
// or a concatenation's parts joined -- goes through the operation's own
// routine (abi.Encoding's GoStubs), its operands in the context's GoArgs,
// past jitGoDirect's and jitHostCall's dispatch, with the interpreter's
// answers, a refusal leaving for the exit, which throws as the interpreter
// does; with collections at the calls and with the code compiled for a
// collector that marks.
func TestJITStringStubs(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range jitMarkingModes() {
		if marking {
			jitMarkingForTest(t)
		}
		calls := 0
		jitTestSwitch(t, func(jc *jitTestConfig) {
			jc.goCallWork = func() {
				if calls++; calls%89 == 0 {
					runtime.GC()
				}
			}
		})
		var rounds []string
		for range 6 {
			rounds = append(rounds, "build(300)", "withSymbol(40)")
		}
		r := jitAgreesForTest(t, stringStubSetup, rounds...)
		jitNativeForTest(t, r, "build")
		stubs := r.jit.goStub
		if _, err := r.Run(compileForTest(t, "''+build(300)")); err != nil {
			t.Fatal(err)
		}
		// An append, a + and a template's parts a turn, at least.
		if got := r.jit.goStub - stubs; got < 3*300 {
			t.Errorf("marking %v: %d calls of a string's + through their routines in 300 turns", marking, got)
		}
	}
}

// keyedStubSetup's loops have Go make the keyed operations native code
// leaves to it through their routines (abi.GoStubDelete, GoStubIndexOf,
// GoStubIndex, GoStubKeyedWrite, GoStubHas): a property's delete, sloppy
// and strict, a frozen object's in strict code throwing its TypeError
// from the exit; a string's and an array's search, from a position too; a
// read and a write by keys no array's index -- strings, a symbol, a number
// past the elements -- and the in operator, a proxy's has trap among them,
// which Go refuses, the exit running it.
const keyedStubSetup = `
const keys = ['a', 'b', 'length', 'x1', 'q', 'zz']
const sym = Symbol('s')
const handler = { has(t, k) { trapped++; return k === 'q' } }
let trapped = 0
function keyed(n) {
	let h = 0, k = 7, f = 0.5
	for (let i = 0; i < n; i++) {
		const o = { a: 1, b: 2, x1: i, [sym]: 3 }
		const key = keys[i % keys.length]
		o[key + (i & 1)] = i; o[sym] = h
		h = (h * 31 + (o[key] | 0) + (o['a' + (i & 1)] | 0) + ((key in o) ? 1 : 0)) | 0
		delete o[key]; delete o.b
		h = (h + Object.keys(o).length + ('hello world'.indexOf(key.charAt(0), i % 4)) + [3, 1, i, 4].indexOf(i, 1) + ('b' in o ? 1 : 0)) | 0
		k = (k * 3 + i) & 255; f += 0.125
	}
	return h + ':' + k + ':' + f
}
function strictDelete(n) {
	'use strict'
	const frozen = Object.freeze({ a: 1, b: 2 })
	let d = 0
	try { for (let i = 0; i < n; i++) { const o = { a: i, b: 1 }; delete o[keys[i % 2]]; d += Object.keys(o).length; if (i == n - 2) delete frozen[keys[i % 2]] } } catch (e) { return e.constructor.name + ':' + d }
	return 'no error:' + d
}
function proxied(n) {
	trapped = 0
	const p = new Proxy({}, handler)
	let c = 0
	for (let i = 0; i < n; i++) { const t = i % 5 == 4 ? p : { q: 1 }; if (keys[i % keys.length] in t) c++ }
	return c + ':' + trapped
}
`

// The keyed operations Go makes go through their own routines
// (abi.Encoding's GoStubs), past jitGoDirect's and jitHostCall's dispatch,
// with the interpreter's answers, what Go refuses -- a strict delete that
// throws, a proxy's trap -- leaving for the exit, which makes it as the
// interpreter does; with collections at the calls and with the code
// compiled for a collector that marks.
func TestJITKeyedStubs(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range jitMarkingModes() {
		if marking {
			jitMarkingForTest(t)
		}
		calls := 0
		jitTestSwitch(t, func(jc *jitTestConfig) {
			jc.goCallWork = func() {
				if calls++; calls%83 == 0 {
					runtime.GC()
				}
			}
		})
		var rounds []string
		for range 6 {
			rounds = append(rounds, "keyed(200)", "strictDelete(60)", "proxied(60)")
		}
		r := jitAgreesForTest(t, keyedStubSetup, rounds...)
		jitNativeForTest(t, r, "keyed")
		stubs := r.jit.goStub
		if _, err := r.Run(compileForTest(t, "''+keyed(200)")); err != nil {
			t.Fatal(err)
		}
		if got := r.jit.goStub - stubs; got < 2*200 {
			t.Errorf("marking %v: %d keyed calls of Go through their routines in 200 turns", marking, got)
		}
	}
}
