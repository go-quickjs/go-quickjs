//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// leafFastSetup's loop calls leaves of each kind -- String's native leaves
// (codePointAt, at, toUpperCase, startsWith), a Map's get, Number(x) and
// Number.isNaN -- a turn of the loop making seven; odd(n) gives at's call,
// at its eleventh turn, an object whose conversion runs script, which the
// leaf's check refuses.
const leafFastSetup = `
var conversions = 0
function leafTurns(s, m, xs, ks, n) {
	let h = 0, out = ''
	for (let i = 0; i < n; i++) {
		const x = xs[i % xs.length]
		const c = s.codePointAt(i % 7)
		out = s.at(ks[i % ks.length]) + ':' + m.get(i % 3) + ':' + Number(x) + ':' + Number.isNaN(x) + ':' + s.toUpperCase().length
		if (s.startsWith('h', i & 1)) h++
		h = (h * 31 + out.length + c) | 0
	}
	return h + '|' + out
}
var sm = new Map([[0, 'a'], [1, 'b'], [2, 'c']])
function leafs(n) { return leafTurns('h\u00e9llo\ud83d\ude00', sm, [1.5, '42', true, null, NaN, -0], [0, 3, -1, 9], n) }
function odd(n) { conversions = 0; return leafTurns('hello', sm, [2], [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, { valueOf() { conversions++; return 1 } }], n) + '|' + conversions }
`

// leafFastWants are leafFastSetup's rounds and their results, Node's
// (v26.10.0).
var leafFastWants = [][2]string{
	{"leafs(40)", "-127136937|undefined:a:0:false:7"},
	{"odd(40)", "112500713|undefined:a:2:false:5|3"},
	{"leafs(40)", "-127136937|undefined:a:0:false:7"},
}

// A leaf's call native code makes (ssa.GoLeaf) goes through the VM's
// routine for leaves (abi.Encoding's LeafFast, jitLeafGo): the call's
// description names its function (abi.GoCallState's Leaf), which native
// code passes no operand for, and Go applies the leaf with no expansion of
// the call nor any dispatch of calls of Go, as the form it made of the
// call at its first says (jitLeafForm), the result's number word back in a
// register. A leaf that refuses -- at's check, of an object -- leaves
// the call to an exit, the interpreter converting the object; each answer
// is Node's and the interpreter's.
func TestJITLeafFast(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitLeafFastForTest(t)
	e := jitNativeForTest(t, r, "leafTurns")
	leaves := 0
	for _, st := range e.ssa.GoCallStates() {
		if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf {
			if st.Leaf == 0 {
				t.Errorf("a leaf's call names no leaf: %+v", st)
			}
			leaves++
		}
	}
	if leaves < 6 {
		// Number.isNaN's made natively, no call (ssa.OpNumberIs).
		t.Errorf("leafTurns' code has %d leaves' calls, want six", leaves)
	}
	fast, refused, calls := r.jit.leafFast, r.jit.leafRefused, e.ssaStats.goCalls
	if _, err := r.Run(compileForTest(t, "leafs(40)")); err != nil {
		t.Fatal(err)
	}
	// Seven leaves a turn, made by the routine but for those native code
	// makes itself (ssa.LeafKind): every Number.isNaN and codePointAt, and
	// at's of 0 and 3, below 256, its of -1, a surrogate, and of 9, past
	// the end, the routine's. The Map's get read once, by Go.
	if got := r.jit.leafFast - fast; got != 4*40+20 || r.jit.leafRefused != refused || e.ssaStats.goCalls-calls > 1 {
		t.Errorf("leafs(40) made %d leaves' calls through the routine, refused %d, %d calls of Go with frames; want 180, 0, at most 1",
			got, r.jit.leafRefused-refused, e.ssaStats.goCalls-calls)
	}
	// Each call's form made once, at its first call, and kept in its
	// description (jitLeafForm).
	forms := 0
	for _, st := range e.ssa.GoCallStates() {
		if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf && st.LeafForm != nil {
			if (*jitLeafForm)(st.LeafForm).keep != st.Keep {
				t.Errorf("a leaf's call made with another's form: %+v", st)
			}
			forms++
		}
	}
	if forms < 5 {
		t.Errorf("%d leaves' calls have their forms, want the five the routine made", forms)
	}
	if _, err := r.Run(compileForTest(t, "odd(40)")); err != nil {
		t.Fatal(err)
	}
	if r.jit.leafRefused == refused {
		t.Error("at's call of an object was not refused")
	}
}

// jitLeafFastForTest runs leafFastSetup's rounds until leafTurns' leaves
// are calls of their own, each round's answer Node's and the
// interpreter's, and returns the runtime.
func jitLeafFastForTest(t *testing.T) *Runtime {
	t.Helper()
	var rounds []string
	for range 6 {
		rounds = append(rounds, "leafs(40)")
	}
	r := jitAgreesForTest(t, leafFastSetup, rounds...)
	jitLeafFastAgree(t, r)
	return r
}

// jitLeafFastAgree checks leafFastWants' rounds on r.
func jitLeafFastAgree(t *testing.T, r *Runtime) {
	t.Helper()
	for _, rd := range leafFastWants {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
		}
	}
}

// A leaf's call through the routine while the collector runs -- a
// collection in the middle of calls (jitGoCallWork), which scans the
// goroutine's stack through the routine's frame, and the code compiled
// for a collector that marks (its stores through Go's barrier) -- makes
// the leaves as it does otherwise: the answers Node's, the routine making
// them.
func TestJITLeafFastWhileCollecting(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		calls := 0
		jitGoCallWork = func() {
			if calls++; calls%97 == 0 {
				runtime.GC()
			}
		}
		r := jitLeafFastForTest(t)
		fast := r.jit.leafFast
		jitLeafFastAgree(t, r)
		jitGoCallWork = nil
		if r.jit.leafFast == fast || calls < 97 {
			t.Errorf("marking %v: %d leaves' calls through the routine while collections ran, %d calls of Go", marking, r.jit.leafFast-fast, calls)
		}
	}
}

// chainSetup's chain makes three leaves' calls in a row, the operands of
// the concatenation around them -- the first call's result, the '-'
// constants -- on the frame's stack across the calls after: what a call
// reads only for its refusal's state (mir's cold saves) is restored for
// the next call, which saves it for its own, and a refusal at the third
// call, of an object key, has its frame made of them, the interpreter
// going on with the object's valueOf.
const chainSetup = `
var gs = '\u03b1\u03b2\u03b3\u03b4\u03b5\u03b6\u03b7\u03b8', calls = 0
function chain3(a, b, c) { let out = '', h = 0; for (let j = 0; j < 8; j++) { out = gs.at(a) + '-' + gs.at(b) + '-' + gs.at(c); h += out.length } return out + h }
function chain(ks, n) { let out = '', h = 0; for (let i = 0; i < n; i++) { out = chain3(ks[i % 3], ks[(i + 1) % 3], ks[i % ks.length]); h = (h * 7 + out.charCodeAt(4)) | 0 } return out + h }
function plain(n) { return chain([1, 2, 3], n) }
function odd(n) { calls = 0; return chain([1, 2, 3, 4, { valueOf() { calls++; return 5 } }], n) + ':' + calls }
`

// Values only a call of Go's refusal reads after it are saved, and
// restored on its way alone, where another call after it reads them too
// for its own state: the answers are Node's (v26.10.0), as the frames the
// refusals make are.
func TestJITLeafColdSaves(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 6 {
		rounds = append(rounds, "plain(30)")
	}
	r := jitAgreesForTest(t, chainSetup, rounds...)
	fast := r.jit.leafFast
	for _, rd := range [][2]string{
		{"plain(30)", "\u03b4-\u03b2-\u03b440-531811864"},
		{"odd(30)", "\u03b4-\u03b2-\u03b6401680843488:48"},
		{"plain(30)", "\u03b4-\u03b2-\u03b440-531811864"},
	} {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
		}
	}
	cl := r.global.getOwn(r.atoms.intern("chain3")).value().Object().fn().closure
	if e := r.jit.hint(cl); e == nil || e.ssa == nil || r.jit.leafFast-fast < 3*8*90-48 {
		t.Errorf("chain3's leaves' calls through the routine: %d, want 3 a call but the refused", r.jit.leafFast-fast)
	}
}

// nativeFormsSetup's functions call String.prototype's codePointAt and
// at, which native code makes itself where it can (ssa.LeafKind), of
// strings of ASCII's bytes, of Latin-1, of UTF-16 with pairs, lone
// surrogates at either end, empty, of one unit, a rope, and of wrapper
// objects, at every kind of key: in range, at and past the end, below 0,
// fractions, -0, NaN, past 2**32, and keys that are no numbers.
const nativeFormsSetup = `
var big = String.fromCharCode(0xd83d, 0xde00, 0x3b1, 0xd800, 0x61, 0xdc00, 0xe9, 0xd83d)
var strs = ['hello', 'h' + String.fromCharCode(0xe9) + 'llo', big, '', 'x']
var keys = [0, 1, 2, 3, 4, 5, 6, 7, 8, -1, -2, -8, -9, 1.5, -0, NaN, 4294967296, '2', undefined, null, true]
function codes(s, ks) { let out = ''; for (let i = 0; i < ks.length; i++) out += s.codePointAt(ks[i]) + ','; return out }
function ats(s, ks) { let out = ''; for (let i = 0; i < ks.length; i++) { const c = s.at(ks[i]); out += (c === undefined ? 'u' : c.charCodeAt(0)) + ',' } return out }
function every() { let out = ''; for (const s of strs) out += codes(s, keys) + '|' + ats(s, keys) + '\n'; const r = strs[0] + strs[2]; return out + codes(r, [0, 5, 6]) + ats(r, [4, -3]) }
function sum(s, n) { let h = 0; for (let i = 0; i < n; i++) h = (h * 31 + s.codePointAt(i % s.length) + s.at(-1 - (i % s.length)).length) | 0; return h }
function wrapped(n) { return codes(new String('abc'), [0, 1, 2]) + ats(new String('abc'), [0, -1]) }
`

// codePointAt and at are made of their receivers' code units natively
// (OpStringCodePointOr, OpStringAtOr), the leaf's call made where native
// code says nothing of the call: every answer is Node's (v26.10.0), and a
// loop of in-range calls on a string of ASCII's bytes makes no call of Go.
func TestJITLeafNativeForms(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 6 {
		rounds = append(rounds, "every()", "sum('hello world', 50)")
	}
	r := jitAgreesForTest(t, nativeFormsSetup, rounds...)
	fast := r.jit.leafFast
	for _, rd := range [][2]string{
		{"every()", "104,101,108,108,111,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,101,104,104,undefined,108,104,104,101,|104,101,108,108,111,u,u,u,u,111,108,u,u,101,104,104,u,108,104,104,101,\n104,233,108,108,111,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,233,104,104,undefined,108,104,104,233,|104,233,108,108,111,u,u,u,u,111,108,u,u,233,104,104,u,108,104,104,233,\n128512,56832,945,55296,97,56320,233,55357,undefined,undefined,undefined,undefined,undefined,56832,128512,128512,undefined,945,128512,128512,56832,|55357,56832,945,55296,97,56320,233,55357,u,55357,233,55357,u,56832,55357,55357,u,945,55357,55357,56832,\nundefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,|u,u,u,u,u,u,u,u,u,u,u,u,u,u,u,u,u,u,u,u,u,\n120,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,120,120,undefined,undefined,120,120,undefined,|120,u,u,u,u,u,u,u,u,120,u,u,u,u,120,120,u,u,120,120,u,\n104,128512,56832,111,56320,"},
		{"sum('hello world', 50)", "1935277998"},
	} {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %q, want Node's %q", rd[0], got, rd[1])
		}
		if rd[0] == "every()" {
			fast = r.jit.leafFast
		}
	}
	if r.jit.leafFast != fast {
		t.Errorf("sum's calls of ASCII's units made %d calls of Go", r.jit.leafFast-fast)
	}
	for _, name := range []string{"codes", "ats", "sum"} {
		jitNativeForTest(t, r, name)
	}
	for _, rd := range [][2]string{
		{"sum(big + 'abc', 50)", "-1963823730"},
		{"wrapped(1)", "97,98,99,97,99,"},
	} {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %q, want Node's %q", rd[0], got, rd[1])
		}
	}
}

// goFastSetup's loop makes calls of Go of other kinds than a leaf's -- a
// comparison of strings (ssa.GoCompare), a concatenation's, an indexOf
// (GoIndexOf) -- and, in oddRuns, compares an object, whose conversion
// runs script, which Go refuses there.
const goFastSetup = `
var conv = 0
function mixed(xs, n) {
	let h = 0, s = ''
	for (let i = 0; i < n; i++) {
		const x = xs[i % xs.length]
		if (x < 'm') h++
		s = 'k' + x + i
		h = (h * 31 + s.length + 'abcdefghijklmnop'.indexOf(s.charAt(1))) | 0
	}
	return h + ':' + s
}
function plainRuns(n) { return mixed(['a', 'q', 'mm', 'b'], n) }
function oddRuns(n) { conv = 0; return mixed(['a', 'q', { valueOf() { conv++; return 'c' }, toString() { conv += 10; return 'd' } }], n) + ':' + conv }
`

// Every other call of Go whose site saves its live registers goes through
// the VM's routine for them (abi.Encoding's GoFast, jitGoDirect), past
// jit's callGo and its call of jit.HostCall and the code's own routine:
// each answer is Node's (v26.10.0), the loop's calls made so, and a
// comparison Go refuses leaves for the interpreter, which converts the
// object.
func TestJITGoFast(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 6 {
		rounds = append(rounds, "plainRuns(40)")
	}
	r := jitAgreesForTest(t, goFastSetup, rounds...)
	fast := r.jit.goFast
	for _, rd := range [][2]string{
		{"plainRuns(40)", "1457902686:kb39"},
		{"oddRuns(40)", "-1317125250:ka39:26"},
		{"plainRuns(40)", "1457902686:kb39"},
	} {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %q, want Node's %q", rd[0], got, rd[1])
		}
	}
	jitNativeForTest(t, r, "mixed")
	if r.jit.goFast-fast < 3*40 {
		t.Errorf("%d calls of Go through the routine in 120 turns, want at least one a turn", r.jit.goFast-fast)
	}
}

// goAddSetup's loops make + of Go (ssa.GoAdd) that no string builder
// takes: a string prepended to, s = x + s, and in mixedAdd a sum of
// BigInts beside a string's concatenation of numbers, booleans, null and
// BigInts; withSymbol's + of a symbol throws, which Go refuses.
const goAddSetup = `
function prepend(n) { let s = ''; for (let i = 0; i < n; i++) s = (i % 10) + s; return s.length + ':' + s.slice(0, 12) }
function mixedAdd(xs, n) { let r = '', t = 0n; for (let i = 0; i < n; i++) { const x = xs[i % xs.length]; if (typeof x === 'bigint') t = x + t; else r = x + r.slice(0, 16) } return r + ':' + t }
function plain(n) { return mixedAdd([1.5, 'ab', 7n, true, null, -0, 12345678901234567890n], n) }
function withSymbol(n) { try { return mixedAdd([1, 'x', Symbol('s')], n) } catch (e) { return e.constructor.name } }
`

// A + of Go that is no string builder's append goes from the VM's routine
// for calls of Go (jitGoDirect) to jitGoAdd, past jitHostCall's dispatch:
// each answer is Node's (v26.10.0), and the loops' calls are made so.
func TestJITGoAddDirect(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 6 {
		rounds = append(rounds, "prepend(40)", "plain(40)")
	}
	r := jitAgreesForTest(t, goAddSetup, rounds...)
	fast := r.jit.goFast
	for _, rd := range [][2]string{
		{"prepend(40)", "40:987654321098"},
		{"plain(40)", "nulltrueab1.50nulltr:61728394506172839492"},
		{"withSymbol(40)", "TypeError"},
		{"plain(40)", "nulltrueab1.50nulltr:61728394506172839492"},
	} {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %q, want Node's %q", rd[0], got, rd[1])
		}
	}
	jitNativeForTest(t, r, "prepend")
	jitNativeForTest(t, r, "mixedAdd")
	if r.jit.goFast-fast < 3*40 {
		t.Errorf("%d calls of Go through the routine in 160 turns, want at least one a turn", r.jit.goFast-fast)
	}
}

// mathSetup's loops call Math's functions Go computes (ssa.OpMathCall) --
// sin, atan2, hypot, exp -- with numbers, a float and an int32, live
// across them; onlySin's loop calls nothing else, so that it counts its
// back edges' budget in a register.
const mathSetup = `
function trig(n) { let s = 0, t = 0.5, k = 3; for (let i = 0; i < n; i++) { s += Math.sin(i * t) * k + Math.atan2(i, k) + Math.hypot(i, t) + Math.exp(-i / n); t += 0.001; k ^= i & 1 } return s + ':' + t + ':' + k }
function onlySin(n) { let s = 0; for (let i = 0; i < n; i++) s += Math.sin(i); return s }
`

// A Math function's call of Go saves only what lives across it (savesOf),
// not every register the code uses, and goes through the VM's routine for
// calls of Go (abi.Encoding's GoFast), the loop's budget in a register
// stored back around it: each answer is Node's (v26.10.0), also with a
// collection in the middle of the calls (jitGoCallWork) and with the code
// compiled for a collector that marks.
func TestJITMathCallsSaveLive(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		calls := 0
		jitGoCallWork = func() {
			if calls++; calls%113 == 0 {
				runtime.GC()
			}
		}
		var rounds []string
		for range 6 {
			rounds = append(rounds, "trig(100)", "onlySin(1000)")
		}
		r := jitAgreesForTest(t, mathSetup, rounds...)
		fast := r.jit.goFast
		for _, rd := range [][2]string{
			{"trig(100)", "5167.733570685861:0.6000000000000001:3"},
			{"onlySin(1000)", "-0.012909906458837123"},
		} {
			v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
			if err != nil {
				t.Fatalf("%s: %v", rd[0], err)
			}
			if got := v.String().Go(); got != rd[1] {
				t.Errorf("marking %v: %s = %q, want Node's %q", marking, rd[0], got, rd[1])
			}
		}
		jitGoCallWork = nil
		for _, name := range []string{"trig", "onlySin"} {
			jitNativeForTest(t, r, name)
		}
		if got := r.jit.goFast - fast; got < 4*100+1000 {
			t.Errorf("marking %v: %d calls of Math's functions through the routine, want 1400", marking, got)
		}
	}
}

// numberIsSetup's functions ask Number's predicates of numbers -- 0, -0,
// fractions, NaN, the infinities, the safe integers' bounds and past
// them, the denormals, 2**63, 2**64 -- and of values that are none, as a
// method and through a local, and with Number.isInteger replaced.
const numberIsSetup = `
var vals = [0, -0, 1, 1.5, -1.5, NaN, Infinity, -Infinity, 9007199254740991, 9007199254740992, -9007199254740991, -9007199254740992, 9007199254740994, 1e300, 5e-324, -5e-324, 9223372036854775808, 18446744073709551616, 4294967296.5, -4294967296, '1', '', true, null, undefined, {}, [], 1n, Symbol('s')]
function preds(vs) { let out = ''; for (let i = 0; i < vs.length; i++) { const x = vs[i]; out += (Number.isNaN(x) ? 1 : 0) + '' + (Number.isFinite(x) ? 1 : 0) + (Number.isInteger(x) ? 1 : 0) + (Number.isSafeInteger(x) ? 1 : 0) + ',' } return out }
function plain(vs) { const f = Number.isInteger, g = Number.isSafeInteger; let c = 0; for (let i = 0; i < vs.length; i++) { if (f(vs[i])) c++; if (g(vs[i])) c += 10 } return c }
function counts(n) { let c = 0; for (let i = 0; i < n; i++) { const x = i * 0.25; if (Number.isInteger(x)) c++; if (Number.isNaN(x - x)) c += 100 } return c }
function swapped(n) { const save = Number.isInteger; Number.isInteger = x => true; try { return counts(n) } finally { Number.isInteger = save } }
`

// Number.isNaN, isFinite, isInteger and isSafeInteger are made natively,
// no call at all (ssa.OpNumberIs), their function checked: each answer is
// Node's (v26.10.0), and the loop with the function replaced fails the
// check, the interpreter calling the replacement.
func TestJITNumberPredicates(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 6 {
		rounds = append(rounds, "preds(vals)", "plain(vals)", "counts(40)")
	}
	r := jitAgreesForTest(t, numberIsSetup, rounds...)
	fast := r.jit.leafFast
	for _, rd := range [][2]string{
		{"preds(vals)", "0111,0111,0111,0100,0100,1000,0000,0000,0111,0110,0111,0110,0110,0110,0100,0100,0110,0110,0100,0111,0000,0000,0000,0000,0000,0000,0000,0000,0000,"},
		{"plain(vals)", "72"},
		{"counts(40)", "10"},
		{"swapped(40)", "40"},
		{"counts(40)", "10"},
	} {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %q, want Node's %q", rd[0], got, rd[1])
		}
	}
	for _, name := range []string{"preds", "plain", "counts"} {
		jitNativeForTest(t, r, name)
	}
	if r.jit.leafFast != fast {
		t.Errorf("Number's predicates made %d calls of Go", r.jit.leafFast-fast)
	}
}
