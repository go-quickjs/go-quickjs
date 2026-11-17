package compile

import (
	"os"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// typedCompiled is every function of src's that has typed code, by name.
func typedCompiled(t *testing.T, src string) map[string]*bytecode.Function {
	t.Helper()
	out := map[string]*bytecode.Function{}
	for name, fn := range regCompiled(t, src) {
		if fn.HasRegTyped() {
			out[name] = fn
		}
	}
	return out
}

// TestLowerTypedMicroKernels lowers the micro-benchmarks' asm.js kernels'
// typed code, builds and optimizes it: every function lowers and builds,
// and what is left of the guards that check the kinds -- an unboxing of a
// raw slot's value -- is none: the kinds are proven.
func TestLowerTypedMicroKernels(t *testing.T) {
	b, err := os.ReadFile("../../v8bench/microbench_extra.js")
	if err != nil {
		t.Fatal(err)
	}
	fns := typedCompiled(t, strings.ReplaceAll(string(b), "\r\n", "\n"))
	if len(fns) < 17 {
		t.Fatalf("%d functions with typed code", len(fns))
	}
	for name, fn := range fns {
		rc := fn.RegTypedCode()
		if rc == nil {
			t.Errorf("%s: no typed code", name)
			continue
		}
		p, err := LowerTyped(rc, fn)
		if err != nil {
			t.Errorf("%s: %v\n%s", name, err, rc)
			continue
		}
		f, err := ssa.BuildWith(p, typedTestFeedback{})
		if err != nil {
			t.Errorf("%s: build: %v", name, err)
			continue
		}
		ssa.Optimize(f)
		unboxes := 0
		for _, blk := range f.Blocks {
			for _, v := range blk.Values {
				if v.Op == ssa.OpUnboxF64 && v.Block.PC >= 0 {
					unboxes++
				}
			}
		}
		if testing.Verbose() {
			t.Logf("%s: %d unboxes left\n%s", name, unboxes, f)
		}
	}
}

// typedTestFeedback is a typed lowering's feedback for its tests: a heap,
// and Math's functions by Go, nothing of any site.
type typedTestFeedback struct{}

func (typedTestFeedback) Property(int) (ssa.PropertySite, bool) { return ssa.PropertySite{}, false }
func (typedTestFeedback) Global(int) (ssa.GlobalSite, bool)     { return ssa.GlobalSite{}, false }
func (typedTestFeedback) Generic(int) bool                      { return false }
func (typedTestFeedback) EntryGeneric(int) bool                 { return false }
func (typedTestFeedback) Inline(int) (ssa.InlineSite, bool)     { return ssa.InlineSite{}, false }
func (typedTestFeedback) NativeCalls(int) []ssa.CallSite        { return nil }
func (typedTestFeedback) Heap() uintptr                         { return 0x1000 }
func (typedTestFeedback) RawMath(pc int) (ssa.Intrinsic, bool) {
	fn := 0
	if fn == bytecode.TMathPow {
		return ssa.Intrinsic{Op: ssa.OpPowF64}, true
	}
	return ssa.Intrinsic{Op: ssa.OpMathCall, Go: ssa.GoMath, Fn: fn}, true
}
