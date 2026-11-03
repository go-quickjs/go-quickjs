//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// mathPlainSetup's functions call Math's functions kept in bindings of
// their own, as a module does -- React's P = Math.clz32, called P(x) --
// captured by the closures (capLoop, and treeId, React's fa, which idLoop
// calls), or globals' (globalLoop); setP and gclz replace them.
const mathPlainSetup = `
var capLoop, setP, treeId, idLoop;
(function () {
	var P = Math.clz32, sq = Math.sqrt, mx = Math.max, im = Math.imul;
	capLoop = function (n) { let t = 0; for (let i = 0; i < n; i++) t += P(i + 1) + sq(i) + mx(i & 7, 3, i % 5) + (im(i, 0x9e3779b1) & 15); return t };
	setP = function (f) { P = f };
	treeId = function (a, c) { var f = 32 - P(a.id), e = 32 - P(c + 1) + f; return {id: 1 << f | c, overflow: e} };
	idLoop = function (n) { let t = 0, a = {id: 1, overflow: ""}; for (let i = 0; i < n; i++) { a = treeId(a, i & 1); if (a.id > 1 << 20) a = {id: 1, overflow: ""}; t += a.id } return t };
})();
var gclz = Math.clz32, gabs = Math.abs;
function globalLoop(n) { let t = 0; for (let i = 0; i < n; i++) t += gclz(i * 977) + gabs(i - 150); return t }
`

// A plain call of one of Math's functions, read from a captured binding
// or a global, is the call native code makes itself, as Math.clz32(x) is
// (jitFeedback.Intrinsic): warm, the loops make no call of Go and never
// leave their code, and none calls Go for a Math function of one number
// (GoMath); treeId, no loop's, is called natively, not through Go, which
// its calls of P had made it, for too little work between them. Each
// answer is Node's (v26.12.1) and the interpreter's; another function in
// the binding is called.
func TestJITMathPlainCall(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	rounds := [][2]string{{"capLoop(300)", "14383.235880500726"}, {"globalLoop(300)", "26952"}, {"idLoop(300)", "15728760"}}
	var srcs []string
	for range 6 {
		for _, rd := range rounds {
			srcs = append(srcs, rd[0])
		}
	}
	r := jitAgreesForTest(t, mathPlainSetup, srcs...)
	for _, rd := range rounds {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
		}
	}
	for _, name := range []string{"capLoop", "globalLoop"} {
		e := jitNativeForTest(t, r, name)
		for _, st := range e.ssa.GoCallStates() {
			if ssa.GoOp(st.Op&0xff) == ssa.GoMath {
				t.Errorf("%s's code calls Go for a Math function (GoMath)", name)
				break
			}
		}
		calls, hosts, entries := e.ssaStats.goCalls, e.ssaStats.hosts, e.ssaStats.entries
		if _, err := r.Run(compileForTest(t, name+"(300)")); err != nil {
			t.Fatal(err)
		}
		calls, hosts, entries = e.ssaStats.goCalls-calls, e.ssaStats.hosts-hosts, e.ssaStats.entries-entries
		if entries == 0 || calls != 0 || hosts != 0 {
			t.Errorf("%s ran natively %d times, made %d calls of Go with frames and left its code %d times", name, entries, calls, hosts)
		}
	}
	treeId := r.global.getOwn(r.atoms.intern("treeId")).value().Object().fn().closure
	if e := r.jit.entryFor(treeId.fn); e == nil || e.link == nil {
		t.Error("treeId has no code native callers call")
	} else {
		for range 20 {
			if _, err := r.Run(compileForTest(t, "idLoop(300)")); err != nil {
				t.Fatal(err)
			}
		}
		in, slow := e.link.in, e.slowCalls
		if _, err := r.Run(compileForTest(t, "idLoop(300)")); err != nil {
			t.Fatal(err)
		}
		if in, slow = e.link.in-in, e.slowCalls-slow; in < 300 || slow != 0 {
			t.Errorf("idLoop's 300 calls of treeId, warm: %d native, %d through Go", in, slow)
		}
	}
	for _, rd := range [][2]string{
		{"setP(x => 7); '' + capLoop(300)", "9081.235880500724"},
		{"gclz = x => -1; '' + globalLoop(300)", "22200"},
	} {
		v, err := r.Run(compileForTest(t, rd[0]))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
		}
	}
}
