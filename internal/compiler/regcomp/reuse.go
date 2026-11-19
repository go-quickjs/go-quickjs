package regcomp

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A call of exec whose result is stored in a binding nothing else can
// reach the array from has its arrays made again, at its next match, in
// the memory of the last (the VM's regexpreuse.go): the call is marked
// RMarkReuse, its Aux the binding (bytecode.ReuseAux). Which calls are is decided
// here, of the function's finished code: the binding l, the call's
// destination, is no parameter -- the arguments object may alias one --
// and no closure captures it, and every instruction that reads its
// register reads only what the array cannot be reached from: its length,
// its index, its input, whether it is truthy, null or undefined, and its
// elements, read by an index that is a number. The pieces' Strings are
// made again too where every element read goes to one other binding, s,
// which is only tested and measured the same way.

// reuseMaxCode is the longest code whose exec calls are looked at.
const reuseMaxCode = 1 << 14

// reuseCand is an exec call the code stores in binding local's register,
// which finish asks reuseOf of.
type reuseCand struct{ pc, local int }

// reuseSites marks each candidate call whose arrays may be made again.
func (g *gen) reuseSites() {
	for _, c := range g.reuseCands {
		l, s, strs, ok := g.reuseOf(c.pc, c.local)
		if !ok || l >= 1<<16 || s+1 >= 1<<15 {
			continue
		}
		g.aux(c.pc, bytecode.MakeReuseAux(l, s, strs))
		g.mark(c.pc, bytecode.RMarkReuse)
		g.reuses = true
	}
}

// regRoles is the registers the instruction w reads and writes, and
// whether they are known: for an operation reuseOf does not know, any
// register its operands name may be read or written.
func regRoles(w bytecode.RInstr) (reads, writes []int, known bool) {
	a, b, c := int(w.A()), int(w.B()), int(w.C())
	span := func(from, n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = from + i
		}
		return out
	}
	switch w.Op() {
	case bytecode.RMove, bytecode.RInc, bytecode.RDec, bytecode.RUnary, bytecode.RTypeof, bytecode.RGetLength,
		bytecode.RGetProp, bytecode.RGetPropNext, bytecode.RGetIndexI, bytecode.RAddI, bytecode.RSubI, bytecode.RAndI, bytecode.ROrI,
		bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI, bytecode.RMulI, bytecode.RAddII32:
		return []int{b}, []int{a}, true
	case bytecode.RLoadInt, bytecode.RLoadConst, bytecode.RLoadUndef, bytecode.RLoadNull, bytecode.RLoadTrue,
		bytecode.RLoadFalse, bytecode.RLoadEmpty, bytecode.RLoadUninit, bytecode.RGetUpval, bytecode.RGetGlobal,
		bytecode.RNewObject, bytecode.RNewArrayK, bytecode.RNewArrayE, bytecode.RClosureW, bytecode.RNewRegExp:
		return nil, []int{a}, true
	case bytecode.RGetIndex, bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin, bytecode.RBitAnd,
		bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr, bytecode.RAddI32,
		bytecode.RSubI32, bytecode.RIn, bytecode.RInstanceOf, bytecode.RInstanceOfNext:
		return []int{b, c}, []int{a}, true
	case bytecode.RSetUpval, bytecode.RSetGlobal, bytecode.RJmpF, bytecode.RJmpT, bytecode.RJNull, bytecode.RJNotNull,
		bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI,
		bytecode.RJGeI, bytecode.RJNGeI, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJSEqI, bytecode.RJSNeI,
		bytecode.RJTypeof, bytecode.RJBool, bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU, bytecode.RRet,
		bytecode.RThrow, bytecode.RStrFreeze, bytecode.RSwitch, bytecode.RSwitchTypeof, bytecode.RAddPropI:
		return []int{a}, nil, true
	case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt, bytecode.RJGe,
		bytecode.RJNGe, bytecode.RJEq, bytecode.RJNe, bytecode.RJSEq, bytecode.RJSNe, bytecode.RSetProp, bytecode.RAddProp:
		return []int{a, b}, nil, true
	case bytecode.RSetIndex, bytecode.RAddIndex:
		return []int{a, b, c}, nil, true
	case bytecode.RSetIndexI:
		return []int{a, c}, nil, true
	case bytecode.RAddIndexK, bytecode.RAddIndexI:
		return []int{a, b}, nil, true
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT, bytecode.RNewW:
		// The window's registers, temporaries, and the result.
		reads := []int{a, a + 1}
		for k := range int(w.X()) {
			reads = append(reads, a+2+k)
		}
		return reads, []int{b}, true
	case bytecode.RGetMethodW:
		return []int{b}, []int{a, a + 1}, true
	case bytecode.RNewObjectLit:
		return span(b, int(w.X())), []int{a}, true
	case bytecode.RNewArray, bytecode.RConcat:
		return span(b, c), []int{a}, true
	case bytecode.RCharCodeAt:
		return []int{b, int(w.X())}, []int{a}, true
	case bytecode.RAppend:
		return []int{a, b}, []int{a}, true
	case bytecode.RJmp, bytecode.RRetUndef, bytecode.RNop:
		return nil, nil, true
	case bytecode.RStepJLt, bytecode.RStepJLe:
		return []int{a, b}, []int{a}, true
	case bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI, bytecode.RStepJLeU:
		return []int{a}, []int{a}, true
	}
	return nil, nil, false
}

// mentions reports whether the instruction w of an operation regRoles
// does not know may name register r, a binding's, in any of its operands.
// A window of registers -- a call's, an array's, a concatenation's -- is
// of temporaries, past every binding's register.
func mentions(w bytecode.RInstr, r int) bool {
	return int(w.A()) == r || int(w.B()) == r || int(w.C()) == r || int(w.X()) == r
}

// reuseOf reports whether the arrays of the exec call at pc, whose result
// the code stores in binding l's register, may be made again, and returns
// whether the elements' Strings may be too, and the binding s they are
// stored in, or -1.
func (g *gen) reuseOf(call, l int) (int, int, bool, bool) {
	fn := g.fn
	code := g.code
	if l < fn.ParamCount || l >= len(fn.Locals) || fn.Locals[l].Captured || len(code) > reuseMaxCode ||
		fn.UsesArguments && fn.MappedArguments {
		return 0, 0, false, false
	}
	reg := g.bindingReg(l)
	if reg < 0 {
		return 0, 0, false, false
	}
	s, strs := -1, true
	// pieces notes an element read into register d at pc: a binding's,
	// s, or a temporary's that is only tested or measured.
	pieces := func(pc, d int) {
		u := g.bindingOf(d)
		if u < 0 {
			strs = strs && g.testedTemp(pc, d)
			return
		}
		if u == l || u < fn.ParamCount || fn.Locals[u].Captured || s >= 0 && s != u {
			strs = false
			return
		}
		s = u
	}
	labels := g.labels()
	for pc, w := range code {
		if pc == call {
			continue
		}
		reads, _, known := regRoles(w)
		if !known {
			if mentions(w, reg) {
				return 0, 0, false, false
			}
			continue
		}
		if !slices.Contains(reads, reg) {
			// Another value stored, which leaves nothing behind it.
			continue
		}
		switch op := w.Op(); {
		case op == bytecode.RGetLength, op == bytecode.RJmpF, op == bytecode.RJmpT,
			op == bytecode.RJNull, op == bytecode.RJNotNull,
			op == bytecode.RUnary && bytecode.Op(w.X()) == bytecode.OpNot:
		case op == bytecode.RJSEq || op == bytecode.RJSNe || op == bytecode.RJEq || op == bytecode.RJNe:
			// A comparison with null or undefined, loaded just before into
			// the other register, at no label.
			other := int(w.B())
			if other == reg {
				other = int(w.A())
			}
			if other == reg || pc == 0 || labels[pc] {
				return 0, 0, false, false
			}
			switch p := code[pc-1]; p.Op() {
			case bytecode.RLoadNull, bytecode.RLoadUndef:
				if int(p.A()) != other {
					return 0, 0, false, false
				}
			default:
				return 0, 0, false, false
			}
		case (op == bytecode.RGetProp || op == bytecode.RGetPropNext) && int(w.B()) == reg:
			switch name := g.siteNameOf(w.C()); name {
			case "index", "input":
			default:
				return 0, 0, false, false
			}
		case op == bytecode.RGetIndexI && int(w.B()) == reg && int(w.A()) != reg:
			pieces(pc, int(w.A()))
		case op == bytecode.RGetIndex && int(w.B()) == reg && int(w.C()) != reg && int(w.A()) != reg && g.numberReg(int(w.C())):
			pieces(pc, int(w.A()))
		default:
			return 0, 0, false, false
		}
	}
	if strs && s >= 0 && !g.piecesReg(g.bindingReg(s)) {
		strs = false
	}
	if !strs {
		s = -1
	}
	return l, s, strs, true
}

// bindingReg is binding k's register, or -1 where it has none of 16 bits.
func (g *gen) bindingReg(k int) int {
	if k < 0 || k >= len(g.slotReg) || g.slotReg[k] < 0 || g.slotReg[k] >= farReg {
		return -1
	}
	return int(g.slotReg[k])
}

// bindingOf is the binding whose register r is, or -1 for a temporary.
func (g *gen) bindingOf(r int) int {
	for k, x := range g.slotReg {
		if int(x) == r {
			return k
		}
	}
	return -1
}

// siteNameOf is the name of cache site s.
func (g *gen) siteNameOf(s uint16) string {
	if int(s) < len(g.siteName) && int(g.siteName[s]) < len(g.fn.Names) {
		return g.fn.Names[g.siteName[s]]
	}
	return ""
}

// numberReg reports whether register r is a binding's whose value is
// never the key __proto__, the one an accessor of the array's prototypes
// may have, wherever it is read: no parameter's, no captured one's, and
// every instruction that writes it loads an integer or the dead zone's
// marker, or steps a value by a number.
func (g *gen) numberReg(r int) bool {
	k := g.bindingOf(r)
	if k < 0 || k < g.fn.ParamCount || g.fn.Locals[k].Captured {
		return false
	}
	for _, w := range g.code {
		_, writes, known := regRoles(w)
		if !known {
			if mentions(w, r) {
				return false
			}
			continue
		}
		if !slices.Contains(writes, r) {
			continue
		}
		switch w.Op() {
		case bytecode.RLoadInt, bytecode.RLoadUninit:
		case bytecode.RInc, bytecode.RDec, bytecode.RAddI, bytecode.RSubI, bytecode.RMulI, bytecode.RAndI,
			bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI, bytecode.RAddII32:
			// A step of anything by a number: a number, a BigInt, or a
			// string that ends in a digit -- a key that is never
			// __proto__.
		case bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI, bytecode.RStepJLeU:
		default:
			return false
		}
	}
	return true
}

// piecesReg reports whether register r, of the binding an array's
// elements are stored in, is only read to be tested or measured.
func (g *gen) piecesReg(r int) bool {
	if r < 0 {
		return false
	}
	for _, w := range g.code {
		reads, _, known := regRoles(w)
		if !known {
			if mentions(w, r) {
				return false
			}
			continue
		}
		if !slices.Contains(reads, r) {
			continue
		}
		switch op := w.Op(); {
		case op == bytecode.RGetLength, op == bytecode.RJmpF, op == bytecode.RJmpT,
			op == bytecode.RUnary && bytecode.Op(w.X()) == bytecode.OpNot:
		default:
			return false
		}
	}
	return true
}

// labels is which pcs of the code a jump, a switch's case or a handler
// goes to.
func (g *gen) labels() []bool {
	code := g.code
	at := make([]bool, len(code)+1)
	mark := func(to int) {
		if to >= 0 && to < len(at) {
			at[to] = true
		}
	}
	for pc, w := range code {
		switch op := w.Op(); op {
		case bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT:
			mark(pc + 1 + int(int32(w.BC())))
		case bytecode.RSwitch, bytecode.RSwitchTypeof:
			// Its cases are bound once the code is done (finish), past
			// any label this asks about.
			for i := range at {
				at[i] = true
			}
		default:
			if regJumps(op) {
				mark(pc + 1 + int(w.SC()))
			}
		}
	}
	for _, h := range g.handlerTable() {
		mark(int(h.To))
	}
	return at
}

// regJumps reports whether op is a compare-and-branch, of SC's offset.
func regJumps(op bytecode.ROp) bool {
	switch op {
	case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt,
		bytecode.RJGe, bytecode.RJNGe, bytecode.RJEq, bytecode.RJNe, bytecode.RJSEq, bytecode.RJSNe,
		bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI,
		bytecode.RJGeI, bytecode.RJNGeI, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJSEqI, bytecode.RJSNeI,
		bytecode.RJNull, bytecode.RJNotNull, bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU,
		bytecode.RJTypeof, bytecode.RJBool, bytecode.RIterNext, bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI,
		bytecode.RStepJGtI, bytecode.RStepJGeI, bytecode.RStepJLeU:
		return true
	}
	return false
}

// testedTemp reports whether temporary d, an element read into it at pc,
// is only tested or measured from there to where it is written again or
// the straight code it is in ends: the temporary is the element's alone
// for as long as the expression that reads it, whose test or measure is
// its one use.
func (g *gen) testedTemp(pc, d int) bool {
	for _, w := range g.code[pc+1:] {
		reads, writes, known := regRoles(w)
		if !known {
			if mentions(w, d) {
				return false
			}
			continue
		}
		if slices.Contains(reads, d) {
			switch op := w.Op(); {
			case op == bytecode.RGetLength, op == bytecode.RJmpF, op == bytecode.RJmpT,
				op == bytecode.RUnary && bytecode.Op(w.X()) == bytecode.OpNot:
			default:
				return false
			}
		}
		if slices.Contains(writes, d) {
			return true
		}
		switch op := w.Op(); {
		case op == bytecode.RJmp, op == bytecode.RJmpF, op == bytecode.RJmpT, regJumps(op),
			op == bytecode.RRet, op == bytecode.RRetUndef, op == bytecode.RThrow:
			return true
		}
	}
	return true
}
