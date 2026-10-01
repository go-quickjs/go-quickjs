package quickjs_test

import "testing"

// TestBranchConditions covers conditions compiled as branches rather than
// values: !x branching the other way, && and || jumping straight to where
// the whole condition goes, comparisons fused with their jumps in both
// directions -- NaN, which makes an ordering's negation another ordering
// no longer, included -- constant conditions with no test, and
// `if (test) break;` and `continue` as the condition's own jumps, and the
// ones that cannot be: through a finally, out of a try or a for-of. The
// order operands are evaluated in, and each answer, are Node's.
func TestBranchConditions(t *testing.T) {
	tests := []struct{ src, want string }{
		{`var log = []; function f(v) { log.push(v); return v }
			if (f(0) && f(1)) log.push("then"); if (f(1) && f(0)) log.push("then"); if (f(1) && f(2)) log.push("then");
			if (f(0) || f(0)) log.push("or"); if (f(0) || f(3)) log.push("or"); if (!f(0) || f(9)) log.push("not"); log.join()`,
			"0,1,0,1,2,then,0,0,0,3,or,0,not"},
		{`var log = []; function f(v) { log.push(v); return v }
			if (!(f(1) && f(0)) && !(f(0) || f(0))) log.push("x"); if (f(1) && !f(2) || f(3) && !(!f(4))) log.push("y"); log.join()`,
			"1,0,0,0,x,1,2,3,4,y"},
		{`var r = []; for (const [a, b] of [[1, 2], [2, 1], [NaN, 1], [1, NaN], [1, 1]]) {
			r.push([a < b, !(a < b), a >= b, !(a >= b), a <= b && !(a > b), !(a == b), !(a !== b)].map(Number).join(""));
			if (!(a < b)) r.push("!lt"); if (!(a <= b) || a != a) r.push("!le") } r.join()`,
			"1001110,0110010,!lt,!le,0101010,!lt,!le,0101010,!lt,!le,0110101,!lt"},
		{`var i = 0, r = []; while (true) { if (i++ > 4) break; if (i % 2 == 0) continue; r.push(i) } r.join() + " " + i`,
			"1,3,5 6"},
		{`var r = []; for (var i = 0; i < 10; i++) { if (!(i < 8)) break; if (i === 2 || i === 5) continue; r.push(i) } r.join()`,
			"0,1,3,4,6,7"},
		{`var x = 0; do { x++ } while (x != 5 && x < 9); var y = 0; do { y++; if (y < 3) continue; } while (false); x + " " + y`,
			"5 1"},
		{`var n = NaN, c = 0; do { c++ } while (n < 1); while (!(n >= 1)) { if (++c > 3) break } c`,
			"4"},
		{`var r = []; outer: for (var i = 0; i < 4; i++) { for (var j = 0; j < 4; j++) { if (j > i) continue outer; if (i + j > 4) break outer; r.push("" + i + j) } } r.join()`,
			"00,10,11,20,21,22,30,31"},
		{`var r = []; lbl: { if (r.length === 0) break lbl; r.push("no") } r.push("yes"); r.join()`,
			"yes"},
		{`var r = []; for (var i = 0; i < 3; i++) { try { if (i == 1) break; r.push("t" + i) } finally { r.push("f" + i) } } r.join()`,
			"t0,f0,f1"},
		{`var r = []; for (var i = 0; i < 3; i++) { try { if (i == 1) continue; r.push("t" + i) } catch (e) {} } try { throw 1 } catch (e) { r.push("caught") } r.join()`,
			"t0,t2,caught"},
		{`var closed = 0, it = { [Symbol.iterator]() { var k = 0; return { next() { return { value: k++, done: k > 9 } }, return() { closed++; return {} } } } };
			var r = []; for (var v of it) { if (v > 2) break; r.push(v) } r.join() + " closed " + closed`,
			"0,1,2 closed 1"},
		{`var fs = []; for (let i = 0; i < 5; i++) { if (i == 2) continue; if (!(i < 4)) break; fs.push(() => i) } fs.map(f => f()).join()`,
			"0,1,3"},
		{`var r = []; while (false) r.push("never"); while (0) r.push("never"); for (; false;) r.push("never"); if (false) r.push("no"); else r.push("else");
			if (1) r.push("one"); if (!0) r.push("not0"); if (!NaN) r.push("notNaN"); do r.push("once"); while (0); r.join()`,
			"else,one,not0,notNaN,once"},
		{`var o = { valueOf() { r.push("v"); return 1 } }, r = []; if (!(o < 2)) r.push("x"); while (o > 2 || o == 5) r.push("y"); r.join()`,
			"v,v,v"},
		{`var x = 3; (x > 2 && !(x > 5) ? "mid" : "out") + " " + (!x ? 1 : 2) + " " + (x || 0 ? "t" : "f")`,
			"mid 2 t"},
		{`var r = []; switch (1) { case 1: if (r.length == 0) break; r.push("no") } r.push("after"); r.join()`,
			"after"},
		{`function g(a) { for (var i = 0; ; i++) { if (a[i] === undefined) break } return i } g([1, 2, 3])`,
			"3"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

// TestLetLoopBindings covers loops whose head declares let or const, which
// get a fresh binding each iteration only where a closure could tell them
// apart: one made by eval, in the test or the update clause, a function
// or class declared in the body, an accessor. Without one, the binding is
// the same slot throughout, and a for-of's is initialized without first
// being marked uninitialized; the dead zone is still enforced, in the head
// and in the body, and a const still refuses assignment. Each answer is
// Node's.
func TestLetLoopBindings(t *testing.T) {
	tests := []struct{ src, want string }{
		{`var s = 0; for (let i = 0; i < 5; i++) s += i; var t = 0; for (const x of [1, 2, 3]) t += x; s + " " + t`,
			"10 6"},
		{`var fs = []; for (let i = 0; i < 3; i++) fs.push(eval("() => i")); fs.map(f => f()).join()`,
			"0,1,2"},
		{`var fs = []; for (let i = 0; fs.push(() => i), i < 2; i++) {} fs.map(f => f()).join()`,
			"0,1,2"},
		{`var fs = []; for (let i = 0; i < 3; fs.push(() => i), i++) {} fs.map(f => f()).join()`,
			"1,2,3"},
		{`var fs = []; for (let i = 0; i < 3; i++) { function g() { return i } fs.push(g) } fs.map(f => f()).join()`,
			"0,1,2"},
		{`var fs = []; for (let i = 0; i < 3; i++) { class K { m() { return i } } fs.push(new K) } fs.map(k => k.m()).join()`,
			"0,1,2"},
		{`var fs = []; for (let i = 0; i < 3; i++) { var o = { get v() { return i } }; fs.push(o) } fs.map(o => o.v).join()`,
			"0,1,2"},
		{`try { for (let i = i; i < 1; i++) {} } catch (e) { e.constructor.name }`,
			"ReferenceError"},
		{`try { for (let x of [x]) {} } catch (e) { e.constructor.name }`,
			"ReferenceError"},
		{`var r = []; for (const x of [1, 2]) { try { y } catch (e) { r.push(e.constructor.name) } let y = x; r.push(y) } r.join()`,
			"ReferenceError,1,ReferenceError,2"},
		{`var r = []; for (let i = 0; i < 3; i++) { try { j } catch (e) { r.push(e.constructor.name) } let j = i * 2; r.push(j) } r.join()`,
			"ReferenceError,0,ReferenceError,2,ReferenceError,4"},
		{`var r = []; for (let [a, b] of [[1, 2], [3, 4]]) r.push(a + b); for (const { k } of [{ k: 5 }]) r.push(k); r.join()`,
			"3,7,5"},
		{`function* g() { for (let i = 0; i < 3; i++) yield i; for (const x of "ab") yield x } [...g()].join()`,
			"0,1,2,a,b"},
		{`var r = []; for (let i = 0; i < 3; i++) { i++; r.push(i) } r.join()`,
			"1,3"},
		{`var r = []; outer: for (let i = 0; i < 3; i++) for (let j = 0; j < 3; j++) { if (j == 1) continue outer; r.push(i + "" + j) } r.join()`,
			"00,10,20"},
		{`"use strict"; var r = []; for (const x of [1, 2]) { try { x = 3 } catch (e) { r.push(e.constructor.name) } } r.join()`,
			"TypeError,TypeError"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

// TestSwitchDeadZone covers the let, const and class bindings of a switch's
// case block, which a case can be entered past the declaration of: reading
// one there is a ReferenceError, wherever the read is written, and not the
// value of the dead zone's mark. Each answer is Node's.
func TestSwitchDeadZone(t *testing.T) {
	tests := []struct{ src, want string }{
		{`var r; switch (1) { case 0: let y = 1; break; case 1: try { y } catch (e) { r = e.constructor.name } } r`, "ReferenceError"},
		{`try { (() => { switch (2) { case 1: let a = 5; case 2: return typeof a } })() } catch (e) { e.constructor.name }`, "ReferenceError"},
		{`try { (() => { switch (2) { case 1: let a = 5; case 2: return (() => a)() } })() } catch (e) { e.constructor.name }`, "ReferenceError"},
		{`try { (() => { switch (2) { case 1: let a = 5; case 2: a = 3; return a } })() } catch (e) { e.constructor.name }`, "ReferenceError"},
		{`var r = []; for (var i = 0; i < 3; i++) switch (i) { case 0: let v = "zero"; r.push(v); break; default: try { r.push(v) } catch (e) { r.push(e.constructor.name) } } r.join()`,
			"zero,ReferenceError,ReferenceError"},
		{`try { (() => { switch (1) { case 0: class K {} case 1: return typeof K } })() } catch (e) { e.constructor.name }`, "ReferenceError"},
		{`(() => { switch (1) { case 1: const c = 1; try { c = 2 } catch (e) { return e.constructor.name + c } } })()`, "TypeError1"},
		{`(() => { switch (1) { case 1: let a = 5; a++; return a + (() => a)() } })()`, "12"},
		{`(() => { switch (0) { case 0: class K {} return typeof K } })()`, "function"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}

// TestDeadZoneMarks covers let and const bindings whose slots are not marked
// as in their dead zone, where nothing could read them there, and the ones
// that must be: a function declared in the block, a class, an eval, a
// mention of the name before the declaration or in its own initializer, a
// pattern's default, a nested block, and a switch, whose cases can be
// jumped to past a declaration. Without a mark, a binding declared with no
// initializer is still undefined each time it is reached. Each answer is
// Node's.
func TestDeadZoneMarks(t *testing.T) {
	tests := []struct{ src, want string }{
		{`var r; { try { f() } catch (e) { r = e.constructor.name } function f() { return y } let y = 1 } r`,
			"ReferenceError"},
		{`try { { let x = x } } catch (e) { e.constructor.name }`,
			"ReferenceError"},
		{`{ let a = 1, b = a + 1; b }`,
			"2"},
		{`var r; switch (1) { case 0: let y = 1; break; case 1: try { y } catch (e) { r = e.constructor.name } } r`,
			"ReferenceError"},
		{`try { { let { a = a } = {} } } catch (e) { e.constructor.name }`,
			"ReferenceError"},
		{`var r = []; for (var i = 0; i < 3; i++) { try { if (i) typeof v; r.push("ok") } catch (e) { r.push(e.constructor.name) } const v = i } r.join()`,
			"ok,ReferenceError,ReferenceError"},
		{`var r = []; for (var i = 0; i < 2; i++) { try { eval("v"); r.push("read") } catch (e) { r.push(e.constructor.name) } let v = i } r.join()`,
			"ReferenceError,ReferenceError"},
		{`var r; { { try { z } catch (e) { r = e.constructor.name } } let z = 1 } r`,
			"ReferenceError"},
		{`var r; { try { new K } catch (e) { r = e.constructor.name } class K { m() { return w } } let w = 1 } r`,
			"ReferenceError"},
		{`function* g() { for (let i = 0; i < 3; i++) { yield i; const v = i * 10; yield v } } [...g()].join()`,
			"0,0,1,10,2,20"},
		{`function f() { try { q } catch (e) { return e.constructor.name } let q = 1; return q } f() + f()`,
			"ReferenceErrorReferenceError"},
		{`var s = 0; for (var i = 0; i < 5; i++) { const sq = i * i; let t = sq + 1; s += t } s`,
			"35"},
		{`var r = []; for (var i = 0; i < 3; i++) { lbl: { if (i == 1) break lbl; let u = i; r.push(u) } } r.join()`,
			"0,2"},
		{`var r = []; for (const x of [1, 2, 3]) { const y = x * 2; let z; r.push(y, z) } r.join()`,
			"2,,4,,6,"},
		{`"use strict"; var r = []; for (var i = 0; i < 2; i++) { const c = i; try { c = 5 } catch (e) { r.push(e.constructor.name, c) } } r.join()`,
			"TypeError,0,TypeError,1"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}
