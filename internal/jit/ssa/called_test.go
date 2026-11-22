package ssa

import (
	"strings"
	"testing"

	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// The closures are analyzed, but their bodies have no feedback to inline.
type calledFeedback struct{ knowsNothing }

func (calledFeedback) CalledClosure(int) (MadeCallback, InlineSite, bool) {
	return MadeCallback{}, InlineSite{}, false
}

func (calledFeedback) CallShape(int) (int, bool, bool) { return 1, false, true }
func (calledFeedback) GoCalls() bool                   { return true }

func (calledFeedback) NativeCalls(int) []CallSite {
	return []CallSite{{Go: GoCall, Argc: 1, ThisSlot: -1}}
}

func closureSource(n int) string {
	return `function f(n){let s=0;for(let i=0;i<n;i++){` +
		strings.Repeat(`{let g=(x)=>x+n;s+=g(s);}`, n) + `}return s}`
}

func TestCalledClosuresWorkspace(t *testing.T) {
	var w Workspace
	for _, n := range []int{32, 1, 16, 32} {
		fn := compiledFunctionForTest(t, closureSource(n))
		p, err := jitcompile.LowerReg(fn.RegCode(), fn)
		if err != nil {
			t.Fatal(err)
		}
		want, err := BuildWith(p, calledFeedback{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := BuildIn(&w, p, calledFeedback{})
		if err != nil {
			t.Fatal(err)
		}
		closures := 0
		for _, block := range got.Blocks {
			for _, v := range block.Values {
				if v.Op == OpCall && len(v.Calls()) == 1 && v.Calls()[0].Go == GoClosure {
					closures++
				}
			}
		}
		if closures != n {
			t.Fatalf("fixture has %d closure calls, want %d", closures, n)
		}
		Optimize(want)
		Optimize(got)
		if err := Check(got); err != nil {
			t.Fatal(err)
		}
		if got.String() != want.String() {
			t.Fatalf("%d closures: workspace changes the optimized SSA", n)
		}
		w.Rewind()
	}
}
