//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// typeofCorpus makes typeof's answer, kept, and compares it with each
// type's name and a name of none.
var typeofCorpus = []string{
	`function f(a,n){let s=0;for(let i=0;i<n;i++){const t=typeof a[i&7];if(t==='number')s+=1;else if(t==='object')s+=2}return s}`,
	`function f(a,n){let s=0;for(let i=0;i<n;i++){const v=a[i&7];if(typeof v==='object')s++;if(typeof v!=='function')s+=2;` +
		`if(typeof v=='symbol')s+=4;if(typeof v==='bigint')s+=8;if(typeof v==='strnig')s+=16;if(typeof v!='undefined')s+=32}return s}`,
}

// typeofs is globals whose typeofs Go makes from native code (a GoCall
// site of TypeOf), and whose VM keeps a table of their names
// (ssa.TypeofFeedback), which native code loads the answer from.
type typeofs struct {
	globals
	fn *bytecode.Function
}

func (typeofs) GoCalls() bool                { return true }
func (typeofs) TypeofCells() (uintptr, bool) { return 0x94000, true }
func (t typeofs) NativeCalls(pc int) []ssa.CallSite {
	if t.fn.Code[pc].Op != bytecode.OpTypeOf {
		return nil
	}
	return []ssa.CallSite{{Go: ssa.GoCall, TypeOf: true, ThisSlot: -1}}
}

// typeof's answer made natively (ssa.OpTypeofCell) and its tests of every
// type (ssa.OpTypeIs) keep the rules, for each target.
func TestMirTypeof(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000006, String: 0xFFF8000000000004, Symbol: 0xFFF8000000000005,
		HeapBigTag: 0xFFFBFFFFFFFFFFFF, ObjectFlags: 2, ObjectClass: 1, FlagHTMLDDA: 8, ClassFunction: 2,
		ClassProxy: 26, UndefinedCell: 0x60000, StoreRef: 0x70000, CallGo: 0x72000}
	cells, tests := 0, 0
	for _, src := range typeofCorpus {
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
			for _, lower := range []func(*bytecode.Function) (*ir.Program, error){jitcompile.LowerSSA, jitcompile.LowerSSAInline} {
				p, err := lower(c.Fn)
				if err != nil {
					continue
				}
				f, err := ssa.BuildWith(p, typeofs{fn: c.Fn})
				if err != nil {
					t.Fatalf("%s: %v", src, err)
				}
				f.CallEntry = true
				ssa.Optimize(f)
				for _, b := range f.Blocks {
					for _, v := range b.Values {
						switch v.Op {
						case ssa.OpTypeofCell:
							cells++
						case ssa.OpTypeIs:
							tests++
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
						t.Fatalf("%s (typeof), %s: %v", src, arch, err)
					}
					if err := check(arch, mc.Bytes[:mc.Text]); err != nil {
						t.Fatalf("%s (typeof), %s: %v", src, arch, err)
					}
				}
			}
		}
	}
	if cells == 0 || tests < 6 {
		t.Errorf("typeof's answers made natively %d, its tests %d", cells, tests)
	}
}
