package vm

import "testing"

// arraySearchSetup searches arrays with Array.prototype's indexOf,
// lastIndexOf and includes: an array of a value of every kind -- numbers
// about 0, NaN, strings flat and joined, a hole, undefined, null, the
// booleans, a symbol, objects, BigInts held in the value and on the heap --
// for targets of every kind from starts of every kind; holes, which no
// prototype fills and then one does, by a value and by a getter, on the
// array's prototype, on Object.prototype and on a prototype of its own;
// starts whose conversion shortens the array or grows it; arrays the
// searches' own loop takes -- a subclass's, a sparse one, a proxy, which
// logs what it is asked, array-likes, a string, a typed array, an
// arguments object; long arrays; and starts that throw.
const arraySearchSetup = `var big = 12345678901234567890123n, sym = Symbol("s"), o1 = {}, o2 = {};
var vals = [1, -0, NaN, "x", "y" + "z", , undefined, null, true, false, sym, o1, 5n, big, 1.5, 0];
var targets = [1, 0, -0, NaN, "x", "yz", undefined, null, true, false, sym, o1, o2, 5n,
	12345678901234567890123n, 1.5, "1", 2, Infinity];
var starts = [undefined, 3, 7, -3, -12, 20, -20, 2.5, NaN, Infinity, -Infinity];
function searchAll(a) {
	var out = "";
	for (var i = 0; i < targets.length; i++) {
		var t = targets[i];
		out += a.indexOf(t) + "," + a.lastIndexOf(t) + "," + a.includes(t) + ":";
		for (var j = 0; j < starts.length; j++)
			out += a.indexOf(t, starts[j]) + "," + a.lastIndexOf(t, starts[j]) + "," + a.includes(t, starts[j]) + ";";
		out += "|";
	}
	return out;
}
function holes() {
	var out = "", h = [, , 3, , 5];
	for (var i = 0; i < 3; i++)
		out += [h.indexOf(undefined), h.includes(undefined), h.lastIndexOf(undefined), h.includes(undefined, 2),
			h.includes(undefined, 4), h.indexOf(3), h.lastIndexOf(5, 3)].join() + ";";
	return out;
}
function shrinking() {
	var out = "", s;
	for (var i = 0; i < 2; i++) {
		s = [1, 2, 3, 4, 5];
		out += s.includes(undefined, { valueOf() { s.length = 2; return 0; } }) + ",";
		s = [1, 2, 3, 4, 5];
		out += s.indexOf(4, { valueOf() { s.length = 2; return 0; } }) + ",";
		s = [1, 2, 3];
		out += s.includes(undefined, { valueOf() { s.length = 1; return 2; } }) + ",";
		s = [1, 2, 3];
		out += s.includes(undefined, { valueOf() { s.length = 1; return 5; } }) + ",";
		s = [1, 2, 3];
		out += s.lastIndexOf(3, { valueOf() { s.length = 1; return 5; } }) + ",";
		s = [1, 2, 3];
		out += s.indexOf(9, { valueOf() { s.push(9); return 0; } }) + ",";
		s = [1, 2, 3];
		out += s.includes(9, { valueOf() { s.push(9); return 0; } }) + ";";
	}
	return out;
}
// The same array searched while what its prototypes hold changes.
function inherited(n) {
	var out = "", q = [1, , 3, , 5];
	for (var i = 0; i < n; i++) {
		if (i == 3) Array.prototype[1] = 7;
		if (i == 5) { delete Array.prototype[1]; Object.prototype[3] = 7; }
		if (i == 7) { delete Object.prototype[3]; Object.defineProperty(Array.prototype, 1, { get() { return 7; }, configurable: true }); }
		if (i == 9) delete Array.prototype[1];
		if (i == 11) Object.setPrototypeOf(q, Object.create(Array.prototype, { 3: { value: 7, configurable: true } }));
		if (i == 13) Object.setPrototypeOf(q, Array.prototype);
		out += q.indexOf(7) + "," + q.lastIndexOf(7) + "," + q.includes(7) + "," + q.includes(undefined) + "," + q.indexOf(5) + ";";
	}
	return out;
}
class Sub extends Array {}
function others() {
	var out = "";
	var sub = Sub.from([4, 5, 6]), sp = [], px = new Proxy([1, 2, 3], {}), like = { length: 3, 0: "a", 2: "b" };
	sp[100000] = 3; sp[5] = 2;
	var log = [], trap = new Proxy([1, , 3], { has(t, k) { log.push("has" + String(k)); return k in t; },
		get(t, k) { log.push("get" + String(k)); return t[k]; } });
	var ta = new Int8Array([1, 2, 3]), args = (function () { return arguments; })(1, 2, 3);
	for (var i = 0; i < 2; i++) {
		out += [sub.indexOf(5), sub.lastIndexOf(6), sub.includes(4), sp.indexOf(3), sp.lastIndexOf(2), sp.includes(3),
			sp.includes(undefined), px.indexOf(2), px.includes(3), Array.prototype.indexOf.call(like, "b"),
			Array.prototype.includes.call(like, undefined), Array.prototype.indexOf.call("abc", "b"),
			Array.prototype.lastIndexOf.call(ta, 2), Array.prototype.indexOf.call(args, 3), trap.indexOf(3), trap.includes(9)].join() + ";";
	}
	return out + log.join();
}
function longSearch() {
	var long = [], out = "";
	for (var i = 0; i < 20000; i++) long.push(i);
	for (var k = 0; k < 2; k++)
		out += [long.indexOf(19999), long.lastIndexOf(0), long.includes(12345), long.indexOf(-1), long.lastIndexOf(10000, 9999),
			long.indexOf(5, 6), long.includes(19999, -1), long.lastIndexOf(19999, -2)].join() + ";";
	var strs = ["a" + "b", "c", "ab"];
	return out + [strs.indexOf("ab"), strs.lastIndexOf("ab"), strs.includes("a" + "b"), strs.indexOf("c", 1)].join();
}
function thrown() {
	var out = "";
	for (var i = 0; i < 2; i++) {
		try { [1].indexOf(1, Symbol()); } catch (e) { out += e.constructor.name + ","; }
		try { [1].includes(1, 1n); } catch (e) { out += e.constructor.name + ","; }
		try { Array.prototype.indexOf.call(null, 1); } catch (e) { out += e.constructor.name + ","; }
		out += [[].indexOf(1, Symbol()), [].includes(1, 1n)].join() + ";";
	}
	return out;
}
`

// arraySearchCases are the searches and Node v26.10.0's answers.
var arraySearchCases = []struct{ src, want string }{
	{"searchAll(vals)", "0,0,true:0,0,true;-1,0,false;-1,0,false;-1,0,false;-1,0,false;-1,0,false;0,-1,true;-1,0,false;0,0,true;-1,0,false;0,-1,true;|1,15,true:1,-1,true;15,1,true;15,1,true;15,1,true;15,1,true;-1,15,false;1,-1,true;15,1,true;1,-1,true;-1,15,false;1,-1,true;|1,15,true:1,-1,true;15,1,true;15,1,true;15,1,true;15,1,true;-1,15,false;1,-1,true;15,1,true;1,-1,true;-1,15,false;1,-1,true;|-1,-1,true:-1,-1,true;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,true;-1,-1,true;-1,-1,true;-1,-1,false;-1,-1,true;|3,3,true:3,-1,true;3,3,true;-1,3,false;-1,3,false;-1,3,false;-1,3,false;3,-1,true;3,-1,true;3,-1,true;-1,3,false;3,-1,true;|4,4,true:4,-1,true;4,-1,true;-1,4,false;-1,4,false;4,4,true;-1,4,false;4,-1,true;4,-1,true;4,-1,true;-1,4,false;4,-1,true;|6,6,true:6,-1,true;6,-1,true;-1,6,false;-1,6,false;6,-1,true;-1,6,false;6,-1,true;6,-1,true;6,-1,true;-1,6,false;6,-1,true;|7,7,true:7,-1,true;7,-1,true;7,7,true;-1,7,false;7,-1,true;-1,7,false;7,-1,true;7,-1,true;7,-1,true;-1,7,false;7,-1,true;|8,8,true:8,-1,true;8,-1,true;8,-1,true;-1,8,false;8,-1,true;-1,8,false;8,-1,true;8,-1,true;8,-1,true;-1,8,false;8,-1,true;|9,9,true:9,-1,true;9,-1,true;9,-1,true;-1,9,false;9,-1,true;-1,9,false;9,-1,true;9,-1,true;9,-1,true;-1,9,false;9,-1,true;|10,10,true:10,-1,true;10,-1,true;10,-1,true;-1,10,false;10,-1,true;-1,10,false;10,-1,true;10,-1,true;10,-1,true;-1,10,false;10,-1,true;|11,11,true:11,-1,true;11,-1,true;11,-1,true;-1,11,false;11,-1,true;-1,11,false;11,-1,true;11,-1,true;11,-1,true;-1,11,false;11,-1,true;|-1,-1,false:-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;|12,12,true:12,-1,true;12,-1,true;12,-1,true;-1,12,false;12,-1,true;-1,12,false;12,-1,true;12,-1,true;12,-1,true;-1,12,false;12,-1,true;|13,13,true:13,-1,true;13,-1,true;13,-1,true;13,13,true;13,-1,true;-1,13,false;13,-1,true;13,-1,true;13,-1,true;-1,13,false;13,-1,true;|14,14,true:14,-1,true;14,-1,true;14,-1,true;14,-1,true;14,-1,true;-1,14,false;14,-1,true;14,-1,true;14,-1,true;-1,14,false;14,-1,true;|-1,-1,false:-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;|-1,-1,false:-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;|-1,-1,false:-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;-1,-1,false;|"},
	{"holes()", "-1,true,-1,true,false,2,-1;-1,true,-1,true,false,2,-1;-1,true,-1,true,false,2,-1;"},
	{"shrinking()", "true,-1,true,false,-1,-1,false;true,-1,true,false,-1,-1,false;"},
	{"inherited(15)", "-1,-1,false,true,4;-1,-1,false,true,4;-1,-1,false,true,4;1,1,true,true,4;1,1,true,true,4;3,3,true,true,4;3,3,true,true,4;1,1,true,true,4;1,1,true,true,4;-1,-1,false,true,4;-1,-1,false,true,4;3,3,true,true,4;3,3,true,true,4;-1,-1,false,true,4;-1,-1,false,true,4;"},
	{"others()", "1,2,true,100000,5,true,true,1,true,2,true,1,1,2,2,false;1,2,true,100000,5,true,true,1,true,2,true,1,1,2,2,false;getindexOf,getlength,has0,get0,has1,has2,get2,getincludes,getlength,get0,get1,get2,getindexOf,getlength,has0,get0,has1,has2,get2,getincludes,getlength,get0,get1,get2"},
	{"longSearch()", "19999,0,true,-1,-1,-1,true,-1;19999,0,true,-1,-1,-1,true,-1;0,2,true,1"},
	{"thrown()", "TypeError,TypeError,TypeError,-1,false;TypeError,TypeError,TypeError,-1,false;"},
}

// TestArraySearches pins Array.prototype's indexOf, lastIndexOf and
// includes, which compare an array's elements where they are when they are
// all there is to its indices (searchElems), and are called without a frame
// then (elemArrIndexOf): each search is made three times, by the
// interpreter at the top level and by the tree tier in each function's
// loop, against Node's answers.
func TestArraySearches(t *testing.T) {
	r := New(Config{})
	defer func() { r.Close(); r.ReleaseClosed() }()
	if _, err := r.Run(compileForTest(t, arraySearchSetup)); err != nil {
		t.Fatal(err)
	}
	for _, c := range arraySearchCases {
		for round := range 3 {
			v, err := r.Run(compileForTest(t, "''+("+c.src+")"))
			if err != nil {
				t.Fatalf("%s: %v", c.src, err)
			}
			if got := v.String().Go(); got != c.want {
				t.Fatalf("%s, round %d:\n got %s\nwant %s", c.src, round, got, c.want)
			}
		}
	}
}
