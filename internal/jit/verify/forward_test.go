//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/arch/arm64/arm64asm"
	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// forwardCorpus are forwarding functions, f.apply(x, arguments), whose
// apply_arguments native code makes as a call of the frame's arguments
// (ssa.CallSite's Forward): alone, in a loop, of a receiver of another
// value, and twice.
var forwardCorpus = []string{
	`function f() { return g.apply(this, arguments) }`,
	`function f() { let s = 0; for (let i = 0; i < 8; i++) s += g.apply(this, arguments); return s }`,
	`function f(a) { return g.apply(a, arguments) + 1 }`,
	`function f() { const x = g.apply(null, arguments); return h.apply(this, arguments) + x }`,
}

// forwards is globals whose apply_arguments have each been seen to call
// what variant says: one function of two parameters; one that reads its
// arguments past them (FrameArgs) and coerces its receiver; two functions,
// as many as the call calls natively (Generic); none, Go's.
type forwards struct {
	globals
	fn      *bytecode.Function
	variant int
}

func (forwards) GoCalls() bool { return true }
func (f forwards) NativeCalls(pc int) []ssa.CallSite {
	if f.fn.Code[pc].Op != bytecode.OpApplyArguments {
		return nil
	}
	site := ssa.CallSite{Callee: 0x50000, Closure: 0x51000, Entry: 0x52000, Count: 0x53000, Argc: 2, Forward: true, Apply: 0x54000,
		Params: 2, LocalCount: 3, MaxStack: 4, ThisSlot: -1}
	switch f.variant {
	case 1:
		site.FrameArgs, site.Coerce, site.ThisSlot, site.Params, site.LocalCount = true, true, 3, 1, 3
	case 2:
		other := site
		other.Callee, other.Closure, other.Entry, other.Count, other.Params = 0x55000, 0x56000, 0x57000, 0x58000, 5
		site.Generic = true
		return []ssa.CallSite{site, other}
	case 3:
		return []ssa.CallSite{{Go: ssa.GoCall, Forward: true, Apply: 0x54000, Argc: 2, ThisSlot: -1}}
	}
	return []ssa.CallSite{site}
}

// A forwarding call's code, of every variant, decodes in full on both
// targets and keeps their reserved registers; made natively, it reads the
// frame's arguments' count from its context (abi.Context's Argc) and
// copies them from there (Args).
func TestMirForwardingCalls(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000,
		LinkEntry: 8, LinkIn: 16, ClosureUpvalues: 24, CallGo: 0x72000}
	made := 0
	for _, src := range forwardCorpus {
		prog, err := parser.Parse("var g, h;"+src, parser.Options{})
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
				if !p.Forwards {
					t.Fatalf("%s: not lowered as a forwarding function", src)
				}
				for variant := range 4 {
					f, err := ssa.BuildWith(p, forwards{fn: c.Fn, variant: variant})
					if err != nil {
						t.Fatalf("%s (%d): %v", src, variant, err)
					}
					f.CallEntry = true
					ssa.Optimize(f)
					native := false
					for _, b := range f.Blocks {
						for _, v := range b.Values {
							if cs := v.Calls(); v.Op == ssa.OpCall && len(cs) != 0 && cs[0].Forward && cs[0].Go == 0 {
								native = true
							}
						}
					}
					if native != (variant != 3) {
						t.Fatalf("%s (%d): native forwarding call %v", src, variant, native)
					}
					for _, arch := range arches {
						compile := mir.CompileAMD64
						if arch == "arm64" {
							compile = mir.CompileARM64
						}
						mc, err := compile(f, enc)
						if err != nil {
							t.Fatalf("%s (%d), %s: %v", src, variant, arch, err)
						}
						code := mc.Bytes[:mc.Text]
						if err := check(arch, code); err != nil {
							t.Fatalf("%s (%d), %s: %v", src, variant, arch, err)
						}
						if native && !readsArgc(arch, code) {
							t.Errorf("%s (%d), %s: no read of the context's Argc", src, variant, arch)
						}
					}
					if native {
						made++
					}
				}
			}
		}
	}
	if made == 0 {
		t.Fatal("no forwarding call made natively")
	}
}

// readsArgc reports whether code loads the context's Argc.
func readsArgc(arch string, code []byte) bool {
	switch arch {
	case "amd64":
		for pc := 0; pc < len(code); {
			in, err := x86asm.Decode(code[pc:], 64)
			if err != nil {
				return false
			}
			if in.Op == x86asm.MOV && ctxMem(in, 1, abi.OffArgc) {
				return true
			}
			pc += in.Len
		}
	case "arm64":
		for pc := 0; pc+4 <= len(code); pc += 4 {
			in, err := arm64asm.Decode(code[pc:])
			if err != nil {
				return false
			}
			if in.Op == arm64asm.LDR && strings.HasSuffix(in.String(), fmt.Sprintf("[X0,#%d]", abi.OffArgc)) {
				return true
			}
		}
	}
	return false
}
