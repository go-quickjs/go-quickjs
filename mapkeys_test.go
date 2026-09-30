package quickjs_test

import "testing"

// TestMapKeyKinds covers the indexes a Map or Set keeps by key kind: numbers,
// strings and the other primitives each in their own, and SameValueZero across
// them -- NaN one key, +0 and -0 one key, 1 and "1" and true and 1n four.
func TestMapKeyKinds(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var m = new Map([[NaN, "nan"], [0, "zero"], [1, "num"], ["1", "str"], [true, "bool"], [1n, "big"],
		                  [null, "null"], [undefined, "undef"], ["", "empty"]])
		  m.set(0 / 0, "nan2").set(-0, "zero2")
		  var built = "o"; built += "k"; m.set(built, "ok");
		  [m.size, m.get(NaN), m.get(-0), m.get(+0), m.get(1), m.get("1"), m.get(true), m.get(1n),
		   m.get(null), m.get(undefined), m.get(""), m.get("ok"), m.has(2), m.has("0"), Object.is([...m.keys()][1], -0)].join()`,
			"10,nan2,zero2,zero2,num,str,bool,big,null,undef,empty,ok,false,false,false"},
		{`var s = new Set([3, "3", 3, "x", 2.5, -2.5, NaN, NaN])
		  s.delete("3"); s.add("3"); s.delete(2.5); s.add(2.5);
		  [...s].map(String).join()`, "3,x,-2.5,NaN,3,2.5"},
		{`var m = new Map(); for (var i = 0; i < 1000; i++) m.set(i, i * 2)
		  for (var i = 0; i < 1000; i += 3) m.delete(i)
		  var sum = 0; for (var [k, v] of m) sum += v - 2 * k;
		  [m.size, sum, m.get(998), m.has(999), m.get(3)].join()`, "666,0,1996,false,"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
