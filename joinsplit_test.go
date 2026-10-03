package quickjs_test

import "testing"

// TestJoinAndSplit covers join, which writes each piece with the unpaired
// surrogates the String already knows it has at either end and sizes its
// buffer from the elements, and split, which collects its pieces in a buffer
// the runtime keeps between calls.
func TestJoinAndSplit(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var r = []
		  r.push(["\uD83D", "\uDE00"].join("") === "😀", ["\uD83D", "\uDE00"].join() === "\uD83D,\uDE00",
		    ["a\uD83D", "b"].join("\uDE00") === "a😀b", ["x", "y"].join("😀") === "x😀y",
		    [, "\uD83D", null, "\uDE00", undefined].join("").length)
		  var big = "q".repeat(100); big += "r".repeat(100)
		  r.push([big, big].join("-").length, [1, "é", true, 2.5].join("|"), [].join(), [7].join())
		  r.join()`, "true,true,true,true,2,401,1|é|true|2.5,,7"},
		{`var a = "a,b,c".split(","), b = "d,e".split(","), r = [a.join(), b.join(), a.length, b.length]
		  var many = "x,".repeat(3000).split(","); r.push(many.length, "f,g".split(",").join("+"))
		  r.push("".split(",").length, "abc".split("").join("."), "a,b,c".split(",", 2).join())
		  r.join(" ")`, "a,b,c d,e 3 2 3001 f+g 1 a.b.c a,b"},
		{`[(1.005).toFixed(2), (-1.5).toFixed(0), (0.5).toFixed(0), (-0).toFixed(2), (1e21).toFixed(2),
		   (123.456).toFixed(10), (2.5).toFixed(0), (1.45).toFixed(1), (0.000001).toFixed(7)].join()`,
			"1.00,-2,1,0.00,1e+21,123.4560000000,3,1.4,0.0000010"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
