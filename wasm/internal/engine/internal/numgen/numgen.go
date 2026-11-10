// Command numgen writes the engine's ops_num_gen.go: the interpreter's nodes
// for the numeric operators.
//
// A node is a closure, and calling a closure for an operand is an indirect
// call most operands -- a local, a constant -- do not need. So each binary
// operator gets a node for each pairing of operand kinds, each reading its
// operands in place: a tree is called, a slot read from the frame, a
// constant captured. There are too many to keep alike by hand.
//
// Usage: go run ./wasm/internal/engine/internal/numgen OUTPUT
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"strings"
)

// typ is a value type of the operators.
type typ struct {
	name   string // the opcode package's type prefix: I32, I64, F32, F64
	gotype string // the Go type a node returns
	tree   string // the tree type
	read   string // reading slot s of the frame, as the Go type
	konst  string // the Go expression converting entry field c to the Go type
	mtype  string // the module type
}

var (
	i32 = typ{"I32", "uint32", "ti32", "uint32(f.l[%s])", "uint32(%s.c)", "module.I32"}
	i64 = typ{"I64", "uint64", "ti64", "f.l[%s]", "%s.c", "module.I64"}
	f32 = typ{"F32", "float32", "tf32", "math.Float32frombits(uint32(f.l[%s]))", "math.Float32frombits(uint32(%s.c))", "module.F32"}
	f64 = typ{"F64", "float64", "tf64", "math.Float64frombits(f.l[%s])", "math.Float64frombits(%s.c)", "module.F64"}
)

// op is an operator: its opcode's name, its operand and result types, and
// the Go expression of its result over a (and b) -- and at, the
// instruction's offset, for an operator that traps.
type op struct {
	name     string
	arg, res typ
	expr     string
}

func bin(t typ, name, expr string) op    { return op{name, t, t, expr} }
func cmp(t typ, name, expr string) op    { return op{name, t, i32, "b2i(" + expr + ")"} }
func un(t typ, name, expr string) op     { return op{name, t, t, expr} }
func conv(a, r typ, name, e string) op   { return op{name, a, r, e} }
func test(t typ, name, expr string) op   { return op{name, t, i32, "b2i(" + expr + ")"} }
func binTrap(t typ, name, e string) op   { return op{name, t, t, e} }
func floatBin(t typ, name, e string) op  { return op{name, t, t, e} }
func floatUn(t typ, name, e string) op   { return op{name, t, t, e} }
func floatCmp(t typ, name, e string) op  { return op{name, t, i32, "b2i(" + e + ")"} }
func floatConv(a, r typ, n, e string) op { return op{n, a, r, e} }

var binaries = []op{
	bin(i32, "I32Add", "a + b"),
	bin(i32, "I32Sub", "a - b"),
	bin(i32, "I32Mul", "a * b"),
	bin(i32, "I32And", "a & b"),
	bin(i32, "I32Ior", "a | b"),
	bin(i32, "I32Xor", "a ^ b"),
	bin(i32, "I32Shl", "a << (b & 31)"),
	bin(i32, "I32ShrS", "uint32(int32(a) >> (b & 31))"),
	bin(i32, "I32ShrU", "a >> (b & 31)"),
	bin(i32, "I32Rol", "bits.RotateLeft32(a, int(b&31))"),
	bin(i32, "I32Ror", "bits.RotateLeft32(a, -int(b&31))"),
	binTrap(i32, "I32DivS", "divS32(a, b, at)"),
	binTrap(i32, "I32DivU", "divU32(a, b, at)"),
	binTrap(i32, "I32RemS", "remS32(a, b, at)"),
	binTrap(i32, "I32RemU", "remU32(a, b, at)"),
	cmp(i32, "I32Eq", "a == b"),
	cmp(i32, "I32Ne", "a != b"),
	cmp(i32, "I32LtS", "int32(a) < int32(b)"),
	cmp(i32, "I32LtU", "a < b"),
	cmp(i32, "I32GtS", "int32(a) > int32(b)"),
	cmp(i32, "I32GtU", "a > b"),
	cmp(i32, "I32LeS", "int32(a) <= int32(b)"),
	cmp(i32, "I32LeU", "a <= b"),
	cmp(i32, "I32GeS", "int32(a) >= int32(b)"),
	cmp(i32, "I32GeU", "a >= b"),

	bin(i64, "I64Add", "a + b"),
	bin(i64, "I64Sub", "a - b"),
	bin(i64, "I64Mul", "a * b"),
	bin(i64, "I64And", "a & b"),
	bin(i64, "I64Ior", "a | b"),
	bin(i64, "I64Xor", "a ^ b"),
	bin(i64, "I64Shl", "a << (b & 63)"),
	bin(i64, "I64ShrS", "uint64(int64(a) >> (b & 63))"),
	bin(i64, "I64ShrU", "a >> (b & 63)"),
	bin(i64, "I64Rol", "bits.RotateLeft64(a, int(b&63))"),
	bin(i64, "I64Ror", "bits.RotateLeft64(a, -int(b&63))"),
	binTrap(i64, "I64DivS", "divS64(a, b, at)"),
	binTrap(i64, "I64DivU", "divU64(a, b, at)"),
	binTrap(i64, "I64RemS", "remS64(a, b, at)"),
	binTrap(i64, "I64RemU", "remU64(a, b, at)"),
	cmp(i64, "I64Eq", "a == b"),
	cmp(i64, "I64Ne", "a != b"),
	cmp(i64, "I64LtS", "int64(a) < int64(b)"),
	cmp(i64, "I64LtU", "a < b"),
	cmp(i64, "I64GtS", "int64(a) > int64(b)"),
	cmp(i64, "I64GtU", "a > b"),
	cmp(i64, "I64LeS", "int64(a) <= int64(b)"),
	cmp(i64, "I64LeU", "a <= b"),
	cmp(i64, "I64GeS", "int64(a) >= int64(b)"),
	cmp(i64, "I64GeU", "a >= b"),

	floatBin(f32, "F32Add", "fadd32(a, b)"),
	floatBin(f32, "F32Sub", "fsub32(a, b)"),
	floatBin(f32, "F32Mul", "fmul32(a, b)"),
	floatBin(f32, "F32Div", "fdiv32(a, b)"),
	floatBin(f32, "F32Min", "fmin32(a, b)"),
	floatBin(f32, "F32Max", "fmax32(a, b)"),
	floatBin(f32, "F32CopySign", "math.Float32frombits(math.Float32bits(a)&0x7fffffff | math.Float32bits(b)&0x80000000)"),
	floatCmp(f32, "F32Eq", "a == b"),
	floatCmp(f32, "F32Ne", "a != b"),
	floatCmp(f32, "F32Lt", "a < b"),
	floatCmp(f32, "F32Gt", "a > b"),
	floatCmp(f32, "F32Le", "a <= b"),
	floatCmp(f32, "F32Ge", "a >= b"),

	floatBin(f64, "F64Add", "fadd64(a, b)"),
	floatBin(f64, "F64Sub", "fsub64(a, b)"),
	floatBin(f64, "F64Mul", "fmul64(a, b)"),
	floatBin(f64, "F64Div", "fdiv64(a, b)"),
	floatBin(f64, "F64Min", "fmin64(a, b)"),
	floatBin(f64, "F64Max", "fmax64(a, b)"),
	floatBin(f64, "F64CopySign", "math.Float64frombits(math.Float64bits(a)&0x7fffffffffffffff | math.Float64bits(b)&0x8000000000000000)"),
	floatCmp(f64, "F64Eq", "a == b"),
	floatCmp(f64, "F64Ne", "a != b"),
	floatCmp(f64, "F64Lt", "a < b"),
	floatCmp(f64, "F64Gt", "a > b"),
	floatCmp(f64, "F64Le", "a <= b"),
	floatCmp(f64, "F64Ge", "a >= b"),
}

var unaries = []op{
	test(i32, "I32Eqz", "a == 0"),
	un(i32, "I32Clz", "uint32(bits.LeadingZeros32(a))"),
	un(i32, "I32Ctz", "uint32(bits.TrailingZeros32(a))"),
	un(i32, "I32Popcnt", "uint32(bits.OnesCount32(a))"),
	un(i32, "I32SExtendI8", "uint32(int32(int8(a)))"),
	un(i32, "I32SExtendI16", "uint32(int32(int16(a)))"),
	test(i64, "I64Eqz", "a == 0"),
	un(i64, "I64Clz", "uint64(bits.LeadingZeros64(a))"),
	un(i64, "I64Ctz", "uint64(bits.TrailingZeros64(a))"),
	un(i64, "I64Popcnt", "uint64(bits.OnesCount64(a))"),
	un(i64, "I64SExtendI8", "uint64(int64(int8(a)))"),
	un(i64, "I64SExtendI16", "uint64(int64(int16(a)))"),
	un(i64, "I64SExtendI32", "uint64(int64(int32(a)))"),

	floatUn(f32, "F32Abs", "math.Float32frombits(math.Float32bits(a) &^ 0x80000000)"),
	floatUn(f32, "F32Neg", "math.Float32frombits(math.Float32bits(a) ^ 0x80000000)"),
	floatUn(f32, "F32Ceil", "fceil32(a)"),
	floatUn(f32, "F32Floor", "ffloor32(a)"),
	floatUn(f32, "F32Trunc", "ftrunc32(a)"),
	floatUn(f32, "F32NearestInt", "fnearest32(a)"),
	floatUn(f32, "F32Sqrt", "float32(math.Sqrt(float64(a)))"),
	floatUn(f64, "F64Abs", "math.Float64frombits(math.Float64bits(a) &^ 0x8000000000000000)"),
	floatUn(f64, "F64Neg", "math.Float64frombits(math.Float64bits(a) ^ 0x8000000000000000)"),
	floatUn(f64, "F64Ceil", "fceil64(a)"),
	floatUn(f64, "F64Floor", "ffloor64(a)"),
	floatUn(f64, "F64Trunc", "ftrunc64(a)"),
	floatUn(f64, "F64NearestInt", "fnearest64(a)"),
	floatUn(f64, "F64Sqrt", "math.Sqrt(a)"),

	conv(i64, i32, "I32ConvertI64", "uint32(a)"),
	floatConv(f32, i32, "I32SConvertF32", "truncS32F32(a, at)"),
	floatConv(f32, i32, "I32UConvertF32", "truncU32F32(a, at)"),
	floatConv(f64, i32, "I32SConvertF64", "truncS32F64(a, at)"),
	floatConv(f64, i32, "I32UConvertF64", "truncU32F64(a, at)"),
	conv(i32, i64, "I64SConvertI32", "uint64(int64(int32(a)))"),
	conv(i32, i64, "I64UConvertI32", "uint64(a)"),
	floatConv(f32, i64, "I64SConvertF32", "truncS64F32(a, at)"),
	floatConv(f32, i64, "I64UConvertF32", "truncU64F32(a, at)"),
	floatConv(f64, i64, "I64SConvertF64", "truncS64F64(a, at)"),
	floatConv(f64, i64, "I64UConvertF64", "truncU64F64(a, at)"),
	floatConv(i32, f32, "F32SConvertI32", "float32(int32(a))"),
	floatConv(i32, f32, "F32UConvertI32", "float32(a)"),
	floatConv(i64, f32, "F32SConvertI64", "i64ToF32(int64(a))"),
	floatConv(i64, f32, "F32UConvertI64", "u64ToF32(a)"),
	floatConv(f64, f32, "F32ConvertF64", "float32(a)"),
	floatConv(i32, f64, "F64SConvertI32", "float64(int32(a))"),
	floatConv(i32, f64, "F64UConvertI32", "float64(a)"),
	floatConv(i64, f64, "F64SConvertI64", "float64(int64(a))"),
	floatConv(i64, f64, "F64UConvertI64", "u64ToF64(a)"),
	floatConv(f32, f64, "F64ConvertF32", "float64(a)"),
	conv(f32, i32, "I32ReinterpretF32", "math.Float32bits(a)"),
	conv(f64, i64, "I64ReinterpretF64", "math.Float64bits(a)"),
	conv(i32, f32, "F32ReinterpretI32", "math.Float32frombits(a)"),
	conv(i64, f64, "F64ReinterpretI64", "math.Float64frombits(a)"),

	floatConv(f32, i32, "I32SConvertSatF32", "satS32F32(a)"),
	floatConv(f32, i32, "I32UConvertSatF32", "satU32F32(a)"),
	floatConv(f64, i32, "I32SConvertSatF64", "satS32F64(a)"),
	floatConv(f64, i32, "I32UConvertSatF64", "satU32F64(a)"),
	floatConv(f32, i64, "I64SConvertSatF32", "satS64F32(a)"),
	floatConv(f32, i64, "I64UConvertSatF32", "satU64F32(a)"),
	floatConv(f64, i64, "I64SConvertSatF64", "satS64F64(a)"),
	floatConv(f64, i64, "I64UConvertSatF64", "satU64F64(a)"),
}

// kind is how a node reads an operand.
type kind int

const (
	tree kind = iota
	slot
	konst
)

func (k kind) test(e string) string {
	switch k {
	case slot:
		return e + ".k == eSlot"
	case konst:
		return e + ".k == eConst"
	}
	return e + ".k == eTree"
}

// capture returns the statement capturing operand e, read as kind k, in v.
func capture(k kind, t typ, v, e string) string {
	switch k {
	case slot:
		return v + " := " + e + ".slot"
	case konst:
		return v + " := " + fmt.Sprintf(t.konst, e)
	}
	return v + " := " + e + ".tree.(" + t.tree + ")"
}

// read returns the expression reading operand v of kind k.
func read(k kind, t typ, v string) string {
	switch k {
	case slot:
		return fmt.Sprintf(t.read, v)
	case konst:
		return v
	}
	return v + "(f)"
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: numgen OUTPUT")
		os.Exit(2)
	}
	var b bytes.Buffer
	b.WriteString(`// Code generated by internal/numgen; DO NOT EDIT.

package engine

import (
	"math"
	"math/bits"

	"encoding/binary"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

var _ = bits.Len

// numeric compiles a numeric operator at offset at, reporting whether op is
// one.
func (c *compiler) numeric(op opcode.Opcode, at uint32) bool {
	switch op {
`)
	for _, o := range binaries {
		fmt.Fprintf(&b, "case opcode.%s:\n", o.name)
		fmt.Fprintf(&b, "y := c.pop()\nx := c.pop()\n")
		pairs := [][2]kind{{slot, slot}, {slot, konst}, {tree, konst}, {slot, tree}, {tree, slot}, {konst, tree}, {konst, slot}}
		b.WriteString("switch {\n")
		for _, p := range pairs {
			fmt.Fprintf(&b, "case %s && %s:\n", p[0].test("x"), p[1].test("y"))
			fmt.Fprintf(&b, "%s\n%s\n", capture(p[0], o.arg, "xa", "x"), capture(p[1], o.arg, "yb", "y"))
			fmt.Fprintf(&b, "c.pushTree(%s, %s(func(f *frame) %s { a, b := %s, %s; return %s }))\n",
				o.res.mtype, o.res.tree, o.res.gotype, read(p[0], o.arg, "xa"), read(p[1], o.arg, "yb"), o.expr)
		}
		b.WriteString("default:\n")
		fmt.Fprintf(&b, "xa, yb := c.%s(x), c.%s(y)\n", strings.ToLower(o.arg.name), strings.ToLower(o.arg.name))
		fmt.Fprintf(&b, "c.pushTree(%s, %s(func(f *frame) %s { a, b := xa(f), yb(f); return %s }))\n",
			o.res.mtype, o.res.tree, o.res.gotype, o.expr)
		b.WriteString("}\n")
	}
	for _, o := range unaries {
		fmt.Fprintf(&b, "case opcode.%s:\n", o.name)
		fmt.Fprintf(&b, "x := c.pop()\n")
		b.WriteString("if x.k == eSlot {\n")
		fmt.Fprintf(&b, "%s\n", capture(slot, o.arg, "xa", "x"))
		fmt.Fprintf(&b, "c.pushTree(%s, %s(func(f *frame) %s { a := %s; return %s }))\n",
			o.res.mtype, o.res.tree, o.res.gotype, read(slot, o.arg, "xa"), o.expr)
		b.WriteString("} else {\n")
		fmt.Fprintf(&b, "xa := c.%s(x)\n", strings.ToLower(o.arg.name))
		fmt.Fprintf(&b, "c.pushTree(%s, %s(func(f *frame) %s { a := xa(f); return %s }))\n",
			o.res.mtype, o.res.tree, o.res.gotype, o.expr)
		b.WriteString("}\n")
	}
	b.WriteString("default:\nreturn false\n}\nreturn true\n}\n")
	writeMemory(&b)
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

// access is a load or a store of memory 0: its opcode, its width, its value
// type, and the Go expression reading a value from, or the statement
// writing v to, the bytes b.
type access struct {
	name  string
	width int
	t     typ
	expr  string
}

var loads = []access{
	{"I32LoadMem", 4, i32, "binary.LittleEndian.Uint32(b)"},
	{"I64LoadMem", 8, i64, "binary.LittleEndian.Uint64(b)"},
	{"F32LoadMem", 4, f32, "math.Float32frombits(binary.LittleEndian.Uint32(b))"},
	{"F64LoadMem", 8, f64, "math.Float64frombits(binary.LittleEndian.Uint64(b))"},
	{"I32LoadMem8S", 1, i32, "uint32(int32(int8(b[0])))"},
	{"I32LoadMem8U", 1, i32, "uint32(b[0])"},
	{"I32LoadMem16S", 2, i32, "uint32(int32(int16(binary.LittleEndian.Uint16(b))))"},
	{"I32LoadMem16U", 2, i32, "uint32(binary.LittleEndian.Uint16(b))"},
	{"I64LoadMem8S", 1, i64, "uint64(int64(int8(b[0])))"},
	{"I64LoadMem8U", 1, i64, "uint64(b[0])"},
	{"I64LoadMem16S", 2, i64, "uint64(int64(int16(binary.LittleEndian.Uint16(b))))"},
	{"I64LoadMem16U", 2, i64, "uint64(binary.LittleEndian.Uint16(b))"},
	{"I64LoadMem32S", 4, i64, "uint64(int64(int32(binary.LittleEndian.Uint32(b))))"},
	{"I64LoadMem32U", 4, i64, "uint64(binary.LittleEndian.Uint32(b))"},
}

var stores = []access{
	{"I32StoreMem", 4, i32, "binary.LittleEndian.PutUint32(b, v)"},
	{"I64StoreMem", 8, i64, "binary.LittleEndian.PutUint64(b, v)"},
	{"F32StoreMem", 4, f32, "binary.LittleEndian.PutUint32(b, math.Float32bits(v))"},
	{"F64StoreMem", 8, f64, "binary.LittleEndian.PutUint64(b, math.Float64bits(v))"},
	{"I32StoreMem8", 1, i32, "b[0] = byte(v)"},
	{"I32StoreMem16", 2, i32, "binary.LittleEndian.PutUint16(b, uint16(v))"},
	{"I64StoreMem8", 1, i64, "b[0] = byte(v)"},
	{"I64StoreMem16", 2, i64, "binary.LittleEndian.PutUint16(b, uint16(v))"},
	{"I64StoreMem32", 4, i64, "binary.LittleEndian.PutUint32(b, uint32(v))"},
}

// writeMemory writes memFast: the loads and stores of memory 0 at an i32
// address, which read the instance's first memory from its field and an
// address in a slot in place, rather than calling a node for either.
func writeMemory(b *bytes.Buffer) {
	b.WriteString(`
// memFast compiles a load or store of memory 0, an i32 memory, at offset
// at, reporting whether it did. An i32 memory's offset off is below 2^32:
// a node captures it with at in its upper half, as oa, which costs the path
// that does not trap nothing that at captured apart would.
func (c *compiler) memFast(op opcode.Opcode, mem uint32, off uint64, at uint32) bool {
	if mem != 0 || c.m.Memories[0].Limits.Is64 {
		return false
	}
	oa := off | uint64(at)<<32
	switch op {
`)
	for _, l := range loads {
		fmt.Fprintf(b, "case opcode.%s:\na := c.pop()\nif a.k == eSlot {\ns := a.slot\n", l.name)
		fmt.Fprintf(b, "c.pushTree(%s, %s(func(f *frame) %s { b := f.inst.mem0.span(uint64(uint32(f.l[s]))+uint64(uint32(oa)), %d, uint32(oa>>32)); return %s }))\n",
			l.t.mtype, l.t.tree, l.t.gotype, l.width, l.expr)
		b.WriteString("} else {\nx := c.i32(a)\n")
		fmt.Fprintf(b, "c.pushTree(%s, %s(func(f *frame) %s { b := f.inst.mem0.span(uint64(x(f))+uint64(uint32(oa)), %d, uint32(oa>>32)); return %s }))\n",
			l.t.mtype, l.t.tree, l.t.gotype, l.width, l.expr)
		b.WriteString("}\n")
	}
	for _, s := range stores {
		fmt.Fprintf(b, "case opcode.%s:\nv := c.%s(c.pop())\na := c.pop()\nc.flushBelow(0)\nif a.k == eSlot {\ns := a.slot\n", s.name, strings.ToLower(s.t.name))
		fmt.Fprintf(b, "c.emit(func(f *frame) int32 { e := uint64(uint32(f.l[s])) + uint64(uint32(oa)); v := v(f); b := f.inst.mem0.span(e, %d, uint32(oa>>32)); %s; return 0 })\n",
			s.width, s.expr)
		b.WriteString("} else {\nx := c.i32(a)\n")
		fmt.Fprintf(b, "c.emit(func(f *frame) int32 { e := uint64(x(f)) + uint64(uint32(oa)); v := v(f); b := f.inst.mem0.span(e, %d, uint32(oa>>32)); %s; return 0 })\n",
			s.width, s.expr)
		b.WriteString("}\n")
	}
	b.WriteString("default:\nreturn false\n}\nreturn true\n}\n")
}
