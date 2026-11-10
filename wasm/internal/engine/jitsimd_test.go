//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// simdFunc is a function of the SIMD test module: one instruction over its
// parameters.
type simdFunc struct {
	name   string
	op     opcode.Opcode
	params []module.ValueType
	result module.ValueType
	body   []byte
}

func uleb(b []byte, v uint64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

func sleb(b []byte, v int64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0) {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

func valType(t module.ValueType) byte {
	switch t.Kind {
	case module.KindI32:
		return 0x7f
	case module.KindI64:
		return 0x7e
	case module.KindF32:
		return 0x7d
	case module.KindF64:
		return 0x7c
	}
	return 0x7b
}

// simdOpBytes is op's encoding: the prefix and its index.
func simdOpBytes(op opcode.Opcode) []byte {
	idx := uint64(op) & 0xff
	if op>>12 == opcode.Opcode(opcode.PrefixSIMD) {
		idx = uint64(op) & 0xfff
	}
	return uleb([]byte{opcode.PrefixSIMD}, idx)
}

// simdModule encodes a module of fns, each exported by its name.
func simdModule(fns []simdFunc) []byte {
	section := func(out []byte, id byte, body []byte) []byte {
		out = append(out, id)
		out = uleb(out, uint64(len(body)))
		return append(out, body...)
	}
	vec := func(n int) []byte { return uleb(nil, uint64(n)) }
	types, funcs, exports, codes := vec(len(fns)), vec(len(fns)), vec(len(fns)), vec(len(fns))
	for i, f := range fns {
		types = append(types, 0x60)
		types = uleb(types, uint64(len(f.params)))
		for _, p := range f.params {
			types = append(types, valType(p))
		}
		types = append(types, 1, valType(f.result))
		funcs = uleb(funcs, uint64(i))
		exports = uleb(exports, uint64(len(f.name)))
		exports = append(exports, f.name...)
		exports = append(exports, 0)
		exports = uleb(exports, uint64(i))
		var body []byte
		body = append(body, 0) // no locals
		for k := range f.params {
			body = append(body, 0x20)
			body = uleb(body, uint64(k))
		}
		body = append(body, f.body...)
		body = append(body, 0x0b)
		codes = uleb(codes, uint64(len(body)))
		codes = append(codes, body...)
	}
	out := []byte{0, 'a', 's', 'm', 1, 0, 0, 0}
	out = section(out, 1, types)
	out = section(out, 3, funcs)
	out = section(out, 7, exports)
	return section(out, 10, codes)
}

// simdTestFuncs are a function for each SIMD instruction that is not a
// load, a store or a constant: each lane of the lane instructions, a set
// of shuffles, and the shifts by their parameter and by constants.
func simdTestFuncs() []simdFunc {
	var fns []simdFunc
	v := module.V128
	add := func(op opcode.Opcode, params []module.ValueType, result module.ValueType, body []byte, suffix string) {
		name := op.Name() + suffix
		fns = append(fns, simdFunc{name: name, op: op, params: params, result: result, body: body})
	}
	var ops []opcode.Opcode
	for i := uint32(0); i <= 0x113; i++ {
		ops = append(ops, opcode.Prefixed(opcode.PrefixSIMD, i))
	}
	for _, op := range ops {
		switch {
		case op.Name() == "invalid opcode", opcode.IsFP16SIMD(op), op == opcode.S128Const,
			op >= opcode.S128LoadMem && op <= opcode.S128StoreMem,
			op >= opcode.S128Load8Lane && op <= opcode.S128Load64Zero:
			continue
		case op == opcode.I8x16Shuffle:
			r := rand.New(rand.NewPCG(3, 4))
			patterns := [][16]byte{}
			var id, rev, inter, hi, bcast [16]byte
			for i := range 16 {
				id[i], rev[i], inter[i], hi[i], bcast[i] = byte(i), byte(31-i), byte(i/2+16*(i%2)), byte(16+i), 5
			}
			patterns = append(patterns, id, rev, inter, hi, bcast)
			for range 6 {
				var p [16]byte
				for i := range p {
					p[i] = byte(r.IntN(32))
				}
				patterns = append(patterns, p)
			}
			for k, p := range patterns {
				add(op, []module.ValueType{v, v}, v, append(simdOpBytes(op), p[:]...), fmt.Sprintf("/%d", k))
			}
		case op >= opcode.I8x16ExtractLaneS && op <= opcode.F64x2ReplaceLane:
			var scalar module.ValueType
			switch op {
			case opcode.I64x2ExtractLane, opcode.I64x2ReplaceLane:
				scalar = module.I64
			case opcode.F32x4ExtractLane, opcode.F32x4ReplaceLane:
				scalar = module.F32
			case opcode.F64x2ExtractLane, opcode.F64x2ReplaceLane:
				scalar = module.F64
			default:
				scalar = module.I32
			}
			lanes := 16
			name := op.Name()
			switch {
			case strings.HasPrefix(name, "i16x8"):
				lanes = 8
			case strings.HasPrefix(name, "i32x4"), strings.HasPrefix(name, "f32x4"):
				lanes = 4
			case strings.HasPrefix(name, "i64x2"), strings.HasPrefix(name, "f64x2"):
				lanes = 2
			}
			for l := range lanes {
				body := append(simdOpBytes(op), byte(l))
				if strings.Contains(name, "replace") {
					add(op, []module.ValueType{v, scalar}, v, body, fmt.Sprintf("/%d", l))
				} else {
					add(op, []module.ValueType{v}, scalar, body, fmt.Sprintf("/%d", l))
				}
			}
		default:
			sig := opcode.Signature(op)
			if sig == nil {
				continue
			}
			add(op, sig.Params, sig.Results[0], simdOpBytes(op), "")
			if strings.Contains(op.Name(), ".sh") {
				// The shifts by constants: a function of the vector alone.
				for _, n := range []int64{0, 1, 3, 7, 8, 9, 15, 16, 17, 31, 32, 33, 63, 64, 65, -1} {
					body := sleb([]byte{0x41}, n)
					body = append(body, simdOpBytes(op)...)
					add(op, []module.ValueType{v}, v, body, fmt.Sprintf("/%d", n))
				}
			}
		}
	}
	return fns
}

// The edges of each lane shape, which the inputs are made of.
var (
	edges8   = []uint64{0, 1, 2, 0x7f, 0x80, 0xff, 0x40, 0xfe, 0x81, 0x0f, 0x10}
	edges16  = []uint64{0, 1, 0x7fff, 0x8000, 0xffff, 0x4000, 0xc000, 0x00ff, 0xff00, 0x8001, 0x7ffe, 0x100}
	edges32  = []uint64{0, 1, 0x7fffffff, 0x80000000, 0xffffffff, 0x10000, 0xffff, 0x80000001, 0x7ffffffe, 0xfffffffe}
	edges64  = []uint64{0, 1, math.MaxInt64, 1 << 63, math.MaxUint64, 1 << 32, 0xffffffff, 1<<63 + 1, math.MaxUint64 - 1}
	edgesF32 = func() (e []uint64) {
		for _, f := range []float32{0, 1, -1, 0.5, -0.5, 1.5, -1.5, 2.5, -2.5, 3.5, 0.49999997, float32(math.Inf(1)), float32(math.Inf(-1)),
			2147483648, -2147483648, 2147483520, -2147483904, 4294967296, 4294967040, 1e10, -1e10, 8388609, 16777217, -0.99} {
			e = append(e, uint64(math.Float32bits(f)))
		}
		return append(e, 1<<31, 0x7fc00000, 0xffc00000, 0x7f800001, 0x7fa00005, 0xff812345, 0x7fc12345, 0xffc54321,
			1, 0x80000001, 0x7f7fffff, 0xff7fffff, 0x007fffff, 0x00800000)
	}()
	edgesF64 = func() (e []uint64) {
		for _, f := range []float64{0, 1, -1, 0.5, -0.5, 1.5, -1.5, 2.5, -2.5, math.Inf(1), math.Inf(-1),
			2147483647, 2147483648, 2147483647.5, -2147483648, -2147483648.5, -2147483649, 4294967295, 4294967295.5,
			4294967296, 1 << 52, 1<<53 + 2, 1e300, -1e300, 3.5e38, -3.5e38, 1e-40, 1.401298464324817e-45, 3.4028235677973366e+38, -0.99} {
			e = append(e, math.Float64bits(f))
		}
		return append(e, 1<<63, 0x7ff8000000000000, 0xfff8000000000000, 0x7ff0000000000001, 0x7ff4000000000005,
			0xfff0000000012345, 0x7ff8000000012345, 1, 0x8000000000000001, 0x7fefffffffffffff, 0x36a0000000000000,
			0x36a0000010000000, 0x3810000000000000)
	}()
)

// shape is lanes of a width, and their edges, and whether they are floats'.
type shape struct {
	bits  int
	edges []uint64
	float bool
}

var shapes = []shape{{8, edges8, false}, {16, edges16, false}, {32, edges32, false}, {64, edges64, false},
	{32, edgesF32, true}, {64, edgesF64, true}}

// splatLanes is lane value e repeated through a vector of lanes of bits.
func splatLanes(e uint64, bits int) V128 {
	var w uint64
	for i := 0; i < 64; i += bits {
		w |= (e & (1<<bits - 1)) << i
	}
	return V128{w, w}
}

// mixLanes is a vector of lanes of sh each an edge chosen by r.
func mixLanes(sh shape, r *rand.Rand) V128 {
	var ws [2]uint64
	for i := 0; i < 128; i += sh.bits {
		e := sh.edges[r.IntN(len(sh.edges))]
		if sh.bits < 64 {
			e &= 1<<sh.bits - 1
		}
		ws[i/64] |= e << (i % 64)
	}
	return V128{ws[0], ws[1]}
}

// simdInputs returns the argument lists a function of params is called
// with: the edges of every shape, alone and paired with each other edge of
// its shape, mixed in vectors, and random bits; and of three vectors,
// every triple of the floats' NaNs and the numbers whose products and sums
// are NaNs of none (ruleNaNs32, ruleNaNs64).
func simdInputs(params []module.ValueType, r *rand.Rand, short bool) [][]Val {
	nv := 0
	for _, p := range params {
		if p.Kind == module.KindV128 {
			nv++
		}
	}
	var vecs [][]V128
	mix := 24
	if short {
		mix = 6
	}
	for _, sh := range shapes {
		switch nv {
		case 1:
			for _, e := range sh.edges {
				vecs = append(vecs, []V128{splatLanes(e, sh.bits)})
			}
			for range mix {
				vecs = append(vecs, []V128{mixLanes(sh, r)})
			}
		case 2, 3:
			for _, e := range sh.edges {
				for _, f := range sh.edges {
					bothNaN := sh.float && isNaNBits(e, sh.bits) && isNaNBits(f, sh.bits)
					if short && r.IntN(4) != 0 && !bothNaN {
						continue
					}
					t := []V128{splatLanes(e, sh.bits), splatLanes(f, sh.bits)}
					if nv == 3 {
						t = append(t, splatLanes(sh.edges[r.IntN(len(sh.edges))], sh.bits))
					}
					vecs = append(vecs, t)
				}
			}
			for range 4 * mix {
				t := []V128{mixLanes(sh, r), mixLanes(sh, r), mixLanes(sh, r)}
				vecs = append(vecs, t[:nv])
			}
			if nv == 3 && sh.float {
				nans := ruleNaNs32
				if sh.bits == 64 {
					nans = ruleNaNs64
				}
				for _, e := range nans {
					for _, f := range nans {
						for _, g := range nans {
							vecs = append(vecs, []V128{splatLanes(e, sh.bits), splatLanes(f, sh.bits), splatLanes(g, sh.bits)})
						}
					}
				}
			}
		}
	}
	for range 2 * mix {
		vecs = append(vecs, []V128{{r.Uint64(), r.Uint64()}, {r.Uint64(), r.Uint64()}, {r.Uint64(), r.Uint64()}}[:nv])
	}
	if nv == 0 {
		vecs = append(vecs, nil)
	}
	var out [][]Val
	for _, vs := range vecs {
		// The scalars: edges of their type, and the shift counts.
		var scalars []uint64
		for _, p := range params {
			switch p.Kind {
			case module.KindI32:
				scalars = append(scalars, 0, 1, 7, 8, 15, 16, 31, 32, 33, 63, 64, 0xffffffff, 0x80000000, 0x7f, 0x80, 0xffff, 0x12345678)
			case module.KindI64:
				scalars = append(scalars, edges64...)
			case module.KindF32:
				scalars = append(scalars, edgesF32...)
			case module.KindF64:
				scalars = append(scalars, edgesF64...)
			}
		}
		if len(scalars) == 0 {
			scalars = []uint64{0}
		}
		n := len(scalars)
		if nv > 0 {
			// Not every scalar with every vector: a few of them.
			n = min(n, 3)
		}
		for k := range n {
			s := scalars[k]
			if nv > 0 {
				s = scalars[r.IntN(len(scalars))]
			}
			var args []Val
			vi := 0
			for _, p := range params {
				if p.Kind == module.KindV128 {
					args = append(args, Val{N: vs[vi].Lo, Hi: vs[vi].Hi})
					vi++
					continue
				}
				args = append(args, Val{N: s})
			}
			out = append(out, args)
		}
	}
	return out
}

// binaryInstance instantiates module b with its functions compiled after
// threshold calls.
func binaryInstance(t testing.TB, b []byte, threshold int) *Instance {
	t.Helper()
	m, derr := decoder.Decode(b, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	code := NewCode(m)
	if !code.EnableJIT(threshold) {
		t.Fatal("EnableJIT refused where JITAvailable")
	}
	inst, err := Instantiate(context.Background(), code, nil)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// TestJITSIMDBits runs every SIMD instruction that is not a load, a store
// or a constant -- the relaxed ones included -- on the edges of each lane
// shape (NaNs of every payload and sign, signed zeros, infinities, the
// integers' and the conversions' bounds), alone, paired and mixed in
// vectors, and on random bits, in the interpreter and natively, and
// requires the same bits of every result, and of every lane with a NaN
// operand the NaN rule's (nan.go, ruleLanes). It reports which
// instructions native code leaves to Go (ExitSIMD).
func TestJITSIMDBits(t *testing.T) {
	if !JITAvailable() {
		t.Skip("no native code in this build")
	}
	n, calls, exits := simdBits(t, func(simdFunc) bool { return true })
	t.Logf("%d functions, %d calls compared; %d instructions run in Go: %s", n, calls, len(exits), strings.Join(exits, " "))
	// Only the fused relaxed_madd and relaxed_nmadd on amd64, which the
	// code does without VEX encodings.
	var want []string
	if runtime.GOARCH == "amd64" && wjit.CPU.RelaxedFused {
		want = []string{"f32x4.qfma", "f32x4.qfms", "f64x2.qfma", "f64x2.qfms"}
	}
	if !slices.Equal(exits, want) {
		t.Fatalf("the instructions run in Go are %v, want %v", exits, want)
	}
}

// simdBits compares the functions of simdTestFuncs that keep takes,
// interpreted and natively, returning how many there were, the calls
// compared, and the names of the instructions native code left to Go.
func simdBits(t *testing.T, keep func(simdFunc) bool) (n, calls int, exits []string) {
	t.Helper()
	var fns []simdFunc
	for _, f := range simdTestFuncs() {
		if keep(f) {
			fns = append(fns, f)
		}
	}
	b := simdModule(fns)
	interp := binaryInstance(t, b, math.MaxInt32)
	native := binaryInstance(t, b, 0)
	r := rand.New(rand.NewPCG(1, 2))
	fails := 0
	for _, f := range fns {
		ei, _ := interp.Export(f.name)
		en, _ := native.Export(f.name)
		for _, args := range simdInputs(f.params, r, testing.Short()) {
			want, err := Invoke(context.Background(), ei.Func, slices.Clone(args))
			if err != nil {
				t.Fatalf("%s: %v", f.name, err)
			}
			got, err := Invoke(context.Background(), en.Func, slices.Clone(args))
			if err != nil {
				t.Fatalf("%s natively: %v", f.name, err)
			}
			calls++
			w, g := want[0], got[0]
			if f.result.Kind == module.KindI32 || f.result.Kind == module.KindF32 {
				w.N, g.N = uint64(uint32(w.N)), uint64(uint32(g.N))
			}
			if f.result.Kind != module.KindV128 {
				w.Hi, g.Hi = 0, 0
			}
			if w.N != g.N || w.Hi != g.Hi {
				fails++
				if fails <= 30 {
					t.Errorf("%s%s = %#x %#x natively, %#x %#x interpreted", f.name, showVals(args), g.N, g.Hi, w.N, w.Hi)
				}
			}
			if f.result.Kind == module.KindV128 {
				vs := make([]V128, len(args))
				for k, a := range args {
					vs[k] = V128{a.N, a.Hi}
				}
				for _, tier := range []struct {
					name string
					v    Val
				}{{"interpreted", w}, {"natively", g}} {
					if msg := ruleLanes(f.op, vs, V128{tier.v.N, tier.v.Hi}); msg != "" {
						fails++
						if fails <= 30 {
							t.Errorf("%s%s %s: %s", f.name, showVals(args), tier.name, msg)
						}
					}
				}
			}
		}
		if !nativeRan(native, f.name) {
			t.Fatalf("%s has no native code", f.name)
		}
	}
	if fails > 0 {
		t.Fatalf("%d differences", fails)
	}
	// The instructions native code left to Go: those whose offsets the
	// exits' code is kept by.
	cj := native.Code.jit.Load()
	names := map[string]bool{}
	cj.simd.Range(func(k, _ any) bool {
		at := k.(uint32)
		for i, f := range native.Code.Module.Functions {
			if at >= f.Code.Offset && at < f.Code.End() {
				names[fns[i].op.Name()] = true
			}
		}
		return true
	})
	for n := range names {
		exits = append(exits, n)
	}
	sort.Strings(exits)
	return len(fns), calls, exits
}

func showVals(args []Val) string {
	var b strings.Builder
	for _, a := range args {
		fmt.Fprintf(&b, " (%#x %#x)", a.N, a.Hi)
	}
	return b.String()
}

// v128 values natively where the register cache runs out -- many live at
// once, spilled and loaded back -- in locals' homes across native calls and
// exits, as arguments and results of calls, in globals, through select,
// blocks, br_table and a loop's parameters, give the interpreter's bits.
func TestJITSIMDValues(t *testing.T) {
	var deep strings.Builder
	// Twenty vectors live at once, then summed in pairs.
	for i := range 20 {
		fmt.Fprintf(&deep, "(i32x4.add (local.get %d) (v128.const i32x4 %d %d %d %d))\n", i%6, i, -i, i*7, 1<<i)
	}
	for range 19 {
		deep.WriteString("(i8x16.add)\n")
	}
	src := `(module
  (global $g (mut v128) (v128.const i64x2 0x1122334455667788 0x99aabbccddeeff00))
  (func $mix (param v128 v128 i32 v128) (result v128 v128)
    (i64x2.add (local.get 0) (local.get 3))
    (select (local.get 1) (local.get 0) (local.get 2)))
  (func (export "deep") (param v128 v128 v128 v128 v128 v128) (result v128)
    ` + deep.String() + `)
  (func (export "calls") (param v128 v128 i32) (result v128) (local v128 v128 v128 v128 v128 v128 f64 f32)
    (local.set 3 (i16x8.mul (local.get 0) (local.get 1)))
    (local.set 4 (i32x4.sub (local.get 1) (local.get 0)))
    (local.set 5 (v128.xor (local.get 3) (local.get 4)))
    (local.set 9 (f64.const 2.5))
    (call $mix (local.get 3) (local.get 4) (local.get 2) (local.get 5))
    (local.set 6)
    (local.set 7)
    (global.set $g (i8x16.sub (global.get $g) (local.get 6)))
    (local.set 8 (i32x4.splat (memory.size)))
    (i64x2.add (local.get 7) (local.get 6))
    (v128.or (global.get $g))
    (i32x4.add (local.get 8))
    (f64x2.replace_lane 1 (local.get 9))
    (v128.xor (local.get 3))
    (v128.and (local.get 4))
    (v128.or (local.get 5)))
  (func (export "flow") (param v128 i32) (result v128) (local v128)
    (local.set 2 (local.get 0))
    (block (result v128)
      (block (result v128)
        (block (result v128)
          (local.get 0)
          (br_table 0 1 2 (local.get 1)))
        (i32x4.add (v128.const i32x4 1 2 3 4))
        (br 1))
      (i8x16.neg))
    (loop (param v128) (result v128)
      (i32x4.add (local.get 2))
      (local.set 2 (i8x16.shl (local.get 2) (i32.const 1)))
      (local.tee 1 (i32.sub (local.get 1) (i32.const 1)))
      (i32.const 0)
      (i32.gt_s)
      (br_if 0)))
  (func (export "global") (result v128) (global.get $g))
  (memory 1))`
	interp := interpretedInstance(t, src)
	native := nativeInstance(t, src)
	r := rand.New(rand.NewPCG(5, 6))
	vec := func() Val { return Val{N: r.Uint64(), Hi: r.Uint64()} }
	for i := range 200 {
		cases := []struct {
			name string
			args []Val
		}{
			{"deep", []Val{vec(), vec(), vec(), vec(), vec(), vec()}},
			{"calls", []Val{vec(), vec(), {N: uint64(i % 2)}}},
			{"flow", []Val{vec(), {N: uint64(i % 7)}}},
			{"global", nil},
		}
		for _, cs := range cases {
			ei, _ := interp.Export(cs.name)
			en, _ := native.Export(cs.name)
			want, err := Invoke(context.Background(), ei.Func, slices.Clone(cs.args))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Invoke(context.Background(), en.Func, slices.Clone(cs.args))
			if err != nil {
				t.Fatal(err)
			}
			if got[0].N != want[0].N || got[0].Hi != want[0].Hi {
				t.Fatalf("%s%s = %#x %#x natively, %#x %#x interpreted", cs.name, showVals(cs.args), got[0].N, got[0].Hi, want[0].N, want[0].Hi)
			}
		}
	}
	for _, name := range []string{"deep", "calls", "flow", "global"} {
		if !nativeRan(native, name) {
			t.Fatalf("%s has no native code", name)
		}
	}
}
