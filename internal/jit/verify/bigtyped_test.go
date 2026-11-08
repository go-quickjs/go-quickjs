//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// bigTypedCorpus reads BigInt typed arrays' elements and sums and compares
// them as BigInts.
var bigTypedCorpus = []string{
	`function f(a,n){let s=0n;for(let i=0;i<n;i++){s+=a[i&7]}return s}`,
	`function f(a,b,n){let c=0;for(let i=0;i<n;i++){if(a[i&7]<b[i&3])c++}return c}`,
}

// bigTyped is feedback whose element reads met BigInt64Arrays, or
// BigUint64Arrays (unsigned) (ssa.BigElementFeedback), and whose
// operators met BigInts.
type bigTyped struct {
	globals
	unsigned bool
}

func (b bigTyped) TypedBigRead(int) (unsigned, ok bool) { return b.unsigned, true }
func (bigTyped) BigInt(int) bool                        { return true }
func (bigTyped) BigIntWide(int) bool                    { return false }
func (bigTyped) BigIntFailed(int) bool                  { return false }

// A BigInt typed array's element read natively (ssa.OpBigElemRead), as an
// Int64 or a Uint64, keeps the rules for each target.
func TestMirBigTypedRead(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000006, String: 0xFFF8000000000004, Symbol: 0xFFF8000000000005,
		ShortBigTag: 0xFFFA000000000000, ShortBigLowBits: 48, ShortBigHigh: 0x100000000,
		ShortBigWideTag: 0xFFF9000000000000, ShortBigWideHigh: 0x200000000,
		HeapBigTag: 0xFFF8800000000000, ObjectFlags: 2, ObjectClass: 1, ClassArray: 3,
		TypedElems: 0, TypedLength: 8, TypedEpoch: 16, TypedKind: 24,
		UndefinedCell: 0x60000, StoreRef: 0x70000, CallGo: 0x72000}
	reads := map[ssa.Type]int{}
	for i, src := range bigTypedCorpus {
		prog, err := parser.Parse(src, parser.Options{})
		if err != nil {
			t.Fatal(err)
		}
		top, err := compiler.Compile(prog, compiler.Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range top.Constants {
			if c.Kind != bytecode.ConstFunction {
				continue
			}
			p, err := jitcompile.LowerSSAWith(c.Fn, jitcompile.Hints{ElementReads: elementReads(c.Fn)})
			if err != nil {
				t.Fatalf("%s: %v", src, err)
			}
			f, err := ssa.BuildWith(p, bigTyped{unsigned: i == 1})
			if err != nil {
				t.Fatalf("%s: %v", src, err)
			}
			ssa.Optimize(f)
			for _, b := range f.Blocks {
				for _, v := range b.Values {
					if v.Op == ssa.OpBigElemRead {
						reads[v.Type]++
					}
				}
			}
			for _, arch := range arches {
				compile := mir.CompileAMD64
				if arch == "arm64" {
					compile = mir.CompileARM64
				}
				mc, err := compile(f, enc)
				if err != nil {
					t.Fatalf("%s, %s: %v", src, arch, err)
				}
				if err := check(arch, mc.Bytes[:mc.Text]); err != nil {
					t.Fatalf("%s, %s: %v", src, arch, err)
				}
			}
		}
	}
	if reads[ssa.Int64] < 1 || reads[ssa.Uint64] < 2 {
		t.Errorf("BigInt typed reads made natively: %v", reads)
	}
}

// elementReads are fn's element reads, by PC, lowered as reads of what the
// element holds (jitcompile.Hints' ElementReads).
func elementReads(fn *bytecode.Function) []int32 {
	var pcs []int32
	for pc, in := range fn.Code {
		if in.Op == bytecode.OpGetIndex {
			pcs = append(pcs, int32(pc))
		}
	}
	return pcs
}
