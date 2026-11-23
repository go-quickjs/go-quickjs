package ssa

import (
	"testing"

	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

type sharedCallFeedback struct {
	goCallsFeedback
	sites []CallSite
}

func (f sharedCallFeedback) NativeCalls(int) []CallSite { return f.sites }

func TestBuildPreservesCallFeedback(t *testing.T) {
	fn := compiledFunctionForTest(t, "function f(g) { var x = g(); return x + 1 }")
	p, err := jitcompile.LowerRegInline(fn.RegCode(), fn)
	if err != nil {
		t.Fatal(err)
	}
	for _, coerce := range []bool{true, false} {
		fb := sharedCallFeedback{sites: []CallSite{{Go: GoCall, ThisSlot: -1, Coerce: coerce}}}
		for range 2 {
			f, err := BuildWith(p, fb)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			for _, blk := range f.Blocks {
				for _, v := range blk.Values {
					if v.Op != OpCall || len(v.Calls()) != 1 || v.Calls()[0].Go != GoCall {
						continue
					}
					calls++
					site := v.Calls()[0]
					if site.Coerce || fb.sites[0].Coerce != coerce {
						t.Fatalf("coercion: call %v, feedback %v, want false and %v", site.Coerce, fb.sites[0].Coerce, coerce)
					}
					if !coerce && site != &fb.sites[0] {
						t.Fatal("an unchanged call description was copied")
					}
				}
			}
			if calls != 1 {
				t.Fatalf("got %d calls, want one", calls)
			}
		}
	}
}
