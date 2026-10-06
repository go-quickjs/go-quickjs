package quickjs_test

import (
	"context"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestCallTree covers calls of functions planTreeCall has given a tree,
// which are called on it without what callDirect and runFD ask of other
// calls: each case calls its functions enough times to be planned, and
// gives what a frame made the other way gives -- this, sloppy and strict,
// of a plain call and of a primitive receiver; arguments past the
// parameters and missing ones; closures over the frame's locals; the
// frames in a stack trace; a call site whose callee changes; a strict tail
// call, which is never planned; and runaway recursion.
func TestCallTree(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function who() { var x = this; return x === globalThis }
		  function strictWho() { "use strict"; var x = this; return x }
		  var o = { m: function () { var x = this; return x === o } };
		  String.prototype.typeOfThis = function () { var x = this; return typeof x };
		  String.prototype.strictTypeOfThis = function () { "use strict"; var x = this; return typeof x };
		  Number.prototype.typeOfThis = String.prototype.typeOfThis;
		  function loop() { var r; for (var i = 0; i < 5; i++) r = [who(), strictWho(), o.m(), "s".typeOfThis(), "s".strictTypeOfThis(), (5).typeOfThis()]; return r.join() }
		  loop() + " " + loop()`, "true,,true,object,string,object true,,true,object,string,object"},
		{`function args(a, b) { var n = arguments.length; return n + ":" + a + ":" + b }
		  function loop() { var r = []; for (var i = 0; i < 3; i++) r.push(args(1), args(1, 2, 3)); return r.join() }
		  loop()`, "1:1:undefined,3:1:2,1:1:undefined,3:1:2,1:1:undefined,3:1:2"},
		{`function mk(n) { var fs = []; for (var i = 0; i < 2; i++) { let j = i + n; fs.push(function () { return j }) } return fs }
		  function loop() { var s = []; for (var k = 0; k < 3; k++) { var fs = mk(k * 10); s.push(fs[0](), fs[1]()) } return s.join() }
		  loop()`, "0,1,10,11,20,21"},
		{`function a(n) { var m = n; if (m == 0) throw new Error("x"); return a(m - 1) + 1 }
		  function b() { try { return a(3) } catch (e) { return e.stack.split("\n").slice(1, 6).map(function (l) { return l.trim().split(" ")[1] }).join(",") } }
		  var r; for (var i = 0; i < 5; i++) r = b(); r`, "a,a,a,a,b"},
		{`var fs = [function () { var x = 1; return x }, function () { var x = 2; return x }, Math.max,
		    function () { "use strict"; var x = this; return x === undefined }.bind(undefined), function (a) { var x = a; return x }];
		  function loop() { var r = []; for (var i = 0; i < 10; i++) r.push(fs[i % 5](7)); return r.join() }
		  loop()`, "1,2,7,true,7,1,2,7,true,7"},
		{`"use strict"; function tc(n) { var m = n; return m == 0 ? "done" : tc(m - 1) }
		  var r; for (var i = 0; i < 3; i++) r = tc(1000); r`, "done"},
		{`function down(n) { var m = n; return 1 + down(m + 1) }
		  var r = []; for (var i = 0; i < 3; i++) { try { down(0) } catch (e) { r.push(e.name) } } r.join()`, "RangeError,RangeError,RangeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestCallTreeLimits covers what a planned call checks at every call: the
// frame depth WithMaxCallDepth sets, and the interrupt a context's end
// makes, in a loop that does nothing but such calls.
func TestCallTreeLimits(t *testing.T) {
	rt := quickjs.New(quickjs.WithMaxCallDepth(100))
	defer rt.Close()
	if _, err := rt.Eval(`function d(n) { var m = n; return m == 0 ? 0 : 1 + d(m - 1) }
		for (var i = 0; i < 5; i++) d(50)`); err != nil {
		t.Fatal(err)
	}
	got, err := rt.Eval(`var r; try { d(200) } catch (e) { r = e.name } r + " " + d(50)`)
	if err != nil || got.String() != "RangeError 50" {
		t.Fatalf("depth: got %v, %v", got, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = rt.EvalContext(ctx, `function f(x) { var t = x + 1; return t } function spin() { for (;;) f(1) } spin()`)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("interrupt: got %v", err)
	}
}
