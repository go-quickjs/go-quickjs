package vm

import "testing"

// arrayHolesSetup makes array literals with holes -- one, two, at either
// end, between elements, nineteen of them, past sixteen elements, beside
// undefined and null, beside a spread of one with holes -- and describes
// each as its length, its keys, what forEach and for-of visit, JSON, the
// in operator, indexOf and includes of undefined and its own names; and a
// loop that makes such a literal and asks in of it at each turn.
const arrayHolesSetup = `function describe(a) {
	var keys = Object.keys(a).join("."), seen = "";
	a.forEach(function (v, i) { seen += i + "=" + v + " "; });
	var it = "";
	for (var v of a) it += String(v) + " ";
	return a.length + "|" + keys + "|" + seen + "|" + it + "|" + JSON.stringify(a) + "|" + (0 in a) + (1 in a) + (2 in a) +
		"|" + a.indexOf(undefined) + a.includes(undefined) + "|" + Object.getOwnPropertyNames(a).join(".");
}
function literals(x) {
	return [[], [,], [, ,], [1, ,], [, 1], [1, , 3], [x, , x + 1, , ], [, , , , , , , , , , , , , , , , , , 9],
		[1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, , 17], [undefined, , null], [, ...[1, , 2], ,]];
}
function holeLoop(n) {
	var out = "", s = 0;
	for (var i = 0; i < n; i++) {
		var a = [i, , i + 1, , ];
		if (1 in a) s += 100;
		if (0 in a) s++;
		if (2 in a) s++;
		s += a.length;
		a[1] = i;
		if (1 in a) s += 1000;
		out = a.join("-");
	}
	return s + ":" + out;
}
function allLiterals(x) {
	var out = "";
	for (var i = 0; i < 2; i++) {
		var ls = literals(x + i);
		for (var j = 0; j < ls.length; j++) out += describe(ls[j]) + "\n";
	}
	return out;
}
`

// arrayHolesCases are arrayHolesSetup's answers, Node v26.10.0's.
var arrayHolesCases = []struct{ src, want string }{
	{"allLiterals(5)", "0||||[]|falsefalsefalse|-1false|length\n1|||undefined |[null]|falsefalsefalse|-1true|length\n2|||undefined undefined |[null,null]|falsefalsefalse|-1true|length\n2|0|0=1 |1 undefined |[1,null]|truefalsefalse|-1true|0.length\n2|1|1=1 |undefined 1 |[null,1]|falsetruefalse|-1true|1.length\n3|0.2|0=1 2=3 |1 undefined 3 |[1,null,3]|truefalsetrue|-1true|0.2.length\n4|0.2|0=5 2=6 |5 undefined 6 undefined |[5,null,6,null]|truefalsetrue|-1true|0.2.length\n19|18|18=9 |undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined 9 |[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,9]|falsefalsefalse|-1true|18.length\n17|0.1.2.3.4.5.6.7.8.9.10.11.12.13.14.16|0=1 1=2 2=3 3=4 4=5 5=6 6=7 7=8 8=9 9=10 10=11 11=12 12=13 13=14 14=15 16=17 |1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 undefined 17 |[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,null,17]|truetruetrue|-1true|0.1.2.3.4.5.6.7.8.9.10.11.12.13.14.16.length\n3|0.2|0=undefined 2=null |undefined undefined null |[null,null,null]|truefalsetrue|0true|0.2.length\n5|1.2.3|1=1 2=undefined 3=2 |undefined 1 undefined 2 undefined |[null,1,null,2,null]|falsetruetrue|2true|1.2.3.length\n0||||[]|falsefalsefalse|-1false|length\n1|||undefined |[null]|falsefalsefalse|-1true|length\n2|||undefined undefined |[null,null]|falsefalsefalse|-1true|length\n2|0|0=1 |1 undefined |[1,null]|truefalsefalse|-1true|0.length\n2|1|1=1 |undefined 1 |[null,1]|falsetruefalse|-1true|1.length\n3|0.2|0=1 2=3 |1 undefined 3 |[1,null,3]|truefalsetrue|-1true|0.2.length\n4|0.2|0=6 2=7 |6 undefined 7 undefined |[6,null,7,null]|truefalsetrue|-1true|0.2.length\n19|18|18=9 |undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined undefined 9 |[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,9]|falsefalsefalse|-1true|18.length\n17|0.1.2.3.4.5.6.7.8.9.10.11.12.13.14.16|0=1 1=2 2=3 3=4 4=5 5=6 6=7 7=8 8=9 9=10 10=11 11=12 12=13 13=14 14=15 16=17 |1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 undefined 17 |[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,null,17]|truetruetrue|-1true|0.1.2.3.4.5.6.7.8.9.10.11.12.13.14.16.length\n3|0.2|0=undefined 2=null |undefined undefined null |[null,null,null]|truefalsetrue|0true|0.2.length\n5|1.2.3|1=1 2=undefined 3=2 |undefined 1 undefined 2 undefined |[null,1,null,2,null]|falsetruetrue|2true|1.2.3.length\n"},
	{"holeLoop(50)", "50300:49-49-50-"},
	{"holeLoop(7)", "7042:6-6-7-"},
}

// TestArrayLiteralHoles pins array literals with holes, which are made by
// one new_array of their elements, a hole the uninitialized value an
// array's storage holds for one, as Node makes them: each case three
// times, by the interpreter and by the tree tier.
func TestArrayLiteralHoles(t *testing.T) {
	r := New(Config{})
	defer func() { r.Close(); r.ReleaseClosed() }()
	if _, err := r.Run(compileForTest(t, arrayHolesSetup)); err != nil {
		t.Fatal(err)
	}
	for _, c := range arrayHolesCases {
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
}
