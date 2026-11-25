//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// accessorCallSetup's getters and setters, kept out of line, are called
// by the loops' reads and writes: sum's of a loop, its receivers of two
// classes; named reads its own function object; count reads its
// arguments; total's setter is given a value and its result dropped; bad
// throws for a negative n, which the loop catches, its stack compared.
const accessorCallSetup = `
	class N { constructor(n) { this.n = n; this.last = 0 } get sum() { let s = 0; for (let i = 0; i < this.n; i++) s += i; if (this.n < 0) throw new RangeError('neg ' + this.n); return s } }
	class M extends N {}
	Object.defineProperty(N.prototype, 'named', { get: function named() { return typeof named === 'function' ? this.n : -1 } })
	Object.defineProperty(N.prototype, 'count', { get() { return arguments.length + this.n } })
	Object.defineProperty(N.prototype, 'total', { set(v) { let s = 0; for (let i = 0; i < v; i++) s += i; this.last = s } })
	const os = [new N(3), new M(5)]
	function sums(n) { let s = 0; for (let i = 0; i < n; i++) s += os[i & 1].sum; return s }
	function others(n) { let s = 0; for (let i = 0; i < n; i++) { const o = os[i & 1]; s += o.named + o.count; o.total = i & 7; s += o.last } return s }
	function bad(n) { const ns = [new N(2), new N(-1)]; let s = 0, e = ''; for (let i = 0; i < n; i++) { try { s += ns[i & 1].sum } catch (x) { e = x.message + '|' + x.stack } } return s + ':' + e }`

// A getter or a setter a read or a write does not inline is called
// natively through its function's link (ssa.AccessorSite's Call), its
// window made past the VM's stack's top -- its function, where it reads
// it, the receiver, a setter's value (mir's windowAccessor) -- as V8's
// code calls an accessor its feedback knows; it had been a call of Go at
// every read: TestJITAccessorsNative's sums made 2,000 of them. One that
// reads its own function object, or its arguments, is given them, and a
// throw leaves the getter's level to Go, the loop catching it with the
// interpreter's stack. Every answer is the interpreter's, marking or not,
// and the loops call each accessor natively, making no call of Go.
func TestJITAccessorCalledNatively(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			jitTestSwitch(t, func(jc *jitTestConfig) {
				jc.noInline = func(fn *bytecode.Function) bool {
					return fn.Name == "get sum" || fn.Name == "named" || fn.Name == "get count" || fn.Name == "set total"
				}
			})
			rounds := []string{}
			for range 8 {
				rounds = append(rounds, `sums(40)`, `others(40)`, `bad(20)`)
			}
			r := jitAgreesForTest(t, accessorCallSetup, rounds...)
			defer holdCollectorNow(t)()
			for _, c := range []struct{ loop, run, want string }{{"sums", "sums(2000)", "13000"}, {"others", "others(2000)", "30000"}} {
				e := jitNativeForTest(t, r, c.loop)
				before := e.ssaStats
				if v, err := r.Run(compileForTest(t, "''+"+c.run)); err != nil || v.String().Go() != c.want {
					t.Fatalf("%s = %s, %v; want %s", c.run, v.String().Go(), err, c.want)
				}
				st := e.ssaStats
				if st.goCalls != before.goCalls || st.hosts != before.hosts || st.guards != before.guards {
					t.Errorf("%s: %+v, before %+v; want no call of Go, no exit", c.run, st, before)
				}
			}
			jitAgreesForTest(t, accessorCallSetup, `sums(40)`, `bad(20)`, `others(40)`, `bad(40)`, `sums(60)`)
		})
	}
}
