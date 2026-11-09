// Command simdgen writes the engine's ops_simd_gen.go: the interpreter's
// nodes for the SIMD operators without immediates, and the lane-wise
// functions of those whose lanes are computed alike.
//
// A node calls the operator's function on its operands. Calling a closure
// for an operand that is a slot of the frame is an indirect call it does not
// need, so each operator gets a node for each pairing of a slot read in
// place and any other operand. A lane-wise operator is written as the
// expression of one result lane over operand lanes a and b, and its
// function unpacks the operands into lanes, computes each and packs them.
//
// Usage: go run ./wasm/internal/engine/internal/simdgen OUTPUT
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
)

// lane is a lane shape: its Go type, its count, and the V128 method and
// function that unpack and pack an array of lanes.
type lane struct {
	gotype       string
	n, bits      int
	unpack, pack string
}

var (
	l8  = lane{"uint8", 16, 8, "u8", "fromU8"}
	l16 = lane{"uint16", 8, 16, "u16", "fromU16"}
	l32 = lane{"uint32", 4, 32, "u32", "fromU32"}
	l64 = lane{"uint64", 2, 64, "u64", "fromU64"}
	f32 = lane{"float32", 4, 32, "f32", "fromF32"}
	f64 = lane{"float64", 2, 64, "f64", "fromF64"}
)

// kind is an operator's signature.
type kind int

const (
	unary   kind = iota // v128 -> v128
	binary              // v128 v128 -> v128
	ternary             // v128 v128 v128 -> v128
	shift               // v128 i32 -> v128
	test                // v128 -> i32
)

// op is an operator: its opcode's name, its signature, and either the
// function computing it or, for a lane-wise one, its lane shapes and the
// expression of a result lane.
type op struct {
	name    string
	kind    kind
	fn      string
	in, out lane
	expr    string
}

func (o op) function() string {
	if o.fn != "" {
		return o.fn
	}
	return "v" + o.name
}

func un(name, fn string) op         { return op{name: name, kind: unary, fn: fn} }
func bin(name, fn string) op        { return op{name: name, kind: binary, fn: fn} }
func tern(name, fn string) op       { return op{name: name, kind: ternary, fn: fn} }
func tst(name, fn string) op        { return op{name: name, kind: test, fn: fn} }
func lun(l lane, name, e string) op { return op{name: name, kind: unary, in: l, out: l, expr: e} }
func lbin(l lane, name, e string) op {
	return op{name: name, kind: binary, in: l, out: l, expr: e}
}
func lcmp(in, out lane, name, e string) op {
	return op{name: name, kind: binary, in: in, out: out, expr: e}
}
func lconv(in, out lane, name, e string) op {
	return op{name: name, kind: unary, in: in, out: out, expr: e}
}
func lshift(l lane, name, e string) op {
	return op{name: name, kind: shift, in: l, out: l, expr: e}
}

// intCmps returns the comparisons of an integer shape, with the unsigned
// ones if unsigned.
func intCmps(l lane, prefix, mask, signed string, unsigned bool) []op {
	sa, sb := signed+"(a)", signed+"(b)"
	ops := []op{
		lcmp(l, l, prefix+"Eq", mask+"(a == b)"),
		lcmp(l, l, prefix+"Ne", mask+"(a != b)"),
		lcmp(l, l, prefix+"LtS", mask+"("+sa+" < "+sb+")"),
		lcmp(l, l, prefix+"GtS", mask+"("+sa+" > "+sb+")"),
		lcmp(l, l, prefix+"LeS", mask+"("+sa+" <= "+sb+")"),
		lcmp(l, l, prefix+"GeS", mask+"("+sa+" >= "+sb+")"),
	}
	if unsigned {
		ops = append(ops,
			lcmp(l, l, prefix+"LtU", mask+"(a < b)"),
			lcmp(l, l, prefix+"GtU", mask+"(a > b)"),
			lcmp(l, l, prefix+"LeU", mask+"(a <= b)"),
			lcmp(l, l, prefix+"GeU", mask+"(a >= b)"),
		)
	}
	return ops
}

func floatCmps(l, out lane, prefix, mask string) []op {
	return []op{
		lcmp(l, out, prefix+"Eq", mask+"(a == b)"),
		lcmp(l, out, prefix+"Ne", mask+"(a != b)"),
		lcmp(l, out, prefix+"Lt", mask+"(a < b)"),
		lcmp(l, out, prefix+"Gt", mask+"(a > b)"),
		lcmp(l, out, prefix+"Le", mask+"(a <= b)"),
		lcmp(l, out, prefix+"Ge", mask+"(a >= b)"),
	}
}

func intMinMax(l lane, prefix, signed string) []op {
	t := l.gotype
	return []op{
		lbin(l, prefix+"MinS", t+"(min("+signed+"(a), "+signed+"(b)))"),
		lbin(l, prefix+"MinU", "min(a, b)"),
		lbin(l, prefix+"MaxS", t+"(max("+signed+"(a), "+signed+"(b)))"),
		lbin(l, prefix+"MaxU", "max(a, b)"),
	}
}

func shifts(l lane, prefix, signed string) []op {
	return []op{
		lshift(l, prefix+"Shl", "a << s"),
		lshift(l, prefix+"ShrS", l.gotype+"("+signed+"(a) >> s)"),
		lshift(l, prefix+"ShrU", "a >> s"),
	}
}

func floatArith(l lane, prefix, sz string) []op {
	return []op{
		lbin(l, prefix+"Add", "a + b"),
		lbin(l, prefix+"Sub", "a - b"),
		lbin(l, prefix+"Mul", "a * b"),
		lbin(l, prefix+"Div", "a / b"),
		lbin(l, prefix+"Min", "fmin"+sz+"(a, b)"),
		lbin(l, prefix+"Max", "fmax"+sz+"(a, b)"),
		lbin(l, prefix+"Pmin", "pmin"+sz+"(a, b)"),
		lbin(l, prefix+"Pmax", "pmax"+sz+"(a, b)"),
		lun(l, prefix+"Ceil", "fceil"+sz+"(a)"),
		lun(l, prefix+"Floor", "ffloor"+sz+"(a)"),
		lun(l, prefix+"Trunc", "ftrunc"+sz+"(a)"),
		lun(l, prefix+"NearestInt", "fnearest"+sz+"(a)"),
	}
}

func ops() []op {
	var o []op
	add := func(x ...op) { o = append(o, x...) }

	// Bitwise.
	add(un("S128Not", "vNot"), bin("S128And", "vAnd"), bin("S128AndNot", "vAndNot"),
		bin("S128Or", "vOr"), bin("S128Xor", "vXor"), tern("S128Select", "vBitselect"),
		tst("V128AnyTrue", "vAnyTrue"))

	// i8x16.
	add(bin("I8x16Swizzle", ""))
	add(intCmps(l8, "I8x16", "mask8", "int8", true)...)
	add(lun(l8, "I8x16Abs", "abs8(a)"), un("I8x16Neg", ""),
		lun(l8, "I8x16Popcnt", "uint8(bits.OnesCount8(a))"),
		tst("I8x16AllTrue", ""), tst("I8x16BitMask", ""),
		bin("I8x16SConvertI16x8", "vI8x16NarrowS"), bin("I8x16UConvertI16x8", "vI8x16NarrowU"))
	add(shifts(l8, "I8x16", "int8")...)
	add(bin("I8x16Add", ""), bin("I8x16Sub", ""),
		lbin(l8, "I8x16AddSatS", "sat8s(int(int8(a)) + int(int8(b)))"),
		lbin(l8, "I8x16AddSatU", "sat8u(int(a) + int(b))"),
		lbin(l8, "I8x16SubSatS", "sat8s(int(int8(a)) - int(int8(b)))"),
		lbin(l8, "I8x16SubSatU", "sat8u(int(a) - int(b))"))
	add(intMinMax(l8, "I8x16", "int8")...)
	add(lbin(l8, "I8x16RoundingAverageU", "uint8((uint(a) + uint(b) + 1) >> 1)"))

	// i16x8.
	add(intCmps(l16, "I16x8", "mask16", "int16", true)...)
	add(un("I16x8ExtAddPairwiseI8x16S", "vI16x8ExtAddPairwiseS"),
		un("I16x8ExtAddPairwiseI8x16U", "vI16x8ExtAddPairwiseU"),
		lun(l16, "I16x8Abs", "abs16(a)"), un("I16x8Neg", ""),
		lbin(l16, "I16x8Q15MulRSatS", "sat16s((int(int16(a))*int(int16(b)) + 0x4000) >> 15)"),
		tst("I16x8AllTrue", ""), tst("I16x8BitMask", ""),
		bin("I16x8SConvertI32x4", "vI16x8NarrowS"), bin("I16x8UConvertI32x4", "vI16x8NarrowU"),
		un("I16x8SConvertI8x16Low", "vI16x8ExtendLowS"), un("I16x8SConvertI8x16High", "vI16x8ExtendHighS"),
		un("I16x8UConvertI8x16Low", "vI16x8ExtendLowU"), un("I16x8UConvertI8x16High", "vI16x8ExtendHighU"))
	add(shifts(l16, "I16x8", "int16")...)
	add(bin("I16x8Add", ""), bin("I16x8Sub", ""),
		lbin(l16, "I16x8AddSatS", "sat16s(int(int16(a)) + int(int16(b)))"),
		lbin(l16, "I16x8AddSatU", "sat16u(int(a) + int(b))"),
		lbin(l16, "I16x8SubSatS", "sat16s(int(int16(a)) - int(int16(b)))"),
		lbin(l16, "I16x8SubSatU", "sat16u(int(a) - int(b))"),
		lbin(l16, "I16x8Mul", "a * b"))
	add(intMinMax(l16, "I16x8", "int16")...)
	add(lbin(l16, "I16x8RoundingAverageU", "uint16((uint(a) + uint(b) + 1) >> 1)"),
		bin("I16x8ExtMulLowI8x16S", "vI16x8ExtMulLowS"), bin("I16x8ExtMulHighI8x16S", "vI16x8ExtMulHighS"),
		bin("I16x8ExtMulLowI8x16U", "vI16x8ExtMulLowU"), bin("I16x8ExtMulHighI8x16U", "vI16x8ExtMulHighU"))

	// i32x4.
	add(intCmps(l32, "I32x4", "mask32", "int32", true)...)
	add(un("I32x4ExtAddPairwiseI16x8S", "vI32x4ExtAddPairwiseS"),
		un("I32x4ExtAddPairwiseI16x8U", "vI32x4ExtAddPairwiseU"),
		lun(l32, "I32x4Abs", "abs32(a)"), un("I32x4Neg", ""),
		tst("I32x4AllTrue", ""), tst("I32x4BitMask", ""),
		un("I32x4SConvertI16x8Low", "vI32x4ExtendLowS"), un("I32x4SConvertI16x8High", "vI32x4ExtendHighS"),
		un("I32x4UConvertI16x8Low", "vI32x4ExtendLowU"), un("I32x4UConvertI16x8High", "vI32x4ExtendHighU"))
	add(shifts(l32, "I32x4", "int32")...)
	add(bin("I32x4Add", ""), bin("I32x4Sub", ""), lbin(l32, "I32x4Mul", "a * b"))
	add(intMinMax(l32, "I32x4", "int32")...)
	add(bin("I32x4DotI16x8S", "vI32x4DotS"),
		bin("I32x4ExtMulLowI16x8S", "vI32x4ExtMulLowS"), bin("I32x4ExtMulHighI16x8S", "vI32x4ExtMulHighS"),
		bin("I32x4ExtMulLowI16x8U", "vI32x4ExtMulLowU"), bin("I32x4ExtMulHighI16x8U", "vI32x4ExtMulHighU"))

	// i64x2.
	add(intCmps(l64, "I64x2", "mask64", "int64", false)...)
	add(lun(l64, "I64x2Abs", "abs64(a)"), un("I64x2Neg", ""),
		tst("I64x2AllTrue", ""), tst("I64x2BitMask", ""),
		un("I64x2SConvertI32x4Low", "vI64x2ExtendLowS"), un("I64x2SConvertI32x4High", "vI64x2ExtendHighS"),
		un("I64x2UConvertI32x4Low", "vI64x2ExtendLowU"), un("I64x2UConvertI32x4High", "vI64x2ExtendHighU"))
	add(shifts(l64, "I64x2", "int64")...)
	add(bin("I64x2Add", ""), bin("I64x2Sub", ""), bin("I64x2Mul", ""),
		bin("I64x2ExtMulLowI32x4S", "vI64x2ExtMulLowS"), bin("I64x2ExtMulHighI32x4S", "vI64x2ExtMulHighS"),
		bin("I64x2ExtMulLowI32x4U", "vI64x2ExtMulLowU"), bin("I64x2ExtMulHighI32x4U", "vI64x2ExtMulHighU"))

	// f32x4 and f64x2.
	add(floatCmps(f32, l32, "F32x4", "mask32")...)
	add(floatCmps(f64, l64, "F64x2", "mask64")...)
	add(un("F32x4Abs", ""), un("F32x4Neg", ""), lun(f32, "F32x4Sqrt", "fsqrt32(a)"))
	add(floatArith(f32, "F32x4", "32")...)
	add(un("F64x2Abs", ""), un("F64x2Neg", ""), lun(f64, "F64x2Sqrt", "math.Sqrt(a)"))
	add(floatArith(f64, "F64x2", "64")...)

	// Conversions.
	add(un("F32x4DemoteF64x2Zero", "vF32x4DemoteZero"), un("F64x2PromoteLowF32x4", "vF64x2PromoteLow"),
		lconv(f32, l32, "I32x4SConvertF32x4", "satS32F32(a)"),
		lconv(f32, l32, "I32x4UConvertF32x4", "satU32F32(a)"),
		lconv(l32, f32, "F32x4SConvertI32x4", "float32(int32(a))"),
		lconv(l32, f32, "F32x4UConvertI32x4", "float32(a)"),
		un("I32x4TruncSatF64x2SZero", ""), un("I32x4TruncSatF64x2UZero", ""),
		un("F64x2ConvertLowI32x4S", "vF64x2ConvertLowS"), un("F64x2ConvertLowI32x4U", "vF64x2ConvertLowU"))

	// Relaxed SIMD: the functions are the host architecture's choices.
	add(bin("I8x16RelaxedSwizzle", "relaxedSwizzle"),
		un("I32x4RelaxedTruncF32x4S", "relaxedTruncF32x4S"),
		un("I32x4RelaxedTruncF32x4U", "relaxedTruncF32x4U"),
		un("I32x4RelaxedTruncF64x2SZero", "relaxedTruncF64x2SZero"),
		un("I32x4RelaxedTruncF64x2UZero", "relaxedTruncF64x2UZero"),
		tern("F32x4Qfma", "relaxedMaddF32x4"), tern("F32x4Qfms", "relaxedNmaddF32x4"),
		tern("F64x2Qfma", "relaxedMaddF64x2"), tern("F64x2Qfms", "relaxedNmaddF64x2"),
		tern("I8x16RelaxedLaneSelect", "relaxedLaneselect8"),
		tern("I16x8RelaxedLaneSelect", "relaxedLaneselect16"),
		tern("I32x4RelaxedLaneSelect", "relaxedLaneselect32"),
		tern("I64x2RelaxedLaneSelect", "relaxedLaneselect64"),
		bin("F32x4RelaxedMin", "relaxedMinF32x4"), bin("F32x4RelaxedMax", "relaxedMaxF32x4"),
		bin("F64x2RelaxedMin", "relaxedMinF64x2"), bin("F64x2RelaxedMax", "relaxedMaxF64x2"),
		bin("I16x8RelaxedQ15MulRS", "relaxedQ15MulR"),
		bin("I16x8DotI8x16I7x16S", "relaxedDotI8x16I7x16S"),
		tern("I32x4DotI8x16I7x16AddS", "relaxedDotI8x16I7x16AddS"))
	return o
}

// operand is how a node reads a v128 operand: in place from a slot, or by
// the reader c.v128 returns.
type operand bool

const (
	inSlot operand = true
	viaFn  operand = false
)

func (k operand) test(e string) string {
	if k == inSlot {
		return e + ".k == eSlot"
	}
	return e + ".k != eSlot"
}

func (k operand) capture(v, e string) string {
	if k == inSlot {
		return v + " := " + e + ".slot"
	}
	return v + " := c.v128(" + e + ")"
}

func (k operand) read(v string) string {
	if k == inSlot {
		return "V128{f.l[" + v + "], f.l[" + v + "+1]}"
	}
	return v + "(f)"
}

func node(b *bytes.Buffer, o op) {
	fmt.Fprintf(b, "case opcode.%s:\n", o.name)
	fn := o.function()
	switch o.kind {
	case unary, test:
		res, tree, gt := "module.V128", "tv128", "V128"
		if o.kind == test {
			res, tree, gt = "module.I32", "ti32", "uint32"
		}
		b.WriteString("x := c.pop()\nif x.k == eSlot {\n")
		fmt.Fprintf(b, "%s\n", inSlot.capture("xa", "x"))
		fmt.Fprintf(b, "c.pushTree(%s, %s(func(f *frame) %s { return %s(%s) }))\n", res, tree, gt, fn, inSlot.read("xa"))
		b.WriteString("} else {\n")
		fmt.Fprintf(b, "%s\n", viaFn.capture("xa", "x"))
		fmt.Fprintf(b, "c.pushTree(%s, %s(func(f *frame) %s { return %s(%s) }))\n", res, tree, gt, fn, viaFn.read("xa"))
		b.WriteString("}\n")
	case binary:
		b.WriteString("y := c.pop()\nx := c.pop()\nswitch {\n")
		for _, p := range [][2]operand{{inSlot, inSlot}, {inSlot, viaFn}, {viaFn, inSlot}, {viaFn, viaFn}} {
			fmt.Fprintf(b, "case %s && %s:\n", p[0].test("x"), p[1].test("y"))
			fmt.Fprintf(b, "%s\n%s\n", p[0].capture("xa", "x"), p[1].capture("yb", "y"))
			fmt.Fprintf(b, "c.pushTree(module.V128, tv128(func(f *frame) V128 { return %s(%s, %s) }))\n",
				fn, p[0].read("xa"), p[1].read("yb"))
		}
		b.WriteString("}\n")
	case ternary:
		b.WriteString("z := c.v128(c.pop())\ny := c.v128(c.pop())\nx := c.v128(c.pop())\n")
		fmt.Fprintf(b, "c.pushTree(module.V128, tv128(func(f *frame) V128 { return %s(x(f), y(f), z(f)) }))\n", fn)
	case shift:
		b.WriteString("y := c.pop()\nx := c.pop()\nswitch {\n")
		for _, k := range []operand{inSlot, viaFn} {
			fmt.Fprintf(b, "case y.k == eConst && %s:\n", k.test("x"))
			fmt.Fprintf(b, "%s\ns := uint32(y.c)\n", k.capture("xa", "x"))
			fmt.Fprintf(b, "c.pushTree(module.V128, tv128(func(f *frame) V128 { return %s(%s, s) }))\n", fn, k.read("xa"))
		}
		b.WriteString("default:\nxa, s := c.v128(x), c.i32(y)\n")
		fmt.Fprintf(b, "c.pushTree(module.V128, tv128(func(f *frame) V128 { return %s(xa(f), s(f)) }))\n", fn)
		b.WriteString("}\n")
	}
}

// laneFunc writes the function of a lane-wise operator.
func laneFunc(b *bytes.Buffer, o op) {
	r := fmt.Sprintf("var r [%d]%s\n", o.out.n, o.out.gotype)
	switch o.kind {
	case unary:
		fmt.Fprintf(b, "func %s(x V128) V128 {\nxs := x.%s()\n%s", o.function(), o.in.unpack, r)
		fmt.Fprintf(b, "for i, a := range xs {\nr[i] = %s\n}\n", o.expr)
	case binary:
		fmt.Fprintf(b, "func %s(x, y V128) V128 {\nxs, ys := x.%s(), y.%s()\n%s", o.function(), o.in.unpack, o.in.unpack, r)
		fmt.Fprintf(b, "for i := range r {\na, b := xs[i], ys[i]\nr[i] = %s\n}\n", o.expr)
	case shift:
		fmt.Fprintf(b, "func %s(x V128, s uint32) V128 {\nxs := x.%s()\ns &= %d\n%s", o.function(), o.in.unpack, o.in.bits-1, r)
		fmt.Fprintf(b, "for i, a := range xs {\nr[i] = %s\n}\n", o.expr)
	}
	fmt.Fprintf(b, "return %s(r)\n}\n\n", o.out.pack)
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: simdgen OUTPUT")
		os.Exit(2)
	}
	all := ops()
	var b bytes.Buffer
	b.WriteString(`// Code generated by internal/simdgen; DO NOT EDIT.

package engine

import (
	"math"
	"math/bits"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

var (
	_ = bits.Len
	_ = math.Sqrt
)

// simdOp compiles a SIMD operator without immediates, reporting whether op
// is one.
func (c *compiler) simdOp(op opcode.Opcode) bool {
	switch op {
`)
	for _, o := range all {
		node(&b, o)
	}
	b.WriteString("default:\nreturn false\n}\nreturn true\n}\n\n")
	for _, o := range all {
		if o.expr != "" {
			laneFunc(&b, o)
		}
	}
	src, err := format.Source(b.Bytes())
	if err != nil {
		os.Stdout.Write(b.Bytes())
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[1], src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
