//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"testing"
)

// A write that adds its property, and a read that finds its property, along
// a prototype chain deeper than a cache holds are noted by the existing
// tiers (jitNoteDeepAdd, jitNoteDeepRead), and code compiled after they ran
// them makes them natively from its first compile: deep's objects are of
// three classes whose chains are four to six deep, as DeltaBlue's
// constraints' are. Before, the adds were learned one exit, and one
// compile, at a time, and the reads, which no cache could hold, left
// native code at every one. Each answer is the interpreter's.
func TestJITSSADeepChainsSeenByTheInterpreter(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Native code adds no property while the collector marks, leaving the
	// add to Go, which would count as its exits.
	runtime.GC()
	defer holdCollectorNow(t)()
	setup := `function K0(){} function K1(){} K1.prototype=Object.create(K0.prototype);
		function K2(){} K2.prototype=Object.create(K1.prototype);
		function K3(){} K3.prototype=Object.create(K2.prototype);
		function K4(){} K4.prototype=Object.create(K3.prototype);
		K0.prototype.w=5;
		var ks=[K2,K3,K4,K2],objs=[];
		function deep(n){let t=0;for(let i=0;i<n;i++){const o=objs[i];o.strength=i;t=(t+o.strength+o.w)|0}return t}`
	// The objects, made at the top level, whose code is compiled for its
	// loop and again for what it inlines: deep's compiles are what is
	// counted. deep is called from a script of its own, which, with no
	// loop, the interpreter runs: compiled top-level code would call it
	// natively before the existing tiers had run it.
	src := `objs=[];for(var i=0;i<6000;i++)objs.push(new ks[i&3]())`
	call := `String(deep(6000))`
	want := newTestRuntime(t, Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := newTestRuntime(t, Config{JIT: true})
	jitDefaultsForTest(r)
	defer func() { r.Close(); r.ReleaseClosed() }()
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2 {
		var wv, gv Value
		for _, src := range []string{src, call} {
			var err error
			if wv, err = want.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			if gv, err = r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
		e := jitCodeForTest(r, "deep")
		if e == nil || e.entrySlow {
			t.Fatalf("round %d: deep did not run natively", i)
		}
		// The compiles again of top-level code.
		top := uint64(0)
		for k, x := range r.jit.regCache {
			if fn := k.Value(); fn != nil && fn.TopLevel && x.life != nil {
				top += uint64(x.life.reopts)
			}
		}
		if n := r.jit.reoptimized - top; n != 0 {
			t.Fatalf("round %d: deep was compiled %d times more", i, n)
		}
		if n := e.ssaStats.hosts + e.ssaStats.guards; n != 0 {
			t.Fatalf("round %d: deep left native code %d times", i, n)
		}
	}
}

// Code called natively that leaves too often is compiled again only if its
// exits taught it something (jitLearning): w's delete of a proxy's
// property, whose trap may run anything, is Go's
// at every call, which no compile changes, and native callers stop
// calling it at once, not after three compiles of the same code. Each
// answer is the interpreter's. Under QJS_JIT_STRESS's budget of three back
// edges -- w's two, run's one -- every native call of w polls at its start
// and is unwound there, a pause, not its leaving: its host exits after
// the poll are what its native callers count (jitRegFinishLevel).
func TestJITSSAUnwoundCompiledAgainOnlyForWhatItLearned(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitNativeCountsForTest(t)
	// While the collector marks, Go makes native calls, which would count
	// as calls leaving native code.
	runtime.GC()
	defer holdCollectorNow(t)()
	setup := `var px=new Proxy({},{});function w(x){let s=0;for(let i=0;i<2;i++)s+=(delete px.y?15:0)*x;return s}
		function run(n){let t=0;for(let i=0;i<n;i++)t+=w(i+0.25);return t}`
	want := newTestRuntime(t, Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := newTestRuntime(t, Config{JIT: true})
	// Compiled on the spot, as the counts below are written for, not in
	// the background (QJS_JIT_STRESS's background), whose code comes
	// checks later.
	r.jitStress.background = false
	defer func() { r.Close(); r.ReleaseClosed() }()
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range []string{`String(run(3000))`, `String(run(3000))`} {
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	e := jitCodeForTest(r, "w")
	if e == nil {
		t.Fatal("w has no code")
	}
	if e.nativeBackoff == 0 {
		t.Fatal("w's native callers still call it")
	}
	if e.unwindReopts != 0 {
		t.Fatalf("w was compiled %d times more for leaving", e.unwindReopts)
	}
}

// A chain of additions whose first meets a string -- tag + s(a) + s(b) +
// s(c), each a string, then file.text += e -- is learned whole, not each
// addition at a deoptimization of its own and a compile after it: each
// is marked where the interpreter makes it of a string (regCode's
// stringAdds), the rest of the chain after the first one's failure in
// compiled code too, which the interpreter runs. PDF.js's
// createTableEntry had run out of compiles so. Each answer is the
// interpreter's.
func TestJITStringAddChainsLearnedAtOnce(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function s(n) { return String.fromCharCode(65 + (n & 15)) }
		function entry(file, tag, a, b, c) {
			let w = 0
			for (let i = 0; i < 40; i++) w = (w * 31 + i) | 0
			const e = tag + s(a) + s(b) + s(c)
			file.text += e
			return w
		}`
	jitDifferential(t, setup+`
		const file = { text: '' }
		for (let k = 0; k < 30; k++) entry(file, 'T' + k, k, k + 1, k + 2)
		entry(file, 7, 1, 2, 3)
		file.text`)
	r := newTestRuntime(t, Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitFewCallsForTest(r)
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	if _, err := r.Run(compileForTest(t, setup+`; const file = { text: '' }`)); err != nil {
		t.Fatal(err)
	}
	for k := range 40 {
		if _, err := r.Run(compileForTest(t, fmt.Sprintf(`entry(file, 'T%d', %d, 2, 3)`, k, k))); err != nil {
			t.Fatal(err)
		}
	}
	jitNativeForTest(t, r, "entry")
	guards := r.JITStats().Guards
	if guards > 2 {
		t.Errorf("entry's chain failed %d guards, learned one addition at a time", guards)
	}
	v, err := r.Run(compileForTest(t, `(() => { for (let k = 0; k < 20; k++) entry(file, 'U', k, 1, 2); return file.text.length })()`))
	if err != nil || v.Number() != 310 {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := r.JITStats().Guards - guards; n != 0 {
		t.Errorf("%d guards failed in 20 calls once warm", n)
	}
}
