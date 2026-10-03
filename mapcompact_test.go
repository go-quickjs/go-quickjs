package quickjs_test

import "testing"

// TestMapCompaction covers a Map or Set dropping its deleted entries while
// something is walking it. The walk must go on from where it was, visiting
// what was added since and nothing deleted, whether the entries moved under it
// once or several times, and whether it is an iterator, forEach, or a Set
// method that calls back into the script.
func TestMapCompaction(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"iterator", `var m = new Map(); for (var i = 0; i < 100; i++) m.set(i, i)
		  var it = m.keys(), seen = []
		  for (var i = 0; i < 10; i++) seen.push(it.next().value)
		  for (var i = 0; i < 90; i++) m.delete(i)
		  for (var i = 100; i < 105; i++) m.set(i, i)
		  for (var x of it) seen.push(x)
		  seen.join()`,
			"0,1,2,3,4,5,6,7,8,9,90,91,92,93,94,95,96,97,98,99,100,101,102,103,104"},
		{"iterator over several compactions", `var m = new Map(), seen = []
		  for (var i = 0; i < 64; i++) m.set(i, i)
		  var it = m.values(); seen.push(it.next().value)
		  for (var round = 0; round < 5; round++) {
		    for (var i = 0; i < 64; i++) m.delete(round * 64 + i)
		    for (var i = 0; i < 64; i++) m.set((round + 1) * 64 + i, i)
		  }
		  var rest = [...it];
		  [seen[0], rest.length, rest[0], rest[63], m.size].join()`, "0,64,0,63,64"},
		{"deleted entry where the iterator stopped", `var m = new Map([[1, 1], [2, 2], [3, 3]])
		  for (var i = 10; i < 50; i++) m.set(i, i)
		  var it = m.keys(); it.next(); it.next()
		  for (var i = 2; i < 50; i++) m.delete(i)
		  m.set("x", 0);
		  [...it].join()`, "x"},
		{"forEach", `var m = new Map(); for (var i = 0; i < 100; i++) m.set(i, i)
		  var seen = []
		  m.forEach(function (v, k) {
		    seen.push(k)
		    if (k === 5) { for (var i = 6; i < 95; i++) m.delete(i); m.set("x", 0) }
		  })
		  seen.join()`, "0,1,2,3,4,5,95,96,97,98,99,x"},
		{"Set forEach", `var s = new Set(); for (var i = 0; i < 100; i++) s.add(i)
		  var seen = []
		  s.forEach(function (v) {
		    seen.push(v)
		    if (v === 0) for (var i = 1; i < 98; i++) s.delete(i)
		  })
		  seen.join()`, "0,98,99"},
		{"Set method calling back", `var s = new Set(); for (var i = 0; i < 100; i++) s.add(i)
		  var asked = []
		  var other = { size: 1000, has: function (v) {
		    asked.push(v)
		    if (v === 0) for (var i = 1; i < 99; i++) s.delete(i)
		    return true }, keys: function () { return [][Symbol.iterator]() } };
		  [s.isSubsetOf(other), asked.join()].join(" ")`, "true 0,99"},
		{"clear while iterating", `var m = new Map(); for (var i = 0; i < 100; i++) m.set(i, i)
		  var it = m.entries(); it.next()
		  m.clear(); m.set("a", 1); m.set("b", 2);
		  [...it].map(function (e) { return e.join(":") }).join()`, "a:1,b:2"},
		{"finished iterator stays finished", `var m = new Map([[1, 1]]), it = m.keys(); it.next(); it.next()
		  for (var i = 0; i < 40; i++) { m.set(i + 10, i); m.delete(i + 10) }
		  m.set(2, 2)
		  it.next().done`, "true"},
		{"first key, the LRU way", `var m = new Map()
		  for (var i = 0; i < 10000; i++) { m.set(i, i); if (m.size > 3) m.delete(m.keys().next().value) }
		  ;[m.size, [...m.keys()].join()].join(" ")`, "3 9997,9998,9999"},
		{"keys through rebuilds", `var m = new Map(), big = 2n ** 80n
		  for (var i = 0; i < 300; i++) { m.set("k" + i, i); m.set(BigInt(i) * big, i); m.set(i + 0.5, i) }
		  for (var i = 0; i < 300; i += 2) { m.delete("k" + i); m.delete(BigInt(i) * big); m.delete(i + 0.5) }
		  var built = "k"; built += 299;
		  [m.size, m.get(built), m.get(299n * 2n ** 80n), m.get(299.5), m.has("k298"), m.has(298n * big)].join()`,
			"450,299,299,299,false,false"},
		{"NaN and zero after rebuilds", `var m = new Map([[NaN, "n"], [-0, "z"]])
		  for (var i = 1; i < 200; i++) m.set(i, i)
		  for (var i = 1; i < 200; i++) m.delete(i);
		  [m.size, m.get(0 / 0), m.get(0), Object.is([...m.keys()][1], 0)].join()`, "2,n,z,true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkEval(t, tc.src, tc.want)
		})
	}
}
