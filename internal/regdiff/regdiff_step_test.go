package regdiff

import "testing"

// TestRegDiffStepAndBranch compares the step-and-branches (step_jlt and
// its kin) with the old tiers: each of the six kinds and each kind of
// step on their fast paths and off them -- counters past 2^53 and across
// the int32 range, -0, NaN, strings, BigInts, objects whose valueOf
// counts its calls or throws, a Symbol, a bound that changes type, an
// upvalue bound -- loops entered and left at their boundaries, a continue
// that lands on the step, captured let counters, and loops past the
// interrupt budget's back edges, each call made twice.
func TestRegDiffStepAndBranch(t *testing.T) {
	regDiffParallel(t)
	src := `function show(x) { return Object.is(x, -0) ? "-0" : typeof x === "bigint" ? x + "n" : typeof x === "string" ? JSON.stringify(x) : String(x) }
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
`
	calls := []string{
		"up(0)",
		"up(1)",
		"up(2)",
		"up(10)",
		"upLe(-1)",
		"upLe(0)",
		"upLe(3)",
		"upImm()",
		"upK(0)",
		"upK(1)",
		"upK(200)",
		"downK(0)",
		"downK(1)",
		"downK(200)",
		"whileDec(0)",
		"whileDec(1)",
		"whileDec(5)",
		"doDec(0)",
		"doDec(1)",
		"doDec(5)",
		"upval(-1)",
		"upval(0)",
		"upval(3)",
		"upvalChange()",
		"cont(0)",
		"cont(7)",
		"big53()",
		"i32()",
		"frac()",
		"negz()",
		"nanC()",
		"inf()",
		"bndType()",
		"strUp()",
		"strAdd()",
		"strSub()",
		"strDec()",
		"midType()",
		"bigC()",
		"bigMix()",
		"bigAdd()",
		"vo()",
		"voAdd()",
		"voSub()",
		"voThrow()",
		"symThrow()",
		"caps()",
		"capVar()",
		"many()",
		"nested()",
	}
	calls = append(calls, calls...)
	if built := regDiff(t, src, calls); built < 40 {
		t.Errorf("only %d functions ran as register code", built)
	}
}
