//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// leafCorpus is calls of leaves -- methods and plain calls, of no argument
// to three, with numbers, booleans and references live across them, in
// loops and out of them -- which leaves has every call make as one of its
// own (ssa.GoLeaf): methods' calls, or, for plain, plain calls.
var leafCorpus = []struct {
	src   string
	plain bool
}{
	{src: `function f(s,n){let h=0;for(let i=0;i<n;i++)h=(h*31+s.codePointAt(i&7))|0;return h}`},
	{src: `function f(s,n){let h=0,x=1.5;for(let i=0;i<n;i++){h+=s.at(i)+x;x*=1.01}return h}`},
	{src: `function f(m,n){let h=0,b=n>3;for(let i=0;i<n;i++){h+=m.get(i&3);if(b)h++}return h}`},
	{src: `function f(a,b,n){let h=0;for(let i=0;i<n;i++)if(isNaN(a+i))h++;else h+=parseFloat(b);return h}`, plain: true},
	{src: `function f(s,t,n){let h='';for(let i=0;i<n;i++)h=s.concat(t,i,h.length);return h}`},
	{src: `function f(s,o){const u=s.toUpperCase(),l=s.startsWith(o,1,2);return u.length+l+o.x}`},
}

// leaves is globals whose calls are each a leaf's (ssa.LeafFeedback), a
// call of Go's before that (a GoCall site), of p: a method's or, plain, a
// plain one's.
type leaves struct {
	globals
	p     *ir.Program
	plain bool
}

func (leaves) GoCalls() bool { return true }
func (l leaves) NativeCalls(pc int) []ssa.CallSite {
	if pc+1 >= len(l.p.Maps) {
		return nil
	}
	argc := l.p.Maps[pc].Depth - l.p.Maps[pc+1].Depth
	if !l.plain {
		argc--
	}
	if argc < 0 {
		return nil
	}
	return []ssa.CallSite{{Go: ssa.GoCall, Argc: argc, Method: !l.plain, ThisSlot: -1}}
}
func (leaves) Leaf(pc int) (uintptr, bool) {
	return 0x86000 + uintptr(pc)*16, true
}

// LeafKind has the leaves at odd PCs codePointAt, those at even PCs at,
// the kinds native code makes itself where the call allows (ssa.LeafKind).
func (leaves) LeafKind(fn uintptr) ssa.LeafKind {
	if (fn-0x86000)/16%2 == 1 {
		return ssa.LeafCodePointAt
	}
	return ssa.LeafAt
}

// The calls of leaves are made through the VM's routine for them
// (abi.Encoding's LeafFast) by code that keeps the rules on both targets,
// each call's description naming its leaf (abi.GoCallState's Leaf); and
// through CallGo, as before, where the encoding has no such routine, the
// other calls of Go through GoFast or the code's routine of them alike. The
// native forms of codePointAt and at, before their calls, keep them too.
func TestMirLeafCalls(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000, CallGo: 0x72000}
	for _, fast := range []uint64{0x73000, 0} {
		e := enc
		e.LeafFast = fast
		if fast != 0 {
			// And every other call of Go through the VM's routine.
			e.GoFast = fast + 0x100
		}
		named, sites := 0, 0
		natives := map[ssa.Op]int{}
		for _, c := range leafCorpus {
			src := c.src
			for i, p := range lowered(t, src) {
				f, err := ssa.BuildWith(p, leaves{p: p, plain: c.plain})
				if err != nil {
					continue
				}
				f.CallEntry = true
				ssa.Optimize(f)
				for _, b := range f.Blocks {
					for _, v := range b.Values {
						if calls := v.Calls(); len(calls) == 1 && calls[0].Go == ssa.GoLeaf {
							sites++
						}
						if v.Op == ssa.OpStringCodePointOr || v.Op == ssa.OpStringAtOr {
							natives[v.Op]++
						}
					}
				}
				for _, arch := range arches {
					compile := mir.CompileAMD64
					if arch == "arm64" {
						compile = mir.CompileARM64
					}
					name := fmt.Sprintf("%s (%d, LeafFast %#x), %s", src, i, fast, arch)
					mc, err := compile(f, e)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if err := check(arch, mc.Bytes[:mc.Text]); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					for _, st := range mc.GoCalls {
						if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf {
							if st.Leaf == 0 {
								t.Errorf("%s: a leaf's call names no leaf", name)
							}
							named++
						}
					}
				}
			}
		}
		if sites < len(leafCorpus) || named < 2*len(leafCorpus) {
			t.Errorf("LeafFast %#x: %d leaves' calls built, %d compiled, want each function's", fast, sites, named)
		}
		if natives[ssa.OpStringCodePointOr] == 0 || natives[ssa.OpStringAtOr] == 0 {
			t.Errorf("native forms built: %v", natives)
		}
	}
}
