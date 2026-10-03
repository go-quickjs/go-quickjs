package quickjs_test

import "testing"

// TestMathMinMaxCalls covers Math.max and Math.min given two numbers, which a
// call applies itself without a frame, as it does Math.floor given one. The
// answers must be the built-ins': NaN and the zeros' signs, the arguments
// that are not two numbers -- fewer, more, or an object whose valueOf must
// run, in order -- and a Math.max that is not the built-in any more.
func TestMathMinMaxCalls(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function f(a, b) { return [Math.max(a, b), Math.min(a, b)] } var r = []
		  for (var p of [[1, 2], [2, 1], [-1, -2], [NaN, 1], [1, NaN], [Infinity, -Infinity], [0.5, 0.25]]) r.push(f(p[0], p[1]).join("/"))
		  r.push(Object.is(Math.max(-0, 0), 0), Object.is(Math.max(0, -0), 0), Object.is(Math.min(0, -0), -0), Object.is(Math.min(-0, 0), -0))
		  r.join()`, "2/1,2/1,-1/-2,NaN/NaN,NaN/NaN,Infinity/-Infinity,0.5/0.25,true,true,true,true"},
		{`var log = [], o = { valueOf() { log.push("o"); return 3 } }, p = { valueOf() { log.push("p"); return 1 } }
		  var r = [Math.max(), Math.min(), Math.max(4), Math.max(1, 5, 3), Math.min(o, p), Math.max("7", 2), Math.min(1, undefined)]
		  r.join() + " " + log.join("")`, "-Infinity,Infinity,4,5,1,7,NaN op"},
		{`var max = Math.max, s = 0; for (var i = 0; i < 5; i++) s += Math.max(i, 2) + Math.min(i, 2)
		  Math.max = function (a, b) { return "mine" }; var after = Math.max(1, 2); Math.max = max; s + " " + after`, "20 mine"},
		{`var r = [];
		  [3, 1].sort(function (a, b) { r.push(Math.max(a, b)); return a - b }); r.join()`, "3"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
