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
	keeps, runs, refs := 0, 0, 0
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
	if keeps == 0 || runs == 0 || refs == 0 || runs == keeps {
		t.Fatalf("keeps %d in %d runs, %d keeprefs compiled", keeps, runs, refs)
	}
	t.Logf("%d keeps in %d runs, %d keeprefs", keeps, runs, refs)
}
