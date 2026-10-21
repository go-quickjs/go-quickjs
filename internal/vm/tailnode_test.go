package vm

import "testing"

// tailCallScript makes calls in tail position in each of the forms a
// return has them -- return f(x), a conditional's, an &&'s, an ||'s, a
// ??'s, a comma's, nested conditionals', (0, f)(x), a method's of this,
// of a local, by a computed key and of another object, a static
// method's, a private method's, a super method's, an arrow's -- of what
// a call may be made of -- a bound function, a built-in, a proxy, a
// getter's function, a class, something not callable, a method whose
// lookup throws -- with arguments a sloppy callee's arguments object, a
// strict one's, a rest parameter and defaults keep, fewer and more than
// the parameters, an arguments object made after the callee's own calls
// made calls in tail position, to a callee rebound along the way, and to
// an error at the bottom, with the frames of its stack. Three rounds, the
// last 40 deep, past tailCallDepth, so that its frames are given up;
// Node, which gives up none, runs them all.
const tailCallScript = `// A sloppy function, which keeps an arguments object of its own.
var sloppyMaker = function (kept) {
	return function s(n, a) { kept.push(arguments); return arguments.length + ":" + Array.prototype.join.call(arguments, ","); };
};
(function () {
	"use strict";
	var out = [];
	function line(name, v) { out.push(name + ": " + v); }
	function attempt(name, f) {
		try { line(name, String(f())); } catch (e) { line(name, "threw " + (e && e.constructor && e.constructor.name)); }
	}
	function names(stack) {
		return stack.split("\n").slice(1).map(function (l) { var m = /at (?:new )?([\w.<>]+)/.exec(l); return m ? m[1] : "?"; }).join(">");
	}
	// Each form runs several rounds, so that its functions are run as
	// trees, deep enough in the last for frames to be given up past the
	// tier's depth, and shallow, as Node, which makes no tail call, runs
	// them all.
	for (var round = 0; round < 3; round++) {
		var depth = round === 2 ? 40 : 3;
		function plain(n, acc) { if (n === 0) return acc; return plain(n - 1, acc + n); }
		function tern(n, acc) { return n === 0 ? acc : tern(n - 1, acc + 1); }
		function even(n) { return n === 0 ? true : odd(n - 1); }
		function odd(n) { return n === 0 ? false : even(n - 1); }
		function and(n) { return n === 0 || and2(n - 1); }
		function and2(n) { return n !== 0 && and(n - 1); }
		function nul(n) { return n === 0 ? "nul" : null ?? nul(n - 1); }
		function comma(n) { var k = 0; return n === 0 ? k : (k = 1, comma(n - 1)); }
		function nested(n, k) { return n === 0 ? k : k % 3 === 0 ? nested(n - 1, k + 1) : k % 3 === 1 ? nested(n - 1, k + 2) : nested(n - 1, k + 3); }
		function paren(n, acc) { if (n === 0) return acc; return (0, paren)(n - 1, acc + "p"); }
		var obj = {
			tag: "obj",
			m(n) { return n === 0 ? this.tag : this.m(n - 1); },
			viaLocal(n) { var self = this; return n === 0 ? self.tag + "!" : self.viaLocal(n - 1); },
			viaIndex(n) { var k = "viaIndex"; return n === 0 ? this.tag + "?" : this[k](n - 1); },
			viaOther(n, o) { return n === 0 ? o.tag : o.viaOther(n - 1, n % 2 ? obj : other); },
		};
		var other = { tag: "other", viaOther: obj.viaOther };
		class Counter {
			constructor(v) { this.v = v; }
			down(n) { return n === 0 ? this.v : this.down(n - 1); }
			static make(n) { return n === 0 ? new Counter("static") : Counter.make(n - 1); }
			#priv(n) { return n === 0 ? "private" : this.#priv(n - 1); }
			callPriv(n) { return this.#priv(n); }
		}
		class Sup { down(n) { return n === 0 ? "sup" : this.down(n - 1); } }
		class Sub extends Sup { down(n) { return super.down(n); } }
		var arrowThis = { tag: "arrow", run(n) { var f = (k) => k === 0 ? this.tag : f(k - 1); return f(n); } };
		line("forms " + round, [plain(depth, 0), tern(depth, 0), even(depth), odd(depth), and(depth), and2(depth), nul(depth),
			comma(depth), nested(depth, 0), paren(depth, ""), obj.m(depth), obj.viaLocal(depth), obj.viaIndex(depth),
			obj.viaOther(depth, obj), new Counter(7).down(depth), Counter.make(depth).v, new Counter(0).callPriv(depth), new Sub().down(depth),
			arrowThis.run(depth)].join(" "));
		// What is called in tail position: a bound function, a built-in, a
		// proxy, a getter's function, a class, something not callable, a
		// method whose lookup throws.
		function toBound(n) { return n === 0 ? "b" : bound(n - 1); }
		var bound = function (n, extra) { return n === 0 ? "bound " + extra : toBound(n); }.bind(null);
		function toNative(n) { return n === 0 ? Math.max(n, 3, 1) : toNative(n - 1); }
		function toMax(n, a, b) { return Math.max(a, b, n); }
		var proxied = new Proxy(function (n) { return n === 0 ? "proxy" : proxied(n - 1); }, {});
		function viaProxy(n) { return proxied(n); }
		var getterObj = { get m() { return function (n) { return n === 0 ? "getter" : getterObj.m(n - 1); }; } };
		function viaGetter(n) { return getterObj.m(n); }
		class K { }
		function toClass() { return K(); }
		function toNothing() { var x = 1; return x(); }
		function toMissing(o) { return o.missing(); }
		var thrower = { get m() { throw new RangeError("lookup"); } };
		function toThrowing() { return thrower.m(); }
		line("callees " + round, [toBound(depth), toNative(depth), toMax(depth, 1, 2), viaProxy(depth), viaGetter(depth)].join(" "));
		attempt("class " + round, toClass);
		attempt("not callable " + round, toNothing);
		attempt("missing " + round, function () { return toMissing({}); });
		attempt("lookup throws " + round, toThrowing);
		// Arguments: a sloppy callee's arguments object, a strict one's, a
		// rest parameter and defaults, each kept past the calls.
		var kept = [];
		var sloppyArgs = sloppyMaker(kept);
		function toSloppy(n, a) { return n === 0 ? sloppyArgs(n, a, "x") : toSloppy(n - 1, a + 1); }
		function strictArgs(n, a) { kept.push(arguments); if (n === 0) return arguments.length; return strictArgs(n - 1, a + 1, "extra"); }
		function rest(n, ...r) { kept.push(r); if (n === 0) return r.join("/"); return rest(n - 1, ...r, n); }
		function defaults(n, a = n * 2, b = a + 1) { if (n === 0) return a + "," + b; return defaults(n - 1, undefined, b); }
		function fewer(n, a, b) { if (n === 0) return String(a) + String(b); return fewer(n - 1, a); }
		function more(n, a) { if (n === 0) return a + ":" + arguments.length; return more(n - 1, a + 1, 9, 9); }
		// An arguments object made late, after calls of the callee's own
		// have made calls in tail position of theirs.
		function spin(n) { return n === 0 ? 0 : spin(n - 1); }
		var late = [];
		function argsLate(n, x) { spin(depth); late.push(arguments[0] + "/" + arguments[1]); return n === 0 ? late.join(",") : argsLate(n - 1, x + 1); }
		var res = [toSloppy(depth, 0), strictArgs(depth, 0), rest(depth), defaults(depth), fewer(depth, "a", "b"), more(depth, 0), argsLate(depth, 0)];
		res.push(kept.slice(0, 3).map(function (a) { return a.length + ":" + Array.prototype.join.call(a, ","); }).join(" "));
		res.push(kept.slice(-3).map(function (a) { return a.length + ":" + Array.prototype.join.call(a, ","); }).join(" "));
		line("arguments " + round, res.join(" | "));
		// A callee rebound in the middle of the calls.
		var target = function (n) { return "first " + n; };
		function rebind(n) { if (n === 2) target = function (k) { return "second " + k; }; return n === 0 ? "end" : target(n - 1); }
		function chain(n) { return rebind(n); }
		target = chain;
		line("rebound " + round, chain(depth));
		// An error at the bottom: what it is, and the frames of a shallow
		// stack, which Node's are too.
		function thrower2(n) { if (n === 0) throw new TypeError("bottom " + n); return thrower2(n - 1); }
		try { thrower2(depth); } catch (e) { line("throw " + round, e.constructor.name + " " + e.message + (round < 2 ? " " + names(e.stack).split(">").slice(0, 3).join(">") : "")); }
		function stackOf(n) { return n === 0 ? names(new Error().stack).split(">").slice(0, 3).join(">") : stackOf(n - 1); }
		if (round < 2) line("stack " + round, stackOf(2));
	}
	return out.join("\n");
})()`

// tailCallWant is Node v26.10.0's answer, the script run by
// vm.runInThisContext.
const tailCallWant = `forms 0: 6 3 false true false true nul 0 4 ppp obj obj! obj? obj 7 static private sup arrow
callees 0: bound undefined 3 3 proxy getter
class 0: threw TypeError
not callable 0: threw TypeError
missing 0: threw TypeError
lookup throws 0: threw RangeError
arguments 0: 3:0,3,x | 3 | 3/2/1 | 0,7 | aundefined | 3:4 | 3/0,2/1,1/2,0/3 | 3:0,3,x 2:3,0 3:2,1,extra | 1:3 2:3,2 3:3,2,1
rebound 0: second 1
throw 0: TypeError bottom 0 thrower2>thrower2>thrower2
stack 0: stackOf>stackOf>stackOf
forms 1: 6 3 false true false true nul 0 4 ppp obj obj! obj? obj 7 static private sup arrow
callees 1: bound undefined 3 3 proxy getter
class 1: threw TypeError
not callable 1: threw TypeError
missing 1: threw TypeError
lookup throws 1: threw RangeError
arguments 1: 3:0,3,x | 3 | 3/2/1 | 0,7 | aundefined | 3:4 | 3/0,2/1,1/2,0/3 | 3:0,3,x 2:3,0 3:2,1,extra | 1:3 2:3,2 3:3,2,1
rebound 1: second 1
throw 1: TypeError bottom 0 thrower2>thrower2>thrower2
stack 1: stackOf>stackOf>stackOf
forms 2: 820 40 true false true false nul 0 60 pppppppppppppppppppppppppppppppppppppppp obj obj! obj? obj 7 static private sup arrow
callees 2: bound undefined 3 40 proxy getter
class 2: threw TypeError
not callable 2: threw TypeError
missing 2: threw TypeError
lookup throws 2: threw RangeError
arguments 2: 3:0,40,x | 3 | 40/39/38/37/36/35/34/33/32/31/30/29/28/27/26/25/24/23/22/21/20/19/18/17/16/15/14/13/12/11/10/9/8/7/6/5/4/3/2/1 | 0,81 | aundefined | 40:4 | 40/0,39/1,38/2,37/3,36/4,35/5,34/6,33/7,32/8,31/9,30/10,29/11,28/12,27/13,26/14,25/15,24/16,23/17,22/18,21/19,20/20,19/21,18/22,17/23,16/24,15/25,14/26,13/27,12/28,11/29,10/30,9/31,8/32,7/33,6/34,5/35,4/36,3/37,2/38,1/39,0/40 | 3:0,40,x 2:40,0 3:39,1,extra | 38:40,39,38,37,36,35,34,33,32,31,30,29,28,27,26,25,24,23,22,21,20,19,18,17,16,15,14,13,12,11,10,9,8,7,6,5,4,3 39:40,39,38,37,36,35,34,33,32,31,30,29,28,27,26,25,24,23,22,21,20,19,18,17,16,15,14,13,12,11,10,9,8,7,6,5,4,3,2 40:40,39,38,37,36,35,34,33,32,31,30,29,28,27,26,25,24,23,22,21,20,19,18,17,16,15,14,13,12,11,10,9,8,7,6,5,4,3,2,1
rebound 2: second 1
throw 2: TypeError bottom 0`

// Calls in tail position answer what Node's do, made by the tree tier's
// nodes (tailAll) and by the interpreter, in a runtime made twice.
func TestTailCallsAgreeWithNode(t *testing.T) {
	defer SetTreeTier(true)
	for round := range 4 {
		SetTreeTier(round < 2)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, tailCallScript))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != tailCallWant {
			diffLinesForTest(t, round, got, tailCallWant)
		}
		r.Close()
	}
}

// deepTailScript recurses 100,000 deep through calls in tail position, in
// the forms tailCallScript makes them -- a private method's and a super
// method's among them -- which a stack of 400 frames holds
// only where each gives its frame up; strict arguments objects kept along
// the way keep what their calls were given; bindings closures keep open,
// made at some of the calls, which are then given frames of their own;
// and an error at the bottom is caught at the top, its stack naming the
// function.
const deepTailScript = `(function () {
	"use strict";
	var N = 100000, r = [];
	function plain(n, acc) { if (n === 0) return acc; return plain(n - 1, (acc + n) | 0); }
	function tern(n, acc) { return n === 0 ? acc : tern(n - 1, acc + 1); }
	function even(n) { return n === 0 ? true : odd(n - 1); }
	function odd(n) { return n === 0 ? false : even(n - 1); }
	function and(n) { return n === 0 || and2(n - 1); }
	function and2(n) { return n !== 0 && and(n - 1); }
	function nested(n, k) { return n === 0 ? k : k % 3 === 0 ? nested(n - 1, k + 1) : k % 3 === 1 ? nested(n - 1, k + 2) : nested(n - 1, k + 3); }
	var obj = { tag: "obj", m(n) { return n === 0 ? this.tag : this.m(n - 1); }, viaIndex(n) { var k = "viaIndex"; return n === 0 ? this.tag + "?" : this[k](n - 1); } };
	class C { constructor(v) { this.v = v; } down(n) { return n === 0 ? this.v : this.down(n - 1); } #p(n) { return n === 0 ? "p" : this.#p(n - 1); } callP(n) { return this.#p(n); } }
	class B { down(n) { return n === 0 ? "super" : this.down(n - 1); } }
	class D extends B { down(n) { return super.down(n); } }
	var bound = function (n) { return n === 0 ? "bound" : toBound(n - 1); }.bind(null);
	function toBound(n) { return bound(n); }
	function strictArgs(n, keep) { if (n % 25000 === 0) keep.push(arguments); return n === 0 ? keep : strictArgs(n - 1, keep, n); }
	function rest(n, ...a) { return n === 0 ? a.length : rest(n - 1, n, n); }
	var arrow = (n) => n === 0 ? "arrow" : arrow(n - 1);
	// A binding a closure keeps open, made at some of the calls, which may
	// not be started again in the frame (selfRestart) and are given frames.
	function cap(n, fs, acc) { if (n === 0) return fs.length + ":" + fs[0]() + ":" + fs[fs.length - 1]() + ":" + acc; if (n % 1000 === 0) fs.push(() => n + acc); return cap(n - 1, fs, acc + 1); }
	var capper = { k: 0, m(n, x) { if (n === 0) return this.k + ":" + x; if (n % 7 === 0) { var self = this; this.k = (() => self.k + 1)(); } return this.m(n - 1, x + 2); } };
	function thrower(n) { if (n === 0) throw new RangeError("deep"); return thrower(n - 1); }
	r.push(plain(N, 0), tern(N, 0), even(N + 1), odd(N), and(N), and2(N + 1), nested(N, 0), obj.m(N), obj.viaIndex(N), new C("c").down(N), new C(0).callP(N), new D().down(N), toBound(N), rest(N), arrow(N), cap(N, [], 0), capper.m(N, 0));
	r.push(strictArgs(N, []).map(function (a) { return a.length + ":" + a[0] + ":" + a[2]; }).join(","));
	try { thrower(N); } catch (e) { r.push(e.constructor.name + " " + e.message + " " + (e.stack.split("\n")[1].indexOf("thrower") >= 0)); }
	return r.join(" ");
})()`

// Recursions through calls in tail position far deeper than the call
// stack run in constant space, as strict code's tail calls must, on the
// tree tier and in the interpreter alike.
func TestDeepTailCalls(t *testing.T) {
	const want = "705082704 100000 false false true true 150000 obj obj? c p super bound 2 arrow 100:100000:100000:100000 14285:200000 2:100000:undefined,3:75000:75001,3:50000:50001,3:25000:25001,3:0:1 RangeError deep true"
	defer SetTreeTier(true)
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{MaxCallDepth: 400})
		v, err := r.Run(compileForTest(t, deepTailScript))
		if err != nil {
			t.Fatalf("tree tier %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got %s, want %s", tree, got, want)
		}
		r.Close()
	}
}

// A call in tail position is one node, the value its return returns, its
// operands evaluated by it, as a call's are (tailAll): its function's tree
// has no statement -- no spill of the callee, the receiver or the
// arguments to their slots -- and a conditional with such a call in it is
// one node too, in the one block the code has besides return_undef's.
func TestTailCallsBuiltAsNodes(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	_, err := r.Run(compileForTest(t, `
		"use strict"
		function tern(n, acc) { return n === 0 ? acc : tern(n - 1, acc + 1) }
		function even(n) { return n === 0 || odd(n - 1) }
		function odd(n) { return n !== 0 && even(n - 1) }
		function plain(n) { return n === 0 ? 0 : plain(n - 1) }
		const o = { m(n) { return n === 0 ? 0 : this.m(n - 1) } }
		function method(p, n) { return p.m(n) }
		for (let i = 0; i < 3; i++) { tern(50, 0); even(50); plain(50); method(o, 50) }`))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tern", "even", "odd", "plain", "method"} {
		fn := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
		tr := (*tree)(fn.VMCode)
		if tr == nil || tr == noTree {
			t.Fatalf("%s: no tree", name)
		}
		stmts := 0
		for _, b := range tr.blocks {
			stmts += len(b.body)
		}
		if len(tr.blocks) != 2 || stmts != 0 {
			t.Errorf("%s: %d blocks, %d statements; want 2 and 0", name, len(tr.blocks), stmts)
		}
	}
}

// A parameter after a default, or after a pattern, is read from the
// call's arguments once what comes before it is evaluated -- which may
// make calls in tail position of its own. A call in tail position past
// tailCallDepth to such a function gives it arguments of their own, not
// tailArgs, which those calls copy theirs into: f's z had been "s3",
// spin's. By a plain call (tailAll) and by a private method's
// (tailCallNode), tree tier on and off: Node v26.10.0's answers.
func TestTailCallArgumentsAfterDefaults(t *testing.T) {
	const src = `(function () {
	"use strict";
	function spin(n, a, b, c) { return n === 0 ? 0 : spin(n - 1, "s1", "s2", "s3"); }
	function f(n, x, y = spin(60, 1, 2, 3), z) { return n === 0 ? x + ":" + z : f(n - 1, x, undefined, "z" + n); }
	function pattern(n, { a = spin(60, 1, 2, 3) } = {}, z) { return n === 0 ? a + ":" + z : pattern(n - 1, {}, "p" + n); }
	class P {
		#f(n, x, y = spin(60, 1, 2, 3), z) { return n === 0 ? x + ":" + z : this.#f(n - 1, x, undefined, "w" + n); }
		run(k) { return k === 0 ? this.#f(5, "x", undefined, "w") : this.run(k - 1); }
	}
	function deep(k, g) { return k === 0 ? g() : deep(k - 1, g); }
	var r = [];
	for (var i = 0; i < 3; i++) {
		r.push(deep(50, function () { return f(5, "x", undefined, "z"); }));
		r.push(deep(50, function () { return pattern(5, {}, "p"); }));
		r.push(new P().run(50));
	}
	return r.join(" ");
})()`
	const want = "x:z1 0:p1 x:w1 x:z1 0:p1 x:w1 x:z1 0:p1 x:w1"
	defer SetTreeTier(true)
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got %s, want %s", tree, got, want)
		}
		r.Close()
	}
}
