//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package verify

import (
	"os"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// typedFeedback is a typed build's feedback: a heap cell's address, Math's
// functions by Go, nothing of any site.
type typedFeedback struct{}

func (typedFeedback) Property(int) (ssa.PropertySite, bool) { return ssa.PropertySite{}, false }
func (typedFeedback) Global(int) (ssa.GlobalSite, bool)     { return ssa.GlobalSite{}, false }
func (typedFeedback) Generic(int) bool                      { return false }
func (typedFeedback) EntryGeneric(int) bool                 { return false }
func (typedFeedback) Inline(int) (ssa.InlineSite, bool)     { return ssa.InlineSite{}, false }
func (typedFeedback) NativeCalls(int) []ssa.CallSite        { return nil }
func (typedFeedback) GoCalls() bool                         { return true }
func (typedFeedback) Heap() uintptr                         { return 0x71000 }
func (typedFeedback) RawMath(int) (ssa.Intrinsic, bool) {
	return ssa.Intrinsic{Op: ssa.OpMathCall, Go: ssa.GoMath, Fn: 1}, true
}

// TestTypedEmitted compiles the typed code of the VM's asm.js tests and
// of the micro-benchmarks' kernels -- every raw operation, the heap's
// loads and stores of every element type, raw slots at entries and exits
// -- for both targets: each function builds, and its code keeps the
// rules (check).
func TestTypedEmitted(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		CallGo: 0x72000, StoreRef: 0x70000}
	n, heaps, divs := 0, 0, 0
	for _, file := range []string{"../../vm/testdata/asm_ops.js", "../../vm/testdata/asm_edges.js", "../../v8bench/microbench_extra.js"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ReplaceAll(string(b), "\r\n", "\n")
		prog, err := parser.Parse(src, parser.Options{})
		if err != nil {
			t.Fatal(err)
		}
		top, err := compiler.Compile(prog, compiler.Options{Text: src})
		if err != nil {
			t.Fatal(err)
		}
		var walk func(fn *bytecode.Function)
		walk = func(fn *bytecode.Function) {
			for _, c := range fn.Constants {
				if c.Kind != bytecode.ConstFunction {
					continue
				}
				walk(c.Fn)
				rc := c.Fn.RegTypedCode()
				if rc == nil {
					continue
				}
				p, err := jitcompile.LowerTyped(rc, c.Fn)
				if err != nil {
					t.Errorf("%s: %s: %v", file, c.Fn.Name, err)
					continue
				}
				f, err := ssa.BuildWith(p, typedFeedback{})
				if err != nil {
					t.Errorf("%s: %s: build: %v", file, c.Fn.Name, err)
					continue
				}
				regShape(f, p)
				f.CallEntry = true
				ssa.Optimize(f)
				for _, blk := range f.Blocks {
					for _, v := range blk.Values {
						switch v.Op {
						case ssa.OpHeapLoad, ssa.OpHeapStore:
							heaps++
						case ssa.OpDivI32, ssa.OpDivU32, ssa.OpRemI32, ssa.OpRemU32:
							divs++
						}
					}
				}
				for _, arch := range arches {
					compile := mir.CompileAMD64
					if arch == "arm64" {
						compile = mir.CompileARM64
					}
					// With a rounding instruction and, on a CPU without
					// SSE4.1, without one: Math.floor's and %'s code then
					// SSE2's.
					for _, round := range []bool{true, false} {
						enc.Round = round
						mc, err := compile(f, enc)
						if err != nil {
							t.Fatalf("%s: %s, %s, round %v: %v", file, c.Fn.Name, arch, round, err)
						}
						if err := check(arch, mc.Bytes[:mc.Text]); err != nil {
							t.Fatalf("%s: %s, %s, round %v: %v", file, c.Fn.Name, arch, round, err)
						}
					}
				}
				n++
			}
		}
		walk(top)
	}
	if n < 40 || heaps == 0 || divs == 0 {
		t.Fatalf("%d typed functions compiled, %d heap accesses, %d divisions", n, heaps, divs)
	}
	t.Logf("%d typed functions compiled for both targets, %d heap accesses, %d divisions", n, heaps, divs)
}
