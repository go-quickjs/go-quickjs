package vm

import "testing"

// TestShortMethodResults pins what Array methods make for short results,
// which have their room in the array's own allocation, and whose view of the
// receiver and result under construction are kept on the method's stack:
// holes kept as holes, a result grown past its room, a subclass's species,
// an array-like receiver and a nullish one's TypeError. The output is
// Node's.
func TestShortMethodResults(t *testing.T) {
	const src = `var out = [];
var h = [1, , 3, , 5];
out.push(JSON.stringify(h.slice()), 1 in h.slice(), h.slice(1).length, h.slice(-1)[0], h.slice(3).hasOwnProperty(0));
var long = [];
for (var i = 0; i < 40; i++) long.push(i);
var s = long.slice(2, 19);
s.push("x");
out.push(s.length, s[0], s[17], long.slice(5, 5).length);
out.push(JSON.stringify([1, 2, 3].map(function (x) { return x * 2 })), JSON.stringify([1, 2, 3, 4].filter(function (x) { return x & 1 })));
out.push(JSON.stringify([1, 2].concat([3], 4)), JSON.stringify([1, 2, 3, 4].splice(1, 2)), [3, 1, 2].indexOf(2), [1, 2, 3].join("-"));
class A extends Array {}
var a = A.from([1, 2, 3]);
out.push(a.slice() instanceof A, a.map(function (x) { return x }) instanceof A);
var arrayLike = {length: 3, 0: "a", 2: "c"};
out.push(JSON.stringify(Array.prototype.slice.call(arrayLike)), Array.prototype.indexOf.call(arrayLike, "c"));
try { Array.prototype.slice.call(null) } catch (e) { out.push(e instanceof TypeError) }
var big = Array.prototype.slice.call({length: 17, 16: "z"});
out.push(big.length, big[16], 0 in big);
JSON.stringify(out);
`
	const want = `["[1,null,3,null,5]",false,4,5,false,18,2,"x",0,"[2,4,6]","[1,3]","[1,2,3,4]","[2,3]",2,"1-2-3",true,true,"[\"a\",null,\"c\"]",2,true,17,"z",false]`
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: got %s, want %s", tree, got, want)
		}
		r.Close()
	}
}
