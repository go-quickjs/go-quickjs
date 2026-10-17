//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
)

// Loose equality of two booleans is strict equality, answered natively:
// x == false of a boolean x had left for Go each time x was true, the
// words differing -- Box2D's e.IsAwake() == false and o == true, some
// 35,000 exits a run. A boolean against what converts -- a number, a
// string, an object -- is still Go's, and against null or undefined
// unequal; each answer is the interpreter's, as a value and as a branch.
func TestJITLooseBooleans(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function flags(n){let out='';for(let i=0;i<n;i++){const a=(i&1)==1,b=(i&2)==2;
			out+=(a==b?1:0)+''+(a!=b?1:0)+(a==false?1:0)+(b==true?1:0)+(a!=true?1:0)+((a==b)+0)+((a!=false)+0)+';'}return out}
		function mixed(xs){let out='';for(let i=0;i<xs.length;i++){const x=xs[i];
			out+=(x==true?1:0)+''+(x==false?1:0)+(true==x?1:0)+(x!=false?1:0)+';'}return out}
		function count(n){let k=0;for(let i=0;i<n;i++){const a=(i&1)==1;if(a==false)k++;if(true==a)k+=2}return k}`
	cases := []string{`flags(40)`, `mixed([true,false,1,0,"1","",null,undefined,{},[],[0],2,NaN,"true"])`,
		`String(count(1000))`, `flags(7)`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
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
				t.Fatalf("%s: %v", c, err)
			}
			if got, w := gv.String().Go(), wv.String().Go(); got != w {
				t.Fatalf("round %d, %s: got %s, interpreter %s", round, c, got, w)
			}
		}
	}
	// A loop of booleans compared loosely runs natively throughout.
	cl := r.global.getOwn(r.atoms.intern("count")).value().Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil {
		t.Fatalf("count was not compiled: %+v", e)
	}
	before := e.ssaStats
	if v, err := r.Run(compileForTest(t, `count(5000)`)); err != nil || v.Number() != 7500 {
		t.Fatal(v, err)
	}
	if st := e.ssaStats; st.work-before.work < 5000 || st.hosts != before.hosts || st.guards != before.guards {
		t.Fatalf("count(5000): %d exits to Go, %d guard failures, %d work; before %+v", st.hosts-before.hosts, st.guards-before.guards, st.work-before.work, before)
	}
}
