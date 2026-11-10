package spectest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/wast"
)

// TestJITDifferential runs every module of the spec scripts -- and of the
// threads proposal's, where WASM_THREADS_DIR names them -- that links with
// the spectest module alone twice -- interpreted, and with every function
// compiled natively at its first call -- and calls each exported function
// whose parameters are numbers, vectors or nullable references with the
// same arguments on both, the values the scripts use, the edges of each
// type and random ones -- for a v128, vectors of each lane shape's edges:
// NaNs of every payload, signed zeros, infinities, the integers' bounds --
// null references and i31s, comparing the results, a v128's bits exactly,
// the traps and their frames -- each frame's function and offset, the
// trapping instruction's in the innermost -- and every memory's bytes and
// global's value after each call, references by what they are (refAlike).
// It does it again with every call of a function with loops that Go makes
// running in the interpreter until its first back edge and going on
// natively from that loop's head (osr), with every function the
// optimizing tier accepts compiled by it, the rest by the baseline (opt),
// and with every function's baseline code replaced by the optimizing
// tier's after its third call, compiled in the background while later
// calls run on, the code of either tier (tiered).
func TestJITDifferential(t *testing.T) {
	dir := os.Getenv("WASM_SPEC_DIR")
	if dir == "" {
		t.Skip("WASM_SPEC_DIR is not set")
	}
	if !engine.JITAvailable() {
		t.Skip("no native code in this build")
	}
	if os.Getenv("WASM_JIT") != "" {
		t.Skip("WASM_JIT compiles the interpreter's side too")
	}
	// Each module is instantiated twice, and a grow the first could make
	// the second may find no memory for: both have a limit far below it.
	m32, m64 := engine.MaxMemory32Pages, engine.MaxMemory64Pages
	engine.MaxMemory32Pages, engine.MaxMemory64Pages = 1024, 1024
	defer func() { engine.MaxMemory32Pages, engine.MaxMemory64Pages = m32, m64 }()
	var files []string
	files = append(files, scripts(t, filepath.Join(dir, "test", "core"))...)
	files = append(files, scripts(t, filepath.Join(dir, "test", "legacy"))...)
	if td := os.Getenv("WASM_THREADS_DIR"); td != "" {
		files = append(files, scripts(t, filepath.Join(td, "test", "core", "threads"))...)
	}
	for _, mode := range []string{"entry", "osr", "opt", "tiered"} {
		t.Run(mode, func(t *testing.T) { differential(t, files, mode) })
	}
}

func differential(t *testing.T, files []string, mode string) {
	only := os.Getenv("WASM_SPEC_ONLY")
	before := engine.ReadJITStats()
	d := &differ{t: t, rng: rand.New(rand.NewPCG(1, 2)), osr: mode == "osr", opt: mode == "opt", tiered: mode == "tiered"}
	for _, f := range files {
		if only != "" && !strings.Contains(filepath.ToSlash(f), only) {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		cmds, err := wast.Parse(src)
		if err != nil {
			continue
		}
		d.file = filepath.Base(f)
		start, calls := time.Now(), d.calls
		d.script(cmds)
		if el := time.Since(start); el > 2*time.Second {
			t.Logf("%s: %d calls in %v", d.file, d.calls-calls, el.Round(time.Millisecond))
		}
	}
	st := engine.ReadJITStats()
	t.Logf("differential: %d modules, %d functions, %d calls compared (%d stopped by the time limit); %d functions compiled natively, %d calls went on natively from a loop",
		d.modules, d.funcs, d.calls, d.timeouts, st.Compiled-before.Compiled, st.OSR-before.OSR)
	if d.opt || d.tiered {
		refused := 0
		for r, n := range st.OptRefused {
			refused += n - before.OptRefused[r]
		}
		t.Logf("differential: the optimizing tier compiled %d functions and refused %d", st.Optimized-before.Optimized, refused)
	}
	if d.failures > 0 {
		t.Fatalf("%d differences", d.failures)
	}
}

type differ struct {
	t      *testing.T
	rng    *rand.Rand
	file   string
	consts map[module.Kind][]uint64
	// vconsts are the script's v128 arguments.
	vconsts []engine.Val
	// osr enters every loop of a native call from the interpreter; opt
	// compiles every function the optimizing tier accepts with it; tiered
	// has it compile them in the background after their third call.
	osr, opt, tiered bool
	modules          int
	funcs            int
	calls            int
	timeouts         int
	failures         int
}

// script runs a script's modules, the constants its assertions use as
// arguments.
func (d *differ) script(cmds []wast.Command) {
	d.consts = map[module.Kind][]uint64{}
	d.vconsts = nil
	for _, cmd := range cmds {
		var a *wast.Action
		switch c := cmd.(type) {
		case *wast.AssertReturn:
			a = &c.Action
		case *wast.AssertTrap:
			a = &c.Action
		case *wast.ActionCommand:
			a = &c.Action
		}
		if a == nil {
			continue
		}
		for _, v := range a.Args {
			if k, ok := kindOf(v.Kind); ok {
				d.consts[k] = append(d.consts[k], v.Bits)
			}
			if v.Kind == wast.V128 {
				d.vconsts = append(d.vconsts, engine.Val{N: binary.LittleEndian.Uint64(v.V128[:8]), Hi: binary.LittleEndian.Uint64(v.V128[8:])})
			}
		}
	}
	for _, cmd := range cmds {
		if c, ok := cmd.(*wast.ModuleCommand); ok && !c.Definition {
			d.module(c)
		}
	}
}

func kindOf(k wast.ValueKind) (module.Kind, bool) {
	switch k {
	case wast.I32:
		return module.KindI32, true
	case wast.I64:
		return module.KindI64, true
	case wast.F32:
		return module.KindF32, true
	case wast.F64:
		return module.KindF64, true
	}
	return 0, false
}

func (d *differ) fail(format string, args ...any) {
	d.failures++
	if d.failures <= 40 {
		d.t.Errorf("%s: %s", d.file, fmt.Sprintf(format, args...))
	}
}

func (d *differ) module(c *wast.ModuleCommand) {
	r := newRunner(d.t, d.file)
	code, err := r.compile(c.Module)
	if err != nil {
		return
	}
	interp, err := r.instantiate(code)
	if err != nil {
		return
	}
	nat := engine.NewCode(code.Module)
	nat.EnableJIT(0)
	if d.osr {
		nat.EnterLoopsForTest()
	}
	switch {
	case d.opt:
		nat.SetOptThreshold(0)
	case d.tiered:
		nat.SetOptThreshold(3)
		nat.SetOptBackground(true)
	default:
		nat.SetOptThreshold(-1)
	}
	jit, err := newRunner(d.t, d.file).instantiate(nat)
	if err != nil {
		d.fail("line %d: instantiated interpreted, not natively: %v", c.Line(), err)
		return
	}
	d.modules++
	d.state(c.Line(), "instantiation", interp, jit)
	for _, e := range code.Module.Exports {
		if e.Kind != module.ExternFunc {
			continue
		}
		fi, _ := interp.Export(e.Name)
		fj, _ := jit.Export(e.Name)
		if fi.Func.Host != nil || !callable(fi.Func.Sig) {
			continue
		}
		d.funcs++
		for k := 0; k < 12; k++ {
			args := d.args(fi.Func.Sig.Params)
			ri, ei, okI := d.invoke(fi.Func, args)
			rj, ej, okJ := d.invoke(fj.Func, args)
			if !okI || !okJ {
				// Out of time: the two may have stopped at different
				// places, and the instances differ from here on.
				d.timeouts++
				return
			}
			d.calls++
			what := fmt.Sprintf("line %d: %s%v", c.Line(), e.Name, showArgs(args, fi.Func.Sig.Params))
			if !sameOutcome(fi.Func.Sig.Results, ri, ei, rj, ej) {
				d.fail("%s: interpreter %v %v, native %v %v", what, ri, ei, rj, ej)
				return
			}
			if fi, fj := trapFrames(ei, interp), trapFrames(ej, jit); !slices.Equal(fi, fj) {
				d.fail("%s: %v: frames %v interpreted, %v natively", what, ei, fi, fj)
				return
			}
			if !d.state(c.Line(), what, interp, jit) {
				return
			}
		}
	}
}

// callable reports whether the test calls a function of type ft: its
// parameters numbers, vectors or nullable references, which it is given
// null or an i31.
func callable(ft *module.FuncType) bool {
	for _, t := range ft.Params {
		if t.IsRef() && !t.Nullable {
			return false
		}
	}
	return true
}

// refAlike reports whether two references the two instances made are the
// same: both null, i31s of one value, the same function of each instance,
// the same host value, or objects of one kind.
func refAlike(a, b engine.Ref) bool {
	switch {
	case a.IsNull() || b.IsNull():
		return a.IsNull() && b.IsNull()
	case a.IsI31() || b.IsI31():
		return a == b
	}
	if fa, fb := a.Func(), b.Func(); fa != nil || fb != nil {
		return fa != nil && fb != nil && fa.Index == fb.Index && (fa.Host == nil) == (fb.Host == nil)
	}
	if ha, ok := a.Host(); ok {
		hb, ok := b.Host()
		return ok && ha == hb
	}
	return a.Kind() == b.Kind()
}

func showArgs(args []engine.Val, ts []module.ValueType) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = show(a, ts[i])
	}
	return out
}

// invoke calls fn under a time limit, reporting false if it ran out.
func (d *differ) invoke(fn *engine.Function, args []engine.Val) ([]engine.Val, error, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, err := engine.Invoke(ctx, fn, append([]engine.Val(nil), args...))
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, nil, false
	}
	return res, err, true
}

// args returns arguments of types ts: a constant of the script's, an edge
// of the type, or random bits.
func (d *differ) args(ts []module.ValueType) []engine.Val {
	out := make([]engine.Val, len(ts))
	for i, t := range ts {
		if t.IsRef() {
			switch t.Heap {
			case module.HeapAny, module.HeapEq, module.HeapI31:
				if d.rng.IntN(2) == 0 {
					out[i] = engine.Val{R: engine.I31(uint32(d.value(module.KindI32)))}
				}
			}
			continue
		}
		if t.Kind == module.KindV128 {
			out[i] = d.vector()
			continue
		}
		out[i] = engine.Val{N: d.value(t.Kind)}
	}
	return out
}

// vector returns a v128 argument: a constant of the script's, a vector of
// one lane shape's edges, or random bits.
func (d *differ) vector() engine.Val {
	n := d.rng.IntN(10)
	if n < 3 && len(d.vconsts) > 0 {
		return d.vconsts[d.rng.IntN(len(d.vconsts))]
	}
	if n < 8 {
		// Each lane an edge of one shape's lane type, or all lanes one.
		shape := []struct {
			bits int
			k    module.Kind
		}{{8, module.KindI32}, {16, module.KindI32}, {32, module.KindI32}, {64, module.KindI64},
			{32, module.KindF32}, {64, module.KindF64}}[d.rng.IntN(6)]
		var w [2]uint64
		e := edges[shape.k]
		one := e[d.rng.IntN(len(e))]
		for i := 0; i < 128; i += shape.bits {
			v := one
			if n < 6 {
				v = e[d.rng.IntN(len(e))]
			}
			if shape.bits < 64 {
				v &= 1<<shape.bits - 1
			}
			w[i/64] |= v << (i % 64)
		}
		return engine.Val{N: w[0], Hi: w[1]}
	}
	return engine.Val{N: d.rng.Uint64(), Hi: d.rng.Uint64()}
}

var edges = map[module.Kind][]uint64{
	module.KindI32: {0, 1, 2, 3, 7, 8, 31, 32, 63, 64, 100, 0x7fffffff, 0x80000000, 0xffffffff, 0xfffffffe, 0xffff, 0x10000, 65532, 65536},
	module.KindI64: {0, 1, 2, 63, 64, 1 << 31, 1 << 32, 1<<63 - 1, 1 << 63, math.MaxUint64, math.MaxUint64 - 1},
	module.KindF32: {0, 1 << 31, uint64(math.Float32bits(1)), uint64(math.Float32bits(-1)), uint64(math.Float32bits(0.5)),
		uint64(math.Float32bits(-1.5)), uint64(math.Float32bits(2.5)), uint64(math.Float32bits(float32(math.Inf(1)))),
		uint64(math.Float32bits(float32(math.Inf(-1)))), 0x7fc00000, 0xffc00000, 0x7fa00000, 1, 0x7f7fffff,
		uint64(math.Float32bits(2147483648)), uint64(math.Float32bits(-2147483904)), uint64(math.Float32bits(4294967296)),
		uint64(math.Float32bits(9223372036854775808)), uint64(math.Float32bits(18446744073709551616))},
	module.KindF64: {0, 1 << 63, math.Float64bits(1), math.Float64bits(-1), math.Float64bits(0.5), math.Float64bits(-0.5),
		math.Float64bits(2.5), math.Float64bits(-2.5), math.Float64bits(math.Inf(1)), math.Float64bits(math.Inf(-1)),
		0x7ff8000000000000, 0xfff8000000000000, 0x7ff4000000000000, 1, 0x7fefffffffffffff,
		math.Float64bits(2147483647.9), math.Float64bits(-2147483648.9), math.Float64bits(4294967295.5),
		math.Float64bits(9223372036854775807), math.Float64bits(18446744073709551615), math.Float64bits(-0.99)},
}

func (d *differ) value(k module.Kind) uint64 {
	switch n := d.rng.IntN(10); {
	case n < 4 && len(d.consts[k]) > 0:
		return d.consts[k][d.rng.IntN(len(d.consts[k]))]
	case n < 7:
		e := edges[k]
		return e[d.rng.IntN(len(e))]
	}
	v := d.rng.Uint64()
	if k == module.KindI32 || k == module.KindF32 {
		v = uint64(uint32(v))
		if d.rng.IntN(2) == 0 && k == module.KindI32 {
			v &= 0xffff
		}
	}
	return v
}

// trapFrames returns the frames of the trap err is, if it is one, each as
// the function's index and the offset, with an asterisk for a function of
// an instance other than inst.
func trapFrames(err error, inst *engine.Instance) []string {
	var tr *engine.Trap
	if !errors.As(err, &tr) {
		return nil
	}
	out := make([]string, len(tr.Frames))
	for i, f := range tr.Frames {
		mark := ""
		if f.Module != inst {
			mark = "*"
		}
		out[i] = fmt.Sprintf("%s%d@%#x", mark, f.Func, f.Offset)
	}
	return out
}

// sameOutcome reports whether two calls ended alike: the same results, a
// NaN for a NaN of any payload, or the same trap or kind of error.
func sameOutcome(ts []module.ValueType, ri []engine.Val, ei error, rj []engine.Val, ej error) bool {
	if (ei == nil) != (ej == nil) {
		return false
	}
	if ei != nil {
		var ti, tj *engine.Trap
		if errors.As(ei, &ti) {
			return errors.As(ej, &tj) && ti.Code == tj.Code && ti.Msg == tj.Msg
		}
		var xi, xj *engine.Exception
		if errors.As(ei, &xi) {
			return errors.As(ej, &xj)
		}
		return ei.Error() == ej.Error()
	}
	for i, t := range ts {
		if t.IsRef() {
			if !refAlike(ri[i].R, rj[i].R) {
				return false
			}
			continue
		}
		a, b := ri[i].N, rj[i].N
		switch t.Kind {
		case module.KindV128:
			// A vector's bits, NaNs' payloads too, are the same.
			if ri[i].Hi != rj[i].Hi {
				return false
			}
		case module.KindI32:
			a, b = uint64(uint32(a)), uint64(uint32(b))
		case module.KindF32:
			fa, fb := math.Float32frombits(uint32(a)), math.Float32frombits(uint32(b))
			if fa != fa && fb != fb {
				continue
			}
			a, b = uint64(uint32(a)), uint64(uint32(b))
		case module.KindF64:
			fa, fb := math.Float64frombits(a), math.Float64frombits(b)
			if fa != fa && fb != fb {
				continue
			}
		}
		if a != b {
			return false
		}
	}
	return true
}

// state compares the instances' memories and globals.
func (d *differ) state(line int, what string, a, b *engine.Instance) bool {
	for i := range a.Memories {
		if !bytes.Equal(a.Memories[i].Bytes(), b.Memories[i].Bytes()) {
			d.fail("%s: memory %d differs after it", what, i)
			return false
		}
	}
	for i, g := range a.Globals {
		h := b.Globals[i]
		if g.Type.IsRef() {
			if !refAlike(g.Ref, h.Ref) {
				d.fail("%s: global %d differs", what, i)
				return false
			}
			continue
		}
		if g.Num != h.Num || g.Hi != h.Hi {
			if (g.Type.Kind == module.KindF32 || g.Type.Kind == module.KindF64) && isNaN(g) && isNaN(h) {
				continue
			}
			d.fail("%s: global %d is %#x %#x interpreted, %#x %#x natively", what, i, g.Num, g.Hi, h.Num, h.Hi)
			return false
		}
	}
	return true
}

func isNaN(g *engine.Global) bool {
	if g.Type.Kind == module.KindF32 {
		f := math.Float32frombits(uint32(g.Num))
		return f != f
	}
	f := math.Float64frombits(g.Num)
	return f != f
}
