package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// The relaxed SIMD operators, their operand counts and whether their lanes
// are floats, whose NaNs' payloads are not compared: which NaN operand a
// fused multiply-add instruction returns depends on the order its form takes
// the operands in, which Node's compilers choose by register.
var relaxedOps = []struct {
	name  string
	arity int
	nan   bool
}{
	{"i8x16.relaxed_swizzle", 2, false},
	{"i32x4.relaxed_trunc_f32x4_s", 1, false},
	{"i32x4.relaxed_trunc_f32x4_u", 1, false},
	{"i32x4.relaxed_trunc_f64x2_s_zero", 1, false},
	{"i32x4.relaxed_trunc_f64x2_u_zero", 1, false},
	{"f32x4.relaxed_madd", 3, true},
	{"f32x4.relaxed_nmadd", 3, true},
	{"f64x2.relaxed_madd", 3, true},
	{"f64x2.relaxed_nmadd", 3, true},
	{"i8x16.relaxed_laneselect", 3, false},
	{"i16x8.relaxed_laneselect", 3, false},
	{"i32x4.relaxed_laneselect", 3, false},
	{"i64x2.relaxed_laneselect", 3, false},
	{"f32x4.relaxed_min", 2, false},
	{"f32x4.relaxed_max", 2, false},
	{"f64x2.relaxed_min", 2, false},
	{"f64x2.relaxed_max", 2, false},
	{"i16x8.relaxed_q15mulr_s", 2, false},
	{"i16x8.relaxed_dot_i8x16_i7x16_s", 2, false},
	{"i32x4.relaxed_dot_i8x16_i7x16_add_s", 3, false},
}

// relaxedModule returns a module exporting each relaxed operator as a
// function of its operands' halves, as six i64s, returning its result's.
func relaxedModule() string {
	var b strings.Builder
	b.WriteString("(module\n")
	b.WriteString("  (func $v (param i64 i64) (result v128) (i64x2.replace_lane 1 (i64x2.splat (local.get 0)) (local.get 1)))\n")
	for _, o := range relaxedOps {
		fmt.Fprintf(&b, "  (func (export %q) (param i64 i64 i64 i64 i64 i64) (result i64 i64) (local v128)\n", o.name)
		fmt.Fprintf(&b, "    (local.set 6 (%s", o.name)
		for i := 0; i < o.arity; i++ {
			fmt.Fprintf(&b, " (call $v (local.get %d) (local.get %d))", 2*i, 2*i+1)
		}
		b.WriteString("))\n    (i64x2.extract_lane 0 (local.get 6)) (i64x2.extract_lane 1 (local.get 6)))\n")
	}
	b.WriteString(")\n")
	return b.String()
}

// relaxedInputs returns operands for the relaxed operators: lanes at the
// edges of the integer and float ranges, NaNs, and multiply-adds whose
// addend cancels the product.
func relaxedInputs(r *rand.Rand, n int) [][3]V128 {
	f32s := []float32{0, float32(math.Copysign(0, -1)), 1, -1, 0.5, -0.5, 1.5, -1.5, 2147483520, 2147483648, -2147483648,
		-2147483904, 4294967040, 4294967296, float32(math.Inf(1)), float32(math.Inf(-1)), 1e-40, 3.4e38, -3.4e38,
		math.Float32frombits(0x7fc00000), math.Float32frombits(0xffc00000), math.Float32frombits(0x7fa00001),
		math.Float32frombits(0x7fc12345)}
	f64s := []float64{0, math.Copysign(0, -1), 1, -1, 0.5, -0.5, -0.9, 2147483647.5, 2147483648, -2147483648.5,
		-2147483649, 4294967295.5, 4294967296, math.Inf(1), math.Inf(-1), 5e-324, 1.7e308,
		math.Float64frombits(0x7ff8000000000000), math.Float64frombits(0xfff8000000000000),
		math.Float64frombits(0x7ff4000000000001), math.Float64frombits(0x7ff8000012345678)}
	vec := func() V128 {
		switch r.Intn(5) {
		case 0:
			var a [4]float32
			for i := range a {
				if r.Intn(4) == 0 {
					a[i] = math.Float32frombits(r.Uint32())
				} else {
					a[i] = f32s[r.Intn(len(f32s))]
				}
			}
			return fromF32(a)
		case 1:
			var a [2]float64
			for i := range a {
				if r.Intn(4) == 0 {
					a[i] = math.Float64frombits(r.Uint64())
				} else {
					a[i] = f64s[r.Intn(len(f64s))]
				}
			}
			return fromF64(a)
		case 2:
			edges := []uint16{0, 1, 0x7fff, 0x8000, 0x8001, 0xffff, 0x4000, 0xc000}
			var a [8]uint16
			for i := range a {
				a[i] = edges[r.Intn(len(edges))]
			}
			return fromU16(a)
		}
		return randV128(r)
	}
	out := make([][3]V128, n)
	for i := range out {
		x, y, z := vec(), vec(), vec()
		switch i % 5 {
		case 0:
			// Addends that cancel the product, rounded.
			a, b := x.f32(), y.f32()
			var c [4]float32
			for j := range c {
				c[j] = -float32(a[j] * b[j])
			}
			z = fromF32(c)
		case 1:
			a, b := x.f64(), y.f64()
			z = fromF64([2]float64{-float64(a[0] * b[0]), -float64(a[1] * b[1])})
		case 2:
			x, y = fromF32([4]float32{1 + 0x1p-23, 1 + 0x1p-12, 3, 1e30}), fromF32([4]float32{1 - 0x1p-23, 1 - 0x1p-12, 1.0 / 3, 1e30})
			z = fromF32([4]float32{-1, -1, -1, -1e30})
		}
		out[i] = [3]V128{x, y, z}
	}
	return out
}

const relaxedNodeScript = `
const fs = require('fs');
const dir = process.argv[2];
const inst = new WebAssembly.Instance(new WebAssembly.Module(fs.readFileSync(dir + '/m.wasm')));
const cases = JSON.parse(fs.readFileSync(dir + '/in.json'));
const hex = (x) => BigInt.asUintN(64, x).toString(16);
const out = cases.map(([fn, args]) => inst.exports[fn](...args.map((a) => BigInt.asIntN(64, BigInt('0x' + a)))).map(hex));
fs.writeFileSync(dir + '/out.json', JSON.stringify(out));
`

// TestRelaxedMatchesNode runs every relaxed SIMD operator on the same
// operands here and in Node, with Node's baseline wasm compiler alone and
// with its optimizing one alone, and fails on any difference.
func TestRelaxedMatchesNode(t *testing.T) {
	if os.Getenv("QUICKJS_COMPARE_NODE_WASM") != "1" {
		t.Skip("set QUICKJS_COMPARE_NODE_WASM=1 to compare with node")
	}
	bin, err := wat.Compile([]byte(relaxedModule()))
	if err != nil {
		t.Fatal(err)
	}
	lim := decoder.V8Limits()
	m, derr := decoder.Decode(bin, lim)
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, lim); derr != nil {
		t.Fatal(derr)
	}
	inst, err := Instantiate(context.Background(), NewCode(m), nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.wasm"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.js"), []byte(relaxedNodeScript), 0o644); err != nil {
		t.Fatal(err)
	}

	type call struct {
		op   int
		args [6]uint64
	}
	var calls []call
	var in [][2]any
	r := rand.New(rand.NewSource(3))
	for k, o := range relaxedOps {
		for _, v := range relaxedInputs(r, 1000) {
			c := call{op: k, args: [6]uint64{v[0].Lo, v[0].Hi, v[1].Lo, v[1].Hi, v[2].Lo, v[2].Hi}}
			calls = append(calls, c)
			hs := make([]string, 6)
			for i, a := range c.args {
				hs[i] = strconv.FormatUint(a, 16)
			}
			in = append(in, [2]any{o.name, hs})
		}
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "in.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	ours := make([]V128, len(calls))
	for i, c := range calls {
		ext, _ := inst.Export(relaxedOps[c.op].name)
		args := make([]Val, 6)
		for j, a := range c.args {
			args[j] = Val{N: a}
		}
		res, err := Invoke(context.Background(), ext.Func, args)
		if err != nil {
			t.Fatal(err)
		}
		ours[i] = V128{res[0].N, res[1].N}
	}

	for _, tier := range [][]string{{"--liftoff", "--no-wasm-tier-up"}, {"--no-liftoff"}} {
		cmd := osexec.Command("node", append(tier, filepath.Join(dir, "run.js"), dir)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("node %v: %v\n%s", tier, err, out)
		}
		data, err := os.ReadFile(filepath.Join(dir, "out.json"))
		if err != nil {
			t.Fatal(err)
		}
		var res [][2]string
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		bad := map[string]int{}
		for i, c := range calls {
			lo, _ := strconv.ParseUint(res[i][0], 16, 64)
			hi, _ := strconv.ParseUint(res[i][1], 16, 64)
			o := relaxedOps[c.op]
			if sameResult(ours[i], V128{lo, hi}, o.name, o.nan) {
				continue
			}
			if bad[o.name]++; bad[o.name] <= 3 {
				t.Errorf("node %v: %s(%016x%016x, %016x%016x, %016x%016x) = %016x%016x, ours %016x%016x",
					tier, o.name, c.args[1], c.args[0], c.args[3], c.args[2], c.args[5], c.args[4], hi, lo, ours[i].Hi, ours[i].Lo)
			}
		}
		for name, n := range bad {
			t.Errorf("node %v: %s: %d of 1000 differ", tier, name, n)
		}
		t.Logf("node %v: %d calls compared", tier, len(calls))
	}
}

// sameResult reports whether a and b are equal, taking any two NaN lanes as
// equal if nan.
func sameResult(a, b V128, name string, nan bool) bool {
	if a == b {
		return true
	}
	if !nan {
		return false
	}
	if strings.HasPrefix(name, "f32x4") {
		x, y := a.f32(), b.f32()
		for i := range x {
			if math.Float32bits(x[i]) != math.Float32bits(y[i]) && !(x[i] != x[i] && y[i] != y[i]) {
				return false
			}
		}
		return true
	}
	x, y := a.f64(), b.f64()
	for i := range x {
		if math.Float64bits(x[i]) != math.Float64bits(y[i]) && !(x[i] != x[i] && y[i] != y[i]) {
			return false
		}
	}
	return true
}
