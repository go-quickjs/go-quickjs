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
