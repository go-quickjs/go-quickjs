//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// levelClosureSetup's callees, each called natively and never inlined,
// make a closure that captures none of their registers: an arrow of
// nothing, one of a global, one of its maker's upvalue, one of its
// maker's this, a function expression of nothing. bump's captures its
// parameter, a register.
const levelClosureSetup = `
	'use strict'
	var base = 3
	function apply(f, x) { return f(x) }
	function none(x) { return apply((y) => y + 1, x) }
	function global(x) { return apply((y) => y * base, x) }
	function maker() { let m = 5; return function up(x) { return apply((y) => y + m, x) } }
	var up = maker()
	var obj = { k: 7, method(x) { return apply((y) => y + this.k, x) } }
	function expr(x) { return apply(function (y) { return y - 1 }, x) }
	function bump(x) { return apply((y) => y + x, 1) }
	function drive(n) {
		let s = 0
		for (let i = 0; i < n; i++) s += none(i) + global(i) + up(i) + obj.method(i) + expr(i)
		return s
	}
	function driveBump(n) { let s = 0; for (let i = 0; i < n; i++) s += bump(i); return s }`

// A native callee's level makes, from native code, a closure that captures
// none of its registers where native code does not -- its pool run out,
// or none given (jitRegLevelClosure): that call of Go had been a host
// exit, Go making the level's frame to run closure_w, where nothing of the
// frame is needed. A sort's driver called show natively once its method
// reads were fed, and left native code at show's arrow. Every answer is
// the interpreter's, and drive's callees, each making closures past a
// pool's capacity, leave native code at none (10 times without it); bump's,
// which captures its parameter, still has Go make its frame, and answers
// the same.
func TestJITLevelClosureNative(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			callees := map[string]bool{"none": true, "global": true, "up": true, "method": true, "expr": true, "bump": true, "apply": true}
			jitTestSwitch(t, func(jc *jitTestConfig) {
				jc.noInline = func(fn *bytecode.Function) bool { return callees[fn.Name] }
			})
			rts := regNativeRuntimes(t, levelClosureSetup)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			run := func(src string) {
				prog := compileForTest(t, src)
				var got [2]string
				for k, r := range rts {
					v, err := r.Run(prog)
					got[k] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("%s: with the JIT %q, without %q", src, got[0], got[1])
				}
			}
			for range 8 {
				run("drive(50)")
				run("driveBump(50)")
			}
			jitCodeNamedForTest(t, r, "drive")
			s := r.jitState()
			hosts := s.hosts
			// Past a pool's capacity (abi.PoolCapacity) of closures from
			// each callee: Go makes some of them, the pools run out.
			run("drive(300)")
			if n := s.hosts - hosts; n != 0 {
				t.Errorf("drive's callees left native code %d times", n)
			}
			run("driveBump(300)")
		})
	}
}
