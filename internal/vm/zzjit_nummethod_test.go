//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// numMethodSetup's loops call methods read off numbers of every kind --
// toString with and without a radix, toFixed, toPrecision, toExponential,
// valueOf, and the script's own on Number.prototype, strict and sloppy --
// and off booleans; and, beside them, what the read must notice:
// Number.prototype.toFixed replaced, made a getter or deleted, a method
// Number.prototype gains that shadows Object.prototype's, Object.prototype
// gaining a property a number reads, and a Number wrapper, a string, a
// boolean and a BigInt at a site that has met numbers, or numbers at one
// that has met booleans. The script's methods say what receiver they were
// given: a strict one the primitive, a sloppy one its wrapper.
const numMethodSetup = `
function numLoop(xs, n) { let h = 0, all = '', out = ''; for (let i = 0; i < n; i++) { const x = xs[i % xs.length]; out = x.toString() + ' ' + x.toString(16) + ' ' + x.toString(36) + ' ' + x.toFixed(2); out += ' ' + x.toPrecision(3) + ' ' + x.toExponential(1) + ' ' + x.valueOf(); if (i < xs.length) all += out + ';'; h = (h * 31 + out.length) | 0 } return h + '|' + all }
function nums(n) { return numLoop([0, 7, -3, 1.5, -0.125, 255.75, -0, NaN, Infinity, -Infinity, 1e21, 123456789.123, 2 ** 53, 5e-7], n) }
function scaled(n) { let s = 0, out = ''; for (let i = 0; i < n; i++) { out = '' + (i * 1.5).toString(16); s = (s * 31 + out.length) | 0 } return s + '|' + out }
function boolLoop(xs, n) { let h = 0, out = ''; for (let i = 0; i < n; i++) { const b = xs[i % xs.length]; const s = b.toString(); h = (h * 31 + s.length + (b.valueOf() === true ? 1 : 2)) | 0; if (i < xs.length) out += s + ',' } return h + '|' + out }
function bools(n) { return boolLoop([true, false], n) }
function mixLoop(xs, n) { let out = ''; for (let i = 0; i < n; i++) { const x = xs[i % xs.length]; out += x.toString() + '/' + x.valueOf() + ',' } return out }
function describeLoop(xs, n) { let out = ''; for (let i = 0; i < n; i++) { const x = xs[i % xs.length]; out += x.describe() + x.toFixed(1) + ',' } return out }
Object.prototype.describe = function () { 'use strict'; return 'O' + typeof this }
Number.prototype.twice = function () { 'use strict'; return typeof this + this * 2 }
Number.prototype.sloppy = function () { return typeof this + (this instanceof Number) }
function ownLoop(xs, n) { let out = ''; for (let i = 0; i < n; i++) { const x = xs[i % xs.length]; out += x.twice() + x.sloppy() + ',' } return out }
function replacedFixed(n) { const save = Number.prototype.toFixed; Number.prototype.toFixed = function (d) { 'use strict'; return 'F' + d + typeof this + this }; try { return nums(n) } finally { Number.prototype.toFixed = save } }
function shadowed(n) { Number.prototype.describe = function () { 'use strict'; return 'N' + typeof this }; try { return describeLoop([1.25, -2, NaN], n) } finally { delete Number.prototype.describe } }
function objectGains(n) { Object.prototype.toFixed = function () { return 'bad' }; Object.prototype.extra = 1; try { return nums(n) + describeLoop([3.5], n) } finally { delete Object.prototype.toFixed; delete Object.prototype.extra } }
function getter(n) { const d = Object.getOwnPropertyDescriptor(Number.prototype, 'toFixed'); Object.defineProperty(Number.prototype, 'toFixed', { get() { 'use strict'; const r = typeof this; return () => 'G' + r }, configurable: true }); try { return nums(n) } finally { Object.defineProperty(Number.prototype, 'toFixed', d) } }
function deleted(n) { const save = Number.prototype.toFixed; delete Number.prototype.toFixed; try { return nums(n) } catch (e) { return e.constructor.name } finally { Number.prototype.toFixed = save } }
function mixed(n) { return mixLoop([1.5, new Number(5), 'abc', true, 7n, -0], n) }
function boolMixed(n) { return boolLoop([true, 4.5, false, new Boolean(false), 'x'], n) }
`

// A method read off a number, or off a boolean, is made natively where the
// read has met numbers, or booleans, and Number.prototype, or
// Boolean.prototype, has the method (jitFeedback's StringProperty,
// jitPrimitiveSeen), as a string's off String.prototype: each answer is
// Node's (v26.10.0) and the interpreter's, and, warm, each loop runs
// natively and never leaves its code -- but ownLoop, at its sloppy
// method's call, whose receiver only Go wraps -- numLoop's seven calls of
// Number's methods each a leaf's call of its own (ssa.GoLeaf) and none a
// call of Go with a frame. The read checks the receiver's kind and the prototype's
// shape, which a change of its table replaces: a method replaced is
// called, a getter is, a method deleted is not there, one the prototype
// gains is read, Object.prototype's do not shadow the prototype's, and a
// receiver of another kind is read by Go.
func TestJITNumberMethods(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	warm := []string{"nums(28)", "scaled(30)", "bools(6)", "mixLoop([1.5, -0, 3], 6)", "describeLoop([1.25, -2, NaN], 6)", "ownLoop([1.25, -0, 3], 9)"}
	var rounds []string
	for range 12 {
		rounds = append(rounds, warm...)
	}
	r := jitAgreesForTest(t, numMethodSetup, rounds...)
	e := jitNativeForTest(t, r, "numLoop")
	leaves := 0
	for _, st := range e.ssa.GoCallStates() {
		if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf {
			leaves++
		}
	}
	if leaves < 7 {
		t.Errorf("numLoop's code has %d leaves' calls, want the seven", leaves)
	}
	for _, c := range []struct{ fn, src string }{
		{"numLoop", "nums(28)"},
		{"scaled", "scaled(30)"},
		{"boolLoop", "bools(6)"},
		{"mixLoop", "mixLoop([1.5, -0, 3], 6)"},
	} {
		e := jitNativeForTest(t, r, c.fn)
		calls, hosts, entries := e.ssaStats.goCalls, e.ssaStats.hosts, e.ssaStats.entries
		if _, err := r.Run(compileForTest(t, c.src)); err != nil {
			t.Fatal(err)
		}
		calls, hosts, entries = e.ssaStats.goCalls-calls, e.ssaStats.hosts-hosts, e.ssaStats.entries-entries
		if entries == 0 || hosts != 0 || c.fn == "numLoop" && calls != 0 {
			t.Errorf("%s: %s ran natively %d times, made %d calls of Go with frames and left its code %d times", c.src, c.fn, entries, calls, hosts)
		}
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	if _, err := want.Run(compileForTest(t, numMethodSetup)); err != nil {
		t.Fatal(err)
	}
	const nums = "-507415500|0 0 0 0.00 0.00 0.0e+0 0;7 7 7 7.00 7.00 7.0e+0 7;-3 -3 -3 -3.00 -3.00 -3.0e+0 -3;1.5 1.8 1.i 1.50 1.50 1.5e+0 1.5;-0.125 -0.2 -0.4i -0.13 -0.125 -1.3e-1 -0.125;255.75 ff.c 73.r 255.75 256 2.6e+2 255.75;0 0 0 0.00 0.00 0.0e+0 0;NaN NaN NaN NaN NaN NaN NaN;Infinity Infinity Infinity Infinity Infinity Infinity Infinity;-Infinity -Infinity -Infinity -Infinity -Infinity -Infinity -Infinity;1e+21 3635c9adc5dea00000 5v1j4f4ds7c000 1e+21 1.00e+21 1.0e+21 1e+21;123456789.123 75bcd15.1f7ced8 21i3v9.4feor 123456789.12 1.23e+8 1.2e+8 123456789.123;9007199254740992 20000000000000 2gosa7pa2gw 9007199254740992.00 9.01e+15 9.0e+15 9007199254740992;5e-7 0.000008637bd05af6c68 0.0000u8e2yc14t4 0.00 5.00e-7 5.0e-7 5e-7;"
	const describe = "Onumber1.3,Onumber-2.0,OnumberNaN,Onumber1.3,Onumber-2.0,OnumberNaN,"
	const boolean = "149766246|true,false,"
	const own = "number2.5objecttrue,number0objecttrue,number6objecttrue,number2.5objecttrue,number0objecttrue,number6objecttrue,number2.5objecttrue,number0objecttrue,number6objecttrue,"
	for _, rd := range [][2]string{
		{"nums(28)", nums},
		{"scaled(30)", "79327327|2b.8"},
		{"bools(6)", boolean},
		{"mixLoop([1.5, -0, 3], 6)", "1.5/1.5,0/0,3/3,1.5/1.5,0/0,3/3,"},
		{"describeLoop([1.25, -2, NaN], 6)", describe},
		{"ownLoop([1.25, -0, 3], 9)", own},
		{"mixed(12)", "1.5/1.5,5/5,abc/abc,true/true,7/7,0/0,1.5/1.5,5/5,abc/abc,true/true,7/7,0/0,"},
		{"boolMixed(10)", "-106915360|true,4.5,false,false,x,"},
		{"replacedFixed(28)", "-1448955516|0 0 0 F2number0 0.00 0.0e+0 0;7 7 7 F2number7 7.00 7.0e+0 7;-3 -3 -3 F2number-3 -3.00 -3.0e+0 -3;1.5 1.8 1.i F2number1.5 1.50 1.5e+0 1.5;-0.125 -0.2 -0.4i F2number-0.125 -0.125 -1.3e-1 -0.125;255.75 ff.c 73.r F2number255.75 256 2.6e+2 255.75;0 0 0 F2number0 0.00 0.0e+0 0;NaN NaN NaN F2numberNaN NaN NaN NaN;Infinity Infinity Infinity F2numberInfinity Infinity Infinity Infinity;-Infinity -Infinity -Infinity F2number-Infinity -Infinity -Infinity -Infinity;1e+21 3635c9adc5dea00000 5v1j4f4ds7c000 F2number1e+21 1.00e+21 1.0e+21 1e+21;123456789.123 75bcd15.1f7ced8 21i3v9.4feor F2number123456789.123 1.23e+8 1.2e+8 123456789.123;9007199254740992 20000000000000 2gosa7pa2gw F2number9007199254740992 9.01e+15 9.0e+15 9007199254740992;5e-7 0.000008637bd05af6c68 0.0000u8e2yc14t4 F2number5e-7 5.00e-7 5.0e-7 5e-7;"},
		{"nums(28)", nums},
		{"shadowed(6)", "Nnumber1.3,Nnumber-2.0,NnumberNaN,Nnumber1.3,Nnumber-2.0,NnumberNaN,"},
		{"describeLoop([1.25, -2, NaN], 6)", describe},
		{"objectGains(14)", "-2094465830|0 0 0 0.00 0.00 0.0e+0 0;7 7 7 7.00 7.00 7.0e+0 7;-3 -3 -3 -3.00 -3.00 -3.0e+0 -3;1.5 1.8 1.i 1.50 1.50 1.5e+0 1.5;-0.125 -0.2 -0.4i -0.13 -0.125 -1.3e-1 -0.125;255.75 ff.c 73.r 255.75 256 2.6e+2 255.75;0 0 0 0.00 0.00 0.0e+0 0;NaN NaN NaN NaN NaN NaN NaN;Infinity Infinity Infinity Infinity Infinity Infinity Infinity;-Infinity -Infinity -Infinity -Infinity -Infinity -Infinity -Infinity;1e+21 3635c9adc5dea00000 5v1j4f4ds7c000 1e+21 1.00e+21 1.0e+21 1e+21;123456789.123 75bcd15.1f7ced8 21i3v9.4feor 123456789.12 1.23e+8 1.2e+8 123456789.123;9007199254740992 20000000000000 2gosa7pa2gw 9007199254740992.00 9.01e+15 9.0e+15 9007199254740992;5e-7 0.000008637bd05af6c68 0.0000u8e2yc14t4 0.00 5.00e-7 5.0e-7 5e-7;Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,Onumber3.5,"},
		{"getter(14)", "1218470402|0 0 0 Gnumber 0.00 0.0e+0 0;7 7 7 Gnumber 7.00 7.0e+0 7;-3 -3 -3 Gnumber -3.00 -3.0e+0 -3;1.5 1.8 1.i Gnumber 1.50 1.5e+0 1.5;-0.125 -0.2 -0.4i Gnumber -0.125 -1.3e-1 -0.125;255.75 ff.c 73.r Gnumber 256 2.6e+2 255.75;0 0 0 Gnumber 0.00 0.0e+0 0;NaN NaN NaN Gnumber NaN NaN NaN;Infinity Infinity Infinity Gnumber Infinity Infinity Infinity;-Infinity -Infinity -Infinity Gnumber -Infinity -Infinity -Infinity;1e+21 3635c9adc5dea00000 5v1j4f4ds7c000 Gnumber 1.00e+21 1.0e+21 1e+21;123456789.123 75bcd15.1f7ced8 21i3v9.4feor Gnumber 1.23e+8 1.2e+8 123456789.123;9007199254740992 20000000000000 2gosa7pa2gw Gnumber 9.01e+15 9.0e+15 9007199254740992;5e-7 0.000008637bd05af6c68 0.0000u8e2yc14t4 Gnumber 5.00e-7 5.0e-7 5e-7;"},
		{"deleted(14)", "TypeError"},
		{"nums(28)", nums},
		{"nums(28)", nums},
		{"bools(6)", boolean},
		{"ownLoop([1.25, -0, 3], 9)", own},
	} {
		for _, rt := range []*Runtime{want, r} {
			v, err := rt.Run(compileForTest(t, "''+("+rd[0]+")"))
			if err != nil {
				t.Fatalf("%s: %v", rd[0], err)
			}
			if got := v.String().Go(); got != rd[1] {
				t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
			}
		}
	}
}
