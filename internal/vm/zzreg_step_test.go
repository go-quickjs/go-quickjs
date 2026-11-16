package vm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regStepSrc runs the step-and-branches (zzreg_zztable_step.go) on their
// fast paths and on every way off them, three rounds, each loop entered
// and left at its boundaries: each of the six kinds, steps of 1, of 63
// and of -64; counters past 2^53 and across the int32 range, fractions,
// -0, NaN, Infinity; a bound that changes type mid-loop, a string's, a
// BigInt's, an object's; an upvalue bound a closure's call changes;
// counters that are strings (++ makes them numbers, += concatenates, -=
// subtracts), BigInts, objects whose valueOf counts its calls or throws,
// a Symbol's TypeError; a counter whose type the body changes; let
// counters closures capture, per iteration; a continue that lands on the
// step; and loops past the interrupt budget's back edges. Its answer is
// Node's.
const regStepSrc = `function show(x) { return Object.is(x, -0) ? "-0" : typeof x === "bigint" ? x + "n" : typeof x === "string" ? JSON.stringify(x) : String(x) }
function up(n) { var s = 0, c = 0; for (let i = 0; i < n; i++) { s += i; c++ } return s + "/" + c }
function upLe(n) { var c = 0, i; for (i = 0; i <= n; i++) c++; return c + "@" + show(i) }
function upImm() { var c = 0, i; for (i = 0; i < 5; i++) c++; return c + "@" + i }
function upK(n) { var c = 0, i; for (i = 0; i < n; i += 63) c++; return c + "@" + i }
function downK(n) { var c = 0, i; for (i = n; i > 0; i -= 64) c++; return c + "@" + i }
function whileDec(n) { var c = 0; while (--n >= 0) c++; return c + "@" + show(n) }
function doDec(j) { var c = 0; do { c++ } while (--j > 0); return c + "@" + show(j) }
function upval(a) { var f = function () { var c = 0, u; for (u = 0; u <= a; u++) c++; return c + "@" + u }; return f() }
function upvalChange() { var a = 3; var f = function () { var c = 0, u; for (u = 0; u <= a; u++) { c++; if (u == 1) a = "4"; if (u == 3) a = 5n; if (c > 9) break } return c + "@" + show(u) }; return f() }
function cont(n) { var c = 0; for (var i = 0; i < n; i++) { if (i & 1) continue; c++ } return c }
function big53() { var c = 0, i; for (i = 2 ** 53 - 2; i < 2 ** 53 + 2; i++) { if (++c > 10) break } return c + "@" + i }
function i32() { var c = 0, i; for (i = 2147483645; i < 2147483650; i++) c++; var d = 0, j; for (j = -2147483646; j > -2147483650; j -= 1) d++; return c + "@" + i + "," + d + "@" + j }
function frac() { var c = 0, i; for (i = 0.5; i < 3; i++) c++; var d = 0, j; for (j = 0.25; j <= 2; j += 1) d++; return c + "@" + i + "," + d + "@" + j }
function negz() { var c = 0, i; for (i = -1; i < 0; i++) c++; var j = -0, d = 0; for (; j < 1; j += 1) d++; var k = 1, e = 0; while (--k > -1) e++; return c + "@" + show(i) + "," + d + "@" + show(j) + "," + e + "@" + show(k) }
function nanC() { var c = 0, i; for (i = NaN; i < 3; i++) c++; var j; for (j = 0; j < NaN; j++) c++; var k = NaN; while (--k >= 0) c++; return c + "@" + i + "," + j + "," + k }
function inf() { var c = 0, i; for (i = 0; i < Infinity; i++) if (++c >= 7) break; var j = Infinity, d = 0; while (--j >= 0) if (++d >= 3) break; return c + "@" + i + "," + d + "@" + j }
function bndType() { var c = 0, n = 3, i; for (i = 0; i < n; i++) { c++; if (i == 1) n = "5"; if (i == 3) n = 6n; if (i == 4) n = { valueOf() { return 6 } }; if (c > 9) break } return c + "@" + i }
function strUp() { var c = 0, i; for (i = "0"; i < 5; i++) c++; return c + "@" + show(i) }
function strAdd() { var r = [], i; for (i = "0"; i < 50; i += 2) r.push(i); return r.join(",") + "@" + show(i) }
function strSub() { var r = [], i; for (i = "9"; i > 0; i -= 2) r.push(show(i)); return r.join(",") }
function strDec() { var r = [], j = "3"; while (--j >= 0) r.push(show(j)); return r.join(",") + "@" + show(j) }
function midType() { var c = 0, i, r = []; for (i = 0; i < 6; i++) { c++; if (i == 1) i = "2"; if (i == 3) i = 4n; r.push(show(i)); if (c > 9) break } return r.join(",") + "@" + show(i) }
function bigC() { var c = 0, i; for (i = 0n; i < 3n; i++) c++; var j; for (j = 5n; j > 0; j--) c++; var k = 3n; while (--k >= 0) c++; return c + "@" + show(i) + show(j) + show(k) }
function bigMix() { var c = 0, i; for (i = 0n; i < 3; i++) c++; var k = 2n; do { c++ } while (--k > 0); return c + "@" + show(i) + show(k) }
function bigAdd() { try { for (var i = 0n; i < 3n; i += 1) {} return "no" } catch (e) { return e.constructor.name + "@" + show(i) } }
function vo() { var calls = 0, o = { valueOf() { calls++; return 1 } }, c = 0, i; for (i = o; i < 3; i++) c++; return c + "/" + calls + "@" + i }
function voAdd() { var calls = 0, o = { valueOf() { calls++; return 1 } }, r = [], i; for (i = o; i < 40; i += 10) r.push(String(i)); return r.join() + "/" + calls }
function voSub() { var calls = 0, o = { valueOf() { calls++; return 20 } }, r = [], i; for (i = o; i > 0; i -= 7) r.push(String(i)); return r.join() + "/" + calls }
function voThrow() { var n = 0, o = { valueOf() { if (++n == 2) throw new RangeError("v" + n); return 0 } }, i = o; try { for (; i < 3; i++) {} return "no" } catch (e) { return e.name + e.message + "@" + typeof i + n } }
function symThrow() { var i = Symbol("s"), c = 0; try { for (; c < 3; i++) c++ } catch (e) { return e.constructor.name + c } }
function caps() { var fs = []; for (let i = 0; i < 3; i++) fs.push(() => i); var gs = []; for (let j = 0; j < 4; j += 2) gs.push(() => j++); return fs.map(f => f()).join() + "," + gs.map(g => g()).join() + gs.map(g => g()).join() }
function capVar() { var i, fs = []; for (i = 0; i < 3; i++) fs.push(() => i); return fs.map(f => f()).join() }
function many() { var s = 0, i; for (i = 0; i < 300000; i++) s += i; var c = 0, j = 300000; while (--j >= 0) c++; var d = 0, k = 100000; do { d++ } while (--k > 0); return s + "," + c + "," + d }
function nested() { var c = 0; for (var i = 0; i < 30; i++) for (var j = 0; j < 30; j++) for (var k = 0; k <= 3; k++) c++; return c }
var out = [];
for (var round = 0; round < 3; round++) {
	out.push([up(0), up(1), up(2), up(10), upLe(-1), upLe(0), upLe(3), upImm(), upK(0), upK(1), upK(200), downK(0), downK(1), downK(200),
		whileDec(0), whileDec(1), whileDec(5), doDec(0), doDec(1), doDec(5), upval(-1), upval(0), upval(3), upvalChange(), cont(0), cont(7),
		big53(), i32(), frac(), negz(), nanC(), inf(), bndType(), strUp(), strAdd(), strSub(), strDec(), midType(), bigC(), bigMix(), bigAdd(),
		vo(), voAdd(), voSub(), voThrow(), symThrow(), caps(), capVar(), many(), nested()].join(" "));
}
out[0] === out[1] && out[1] === out[2] ? out[0] : out.join(" | ")
`

const regStepWant = `0/0 0/1 1/2 45/10 0@0 1@1 4@4 5@5 0@0 1@63 4@252 0@0 1@-63 4@-56 0@-1 1@-1 5@-1 1@-1 1@0 5@0 0@0 1@1 4@4 6@6 0 4 11@9007199254740992 5@2147483650,4@-2147483650 3@3.5,2@2.25 1@0,1@1,1@-1 0@NaN,0,NaN 7@6,3@Infinity 6@6 5@5 0,02,022@"0222" "9",7,5,3,1 2,1,0@-1 0,"2",4n,5n@6n 11@3n0n-1n 5@3n0n TypeError@0n 2/2@3 [object Object],11,21,31/2 [object Object],13,6/2 RangeErrorv2@object2 TypeError1 0,1,2,0,21,3 3,3,3 44999850000,300000,100000 3600`

// TestRegStepAndBranch runs regStepSrc as register code, by the Go table
// and natively: Node's answers, and every kind of step-and-branch in the
// code.
func TestRegStepAndBranch(t *testing.T) {
	fn := compileForTest(t, regStepSrc)
	kinds := map[bytecode.ROp]int{}
	var walk func(f *bytecode.Function)
	walk = func(f *bytecode.Function) {
		if rc := f.RegCode(); rc != nil {
			for _, w := range rc.Code {
				if _, ok := w.Op().StepPartner(); ok {
					kinds[w.Op()]++
				}
			}
		}
		for _, c := range f.Constants {
			if c.Kind == bytecode.ConstFunction && c.Fn != nil {
				walk(c.Fn)
			}
		}
	}
	walk(fn)
	for _, op := range []bytecode.ROp{bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI, bytecode.RStepJLeU} {
		if kinds[op] == 0 {
			t.Errorf("the code has no %s", op)
		}
	}
	regRunBoth(t, regStepSrc, regStepWant, 30)
	for _, native := range []bool{false, true} {
		if got := regTableRunSrc(t, regStepSrc, native); got != regStepWant {
			t.Errorf("native %v:\n%s\nwant\n%s", native, got, regStepWant)
		}
	}
}

// TestRegStepThrowPosition pins where a step-and-branch's throws are
// placed: a valueOf that throws in the step is the step's, i++, and a
// comparison's throw is the partner's, the test i < n, each at its line
// and column.
func TestRegStepThrowPosition(t *testing.T) {
	const src = `
function inStep() {
  var n = 0, o = { valueOf() { if (++n == 2) throw new Error("step"); return 0 } }, i = o;
  try { for (; i < 3;
      i++) {} } catch (e) { return e.stack.split("\n")[2].trim() }
}
function inTest() {
  var n = 0, b = { valueOf() { if (++n == 2) throw new Error("test"); return 5 } }, i;
  try { for (i = 0;
      i < b; i++) {} } catch (e) { return e.stack.split("\n")[2].trim() + "@" + i }
}
inStep() + " " + inTest()`
	const want = "at inStep (test:5:7) at inTest (test:10:7)@1"
	regRunBoth(t, src, want, 2)
	for _, native := range []bool{false, true} {
		if got := regTableRunSrc(t, src, native); got != want {
			t.Errorf("native %v: %s, want %s", native, got, want)
		}
	}
}

// TestRegStepInterrupt stops a loop of each kind of step-and-branch whose
// back edges spend the interrupt budget, by the runtime's context, by the
// Go table and natively; the runtime then runs a loop of each to its end.
func TestRegStepInterrupt(t *testing.T) {
	forever := []string{
		`(function () { for (var i = 0; i < Infinity; i++) {} })()`,
		`(function (n) { for (var i = 0; i <= n; i++) {} })(Infinity)`,
		`(function () { for (var i = -1e300; i < 5; i++) {} })()`,
		`(function () { for (var i = 1e300; i > 0; i -= 2) {} })()`,
		`(function () { var j = Infinity; while (--j >= 0) {} })()`,
		`(function (a) { return function () { for (var u = 0; u <= a; u++) {} } })(Infinity)()`,
	}
	const finite = `(function () { var c = 0; for (var i = 0; i < 200000; i++) c++; var j = 200000; while (--j >= 0) c++;
		var a = 199999; (function () { for (var u = 0; u <= a; u++) c++ })(); return String(c) })()`
	for _, native := range []bool{false, true} {
		var fns []*bytecode.Function
		for _, src := range forever {
			fns = append(fns, compileForTest(t, src))
		}
		fin := compileForTest(t, finite)
		was := SetRegNative(native)
		r := New(Config{})
		for i, fn := range fns {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			r.SetContext(ctx)
			unwatch := watchStopForTest(r, ctx)
			_, err := r.Run(fn)
			unwatch()
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("native %v: loop %d stopped with %v", native, i, err)
			}
			r.SetContext(context.Background())
			v, err := r.Run(fin)
			if err != nil {
				t.Errorf("native %v: after loop %d: %v", native, i, err)
			} else if got := v.String().Go(); got != "600000" {
				t.Errorf("native %v: after loop %d: %s, want 600000", native, i, got)
			}
		}
		r.Close()
		SetRegNative(was)
	}
}
