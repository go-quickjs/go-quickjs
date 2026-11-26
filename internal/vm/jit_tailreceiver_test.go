//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regTailReceiverSetup's calls in tail position give a frame that a plain
// call made a method's receiver, deep in the stack, where the frame is given
// up. expr's closure, which run calls plainly, calls r.base(t) in tail
// position: a jump to base's code in the closure's frame (mir's
// windowTailJump), as Babel's parser calls r.parseExpressionBase(t) from the
// closure it gives allowInAnd. o.f, called plainly by run, calls itself as
// o's method: a jump to its own start with o its receiver (tailSelf). An
// exit after the jump -- base's call of m at a depth its code had not run
// at, f's of m once late is set -- has Go make the frame of the level,
// whose receiver is the one the tail call gave. Written over with the plain
// call's undefined, base's this.n++ threw "cannot read property "n" of
// undefined", as Babel's second transform of a file threw at this.match.
const regTailReceiverSetup = `"use strict";
	function P() { this.n = 0 }
	P.prototype.run = function (e) { return e() }
	P.prototype.m = function () {}
	P.prototype.base = function (e) { this.m(); this.n++; if (e > 0) return this.expr(e - 1); return 0 }
	P.prototype.expr = function (t) { var r = this; return this.run(function () { return r.base(t) }) }
	var o = { n: 0, late: false, m: function () { return 1 } }
	o.f = function (k) { if (this === undefined) return o.f(k); if (o.late) this.m(); this.n++; if (k > 0) return o.f(k - 1); return this.n }
	function run(fn, k) { return fn(k) + 0 }
	function rec(d, k) { if (d > 0) return rec(d - 1, k) + 0; return run(o.f, k) }
	function self(n) { var s = 0; for (var i = 0; i < n; i++) { if (i === n / 2) o.late = true; s += rec(40, 3) } return s + ':' + o.n }
`

// TestJITRegTailReceiver runs regTailReceiverSetup with every function
// compiled at its first call, with the native handlers and with the Go
// table, the collector idle and marking: the frame Go makes of a native
// level that a plain call called keeps the receiver a call in tail position
// gave it (jitRegConstructs). The first round's loop is the script's own,
// which calls expr from Go at each turn: in a function, the loop's code
// inlined expr, run and the closure, and no native call was left to give
// its frame.
func TestJITRegTailReceiver(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, native := range []bool{false, true} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/marking=%v", native, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				r := jitRuntimeForTest(t, Config{JIT: true}.withNative(native))
				r.jitStress = jitStressConfig{threshold: true}
				if _, err := r.Run(compileForTest(t, regTailReceiverSetup)); err != nil {
					t.Fatal(err)
				}
				for _, c := range []struct{ src, want string }{
					{"var p = new P(); for (var i = 0; i < 300; i++) p.expr(i % 80); '' + p.n", "11550"},
					{"self(300)", "180600:1200"},
				} {
					v, err := r.Run(compileForTest(t, c.src))
					if err != nil {
						t.Errorf("%s: %v", c.src, err)
						continue
					}
					if got := v.String().Go(); got != c.want {
						t.Errorf("%s = %s, want %s", c.src, got, c.want)
					}
				}
			})
		}
	}
}
