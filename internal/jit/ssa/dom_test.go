package ssa

import "testing"

// A guard repeats one that dominates it, in any block, and goes; one in a
// sibling branch, or after the branches meet, does not repeat either arm's.
// a's tag is checked once where it is used before the branch, and three
// times where it is not: in each arm and after them.
func TestOptimizeMergesDominatedGuards(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{
		{`function f(a,n){let t=a*1,s=0;if(n>0){s=a*2}else{s=a*3}return s+t+a*4}`, 1},
		{`function f(a,n){let s=0;if(n>0){s=a*2}else{s=a*3}return s+a*4}`, 3},
	} {
		f := lowerJS(t, tc.src)
		Optimize(f)
		if err := Check(f); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op == OpUnboxF64 && v.Args[0].Op == OpLoadSlot && v.Args[0].Aux == 0 {
					n++
				}
			}
		}
		if n != tc.want {
			t.Errorf("%s: a's tag checked %d times, want %d:\n%v", tc.src, n, tc.want, f)
		}
	}
}

// A value a dominating guard found a number is initialized: the check a
// return makes of it goes. One found a number in one branch only is
// checked still.
func TestOptimizeDropsDominatedInitChecks(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{
		{`function f(a,n){n=a*2;return a}`, 0},
		{`function f(a,n){if(n>0){n=a*2}return a}`, 1},
		{`function f(a,n){if(n>0){return a}return a}`, 2},
	} {
		f := lowerJS(t, tc.src)
		Optimize(f)
		if err := Check(f); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op == OpCheckInit && v.Args[0].Op == OpLoadSlot && v.Args[0].Aux == 0 {
					n++
				}
			}
		}
		if n != tc.want {
			t.Errorf("%s: a checked initialized %d times, want %d:\n%v", tc.src, n, tc.want, f)
		}
	}
}
