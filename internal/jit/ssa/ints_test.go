package ssa

import (
	"testing"

	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// int32Phis counts f's int32 phis, and its phis of doubles a loop's
// header has.
func int32Phis(f *Func) (ints, doubles int) {
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpPhi || !b.LoopHeader {
				continue
			}
			switch v.Type {
			case Int32:
				ints++
			case Float64:
				doubles++
			}
		}
	}
	return ints, doubles
}

// A loop's phi of doubles that only carries int32s is an int32 phi where
// its uses take it as one (intPhis, intPhiUses): Emscripten's counter and
// sums are; Crypto's am3 masks, x & 0x3fff and x >> 14, carried round its
// loop by its entry's phis for products with its digits, are not -- as
// int32s they had been converted to doubles at every digit, the V8
// suite's Crypto 27% slower. A sum read only after its loop is one too.
func TestIntPhisWhereTheyPay(t *testing.T) {
	for _, c := range []struct {
		src     string
		minInts int
		ints    bool
	}{
		{`function f(n){var j=0,a=1,b=2,s=0;for(j=0;(j|0)<(n|0);j=(j+1)|0){a=(a*1103+b)|0;b=(b^(a>>>7))|0;s=(s+(a&255)-(b>>29))|0}return s}`, 4, true},
		{`function f(t,w,x,n){var xl=x&0x3fff,xh=x>>14,c=0,i=0,j=0;while(--n>=0){var l=t[i]&0x3fff;var h=t[i++]>>14;var m=xh*l+h*xl;l=xl*l+((m&0x3fff)<<14)+w[j]+c;c=(l>>28)+(m>>14)+xh*h;w[j++]=l&0xfffffff}return c}`, 0, false},
		{`function f(n){var s=0;for(var i=0;i<n;i=(i+1)|0)s=(s+i)|0;return s}`, 2, true},
	} {
		f := lowerJS(t, c.src)
		Optimize(f)
		ints, _ := int32Phis(f)
		if c.ints && ints < c.minInts || !c.ints && ints != 0 {
			t.Errorf("%s: %d int32 phis, want %v (at least %d):\n%s", c.src, ints, c.ints, c.minInts, f)
		}
	}
}

// An int32 phi's number that a value after its loop takes as a value -- a
// sum stored, or returned -- is boxed there, not in the loop's header at
// every turn: the header keeps only the box its state names, which exits
// make (sinkLoopBoxes). A sum
// stored to a global after its loop, (s + e) | 0 of what a catch caught,
// had been converted and boxed at every turn: the micro-benchmarks
// try_throw, closure_counter and try_catch 15, 14 and 4% slower.
func TestIntPhiBoxesSunk(t *testing.T) {
	for _, src := range []string{
		`function f(n,o){var s=0;for(var i=0;(i|0)<(n|0);i=(i+1)|0)s=(s+i)|0;o[0]=s;return 0}`,
		`function f(n){var s=0;for(var i=0;(i|0)<(n|0);i=(i+1)|0)s=(s+i)|0;return s}`,
		`function f(n,e,o){var s=0;for(var j=0;j<n;j++)s=(s+e)|0;o[0]=s;return 0}`,
	} {
		f := lowerJS(t, src)
		Optimize(f)
		if ints, _ := int32Phis(f); ints == 0 {
			t.Fatalf("%s: no int32 phi:\n%s", src, f)
		}
		ids := 0
		for _, b := range f.Blocks {
			ids = max(ids, b.ID+1)
		}
		index := make([]int, ids)
		for i, b := range f.Blocks {
			index[b.ID] = i
		}
		// inLoop is whether b is in the loop of the header h.
		inLoop := func(h, b *Block) bool {
			in := make([]bool, len(f.Blocks))
			loopBlocks(h, in, index)
			return in[index[b.ID]]
		}
		check := func(u, a *Value) {
			h := a.Block
			if !h.LoopHeader || inLoop(h, u.Block) {
				return
			}
			conv := func(v *Value) bool {
				return v.Op == OpI32ToF64 && v.Args[0].Op == OpPhi && v.Args[0].Block == h && v.Args[0].Type == Int32
			}
			if a.Op == OpBoxF64 && len(a.Args) == 1 && conv(a.Args[0]) {
				t.Errorf("%s: v%d in b%d takes v%d of the header b%d:\n%s", src, u.ID, u.Block.ID, a.ID, h.ID, f)
			}
		}
		for _, b := range f.Blocks {
			for _, u := range b.Values {
				for _, a := range u.Args {
					check(u, a)
				}
			}
			if c := b.Control; c != nil {
				check(&Value{Block: b}, c)
			}
		}
	}
}

// lowerRegJS is the first function src declares, compiled to register
// code, lowered and built.
func lowerRegJS(t *testing.T, src string) *Func {
	t.Helper()
	fn := compiledFunctionForTest(t, src)
	if fn.Reg == nil {
		t.Fatalf("%s: no register code", src)
	}
	p, err := jitcompile.LowerReg(fn.RegCode(), fn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A loop's int32 read after it reaches the exit of register code's loop,
// which tests at its end, through a phi of the turn's value and the one
// before the first turn: that phi is an int32 phi, converted after it,
// not its argument converted in the test's block at every turn: the
// micro-benchmark try_catch's sum, read after its loop, had cost one
// conversion a turn.
func TestIntPhiAtRotatedLoopExit(t *testing.T) {
	for _, src := range []string{
		`function f(n){var i,s=0;for(i=0;i<n;i++){try{s=(s+i)|0}catch(e){s=0}}g=s;return n}`,
		`function f(n){var s=0;for(var i=0;i<n;i++)s=(s+i)|0;return s}`,
	} {
		f := lowerRegJS(t, src)
		Optimize(f)
		if err := Check(f); err != nil {
			t.Fatal(err)
		}
		ids := 0
		for _, b := range f.Blocks {
			ids = max(ids, b.ID+1)
		}
		index := make([]int, ids)
		for i, b := range f.Blocks {
			index[b.ID] = i
		}
		in := make([]bool, len(f.Blocks))
		for _, h := range f.Blocks {
			if h.LoopHeader {
				loopBlocks(h, in, index)
			}
		}
		// Each conversion in a loop's block has a use there.
		used := map[*Value]bool{}
		for _, b := range f.Blocks {
			for _, u := range b.Values {
				for i, a := range u.Args {
					at := b
					if u.Op == OpPhi {
						at = b.Preds[i]
						if !in[index[b.ID]] {
							continue
						}
					}
					if in[index[at.ID]] {
						used[a] = true
					}
				}
			}
			if c := b.Control; c != nil && in[index[b.ID]] {
				used[c] = true
			}
		}
		for _, b := range f.Blocks {
			if !in[index[b.ID]] {
				continue
			}
			for _, v := range b.Values {
				if v.Op == OpI32ToF64 && !used[v] {
					t.Errorf("%s: v%d in the loop's b%d is used only after the loop:\n%s", src, v.ID, b.ID, f)
				}
			}
		}
	}
}
