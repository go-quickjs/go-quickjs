package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// Register code's numeric operations (README.md, "Instructions"). Each
// operator is an operation of its own, its handler reading its operands from
// the frame's slots, or one of them an immediate, and writing its result to
// a slot:
//
//	binary      A = B op C
//	immediate   A = B op SC (C a 16-bit immediate, sign-extended)
//	unary       A = op B
//	branch      if A op B: pc += SC
//	branch imm  if A op SB: pc += SC
//	load        A = mem0[B + C]
//	store       mem0[A + C] = B
//
// The operators used least are in the second table (rExt's, the operation in
// X), so that the first keeps room under 256 values.

// cold names the operators in the second table.
var cold = map[string]bool{
	"F32CopySign": true, "F64CopySign": true,
	"I32Popcnt": true, "I64Popcnt": true,
	"F32Ceil": true, "F32Floor": true, "F32Trunc": true, "F32NearestInt": true,
	"F64NearestInt":     true,
	"I32SConvertSatF32": true, "I32UConvertSatF32": true, "I32SConvertSatF64": true, "I32UConvertSatF64": true,
	"I64SConvertSatF32": true, "I64UConvertSatF32": true, "I64SConvertSatF64": true, "I64UConvertSatF64": true,
}

// moves are the operators that change no bit of a slot: an i32's upper half
// is clear, so its zero extension is a move, and a reinterpretation keeps
// the bits.
var moves = map[string]bool{
	"I64UConvertI32":    true,
	"I32ReinterpretF32": true, "I64ReinterpretF64": true,
	"F32ReinterpretI32": true, "F64ReinterpretI64": true,
}

// immOps are the binary operators with an immediate form.
var immOps = map[string]bool{
	"I32Add": true, "I32Sub": true, "I32Mul": true, "I32And": true, "I32Ior": true, "I32Xor": true,
	"I32Shl": true, "I32ShrS": true, "I32ShrU": true, "I32Rol": true, "I32Ror": true,
	"I64Add": true, "I64Sub": true, "I64Mul": true, "I64And": true, "I64Ior": true, "I64Xor": true,
	"I64Shl": true, "I64ShrS": true, "I64ShrU": true,
}

// commutative are the binary operators whose immediate may be on the left.
var commutative = map[string]bool{
	"I32Add": true, "I32Mul": true, "I32And": true, "I32Ior": true, "I32Xor": true,
	"I64Add": true, "I64Mul": true, "I64And": true, "I64Ior": true, "I64Xor": true,
}

// intCompares are the integer comparisons, each with its operands swapped
// and its negation, which fuse with the branch that tests them.
var intCompares = map[string][2]string{}

func init() {
	for _, t := range []string{"I32", "I64"} {
		for _, c := range [][3]string{
			{"Eq", "Eq", "Ne"}, {"Ne", "Ne", "Eq"},
			{"LtS", "GtS", "GeS"}, {"LtU", "GtU", "GeU"},
			{"GtS", "LtS", "LeS"}, {"GtU", "LtU", "LeU"},
			{"LeS", "GeS", "GtS"}, {"LeU", "GeU", "GtU"},
			{"GeS", "LeS", "LtS"}, {"GeU", "LeU", "LtU"},
		} {
			intCompares[t+c[0]] = [2]string{t + c[1], t + c[2]}
			immOps[t+c[0]] = true
		}
	}
}

// regRead returns the expression reading slot field e as type t.
func regRead(t typ, e string) string {
	p := "*rs(l, " + e + ")"
	switch t.name {
	case "I32":
		return "uint32(" + p + ")"
	case "F32":
		return "math.Float32frombits(uint32(" + p + "))"
	case "F64":
		return "math.Float64frombits(" + p + ")"
	}
	return p
}

// regImm returns the expression of the 16-bit immediate at field e as type
// t, sign-extended.
func regImm(t typ, e string) string {
	if t.name == "I64" {
		return "uint64(int64(int16(" + e + ")))"
	}
	return "uint32(int32(int16(" + e + ")))"
}

// regWrite returns the slot's word of expression e of type t.
func regWrite(t typ, e string) string {
	switch t.name {
	case "I32":
		return "uint64(" + e + ")"
	case "F32":
		return "uint64(math.Float32bits(" + e + "))"
	case "F64":
		return "math.Float64bits(" + e + ")"
	}
	return e
}

// regExpr is an operator's expression in a handler: a trap's offset is the
// instruction's pc, marked (regAt), which the trap's recovery makes the
// instruction's offset in the module.
func regExpr(e string) string { return strings.ReplaceAll(e, ", at)", ", regAt(pc))") }

// cond returns a comparison's condition, its expression without b2i.
func cond(e string) string { return strings.TrimSuffix(strings.TrimPrefix(e, "b2i("), ")") }

type regGen struct {
	b, h, info, init bytes.Buffer
	first, ext       []string
}

// handler writes a handler's function and gives it a value in its table,
// with the descriptor of its fields the verifier checks (rd).
func (g *regGen) handler(name, body string, isCold bool, desc string) {
	fmt.Fprintf(&g.h, "func rh%s(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {\n%s}\n\n", name, body)
	if isCold {
		g.ext = append(g.ext, name)
		fmt.Fprintf(&g.init, "regExtTable[x%s] = rh%s\nregExtDescs[x%s] = rd(%q)\n", name, name, name, desc)
	} else {
		g.first = append(g.first, name)
		fmt.Fprintf(&g.init, "regTable[r%s] = rh%s\nregDescs[r%s] = rd(%q)\n", name, name, name, desc)
	}
}

func writeReg(path string) {
	var g regGen
	for _, o := range binaries {
		isCold := cold[o.name]
		body := fmt.Sprintf("a, b := %s, %s\n*rs(l, w>>16) = %s\nreturn pc + 1\n",
			regRead(o.arg, "w>>32"), regRead(o.arg, "w>>48"), regWrite(o.res, regExpr(o.expr)))
		g.handler(o.name, body, isCold, "nnn")
		ref := "r" + o.name
		if isCold {
			ref = "x" + o.name
		}
		fmt.Fprintf(&g.info, "case opcode.%s:\nreturn regNum{op: %s, ext: %v", o.name, ref, isCold)
		if immOps[o.name] {
			body := fmt.Sprintf("a, b := %s, %s\n*rs(l, w>>16) = %s\nreturn pc + 1\n",
				regRead(o.arg, "w>>32"), regImm(o.arg, "w>>48"), regWrite(o.res, regExpr(o.expr)))
			g.handler(o.name+"I", body, false, "nn-")
			fmt.Fprintf(&g.info, ", imm: r%sI", o.name)
		}
		if commutative[o.name] {
			g.info.WriteString(", comm: true")
		}
		if c, ok := intCompares[o.name]; ok {
			body := fmt.Sprintf("a, b := %s, %s\nif %s {\nreturn regTake16(w, pc, f)\n}\nreturn pc + 1\n",
				regRead(o.arg, "w>>16"), regRead(o.arg, "w>>32"), cond(o.expr))
			g.handler("Br"+o.name, body, false, "nno")
			body = fmt.Sprintf("a, b := %s, %s\nif %s {\nreturn regTake16(w, pc, f)\n}\nreturn pc + 1\n",
				regRead(o.arg, "w>>16"), regImm(o.arg, "w>>32"), cond(o.expr))
			g.handler("Br"+o.name+"I", body, false, "n-o")
			fmt.Fprintf(&g.init, "regSlowTable[rBr%s] = regSlow16\nregSlowTable[rBr%sI] = regSlow16\n", o.name, o.name)
			fmt.Fprintf(&g.info, ", br: rBr%s, brI: rBr%sI, swap: opcode.%s, neg: opcode.%s", o.name, o.name, c[0], c[1])
		}
		fmt.Fprintf(&g.info, ", res: %s}, true\n", o.res.mtype)
	}
	for _, o := range unaries {
		if moves[o.name] {
			fmt.Fprintf(&g.info, "case opcode.%s:\nreturn regNum{op: rMov, unary: true, res: %s}, true\n", o.name, o.res.mtype)
			continue
		}
		isCold := cold[o.name]
		body := fmt.Sprintf("a := %s\n*rs(l, w>>16) = %s\nreturn pc + 1\n",
			regRead(o.arg, "w>>32"), regWrite(o.res, regExpr(o.expr)))
		g.handler(o.name, body, isCold, "nn-")
		ref := "r" + o.name
		if isCold {
			ref = "x" + o.name
		}
		fmt.Fprintf(&g.info, "case opcode.%s:\nreturn regNum{op: %s, ext: %v, unary: true, res: %s}, true\n", o.name, ref, isCold, o.res.mtype)
	}
	var mem bytes.Buffer
	for _, l := range loads {
		body := fmt.Sprintf("b := f.inst.mem0.span(uint64(uint32(*rs(l, w>>32)))+uint64(uint16(w>>48)), %d, regAt(pc))\n*rs(l, w>>16) = %s\nreturn pc + 1\n",
			l.width, regWrite(l.t, l.expr))
		g.handler(l.name, body, false, "nn-")
		fmt.Fprintf(&mem, "case opcode.%s:\nreturn r%s, false, true\n", l.name, l.name)
	}
	for _, s := range stores {
		body := fmt.Sprintf("v := %s\nb := f.inst.mem0.span(uint64(uint32(*rs(l, w>>16)))+uint64(uint16(w>>48)), %d, regAt(pc))\n%s\nreturn pc + 1\n",
			regRead(s.t, "w>>32"), s.width, s.expr)
		g.handler(s.name, body, false, "nn-")
		fmt.Fprintf(&mem, "case opcode.%s:\nreturn r%s, true, true\n", s.name, s.name)
	}
	if n := len(g.first); n > 256-firstReserved {
		fmt.Fprintf(os.Stderr, "numgen: %d numeric operations do not fit the first table\n", n)
		os.Exit(1)
	}

	b := &g.b
	b.WriteString(`// Code generated by internal/numgen; DO NOT EDIT.

package engine

import (
	"encoding/binary"
	"math"
	"math/bits"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The numeric operations of register code: in the first table from
// regNumFirst, and in the second, rExt's, from regExtNumFirst.
const (
`)
	for i, n := range g.first {
		if i == 0 {
			fmt.Fprintf(b, "r%s = regNumFirst + iota\n", n)
		} else {
			fmt.Fprintf(b, "r%s\n", n)
		}
	}
	b.WriteString("// regOpCount is the number of values the first table's operations take.\nregOpCount\n)\n\nconst (\n")
	for i, n := range g.ext {
		if i == 0 {
			fmt.Fprintf(b, "x%s = regExtNumFirst + iota\n", n)
		} else {
			fmt.Fprintf(b, "x%s\n", n)
		}
	}
	b.WriteString("// regExtOpCount is the number of values the second table's operations take.\nregExtOpCount\n)\n\n")
	b.Write(g.h.Bytes())
	b.WriteString(`// regNumeric returns how numeric operator op compiles to register code,
// reporting whether op is one.
func regNumeric(op opcode.Opcode) (regNum, bool) {
	switch op {
`)
	b.Write(g.info.Bytes())
	b.WriteString(`}
	return regNum{}, false
}

// regMemOp returns the operation of a load or store of memory 0 at an i32
// address, reporting whether op is one.
func regMemOp(op opcode.Opcode) (rop regOp, store, ok bool) {
	switch op {
`)
	b.Write(mem.Bytes())
	b.WriteString("}\nreturn 0, false, false\n}\n\n")
	// The loads and stores of any memory at any address, by opcode.
	b.WriteString("// regLoadAny returns the word load op reads from memory m at ea, trapping as\n// the instruction at offset at.\n")
	b.WriteString("func regLoadAny(op opcode.Opcode, m *Memory, ea uint64, at uint32) uint64 {\nswitch op {\n")
	for _, l := range loads {
		fmt.Fprintf(b, "case opcode.%s:\nb := m.span(ea, %d, at)\nreturn %s\n", l.name, l.width, regWrite(l.t, l.expr))
	}
	b.WriteString("}\npanic(\"wasm: not a load\")\n}\n\n")
	b.WriteString("// regStoreAny stores word w as store op does into memory m at ea, trapping\n// as the instruction at offset at.\n")
	b.WriteString("func regStoreAny(op opcode.Opcode, m *Memory, ea uint64, at uint32, w uint64) {\nswitch op {\n")
	for _, s := range stores {
		fmt.Fprintf(b, "case opcode.%s:\nv := %s\nb := m.span(ea, %d, at)\n%s\n", s.name, wordRead(s.t, "w"), s.width, s.expr)
	}
	b.WriteString("default:\npanic(\"wasm: not a store\")\n}\n}\n\nfunc init() {\n")
	b.Write(g.init.Bytes())
	b.WriteString("}\n\nvar _ = bits.Len\nvar _ = binary.LittleEndian\nvar _ module.ValueType\n")
	writeFile(path, b.Bytes())
}

// firstReserved is the number of the first table's values the hand-written
// operations take (regNumFirst).
const firstReserved = 24

// wordRead returns the expression reading word e as type t.
func wordRead(t typ, e string) string {
	switch t.name {
	case "I32":
		return "uint32(" + e + ")"
	case "F32":
		return "math.Float32frombits(uint32(" + e + "))"
	case "F64":
		return "math.Float64frombits(" + e + ")"
	}
	return e
}
