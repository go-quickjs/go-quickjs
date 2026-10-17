//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
)

// Math.random() is made natively, as V8's code takes it: the next number of
// the runtime's cache, which a call of Go fills again only when it is
// empty. Both tiers take from the one cache, in the same order -- a call
// whose number is unused takes one too -- so a runtime with the JIT draws
// the numbers one without it does from the same state, whichever tier
// makes each call. A Math.random replaced, or a method of another object
// named random, is called as before. The calls are made in native code,
// which calls Go once a cache: Math.random had been a call of Go each
// time, 50 ns, slower than the interpreter's 37.
func TestJITMathRandom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function draw(k){let out=[];for(let i=0;i<k;i++)out.push(Math.random());return out.join(',')}
		function sum(k){let s=0;for(let i=0;i<k;i++)s+=Math.random();return s}
		function unused(k){for(let i=0;i<k;i++)Math.random();return Math.random()}
		function index(k){const a=new Array(8).fill(0);for(let i=0;i<k;i++)a[(Math.random()*8)|0]++;return a.join(',')}
		function other(k){const m={random(){return 0.25}};let s=0;for(let i=0;i<k;i++)s+=m.random();return s}
		function replaced(k){const r=Math.random;Math.random=function(){return 2};try{return draw(k)}finally{Math.random=r}}`
	cases := []string{`draw(100)`, `String(sum(500))`, `String(unused(70))`, `draw(3)`, `index(300)`, `String(other(100))`,
		`replaced(5)`, `draw(130)`, `String(Math.random())`, `String(sum(64))`}
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		want := New(Config{})
		r := jitRuntimeForTest(t, Config{JIT: true})
		for _, rt := range []*Runtime{want, r} {
			seedRandomForTest(rt, 0x243f6a8885a308d3, 0x13198a2e03707344)
			if _, err := rt.Run(compileForTest(t, setup)); err != nil {
				t.Fatal(err)
			}
		}
		for round := 0; round < 3; round++ {
			for _, c := range cases {
				wv, err := want.Run(compileForTest(t, c))
				if err != nil {
					t.Fatal(err)
				}
				gv, err := r.Run(compileForTest(t, c))
				if err != nil {
					t.Fatalf("marking %v, %s: %v", marking, c, err)
				}
				if got, w := gv.String().Go(), wv.String().Go(); got != w {
					t.Fatalf("marking %v, round %d, %s: got %.200s, interpreter %.200s", marking, round, c, got, w)
				}
			}
		}
		want.Close()
		want.ReleaseClosed()
		// sum(6400) runs natively, with no call of a function through Go
		// (jitGoCall), which Math.random had been at each call, and never
		// leaves: its cache's refills are calls of Go of an operation, as
		// a Math function's are, which goCalls does not count.
		cl := r.global.getOwn(r.atoms.intern("sum")).value().Object().fn().closure
		e := r.jit.hint(cl)
		if e == nil || e.ssa == nil {
			t.Fatalf("marking %v: sum was not compiled: %+v", marking, e)
		}
		before := e.ssaStats
		if v, err := r.Run(compileForTest(t, `sum(6400) > 0`)); err != nil || !v.Truthy() {
			t.Fatal(v, err)
		}
		st := e.ssaStats
		if r.jit.hint(cl) != e || st.entries == before.entries || st.work-before.work < 6400 {
			t.Fatalf("marking %v: sum(6400) did not run natively: %+v, before %+v", marking, st, before)
		}
		if made := st.goCalls - before.goCalls; made != 0 {
			t.Errorf("marking %v: 6400 calls of Math.random made %d calls through Go", marking, made)
		}
		if left := st.hosts - before.hosts; left != 0 {
			t.Errorf("marking %v: 6400 calls of Math.random left native code %d times", marking, left)
		}
	}
}
