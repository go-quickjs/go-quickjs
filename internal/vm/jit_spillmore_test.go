//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// spillMoreSource is a function of a loop whose n values are all live at
// once, past every register, so that its code spills most of them, past
// the context's own slots (abi.Context's SpillMore): int32s, which the
// general registers' pass spills first, then references and doubles, whose
// slots are past them. The int32s and most doubles are read first; the
// references and the last keep doubles live on across a collection, whose
// junk reuses what it freed, and a guard that a string deoptimizes, and
// are read after it. No exit or call is made while more than
// abi.MaxRecords values live but where each is in its slot (mir's
// recordRoom), and none while all of them do.
func spillMoreSource(name string, n, keep int) string {
	const refs = 30
	var b strings.Builder
	fmt.Fprintf(&b, "function %s(a, b, c, o, p, rounds) {\nvar s = 0, z = 0;\nfor (var r = 0; r < rounds; r++) {\n", name)
	ints := n / 3
	for i := range ints {
		fmt.Fprintf(&b, "var i%d = (a * %d + r) | 0;\n", i, i+3)
	}
	for i := range refs {
		fmt.Fprintf(&b, "var o%d = (r + %d) & 1 ? o : p;\n", i, i)
	}
	doubles := n - ints - refs
	for i := range doubles {
		fmt.Fprintf(&b, "var d%d = a * %d + b + r;\n", i, i+1)
	}
	for i := range ints {
		fmt.Fprintf(&b, "s = s * 0.5 + i%d;\n", i)
	}
	for i := range doubles - keep {
		fmt.Fprintf(&b, "s = s * 0.5 + d%d;\n", i)
	}
	b.WriteString("collect(); junk();\nz = c + 1;\n")
	for i := range refs {
		fmt.Fprintf(&b, "s = s * 0.5 + (typeof o%d === 'string' ? o%d.length : o%d.v);\n", i, i, i)
	}
	for i := doubles - keep; i < doubles; i++ {
		fmt.Fprintf(&b, "s = s * 0.5 + d%d;\n", i)
	}
	b.WriteString("}\nreturn z + ':' + s;\n}\n")
	return b.String()
}

const spillMoreHelpers = `
function junk() { var t = []; for (var i = 0; i < 3000; i++) t.push({ v: -i, w: 'junk' + i }); return t.length }
`

// spillMoreObject is {v: 100 + i}, made in r.
func spillMoreObject(r *Runtime, i int) Value {
	o := Obj(r.NewObject())
	if err := r.SetProp(o, r.Intern("v"), Float(float64(100+i))); err != nil {
		panic(err)
	}
	return o
}

// spillMoreCollect defines collect, a collection, in r.
func spillMoreCollect(r *Runtime) {
	r.global.setOwnRaw(r.atoms.intern("collect"), Obj(r.newNativeFunc("collect", 0,
		func(*Runtime, Value, []Value) (Value, error) {
			runtime.GC()
			return Undefined, nil
		})), propDefault)
}

// Functions whose values need about 1,400 and 6,700 spill slots compile
// and run natively from their register code, their slots past the
// context's 256 in its SpillMore, with the collector idle and marking:
// references live in those slots across a collection, and a
// deoptimization with values live in them, the exit reading them. Each
// answer is an ordinary call's.
func TestJITSpillMore(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	sizes := []int{1000, 5000}
	if testing.Short() {
		sizes = sizes[:1]
	}
	for _, n := range sizes {
		name := fmt.Sprintf("spill%d", n)
		src := spillMoreHelpers + spillMoreSource(name, n, 150)
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("reg/%d/marking=%v", n, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				rts := regNativeRuntimes(t, src)
				for _, r := range rts {
					spillMoreCollect(r)
					r.jitMemoryLimit = 1 << 36
				}
				for i, c := range []Value{Float(7), Float(9), Str(NewString("q"))} {
					args := []Value{Float(1.25 + float64(i)), Float(0.5), c, Undefined, Str(NewString("pp" + fmt.Sprint(i))), Float(2)}
					fn0, _ := rts[0].GetProp(Obj(rts[0].Global()), rts[0].Intern(name))
					guards := rts[0].RegNativeStats().Guards
					args[3] = spillMoreObject(rts[0], i)
					res := rts[0].RegNativeCall(fn0, args, false)
					if res.Skipped != "" || res.Entered < 0 {
						t.Fatalf("%s: not native: %q", name, res.Skipped)
					}
					got := regNativeShow(rts[0], res.Value, res.Err)
					if i == 2 && rts[0].RegNativeStats().Guards == guards {
						t.Errorf("%s: no deoptimization at the string", name)
					}
					fn1, _ := rts[1].GetProp(Obj(rts[1].Global()), rts[1].Intern(name))
					args[3] = spillMoreObject(rts[1], i)
					v, err := rts[1].Call(fn1, Undefined, args)
					if want := regNativeShow(rts[1], v, err); got != want {
						t.Fatalf("%s(%d): natively %q, called %q", name, i, got, want)
					}
				}
				if s := rts[0].jit.ssaSpills; s < n/2 {
					t.Errorf("%s: %d spill slots, want at least %d", name, s, n/2)
				} else {
					t.Logf("%s: %d spill slots", name, s)
				}
				for _, c := range rts[0].jit.ssaCtxs {
					if len(c.SpillMore) < rts[0].jit.ssaSpills-abi.SpillSlots {
						t.Errorf("a context has room for %d slots past its own, want %d", len(c.SpillMore), rts[0].jit.ssaSpills-abi.SpillSlots)
					}
				}
			})
		}
	}
}

// A function whose values need some 13,000 spill slots compiles, with
// amd64's backend and arm64's, on any host: past 4,096 slots beyond the
// context's own, arm64 adds to SpillMore's address in steps. Its code is
// past 1 MB, which is placed as any code is (TestJITLongCode, which runs
// code of that length); here it is compiled only. Its frame states share
// their slots (TestJITSpillStatesLinear), so the two builds of it take
// memory linear in its length.
func TestJITSpillMoreCompiles(t *testing.T) {
	if !jitSSABackend || testing.Short() {
		t.Skip("no SSA backend on this architecture, or short")
	}
	const n = 10000
	name := fmt.Sprintf("spill%d", n)
	rts := regNativeRuntimes(t, spillMoreHelpers+spillMoreSource(name, n, 150))
	r := rts[0]
	spillMoreCollect(r)
	r.jitMemoryLimit = 1 << 36
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
	r.RegNativeCall(fn, []Value{Float(1.25), Float(0.5), Float(7), spillMoreObject(r, 0), Str(NewString("pp")), Float(2)}, false)
	cl := fn.Object().fn().closure
	e := r.jit.regCache[r.jitRegKeyOf(cl)]
	if e == nil || e.p == nil {
		t.Fatalf("%s: not lowered", name)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		f, err := ssa.BuildWith(e.p, r.jitRegFeedbackFor(cl, e.p))
		if err != nil {
			t.Fatal(err)
		}
		jitRegShape(f, e.p, cl, cl.reg)
		ssa.Optimize(f)
		compile := mir.CompileAMD64
		if arch == "arm64" {
			compile = mir.CompileARM64
		}
		mc, err := compile(f, jitEncoding)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if mc.Spills < n {
			t.Errorf("%s: %d spill slots, want at least %d", arch, mc.Spills, n)
		}
		t.Logf("%s: %d spill slots, %d bytes", arch, mc.Spills, len(mc.Bytes))
	}
}

// A function's compile takes memory linear in its size, however many
// values its states name: the states of a function of n values live at
// once and an exit at each of its instructions share their slots
// (internal/jit/ssa's slots.go) rather than each name all n, which had
// made a compile of 13,000 slots take some seven gigabytes. Eight times
// the values take some eight times the memory, not sixty-four.
func TestJITSpillStatesLinear(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	alloc := func(n int) uint64 {
		name := fmt.Sprintf("spill%d", n)
		rts := regNativeRuntimes(t, spillMoreHelpers+spillMoreSource(name, n, 150))
		r := rts[0]
		spillMoreCollect(r)
		r.jitMemoryLimit = 1 << 36
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
		r.RegNativeCall(fn, []Value{Float(1.25), Float(0.5), Float(7), spillMoreObject(r, 0), Str(NewString("pp")), Float(2)}, false)
		cl := fn.Object().fn().closure
		e := r.jit.regCache[r.jitRegKeyOf(cl)]
		if e == nil || e.p == nil {
			t.Fatalf("%s: not lowered", name)
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		before := ms.TotalAlloc
		f, err := ssa.BuildWith(e.p, r.jitRegFeedbackFor(cl, e.p))
		if err != nil {
			t.Fatal(err)
		}
		jitRegShape(f, e.p, cl, cl.reg)
		ssa.Optimize(f)
		if _, err := mir.CompileAMD64(f, jitEncoding); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&ms)
		return ms.TotalAlloc - before
	}
	small, big := alloc(500), alloc(4000)
	t.Logf("500 values: %.1f MB, 4,000: %.1f MB", float64(small)/(1<<20), float64(big)/(1<<20))
	if big > 12*small {
		t.Errorf("eight times the values took %.1f times the memory", float64(big)/float64(small))
	}
}
