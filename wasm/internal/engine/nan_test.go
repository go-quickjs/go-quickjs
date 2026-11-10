package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The NaN rule of nan.go, modelled on bits apart from the engine's code:
// which NaN each float operation returns, for the tests to hold the
// interpreter and native code to it rather than only to each other.

// nanBits are the masks of a float of w bits.
func nanBits(w int) (sign, exp, quiet uint64) {
	if w == 32 {
		return 1 << 31, 0x7f800000, 1 << 22
	}
	return 1 << 63, 0x7ff0000000000000, 1 << 51
}

func isNaNBits(x uint64, w int) bool {
	sign, exp, _ := nanBits(w)
	return x&^sign > exp
}

func isSignalingBits(x uint64, w int) bool {
	_, _, quiet := nanBits(w)
	return isNaNBits(x, w) && x&quiet == 0
}

func isInfBits(x uint64, w int) bool {
	sign, exp, _ := nanBits(w)
	return x&^sign == exp
}

func isZeroBits(x uint64, w int) bool {
	sign, _, _ := nanBits(w)
	return x&^sign == 0
}

func quietBits(x uint64, w int) uint64 {
	_, _, quiet := nanBits(w)
	return x | quiet
}

func defaultNaNBits(w int) uint64 {
	if w == 32 {
		return ruleDefaultNaN32
	}
	return ruleDefaultNaN64
}

// rulePick is the NaN an arithmetic operation of a and b returns, one of
// them a NaN: on amd64 and 386 the first NaN, quieted; elsewhere a
// signaling NaN ahead of a quiet one, then the first.
func rulePick(a, b uint64, w int) uint64 {
	switch {
	case ruleX86 && isNaNBits(a, w):
		return quietBits(a, w)
	case ruleX86:
		return quietBits(b, w)
	case isSignalingBits(a, w):
		return quietBits(a, w)
	case isSignalingBits(b, w):
		return quietBits(b, w)
	case isNaNBits(a, w):
		return a
	}
	return b
}

// ruleArith is what an arithmetic operation of a and b whose result is r
// must return, if the rule says: the pick of NaN operands, or the default
// NaN for a NaN of none.
func ruleArith(a, b, r uint64, w int) (uint64, bool) {
	switch {
	case isNaNBits(a, w) || isNaNBits(b, w):
		return rulePick(a, b, w), true
	case isNaNBits(r, w):
		return defaultNaNBits(w), true
	}
	return 0, false
}

// ruleUnary is what sqrt or a rounding of a, whose result is r, must
// return, if the rule says.
func ruleUnary(a, r uint64, w int) (uint64, bool) {
	switch {
	case isNaNBits(a, w):
		return quietBits(a, w), true
	case isNaNBits(r, w):
		return defaultNaNBits(w), true
	}
	return 0, false
}

// ruleDemote and rulePromote are a NaN converted: its sign, its payload's
// top bits, the quiet bit set.
func ruleDemote(a uint64) uint64 {
	return a>>32&(1<<31) | 0x7fc00000 | a>>29&(1<<22-1)
}

func rulePromote(a uint64) uint64 {
	return a>>31&1<<63 | 0x7ff8000000000000 | (a&(1<<22-1))<<29
}

// ruleFMA is what a fused a*b+c, or -(a*b)+c if neg, whose result is r,
// must return, if the rule says. On amd64 and 386 the first NaN of a, b
// and c, quieted, the negation leaving its sign; elsewhere the signaling
// NaNs of c, a negated if neg, and b, in that order, then a quiet c --
// the default NaN if the product is of an infinity and a zero -- then the
// quiet a and b.
func ruleFMA(a, b, c, r uint64, neg bool, w int) (uint64, bool) {
	if !isNaNBits(a, w) && !isNaNBits(b, w) && !isNaNBits(c, w) {
		if isNaNBits(r, w) {
			return defaultNaNBits(w), true
		}
		return 0, false
	}
	if ruleX86 {
		for _, x := range []uint64{a, b, c} {
			if isNaNBits(x, w) {
				return quietBits(x, w), true
			}
		}
	}
	if neg {
		sign, _, _ := nanBits(w)
		a ^= sign
	}
	for _, x := range []uint64{c, a, b} {
		if isSignalingBits(x, w) {
			return quietBits(x, w), true
		}
	}
	if isNaNBits(c, w) {
		if (isInfBits(a, w) && isZeroBits(b, w)) || (isZeroBits(a, w) && isInfBits(b, w)) {
			return defaultNaNBits(w), true
		}
		return c, true
	}
	if isNaNBits(a, w) {
		return a, true
	}
	return b, true
}

// ruleUnfused is ruleFMA for a multiplication rounded and then an addition
// (p + c) or a subtraction (c - p), as relaxed_madd and relaxed_nmadd are
// where they do not fuse.
func ruleUnfused(a, b, c, r uint64, neg bool, w int) (uint64, bool) {
	var p uint64
	pNaN := true
	switch {
	case isNaNBits(a, w) || isNaNBits(b, w):
		p = rulePick(a, b, w)
	case (isInfBits(a, w) && isZeroBits(b, w)) || (isZeroBits(a, w) && isInfBits(b, w)):
		p = defaultNaNBits(w)
	default:
		pNaN = false
	}
	if !pNaN {
		// The product is a number, its bits not modelled: only c's NaN.
		return ruleArith(0, c, r, w)
	}
	if neg {
		return rulePick(c, p, w), true
	}
	return rulePick(p, c, w), true
}

// lane is lane i of v, of w bits.
func lane(v V128, i, w int) uint64 {
	word := v.Lo
	if i*w >= 64 {
		word = v.Hi
	}
	x := word >> (i * w % 64)
	if w == 32 {
		x &= 1<<32 - 1
	}
	return x
}

// ruleLanes checks the NaN lanes of the result res of the float lane
// instruction op on args against the rule, returning a description of the
// first lane that breaks it, or "". An instruction the rule says nothing
// of, or a lane without a NaN, passes.
func ruleLanes(op opcode.Opcode, args []V128, res V128) string {
	w := 32
	if strings.HasPrefix(op.Name(), "f64x2") {
		w = 64
	}
	for i := 0; i < 128/w; i++ {
		x := make([]uint64, len(args))
		nan := false
		for k, a := range args {
			x[k] = lane(a, i, w)
			nan = nan || isNaNBits(x[k], w)
		}
		r := lane(res, i, w)
		var want uint64
		ok := false
		switch op {
		case opcode.F32x4Add, opcode.F32x4Sub, opcode.F32x4Mul, opcode.F32x4Div, opcode.F32x4Min, opcode.F32x4Max,
			opcode.F64x2Add, opcode.F64x2Sub, opcode.F64x2Mul, opcode.F64x2Div, opcode.F64x2Min, opcode.F64x2Max:
			want, ok = ruleArith(x[0], x[1], r, w)
		case opcode.F32x4RelaxedMin, opcode.F32x4RelaxedMax, opcode.F64x2RelaxedMin, opcode.F64x2RelaxedMax:
			if ruleX86 {
				// MINPS and MAXPS: the second operand, as it is.
				want, ok = x[1], nan
			} else {
				want, ok = ruleArith(x[0], x[1], r, w)
			}
		case opcode.F32x4Pmin, opcode.F32x4Pmax, opcode.F64x2Pmin, opcode.F64x2Pmax:
			// No comparison with a NaN is true: the first operand.
			want, ok = x[0], nan
		case opcode.F32x4Sqrt, opcode.F32x4Ceil, opcode.F32x4Floor, opcode.F32x4Trunc, opcode.F32x4NearestInt,
			opcode.F64x2Sqrt, opcode.F64x2Ceil, opcode.F64x2Floor, opcode.F64x2Trunc, opcode.F64x2NearestInt:
			want, ok = ruleUnary(x[0], r, w)
		case opcode.F32x4Qfma, opcode.F32x4Qfms, opcode.F64x2Qfma, opcode.F64x2Qfms:
			neg := op == opcode.F32x4Qfms || op == opcode.F64x2Qfms
			if ruleFused() {
				want, ok = ruleFMA(x[0], x[1], x[2], r, neg, w)
			} else {
				want, ok = ruleUnfused(x[0], x[1], x[2], r, neg, w)
			}
		default:
			return ""
		}
		if ok && r != want {
			return fmt.Sprintf("lane %d of %#x is %#x, the rule's %#x", i, x, r, want)
		}
	}
	return ""
}

// ruleNaNs32 and ruleNaNs64 are NaNs of each kind and sign, with the
// numbers that make NaNs of none.
var (
	ruleNaNs32 = []uint64{0x7fc00000, 0xffc00000, 0x7f800001, 0x7fa00005, 0xff812345, 0x7fc12345, 0xffc54321,
		0x7f800000, 0xff800000, 0, 1 << 31, 0x3f800000, 0xbf800000}
	ruleNaNs64 = []uint64{0x7ff8000000000000, 0xfff8000000000000, 0x7ff0000000000001, 0x7ff4000000000005,
		0xfff0000000012345, 0x7ff8000000012345, 0xfff8000000054321,
		0x7ff0000000000000, 0xfff0000000000000, 0, 1 << 63, 0x3ff0000000000000, 0xbff0000000000000}
)

// TestNaNRule holds the float operations to the NaN rule of nan.go: every
// pair of NaNs and numbers in both orders for the scalar operations and
// the lanes, every triple for the multiply-adds.
func TestNaNRule(t *testing.T) {
	var src strings.Builder
	src.WriteString("(module\n")
	for _, ty := range []string{"f32", "f64"} {
		for _, n := range []string{"add", "sub", "mul", "div", "min", "max"} {
			fmt.Fprintf(&src, "(func (export %q) (param %s %s) (result %s) (%s.%s (local.get 0) (local.get 1)))\n",
				ty+"."+n, ty, ty, ty, ty, n)
		}
		for _, n := range []string{"sqrt", "ceil", "floor", "trunc", "nearest"} {
			fmt.Fprintf(&src, "(func (export %q) (param %s) (result %s) (%s.%s (local.get 0)))\n", ty+"."+n, ty, ty, ty, n)
		}
	}
	src.WriteString(`(func (export "f32.demote_f64") (param f64) (result f32) (f32.demote_f64 (local.get 0)))
(func (export "f64.promote_f32") (param f32) (result f64) (f64.promote_f32 (local.get 0)))
(func $v (param i64 i64) (result v128) (i64x2.replace_lane 1 (i64x2.splat (local.get 0)) (local.get 1)))
`)
	vops := []opcode.Opcode{opcode.F32x4Add, opcode.F32x4Sub, opcode.F32x4Mul, opcode.F32x4Div, opcode.F32x4Min,
		opcode.F32x4Max, opcode.F32x4Pmin, opcode.F32x4Pmax, opcode.F32x4RelaxedMin, opcode.F32x4RelaxedMax,
		opcode.F32x4Sqrt, opcode.F32x4Ceil, opcode.F32x4NearestInt, opcode.F32x4Qfma, opcode.F32x4Qfms,
		opcode.F64x2Add, opcode.F64x2Sub, opcode.F64x2Mul, opcode.F64x2Div, opcode.F64x2Min,
		opcode.F64x2Max, opcode.F64x2Pmin, opcode.F64x2Pmax, opcode.F64x2RelaxedMin, opcode.F64x2RelaxedMax,
		opcode.F64x2Sqrt, opcode.F64x2Floor, opcode.F64x2Trunc, opcode.F64x2Qfma, opcode.F64x2Qfms}
	arity := func(op opcode.Opcode) int { return len(opcode.Signature(op).Params) }
	for _, op := range vops {
		fmt.Fprintf(&src, "(func (export %q) (param i64 i64 i64 i64 i64 i64) (result i64 i64) (local v128)\n", op.Name())
		fmt.Fprintf(&src, "  (local.set 6 (%s", strings.NewReplacer("qfma", "relaxed_madd", "qfms", "relaxed_nmadd").Replace(op.Name()))
		for i := range arity(op) {
			fmt.Fprintf(&src, " (call $v (local.get %d) (local.get %d))", 2*i, 2*i+1)
		}
		src.WriteString("))\n  (i64x2.extract_lane 0 (local.get 6)) (i64x2.extract_lane 1 (local.get 6)))\n")
	}
	src.WriteString(")")
	inst := benchInstance(t, src.String())
	run := func(name string, args ...uint64) []Val {
		t.Helper()
		ext, ok := inst.Export(name)
		if !ok {
			t.Fatalf("no export %q", name)
		}
		vals := make([]Val, len(args))
		for i, a := range args {
			vals[i] = Val{N: a}
		}
		res, err := Invoke(context.Background(), ext.Func, vals)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	fails := 0
	fail := func(format string, args ...any) {
		t.Helper()
		if fails++; fails <= 30 {
			t.Errorf(format, args...)
		}
	}

	for _, w := range []int{32, 64} {
		ty, vals := "f32", ruleNaNs32
		if w == 64 {
			ty, vals = "f64", ruleNaNs64
		}
		mask := uint64(1)<<w - 1
		for _, n := range []string{"add", "sub", "mul", "div", "min", "max"} {
			for _, a := range vals {
				for _, b := range vals {
					r := run(ty+"."+n, a, b)[0].N & mask
					if want, ok := ruleArith(a, b, r, w); ok && r != want {
						fail("%s.%s(%#x, %#x) = %#x, the rule's %#x", ty, n, a, b, r, want)
					}
				}
			}
		}
		for _, n := range []string{"sqrt", "ceil", "floor", "trunc", "nearest"} {
			for _, a := range vals {
				r := run(ty+"."+n, a)[0].N & mask
				if want, ok := ruleUnary(a, r, w); ok && r != want {
					fail("%s.%s(%#x) = %#x, the rule's %#x", ty, n, a, r, want)
				}
			}
		}
		if r := run(ty+".sqrt", vals[len(vals)-1])[0].N & mask; r != defaultNaNBits(w) {
			fail("%s.sqrt(-1) = %#x, the default NaN %#x", ty, r, defaultNaNBits(w))
		}
	}
	for _, a := range ruleNaNs64[:7] {
		if r, want := run("f32.demote_f64", a)[0].N&(1<<32-1), ruleDemote(a); r != want {
			fail("f32.demote_f64(%#x) = %#x, the rule's %#x", a, r, want)
		}
	}
	for _, a := range ruleNaNs32[:7] {
		if r, want := run("f64.promote_f32", a)[0].N, rulePromote(a); r != want {
			fail("f64.promote_f32(%#x) = %#x, the rule's %#x", a, r, want)
		}
	}

	// The lanes: each lane a different operand, so that one call has every
	// pairing of a lane's operands with the others' in its lanes.
	for _, op := range vops {
		w, vals := 32, ruleNaNs32
		if strings.HasPrefix(op.Name(), "f64x2") {
			w, vals = 64, ruleNaNs64
		}
		splat := func(x uint64) V128 {
			if w == 32 {
				x |= x << 32
			}
			return V128{x, x}
		}
		var argss [][]V128
		switch arity(op) {
		case 1:
			for _, a := range vals {
				argss = append(argss, []V128{splat(a)})
			}
		case 2:
			for _, a := range vals {
				for _, b := range vals {
					argss = append(argss, []V128{splat(a), splat(b)})
				}
			}
		case 3:
			for _, a := range vals {
				for _, b := range vals {
					for _, c := range vals {
						argss = append(argss, []V128{splat(a), splat(b), splat(c)})
					}
				}
			}
		}
		for _, args := range argss {
			in := make([]uint64, 6)
			for k, a := range args {
				in[2*k], in[2*k+1] = a.Lo, a.Hi
			}
			res := run(op.Name(), in...)
			if msg := ruleLanes(op, args, V128{res[0].N, res[1].N}); msg != "" {
				fail("%s: %s", op.Name(), msg)
			}
		}
	}
	if fails > 0 {
		t.Fatalf("%d results break the rule", fails)
	}
}
