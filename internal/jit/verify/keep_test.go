//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// props is globals whose property reads and writes have each met objects
// of one shape, which has the property at an index by its key: so that
// values read from cells are kept across the stores and calls after them
// (ssa's keep.go).
// The property's index is its key's, in p.
type props struct {
	globals
	p *ir.Program
}

func (props) Accessor(int) (ssa.AccessorSite, bool)       { return ssa.AccessorSite{}, false }
func (props) StringProperty(int) (ssa.PropertySite, bool) { return ssa.PropertySite{}, false }

func (f props) Property(pc int) (ssa.PropertySite, bool) {
	key := f.p.Code[pc].Key
	return ssa.PropertySite{Key: key, Shape: 0x20000, Index: int32(key % 4)}, true
}

// keepCorpus reads properties and stores them back while what it read is
// still used: each store keeps what it may overwrite.
var keepCorpus = []string{
	`function f(o,n){for(let i=0;i<n;i++){const p=o.a,q=o.b,r=o.c;o.a=q;o.b=r;o.c=p;o=p.d}return o}`,
	`function f(o,n){for(let i=0;i<n;i++){const p=o.a;o.a=o.b;o.b=p;o=p}return o}`,
	`function f(o,p,n){for(let i=0;i<n;i++){const a=o.x,b=p.x;o.x=b;p.x=a;o=a}return o}`,
	`function f(o,g){const a=o.x,b=o.y;g(a);return a.z+b.z}`,
}

// stretch is the instructions of the stretch of code mark i begins.
func stretch(mc *mir.Code, i int) []x86asm.Inst {
	end := mc.Text
	if i+1 < len(mc.Marks) {
		end = min(end, mc.Marks[i+1].Off)
	}
	var out []x86asm.Inst
	for off := mc.Marks[i].Off; off < end; {
		in, err := x86asm.Decode(mc.Bytes[off:end], 64)
		if err != nil {
			return out
		}
		out = append(out, in)
		off += in.Len
	}
	return out
}

// spillAccess reports an instruction that reads or writes a spill slot.
func spillAccess(in x86asm.Inst) bool {
	if in.Op != x86asm.MOV {
		return false
	}
	for _, a := range in.Args[:2] {
		if m, ok := a.(x86asm.Mem); ok && m.Base == x86asm.RDI && m.Disp >= int64(abi.OffSpill) && m.Disp < int64(abi.OffSpill)+8*abi.SpillSlots {
			return true
		}
	}
	return false
}

// The keeps a store makes (ssa's OpKeep) are cheap (mir's keep): a run of
// them tests the write-barrier flag once, at its first; each stores its
// value's two words and makes its cell's address, no word moved through an
// SSE register; and the pointer word read before (OpKeepRef) is a load
// where it comes from a cell. A keep had taken thirteen instructions, a
// keepref six.
func TestMirKeepsCheap(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000}
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	keeps, runs, refs, fused := 0, 0, 0, 0
	for _, src := range keepCorpus {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, props{p: p})
			if err != nil {
				continue
			}
			ssa.Optimize(f)
			mc, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			byID := map[string]*ssa.Value{}
			for _, b := range f.Blocks {
				for _, v := range b.Values {
					byID[fmt.Sprintf("v%d", v.ID)] = v
				}
			}
			first := true
			for k, m := range mc.Marks {
				ins := stretch(mc, k)
				tag := strings.Fields(m.Tag)
				if len(tag) != 2 || tag[0][0] != 'v' {
					first = true
					continue
				}
				switch tag[1] {
				case "keep":
					keeps++
					n, flags := 0, 0
					for _, in := range ins {
						if in.Op == x86asm.MOVQ {
							t.Errorf("%s (%d): %s moves a word through an SSE register: %v", src, i, m.Tag, in)
						}
						if in.Op == x86asm.MOVZX {
							// The barrier flag's byte.
							flags++
						}
						if !spillAccess(in) {
							n++
						}
					}
					want := 3
					if v := byID[tag[0]]; len(v.Args) == 1 {
						// Its pointer word read itself, as its ref had
						// (ssa's fuseKeepRefs): a load, tested first where
						// the cell is a phi's, which may be none.
						fused++
						want++
						if x := v.Args[0]; x.Shadow != nil && x.Shadow.Op == ssa.OpPhi {
							want += 3
						}
					}
					if first {
						runs++
						want += 4
						if flags != 1 {
							t.Errorf("%s (%d): %s, a run's first keep, reads the barrier flag %d times:\n%s", src, i, m.Tag, flags, mc.Listing)
						}
					} else if flags != 0 {
						t.Errorf("%s (%d): %s, past a run's first keep, reads the barrier flag", src, i, m.Tag)
					}
					if n > want {
						t.Errorf("%s (%d): %s takes %d instructions, past its spill slots' loads and stores, not %d: %v", src, i, m.Tag, n, want, ins)
					}
					first = false
					continue
				case "keepref":
					refs++
					n := 0
					for _, in := range ins {
						if !spillAccess(in) {
							n++
						}
					}
					// The cell's pointer word into the value's register, or
					// into scratch and on to its spill slot; tested first
					// where the cell is a phi's, which may be none.
					want := 2
					if x := byID[tag[0]].Args[0]; x.Shadow != nil && x.Shadow.Op == ssa.OpPhi {
						want = 5
					}
					if n > want {
						t.Errorf("%s (%d): %s takes %d instructions, not %d: %v", src, i, m.Tag, n, want, ins)
					}
				}
				first = true
			}
		}
	}
	if keeps == 0 || runs == 0 || fused == 0 || runs == keeps {
		t.Fatalf("keeps %d in %d runs, %d reading their pointer words, %d keeprefs compiled", keeps, runs, fused, refs)
	}
	t.Logf("%d keeps in %d runs, %d reading their pointer words, %d keeprefs", keeps, runs, fused, refs)
}

// keepRenameCorpus keeps values at stores in several blocks of one
// function, each value kept again at each store while it is still used:
// a use, an operand's or a frame state's slot, takes the copy kept last
// before it in its block (ssa's reconstruct), or what reaches the block.
var keepRenameCorpus = []string{
	`function f(o,n){let s=0;for(let i=0;i<n;i++){const p=o.a,q=o.b;o.a=q;s+=p.v;if(i&1){o.b=p;s+=q.v}else{o.c=q;s+=p.w}o.d=p;s+=p.v+q.v}return s}`,
	`function f(o,g){const a=o.x,b=o.y;o.x=b;g(a);o.y=a;if(a.k){o.z=b;g(b)}o.w=a;return a.z+b.z}`,
	`function f(o){const p=o.a,q=o.b;o.c=q;o.d=p;o.e=q;o.a=p;return p.v+q.v}`,
	`function f(o,x){const p=o.a;o.a=x;const s=p.v;o.a=s;return p.v+s}`,
	`function f(o,g){const p=o.a;g(p);g(p);o.b=p;g(p);return p.v}`,
}

// Each use of a kept value takes the last of its copies (OpKept) made
// before the use in the use's block, or, if none is, the one that reaches
// the block: a use reading an earlier copy, past a store that may have
// written the cell that copy's pointer word is read from, would read
// another value's.
func TestSSAKeptUsesTakeTheLastCopy(t *testing.T) {
	kept := 0
	for _, src := range append(append([]string(nil), keepCorpus...), keepRenameCorpus...) {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, calls{props{p: p}})
			if err != nil {
				continue
			}
			ssa.Optimize(f)
			if err := ssa.Check(f); err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			// What each copy is a copy of, followed to the first value.
			origin := func(v *ssa.Value) *ssa.Value {
				for v != nil && v.Op == ssa.OpKept {
					v = v.Args[1]
				}
				return v
			}
			for _, b := range f.Blocks {
				// The copy of each value made last so far in the block.
				last := map[*ssa.Value]*ssa.Value{}
				check := func(at string, x *ssa.Value) {
					if x == nil || x.Type != ssa.Tagged {
						return
					}
					if l, ok := last[origin(x)]; ok && l != x {
						t.Errorf("%s (%d): b%d %s takes v%d, not v%d, its value's last copy before it", src, i, b.ID, at, x.ID, l.ID)
					}
				}
				for _, v := range b.Values {
					if v.Op != ssa.OpPhi {
						for _, a := range v.Args {
							if v.Op != ssa.OpKept && v.Op != ssa.OpKeep && v.Op != ssa.OpKeepRef {
								check(fmt.Sprintf("v%d's operand", v.ID), a)
							}
						}
						if v.State != nil {
							for _, x := range v.State.Slots {
								check(fmt.Sprintf("v%d's state", v.ID), x)
							}
						}
					}
					if v.Op == ssa.OpKept {
						kept++
						last[origin(v)] = v
					}
				}
			}
		}
	}
	if kept == 0 {
		t.Fatal("the corpus keeps nothing")
	}
}

// keepSwapCorpus swaps values read from cells round a loop: at the store
// that keeps both, one's source is the keep cell the other's keep writes,
// the copy kept there the last time round.
var keepSwapCorpus = []string{
	`function f(o,n){let p=o.a,q=o.b;for(let i=0;i<n;i++){o.a=q;o.b=p;const t=p;p=q;q=t}return p.v+q.v}`,
	`function f(o,n){let p=o.a,q=o.b;for(let i=0;i<n;i++){const t=i&1?p:q;o.a=p;o.b=q;p=t}return p.v+q.v}`,
	`function f(o,g,n){let p=o.a,q=o.b;for(let i=0;i<n;i++){g(p,q);const t=p;p=q;q=t}return p.v+q.v}`,
}

// A keep reads the pointer word it copies itself (ssa's fuseKeepRefs)
// unless another keep of its store writes the cell it is read from before
// it: there it is read first, with the store's other pointer words
// (OpKeepRef), before any keep cell is written.
func TestSSAKeepsReadTheirPointerWords(t *testing.T) {
	count := func(srcs []string) (keeps, refs int) {
		for _, src := range srcs {
			for i, p := range lowered(t, src) {
				f, err := ssa.BuildWith(p, calls{props{p: p}})
				if err != nil {
					continue
				}
				ssa.Optimize(f)
				if err := ssa.Check(f); err != nil {
					t.Fatalf("%s (%d): %v", src, i, err)
				}
				for _, b := range f.Blocks {
					for at, v := range b.Values {
						switch {
						case v.Op == ssa.OpKeep:
							keeps++
						case v.Op == ssa.OpKeepRef:
							refs++
							// Its keep's value's source is a keep cell another
							// keep between them writes.
							var k *ssa.Value
							written := map[int]bool{}
							for _, w := range b.Values[at+1:] {
								if w.Op == ssa.OpKeep && len(w.Args) == 2 && w.Args[1] == v {
									k = w
									break
								}
								if w.Op == ssa.OpKeep {
									written[w.Index] = true
								}
							}
							if k == nil {
								t.Fatalf("%s (%d): v%d is no keep's ref", src, i, v.ID)
							}
							conflict, seen := false, map[*ssa.Value]bool{}
							var walk func(s *ssa.Value)
							walk = func(s *ssa.Value) {
								if s == nil || seen[s] {
									return
								}
								seen[s] = true
								switch s.Op {
								case ssa.OpKeep:
									conflict = conflict || written[s.Index]
								case ssa.OpPhi:
									for _, a := range s.Args {
										walk(a)
									}
								}
							}
							walk(k.Args[0].Shadow)
							if !conflict {
								t.Errorf("%s (%d): v%d keeps its ref, its source no cell its store's keeps write before it", src, i, k.ID)
							}
						}
					}
				}
			}
		}
		return keeps, refs
	}
	if keeps, refs := count(append(append([]string(nil), keepCorpus...), keepRenameCorpus...)); keeps == 0 || refs != 0 {
		t.Errorf("the keep corpora: %d keeps, %d refs, where every keep may read its own pointer word", keeps, refs)
	}
	if keeps, refs := count(keepSwapCorpus); keeps == 0 || refs == 0 {
		t.Errorf("the swaps: %d keeps, %d refs, where some read a cell another keep writes", keeps, refs)
	}
}
