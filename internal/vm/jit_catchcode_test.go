//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// catchCodeSetup's loops catch, in each turn, a throw from a call of Go:
// JSON.parse's SyntaxError and a read off undefined's TypeError, the
// caught value used after, the loop's sums carried round; cells' handler
// assigns a let an arrow made before the throw captured, which its code
// keeps in a cell of its own -- code that leaves such a throw to Go.
const catchCodeSetup = `
	function parses(n) { let s = 0; for (let i = 0; i < n; i++) { try { s += JSON.parse(i & 1 ? '{' : '{"v":' + i + '}').v } catch (e) { s += e.name.length } } return s }
	function reads(o, n) { let s = 0; for (let i = 0; i < n; i++) { try { s += o.p.q } catch (e) { s += e instanceof TypeError ? 1 : 1000 } } return s }
	function cells(n) { let s = 0; for (let i = 0; i < n; i++) { let k = i; const f = () => k; try { JSON.parse(i & 1 ? '{' : '1') } catch (e) { k += 100 } s += f() } return s }`

// A throw from a call of Go the frame's handler takes goes on at the
// handler in native code (abi.GoCallState's Catch, mir's catchStub): the
// frame written, the handler's register given what was thrown, the code
// going on at the handler's entry, as V8's optimized code does. It had
// left native code at each, Go catching it at the exit and entering the
// code again, and a loop whose every turn threw was judged slow and run
// by the interpreter. Every answer is the interpreter's, marking or not,
// and the loops run natively throughout: no exit, no run ended in the
// interpreter, the code kept.
func TestJITCatchInCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rounds := []string{}
			for range 10 {
				rounds = append(rounds, `parses(100)`, `reads({p: {q: 1}}, 20)`, `reads({}, 100)`, `cells(50)`)
			}
			r := jitAgreesForTest(t, catchCodeSetup, rounds...)
			defer holdCollectorNow(t)()
			for _, c := range []struct{ name, run, want string }{
				{"parses", "parses(200)", "11000"},
				{"reads", "reads({}, 200)", "200"},
			} {
				e := jitNativeForTest(t, r, c.name)
				cl := jitGlobalClosureForTest(r, c.name)
				v, _ := jitViewForTest(r, cl)
				before, interp := e.ssaStats, r.jit.interpreted
				if got, err := r.Run(compileForTest(t, "''+"+c.run)); err != nil || got.String().Go() != c.want {
					t.Fatalf("%s = %s, %v; want %s", c.run, got.String().Go(), err, c.want)
				}
				st := e.ssaStats
				if st.caught == before.caught || st.hosts != before.hosts || st.guards != before.guards || r.jit.interpreted != interp || !v.same(r, cl) {
					t.Errorf("%s: %+v, before %+v, %d runs ended in the interpreter; want its throws caught natively, no exit", c.run, st, before, r.jit.interpreted-interp)
				}
			}
		})
	}
}
