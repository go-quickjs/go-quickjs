//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// paramsSetup's functions have parameter lists with defaults and patterns,
// whose prologues bind each parameter in turn: withDefault's default
// runs where its argument is missing or undefined; withPattern's first
// parameter is a pattern -- its source, a getter's object, read by Go, after
// an exit in the prologue -- and its second a default; withCall's defaults
// call a function and read the parameter before them; tdz's default reads
// a parameter after it, in the dead zone; and run calls them all.
const paramsSetup = `
function withDefault(a, b = 2) { let s = 0; for (let i = 0; i < a; i++) s += b; return s }
function withPattern({ x, y }, k = 3) { let s = 0; for (let i = 0; i < 50; i++) s += x * k + y; return s }
function side(log, v) { log.push(v); return v }
function withCall(o, a, b = side(o.log, a + 1), c = b * 2) { let s = 0; for (let i = 0; i < 20; i++) s += a + b + c; return s }
function tdz(a = b, b = 1) { return a }
var getter = { get x() { return 7 }, y: 1 }
function run() { const o = { log: [] }; return [withDefault(10), withDefault(10, 5), withDefault(3, undefined), withPattern({ x: 4, y: 2 }), withPattern({ x: 1, y: 1 }, 1), withPattern(getter), withPattern(getter, 2), withCall(o, 4), withCall(o, 4, 1), withCall(o, 4, 1, 1), o.log.join()].join(' ') }
function threw(f) { try { f() } catch (e) { return e.constructor.name } return 'none' }
`

// A function whose parameter list has a default or a pattern is native
// code's, for Go to enter (not native callers', whose frames have no
// arguments for the interpreter to go on with): its prologue binds each
// parameter from what the slots had at the function's start (ssa's
// ParamsDead), never entered inside it, and where it leaves -- a getter
// a pattern reads, a call a default makes -- the interpreter goes on with
// the call's arguments. Such functions -- 1,044 of the 13,986 of npm's
// own code -- had been refused whole. Each answer is Node's (v26.10.0)
// and the interpreter's, and the functions' loops run natively.
func TestJITParameterPrologues(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, want := jitAnswersForTest(t, paramsSetup)
	for range 200 {
		want(`run()`, "20 50 6 700 100 1100 750 380 140 120 5")
	}
	for _, name := range []string{"withDefault", "withPattern", "withCall"} {
		jitNativeForTest(t, r, name)
	}
	want(`threw(tdz)`, "ReferenceError")
	want(`threw(() => withPattern(null))`, "TypeError")
	want(`threw(() => withPattern())`, "TypeError")
	want(`run()`, "20 50 6 700 100 1100 750 380 140 120 5")
}
