package vm

import (
	"fmt"
	"strings"
	"testing"
)

// inOperatorSetup asks the in operator of every key of a set -- indices in
// range, past it, negative, -0, fractions, NaN, about 2**31 and 2**32, as
// numbers and as strings, canonical and not; names own, inherited, absent,
// hidden, an accessor's, a dictionary's, one no atom names; symbols; true,
// null and undefined -- of every object of a set: an array with holes, a
// sparse one, one of length 2**32-1, a subclass's, an object with no
// prototype, one with elements, its child, a deep chain, a String object,
// a typed array, a function, arguments objects mapped and not, a proxy
// whose trap logs, a dictionary, the global object. Then a key's
// conversion and a primitive operand; a key that names no atom and then
// one; the same sites while what an array's prototypes hold changes -- an
// index by a value and by a getter, on Array.prototype and on
// Object.prototype, the array made sparse, given a prototype of its own --
// and a loop of an array's indices; keys of many kinds at one site.
const inOperatorSetup = `var log = [];
var sym = Symbol("s"), unseen = Symbol("u");
var arr = [1, , 3, undefined, , 6];
var sparse = [1, 2]; sparse[100000] = 3; sparse[3000000000] = 4;
var big = []; big[4294967294] = 1;
class Sub extends Array {}
var sub = Sub.from([1, , 3]);
var bare = Object.create(null); bare[2] = "b"; bare.k = 1;
var plain = { 0: "a", 1: "b", a: 1, [sym]: 2 };
Object.defineProperty(plain, "hidden", { value: 1, enumerable: false });
Object.defineProperty(plain, "acc", { get() { log.push("getter"); return 1; } });
var child = Object.create(plain); child.own = 1;
var proto = { inherited: 1, 5: "five" };
var deep = Object.create(Object.create(Object.create(proto)));
var str = new String("ab");
var ta = new Int8Array(3);
var fn = function named(a, b) {};
var argsMapped = (function (a, b) { return arguments; })(1);
var argsStrict = (function (a, b) { "use strict"; return arguments; })(1, 2);
var trap = new Proxy({ t: 1 }, { has(t, k) { log.push("has:" + String(k)); return k in t; } });
var dict = {}; for (var i = 0; i < 40; i++) dict["p" + i] = i; for (var i = 0; i < 40; i += 2) delete dict["p" + i];
// An array with a hole at i, made by a delete, which a literal's hole is too.
function holey(a, i) { delete a[i]; return a; }
var keyObj = { toString() { log.push("toString"); return "a"; } };
var keys = [0, 1, 2, 3, 4, 5, 6, -1, -0, 0.5, NaN, Infinity, 2147483647, 2147483648, 4294967294, 4294967295, 100000, 3000000000,
	"0", "1", "01", "-0", "1.0", "length", "push", "toString", "constructor", "a", "k", "own", "inherited", "five", "5",
	"hidden", "acc", "t", "p1", "p2", "p39", "x" + "y" + "zzy", "name", "prototype", "caller", sym, unseen, Symbol.iterator, true, null, undefined];
var objects = [arr, sparse, big, sub, bare, plain, child, deep, str, ta, fn, argsMapped, argsStrict, trap, dict, [], {}, globalThis];
var names = ["arr", "sparse", "big", "sub", "bare", "plain", "child", "deep", "str", "ta", "fn", "argsMapped", "argsStrict", "trap", "dict", "[]", "{}", "global"];
// Every key in every object, each answer a bit of a row, in a loop: the
// tree tier's and native code's.
function table() {
	var out = "";
	log.length = 0;
	for (var o = 0; o < objects.length; o++) {
		var row = "";
		for (var k = 0; k < keys.length; k++) row += keys[k] in objects[o] ? "1" : "0";
		out += names[o] + ":" + row + ";";
	}
	return out + log.join(",");
}
// A key converted after the operand is known to be an object, whose
// conversion is called once; a primitive operand, a TypeError before the
// key is converted.
function conversions() {
	var out = "";
	for (var i = 0; i < 2; i++) {
		log.length = 0;
		out += (keyObj in plain) + "," + log.join() + ";";
		log.length = 0;
		try { keyObj in 5; } catch (e) { out += e.constructor.name + "," + log.length + ";"; }
		try { "a" in "abc"; } catch (e) { out += e.constructor.name + ";"; }
		try { 0 in undefined; } catch (e) { out += e.constructor.name + ";"; }
	}
	return out;
}
// A key no atom names, then one, by another string of the same text:
// the answer follows.
function atoms() {
	var o = {}, k = "zq" + "wv", s = 0, out = "";
	for (var i = 0; i < 20; i++) {
		if (i == 10) o[("zqw" + "v")] = 1;
		if (k in o) s += i;
		if (("never" + "seen" + "key") in o) s += 1000;
	}
	return s;
}
/// A key no atom names, asked of an object, which finds it names none,
// and at once of a proxy, whose trap is called with it, and of an object
// whose prototype is the proxy; then of the object again.
function fresh() {
	var out = "";
	for (var i = 0; i < 3; i++) {
		log.length = 0;
		var k = "fresh" + i + "key";
		out += (k in plain) + "," + (k in trap) + "," + (k in Object.create(trap)) + "," + (k in plain) + "," + log.join() + ";";
	}
	return out;
}
// The same sites while what the objects and their prototypes hold
// changes: an index the array's prototype gains, by a value and by a
// getter, Object.prototype's, the array made sparse, a prototype of its
// own, an element deleted and one added; a property deleted and added.
function changing(n) {
	var out = "", a = holey([1, 0, 3], 1), o = { a: 1 };
	log.length = 0;
	for (var i = 0; i < n; i++) {
		if (i == 4) Array.prototype[1] = "p";
		if (i == 6) { delete Array.prototype[1]; Object.prototype[4] = "o"; }
		if (i == 8) { delete Object.prototype[4]; Object.defineProperty(Array.prototype, 5, { get() { log.push("get5"); return 5; }, configurable: true }); }
		if (i == 10) delete Array.prototype[5];
		if (i == 12) a[2000000] = 1;
		if (i == 14) { a = holey([1, 0, 3], 1); Object.setPrototypeOf(a, Object.create(Array.prototype, { 1: { value: 1 } })); }
		if (i == 16) { a = holey([1, 0, 3], 1); delete a[0]; a[1] = 2; }
		if (i == 18) { delete o.a; o.b = 1; }
		if (i == 20) { o.a = 2; Object.prototype.b = 3; }
		if (i == 22) delete Object.prototype.b;
		out += (0 in a ? 1 : 0) + "" + (1 in a ? 1 : 0) + (4 in a ? 1 : 0) + (5 in a ? 1 : 0) + (9 in a ? 1 : 0) +
			("a" in o ? 1 : 0) + ("b" in o ? 1 : 0) + ("length" in a ? 1 : 0) + ";";
	}
	return out + log.join(",");
}
// A loop that asks of an array's indices, a hole's and two past its end.
function holes(n) {
	var a = holey([1, 2, 3, 4, 5, 6, 7, 8], 3), s = 0;
	for (var j = 0; j < n; j++) {
		if ((j & 7) in a) s++;
		if (12 in a) s += 100;
		if (8 in a) s += 1000;
	}
	return s;
}
/// Indices past what an atom of an index carries, and the last it does, of
// an array that has none of them.
function far(n) {
	var a = [1, 2], s = 0;
	for (var i = 0; i < n; i++) {
		if (3000000000 in a) s++;
		if (2147483648 in a) s += 10;
		if (2147483647 in a) s += 100;
		if (1 in a) s += 1000;
	}
	return s;
}
/// A sparse array's indices, which its table has, and one it has not.
function sparseLoop(n) {
	var a = [1, 2], s = 0;
	a[100000] = 3; a[5000] = 4;
	for (var i = 0; i < n; i++) {
		if (100000 in a) s++;
		if (5000 in a) s += 10;
		if (7 in a) s += 100;
	}
	return s;
}
// Keys of many kinds at one site.
function mixed(n) {
	var ks = [0, "a", 1, sym, "length", 3, "zz", 2.5, -0, -1, 2], o = holey([9, 0, 8], 1), s = "";
	o.a = 1; o[sym] = 2;
	for (var i = 0; i < n; i++) s += ks[i % ks.length] in o ? "1" : "0";
	return s;
}
`

// inOperatorCases are inOperatorSetup's answers, Node v26.10.0's.
var inOperatorCases = []struct{ src, want string }{
	{"table()", "arr:1011010010000000001000011110000010000000000001000;sparse:1100000010000000111100011110000000000000000001000;big:0000000000000010000000011110000000000000000001000;sub:1110000010000000001100011110000000000000000001000;bare:0010000000000000000000000000100000000000000000000;plain:1100000010000000001100000111000001100000000100000;child:1100000010000000001100000111010001100000000100000;deep:0000010000000000000000000110001010000000000000000;str:1100000010000000001100010110000000000000000001000;ta:1110000010000000001100010110000000000000000001000;fn:0000000000000000000000010110000000000000111000000;argsMapped:1000000010000000001000010110000000000000000001000;argsStrict:1100000010000000001100010110000000000000000001000;trap:0000000000000000000000000110000000010000000000000;dict:0000000000000000000000000110000000001010000000000;[]:0000000000000000000000011110000000000000000001000;{}:0000000000000000000000000110000000000000000000000;global:0000000000110000000000000110000000000000000000001;has:0,has:1,has:2,has:3,has:4,has:5,has:6,has:-1,has:0,has:0.5,has:NaN,has:Infinity,has:2147483647,has:2147483648,has:4294967294,has:4294967295,has:100000,has:3000000000,has:0,has:1,has:01,has:-0,has:1.0,has:length,has:push,has:toString,has:constructor,has:a,has:k,has:own,has:inherited,has:five,has:5,has:hidden,has:acc,has:t,has:p1,has:p2,has:p39,has:xyzzy,has:name,has:prototype,has:caller,has:Symbol(s),has:Symbol(u),has:Symbol(Symbol.iterator),has:true,has:null,has:undefined"},
	{"table()", "arr:1011010010000000001000011110000010000000000001000;sparse:1100000010000000111100011110000000000000000001000;big:0000000000000010000000011110000000000000000001000;sub:1110000010000000001100011110000000000000000001000;bare:0010000000000000000000000000100000000000000000000;plain:1100000010000000001100000111000001100000000100000;child:1100000010000000001100000111010001100000000100000;deep:0000010000000000000000000110001010000000000000000;str:1100000010000000001100010110000000000000000001000;ta:1110000010000000001100010110000000000000000001000;fn:0000000000000000000000010110000000000000111000000;argsMapped:1000000010000000001000010110000000000000000001000;argsStrict:1100000010000000001100010110000000000000000001000;trap:0000000000000000000000000110000000010000000000000;dict:0000000000000000000000000110000000001010000000000;[]:0000000000000000000000011110000000000000000001000;{}:0000000000000000000000000110000000000000000000000;global:0000000000110000000000000110000000000000000000001;has:0,has:1,has:2,has:3,has:4,has:5,has:6,has:-1,has:0,has:0.5,has:NaN,has:Infinity,has:2147483647,has:2147483648,has:4294967294,has:4294967295,has:100000,has:3000000000,has:0,has:1,has:01,has:-0,has:1.0,has:length,has:push,has:toString,has:constructor,has:a,has:k,has:own,has:inherited,has:five,has:5,has:hidden,has:acc,has:t,has:p1,has:p2,has:p39,has:xyzzy,has:name,has:prototype,has:caller,has:Symbol(s),has:Symbol(u),has:Symbol(Symbol.iterator),has:true,has:null,has:undefined"},
	{"conversions()", "true,toString;TypeError,0;TypeError;TypeError;true,toString;TypeError,0;TypeError;TypeError;"},
	{"atoms()", "145"},
	{"fresh()", "false,false,false,false,has:fresh0key,has:fresh0key;false,false,false,false,has:fresh1key,has:fresh1key;false,false,false,false,has:fresh2key,has:fresh2key;"},
	{"changing(24)", "10000101;10000101;10000101;10000101;11000101;11000101;10100101;10100101;10010101;10010101;10000101;10000101;10000101;10000101;11000101;11000101;01000101;01000101;01000011;01000011;01000111;01000111;01000111;01000111;"},
	{"holes(80)", "70"},
	{"far(10)", "10000"},
	{"sparseLoop(10)", "110"},
	{"mixed(40)", "1101100010111011000101110110001011101100"},
}

// TestInOperator pins the in operator, which the tree tier answers from
// what plain objects and arrays hold (hasQuick) and inOperator otherwise:
// each case three times, by the interpreter at the top level and by the
// tree tier in each function's loop, as Node answers it.
func TestInOperator(t *testing.T) {
	r := New(Config{})
	defer func() { r.Close(); r.ReleaseClosed() }()
	if _, err := r.Run(compileForTest(t, inOperatorSetup)); err != nil {
		t.Fatal(err)
	}
	for _, c := range inOperatorCases {
		for round := range 3 {
			v, err := r.Run(compileForTest(t, "''+("+c.src+")"))
			if err != nil {
				t.Fatalf("%s: %v", c.src, err)
			}
			if got := v.String().Go(); got != c.want {
				t.Fatalf("%s, round %d:\n got %q\nwant %q", c.src, round, got, c.want)
			}
		}
	}
	// The interpreter's own case, of every key and object at the top
	// level, where no loop runs it: table's rows.
	rows := strings.Split(inOperatorCases[0].want, ";")
	var src strings.Builder
	src.WriteString("names[o] + ':'")
	for k := range len(rows[0]) - len("arr:") {
		fmt.Fprintf(&src, " + (keys[%d] in objects[o] ? '1' : '0')", k)
	}
	for o := range 18 {
		v, err := r.Run(compileForTest(t, fmt.Sprintf("var o = %d; %s", o, src.String())))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != rows[o] {
			t.Fatalf("interpreter, object %d:\n got %s\nwant %s", o, got, rows[o])
		}
	}
}
