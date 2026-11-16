// Package regform makes a function's register code from its stack code: the
// backend of the register interpreter (docs/register-interpreter.md).
package regform

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register form's backend (docs/register-interpreter.md).
//
// RegisterCode makes a function's register code from its stack code, which
// is the front end's output with every scope already resolved: each local is
// the slot the stack form gave it, each upvalue, global and property site the
// one it named. So the two forms of a function agree on everything but how an
// expression's temporaries are held, and a frame of either is laid out the
// same: the locals, then the temporaries.
//
// The translation runs the stack code over a symbolic stack. An entry of it
// is a value not yet put anywhere -- a local's, a constant's, an operation's
// result -- or one already in its home, the register of its depth. A value
// is put in its home only when something needs it there: an argument of a
// call, a value live across a jump, a store that would change the local it
// still is. An operation reads its operands where they are, and its result
// goes straight to the local a store puts it in. `a = b` is then one move,
// `s = s + x` one add, and `j < n` the operands of the jump that tests it.

// Register limits of the form: a register and a site are 16 bits.
const regMax = 1 << 16

// entry kinds of the symbolic stack.
const (
	eHome    = iota // in the register of its depth
	eLocal          // the value of register reg, which nothing has changed since
	eConst          // a constant, loaded by load with its A set
	ePending        // the result of pend, not yet emitted, its destination open
)

type rentry struct {
	kind uint8
	reg  uint16
	load bytecode.RInstr
}

type regTranslator struct {
	fn       *bytecode.Function
	locals   int
	stackMax int
	// shared marks the locals a closure captures, whose value a call or
	// anything that may run code can change: they are read when the stack
	// form reads them, never later.
	shared []bool

	code  []bytecode.RInstr
	src   []uint32
	stack []rentry
	// pend is the instruction an ePending entry, which is only ever the top
	// one, stands for; pendSrc the stack pc it came from.
	pend    bytecode.RInstr
	pendSrc uint32

	// start is, for each stack pc, where its register code begins; depth
	// is the stack depth at each leader that a jump has reached, -1 for
	// none yet.
	start  []int
	depth  []int
	leader []bool
	fixups []regFixup

	thisReg  int
	scratch  int // registers past the this register in use as scratch
	maxScr   int
	siteName []uint32
	pc       uint32
	// done marks the stack pcs translated; bad is what went wrong where
	// nothing could return it.
	done []bool
	bad  string
}

type regFixup struct {
	at     int // the jump's register pc
	target int // the stack pc it jumps to
	short  bool
}

// RegisterCode translates fn's code into the register form. It returns nil
// and the reason when the function has something the form does not cover
// yet, which then runs in the stack form.
func RegisterCode(fn *bytecode.Function) (*bytecode.RegCode, string) {
	switch {
	case fn.Generator || fn.Async:
		return nil, "generator or async function"
	case fn.HasDirectEval:
		return nil, "direct eval"
	case fn.Debug != nil:
		return nil, "compiled for a debugger"
	case fn.TopLevel:
		return nil, "top-level code"
	case fn.HasTailCall:
		return nil, "tail call"
	case fn.UsesArguments || fn.MakesArguments || fn.HasRest || !fn.HasSimpleParams || fn.ParamsAreLexical:
		return nil, "arguments or a parameter prologue"
	case fn.Kind == bytecode.KindConstructor || fn.Kind == bytecode.KindDerivedConstructor ||
		fn.Kind == bytecode.KindClassFieldInit || fn.Kind == bytecode.KindStaticBlock:
		return nil, "class code"
	case fn.LocalCount+fn.MaxStack+2 >= regMax || fn.PropSites >= regMax || len(fn.Names) >= regMax:
		return nil, "too many registers or sites"
	}
	t := &regTranslator{
		fn:       fn,
		locals:   fn.LocalCount,
		stackMax: fn.MaxStack,
		shared:   make([]bool, fn.LocalCount),
		start:    make([]int, len(fn.Code)+1),
		depth:    make([]int, len(fn.Code)+1),
		leader:   make([]bool, len(fn.Code)+1),
		done:     make([]bool, len(fn.Code)+1),
		thisReg:  -1,
		siteName: make([]uint32, fn.PropSites+1),
	}
	for i, l := range fn.Locals {
		if i < len(t.shared) && l.Captured {
			t.shared[i] = true
		}
	}
	for i := range t.depth {
		t.depth[i] = -1
	}
	for _, in := range fn.Code {
		switch in.Op {
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse,
			bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep:
			if int(in.A) > len(fn.Code) {
				return nil, "jump out of the code"
			}
			t.leader[in.A] = true
		}
	}
	if why := t.translate(); why != "" {
		return nil, why
	}
	return t.finish()
}

// home is the register of stack depth d.
func (t *regTranslator) home(d int) uint16 { return uint16(t.locals + d) }

func (t *regTranslator) emit(w bytecode.RInstr) int {
	t.code = append(t.code, w)
	t.src = append(t.src, t.pc)
	return len(t.code) - 1
}

// withDest is w with its destination set to d: B for a call, A for the rest.
func withDest(w bytecode.RInstr, d uint16) bytecode.RInstr {
	switch w.Op() {
	case bytecode.RCall, bytecode.RCallMethod, bytecode.RNew:
		return w&^(0xffff<<32) | bytecode.RInstr(uint64(d)<<32)
	}
	return w.WithA(d)
}

func (t *regTranslator) push(e rentry) { t.stack = append(t.stack, e) }

// pushResult pushes the result of w, an instruction whose destination is
// still open.
func (t *regTranslator) pushResult(w bytecode.RInstr) {
	t.pend, t.pendSrc = w, t.pc
	t.push(rentry{kind: ePending})
}

func (t *regTranslator) pushConst(w bytecode.RInstr) { t.push(rentry{kind: eConst, load: w}) }

// flushPending emits the pending result into its home.
func (t *regTranslator) flushPending() {
	if n := len(t.stack); n > 0 && t.stack[n-1].kind == ePending {
		t.place(n - 1)
	}
}

// place puts the entry at depth d in its home.
func (t *regTranslator) place(d int) {
	e := &t.stack[d]
	h := t.home(d)
	switch e.kind {
	case eLocal:
		if e.reg != h {
			t.emit(bytecode.MakeR(bytecode.RMove, 0, h, e.reg, 0))
		}
	case eConst:
		t.emit(e.load.WithA(h))
	case ePending:
		pc := t.pc
		t.pc = t.pendSrc
		t.emit(withDest(t.pend, h))
		t.pc = pc
	}
	*e = rentry{kind: eHome}
}

// placeAll puts every entry in its home, as a jump or a leader needs.
func (t *regTranslator) placeAll() {
	for d := range t.stack {
		t.place(d)
	}
}

// pop removes the top entry and returns a register holding its value,
// emitting what puts it there if it is not in one already.
func (t *regTranslator) pop() uint16 {
	d := len(t.stack) - 1
	e := t.stack[d]
	var r uint16
	switch e.kind {
	case eLocal:
		r = e.reg
	default:
		t.place(d)
		r = t.home(d)
	}
	t.stack = t.stack[:d]
	return r
}

// popEntry removes the top entry and returns it as it is.
func (t *regTranslator) popEntry() rentry {
	d := len(t.stack) - 1
	e := t.stack[d]
	t.stack = t.stack[:d]
	return e
}

// beforeWrite puts in their homes the entries that still are local r's
// value, which a store to r is about to change.
func (t *regTranslator) beforeWrite(r uint16) {
	for d := range t.stack {
		if e := t.stack[d]; e.kind == eLocal && e.reg == r {
			t.place(d)
		}
	}
}

// store puts the entry e, just popped from depth d, in register r.
func (t *regTranslator) store(r uint16, e rentry, d int) {
	t.beforeWrite(r)
	switch e.kind {
	case eHome:
		t.emit(bytecode.MakeR(bytecode.RMove, 0, r, t.home(d), 0))
	case eLocal:
		if e.reg != r {
			t.emit(bytecode.MakeR(bytecode.RMove, 0, r, e.reg, 0))
		}
	case eConst:
		t.emit(e.load.WithA(r))
	case ePending:
		pc := t.pc
		t.pc = t.pendSrc
		t.emit(withDest(t.pend, r))
		t.pc = pc
	}
}

// pushLocal pushes local r's value.
func (t *regTranslator) pushLocal(r uint16) {
	t.flushPending()
	if int(r) < len(t.shared) && t.shared[r] {
		d := len(t.stack)
		t.emit(bytecode.MakeR(bytecode.RMove, 0, t.home(d), r, 0))
		t.push(rentry{kind: eHome})
		return
	}
	t.push(rentry{kind: eLocal, reg: r})
}

// immOK reports whether e is an int32 constant that fits a 16-bit
// immediate, and the value.
func immOK(e rentry) (int32, bool) {
	if e.kind != eConst || e.load.Op() != bytecode.RLoadInt {
		return 0, false
	}
	v := int32(e.load.BC())
	return v, v >= -1<<15 && v < 1<<15
}

// binary is the stack form's binary operator op over the two top entries.
func (t *regTranslator) binary(op bytecode.Op) {
	be := t.stack[len(t.stack)-1]
	if imm, ok := immOK(be); ok {
		switch op {
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpBitAnd, bytecode.OpBitOr,
			bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
			t.stack = t.stack[:len(t.stack)-1]
			a := t.pop()
			t.pushResult(bytecode.MakeR(bytecode.RBinI, uint8(op), 0, a, uint16(int16(imm))))
			return
		}
	}
	if n := len(t.stack); n >= 2 && t.stack[n-2].kind == ePending {
		// The left operand's operation comes first: the right one, a
		// constant or a local pushed on top of it, is put in its home after.
		t.place(n - 2)
	}
	b := t.pop()
	a := t.pop()
	switch op {
	case bytecode.OpAdd:
		t.pushResult(bytecode.MakeR(bytecode.RAdd, 0, 0, a, b))
	case bytecode.OpSub:
		t.pushResult(bytecode.MakeR(bytecode.RSub, 0, 0, a, b))
	case bytecode.OpMul:
		t.pushResult(bytecode.MakeR(bytecode.RMul, 0, 0, a, b))
	default:
		t.pushResult(bytecode.MakeR(bytecode.RBin, uint8(op), 0, a, b))
	}
}

// binaryOp reports whether op is a binary operator binary translates.
func binaryOp(op bytecode.Op) bool {
	switch op {
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod, bytecode.OpPow,
		bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
		bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe,
		bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe:
		return true
	}
	return false
}

// window puts the n top entries in their homes, as a call's callee and
// arguments must be, and returns the first one's register.
func (t *regTranslator) window(n int) uint16 {
	base := len(t.stack) - n
	for d := base; d < len(t.stack); d++ {
		t.place(d)
	}
	return t.home(base)
}

// shuffle rearranges the k top entries: the new top k are old[from[i]],
// where old[0] is the deepest of them.
func (t *regTranslator) shuffle(k int, from []int) bool {
	t.flushPending()
	if len(t.stack) < k {
		return false
	}
	base := len(t.stack) - k
	old := append([]rentry(nil), t.stack[base:]...)
	// A value in a home that moves to another depth is copied to a scratch
	// register first, so that no home is written before it has been read.
	scr := make([]int, k)
	for i := range scr {
		scr[i] = -1
	}
	for i, j := range from {
		if old[j].kind == eHome && j != i && scr[j] < 0 {
			r := t.locals + t.stackMax + 1 + t.scratch
			t.scratch++
			scr[j] = r
			t.emit(bytecode.MakeR(bytecode.RMove, 0, uint16(r), t.home(base+j), 0))
		}
	}
	t.stack = t.stack[:base]
	for i, j := range from {
		d := base + i
		e := old[j]
		if e.kind == eHome && j != i {
			t.emit(bytecode.MakeR(bytecode.RMove, 0, t.home(d), uint16(scr[j]), 0))
		}
		if e.kind == eHome {
			e = rentry{kind: eHome}
		}
		t.stack = append(t.stack, e)
	}
	if t.scratch > t.maxScr {
		t.maxScr = t.scratch
	}
	t.scratch = 0
	return true
}

// thisRegister is the register the function's this is loaded into as it
// starts.
func (t *regTranslator) thisRegister() uint16 {
	if t.thisReg < 0 {
		t.thisReg = t.locals + t.stackMax
	}
	return uint16(t.thisReg)
}

func (t *regTranslator) jump(w bytecode.RInstr, target int, short bool) {
	at := t.emit(w)
	t.fixups = append(t.fixups, regFixup{at: at, target: target, short: short})
	if d := len(t.stack); t.depth[target] < 0 {
		t.depth[target] = d
	} else if t.depth[target] != d {
		t.bad = "stack depths disagree at a jump"
	}
}

// translate translates the stack code, instruction by instruction.
func (t *regTranslator) translate() string {
	code := t.fn.Code
	reachable := true
	for pc := 0; pc < len(code); pc++ {
		in := code[pc]
		t.pc = uint32(pc)
		if t.leader[pc] {
			if reachable {
				t.placeAll()
				if t.depth[pc] >= 0 && t.depth[pc] != len(t.stack) {
					return "stack depths disagree at a join"
				}
				t.depth[pc] = len(t.stack)
			} else if t.depth[pc] >= 0 {
				t.stack = t.stack[:0]
				for i := 0; i < t.depth[pc]; i++ {
					t.push(rentry{kind: eHome})
				}
				reachable = true
			}
		}
		t.start[pc] = len(t.code)
		if !reachable {
			continue
		}
		t.done[pc] = true
		if why := t.op(pc, in, &reachable); why != "" {
			return why
		}
		if len(t.stack) > t.stackMax {
			return "stack deeper than its maximum"
		}
	}
	t.start[len(code)] = len(t.code)
	if t.bad != "" {
		return t.bad
	}
	if reachable {
		t.emit(bytecode.MakeR(bytecode.RRetUndef, 0, 0, 0, 0))
	}
	return ""
}

func u16(v uint32) (uint16, bool) { return uint16(v), v < regMax }

// op translates one instruction.
func (t *regTranslator) op(pc int, in bytecode.Instr, reachable *bool) string {
	switch op := in.Op; {
	case binaryOp(op):
		t.binary(op)
		return ""
	}
	switch in.Op {
	case bytecode.OpNop:
	case bytecode.OpPushInt:
		t.flushPending()
		t.pushConst(bytecode.MakeRBC(bytecode.RLoadInt, 0, 0, in.A))
	case bytecode.OpPushConst:
		t.flushPending()
		t.pushConst(bytecode.MakeRBC(bytecode.RLoadConst, 0, 0, in.A))
	case bytecode.OpPushUndef, bytecode.OpPushNull, bytecode.OpPushTrue, bytecode.OpPushFalse,
		bytecode.OpPushEmptyString, bytecode.OpPushUninitialized:
		t.flushPending()
		var r bytecode.ROp
		switch in.Op {
		case bytecode.OpPushUndef:
			r = bytecode.RLoadUndef
		case bytecode.OpPushNull:
			r = bytecode.RLoadNull
		case bytecode.OpPushTrue:
			r = bytecode.RLoadTrue
		case bytecode.OpPushFalse:
			r = bytecode.RLoadFalse
		case bytecode.OpPushEmptyString:
			r = bytecode.RLoadEmpty
		default:
			r = bytecode.RLoadUninit
		}
		t.pushConst(bytecode.MakeR(r, 0, 0, 0, 0))
	case bytecode.OpPushThis:
		t.flushPending()
		t.push(rentry{kind: eLocal, reg: t.thisRegister()})

	case bytecode.OpGetLocal:
		t.pushLocal(uint16(in.A))
	case bytecode.OpGetLocal2:
		t.pushLocal(uint16(in.A))
		t.pushLocal(uint16(in.B))
	case bytecode.OpGetLocalCheck:
		t.flushPending()
		t.emit(bytecode.MakeR(bytecode.RCheckRef, 0, uint16(in.A), uint16(in.A), 0))
		t.pushLocal(uint16(in.A))
	case bytecode.OpSetLocal, bytecode.OpInitLocal:
		d := len(t.stack) - 1
		t.store(uint16(in.A), t.popEntry(), d)
	case bytecode.OpSetLocalCheck:
		if int(in.A) >= len(t.fn.Locals) || !t.fn.Locals[in.A].Mutable {
			return "assignment to a constant"
		}
		d := len(t.stack) - 1
		t.place(d)
		t.emit(bytecode.MakeR(bytecode.RCheckRef, 0, uint16(in.A), uint16(in.A), 0))
		t.store(uint16(in.A), t.popEntry(), d)
	case bytecode.OpPutLocal:
		d := len(t.stack) - 1
		e := t.popEntry()
		t.store(uint16(in.A), e, d)
		t.pushLocalAfterStore(uint16(in.A), e)
	case bytecode.OpSetLocalGet:
		d := len(t.stack) - 1
		t.store(uint16(in.A), t.popEntry(), d)
		t.pushLocal(uint16(in.B))
	case bytecode.OpClearLocal:
		t.flushPending()
		t.beforeWrite(uint16(in.A))
		t.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, uint16(in.A), 0, 0))
	case bytecode.OpIncLocal, bytecode.OpDecLocal:
		t.flushPending()
		r := uint16(in.A)
		t.beforeWrite(r)
		op := bytecode.RInc
		if in.Op == bytecode.OpDecLocal {
			op = bytecode.RDec
		}
		t.emit(bytecode.MakeR(op, 0, r, r, 0))
	case bytecode.OpLocalBinImm:
		t.pushLocal(uint16(in.A & (1<<24 - 1)))
		t.pushConst(bytecode.MakeRBC(bytecode.RLoadInt, 0, 0, in.B))
		t.binary(bytecode.Op(in.A >> 24))
	case bytecode.OpBinImm:
		t.pushConst(bytecode.MakeRBC(bytecode.RLoadInt, 0, 0, in.A))
		t.binary(bytecode.Op(in.B))
	case bytecode.OpBinLocal:
		// The local is read after the operand beneath it, as the stack form
		// reads it.
		t.pushLocalNoFlush(uint16(in.A))
		t.binary(bytecode.Op(in.B))
	case bytecode.OpGetLocalIndex:
		t.pushLocal(uint16(in.A))
		t.pushLocal(uint16(in.B))
		k := t.pop()
		o := t.pop()
		t.pushResult(bytecode.MakeR(bytecode.RGetIndex, 0, 0, o, k))

	case bytecode.OpInc, bytecode.OpDec:
		a := t.pop()
		op := bytecode.RInc
		if in.Op == bytecode.OpDec {
			op = bytecode.RDec
		}
		t.pushResult(bytecode.MakeR(op, 0, 0, a, 0))
	case bytecode.OpNeg, bytecode.OpPos, bytecode.OpNot, bytecode.OpBitNot, bytecode.OpTypeOf:
		a := t.pop()
		t.pushResult(bytecode.MakeR(bytecode.RUnary, uint8(in.Op), 0, a, 0))

	case bytecode.OpDrop:
		d := len(t.stack) - 1
		if t.stack[d].kind == ePending {
			// What it computes may still do something: a valueOf, a getter.
			t.place(d)
		}
		t.stack = t.stack[:d]
	case bytecode.OpDup:
		t.shuffle(1, []int{0, 0})
	case bytecode.OpDup2:
		t.shuffle(2, []int{0, 1, 0, 1})
	case bytecode.OpSwap:
		t.shuffle(2, []int{1, 0})
	case bytecode.OpRot3:
		t.shuffle(3, []int{1, 2, 0})
	case bytecode.OpRot4:
		t.shuffle(4, []int{1, 2, 3, 0})
	case bytecode.OpInsert2:
		t.shuffle(2, []int{1, 0, 1})
	case bytecode.OpInsert3:
		t.shuffle(3, []int{2, 0, 1, 2})
	case bytecode.OpInsert4:
		t.shuffle(4, []int{3, 0, 1, 2, 3})
	case bytecode.OpNipUnder:
		n := int(in.A)
		t.shuffle(n+1, []int{n})

	case bytecode.OpGetUpvalue:
		t.flushPending()
		t.pushResult(bytecode.MakeRBC(bytecode.RGetUpval, 0, 0, in.A))
	case bytecode.OpSetUpvalue:
		v := t.pop()
		t.emit(bytecode.MakeRBC(bytecode.RSetUpval, 0, v, in.A))
	case bytecode.OpCloseUpvalues:
		t.flushPending()
		t.emit(bytecode.MakeR(bytecode.RCloseUpvals, 0, uint16(in.A), 0, 0))

	case bytecode.OpGetGlobal:
		t.flushPending()
		name, ok1 := u16(in.A)
		site, ok2 := u16(in.B)
		if !ok1 || !ok2 {
			return "a name or site past 16 bits"
		}
		t.pushResult(bytecode.MakeR(bytecode.RGetGlobal, 0, 0, name, site))
	case bytecode.OpSetGlobal, bytecode.OpSetGlobalStrict:
		name, ok1 := u16(in.A)
		site, ok2 := u16(in.B)
		if !ok1 || !ok2 {
			return "a name or site past 16 bits"
		}
		v := t.pop()
		var strict uint8
		if in.Op == bytecode.OpSetGlobalStrict {
			strict = 1
		}
		t.emit(bytecode.MakeR(bytecode.RSetGlobal, strict, v, name, site))

	case bytecode.OpGetProp:
		o := t.pop()
		t.siteName[in.B] = in.A
		t.pushResult(bytecode.MakeR(bytecode.RGetProp, 0, 0, o, uint16(in.B)))
	case bytecode.OpGetPropThis:
		// The receiver stays where it is, beneath the method.
		t.flushPending()
		d := len(t.stack) - 1
		var o uint16
		if e := t.stack[d]; e.kind == eLocal {
			o = e.reg
		} else {
			t.place(d)
			o = t.home(d)
		}
		t.siteName[in.B] = in.A
		t.pushResult(bytecode.MakeR(bytecode.RGetMethod, 0, 0, o, uint16(in.B)))
	case bytecode.OpSetProp:
		v := t.pop()
		o := t.pop()
		t.siteName[in.B] = in.A
		t.emit(bytecode.MakeR(bytecode.RSetProp, 0, o, v, uint16(in.B)))
	case bytecode.OpGetIndex:
		k := t.pop()
		o := t.pop()
		t.pushResult(bytecode.MakeR(bytecode.RGetIndex, 0, 0, o, k))
	case bytecode.OpSetIndex:
		v := t.pop()
		k := t.pop()
		o := t.pop()
		t.emit(bytecode.MakeR(bytecode.RSetIndex, 0, o, k, v))
	case bytecode.OpDefineField:
		name, ok := u16(in.A)
		if !ok {
			return "a name past 16 bits"
		}
		v := t.pop()
		d := len(t.stack) - 1
		t.place(d)
		t.emit(bytecode.MakeR(bytecode.RDefineField, 0, t.home(d), v, name))
	case bytecode.OpNewObject:
		t.flushPending()
		hint, ok := u16(in.A)
		if !ok {
			hint = regMax - 1
		}
		if in.B >= regMax {
			return "a site past 16 bits"
		}
		d := len(t.stack)
		t.emit(bytecode.MakeR(bytecode.RNewObject, 0, t.home(d), hint, uint16(in.B)))
		t.push(rentry{kind: eHome})
	case bytecode.OpNewArray:
		if in.B != 0 || in.A >= regMax {
			return "an array literal of constants"
		}
		n := int(in.A)
		t.flushPending()
		base := t.window(n)
		t.stack = t.stack[:len(t.stack)-n]
		t.pushResult(bytecode.MakeR(bytecode.RNewArray, 0, 0, base, uint16(n)))
	case bytecode.OpClosure:
		t.flushPending()
		t.pushResult(bytecode.MakeRBC(bytecode.RClosure, 0, 0, in.A))

	case bytecode.OpCall, bytecode.OpNew:
		if in.A > 255 {
			return "more than 255 arguments"
		}
		n := int(in.A) + 1
		t.flushPending()
		base := t.window(n)
		t.stack = t.stack[:len(t.stack)-n]
		op := bytecode.RCall
		if in.Op == bytecode.OpNew {
			op = bytecode.RNew
		}
		t.pushResult(bytecode.MakeR(op, uint8(in.A), base, 0, 0))
	case bytecode.OpCallMethod:
		if in.A > 255 {
			return "more than 255 arguments"
		}
		n := int(in.A) + 2
		t.flushPending()
		base := t.window(n)
		t.stack = t.stack[:len(t.stack)-n]
		t.pushResult(bytecode.MakeR(bytecode.RCallMethod, uint8(in.A), base, 0, 0))

	case bytecode.OpJump:
		t.placeAll()
		t.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), int(in.A), false)
		*reachable = false
	case bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue:
		v := t.pop()
		t.placeAll()
		op := bytecode.RJmpF
		if in.Op == bytecode.OpJumpIfTrue {
			op = bytecode.RJmpT
		}
		t.jump(bytecode.MakeR(op, 0, v, 0, 0), int(in.A), false)
	case bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep:
		t.placeAll()
		d := len(t.stack) - 1
		op := bytecode.RJmpF
		if in.Op == bytecode.OpJumpIfTrueKeep {
			op = bytecode.RJmpT
		}
		t.jump(bytecode.MakeR(op, 0, t.home(d), 0, 0), int(in.A), false)
		t.stack = t.stack[:d]
	case bytecode.OpJumpIfCmpFalse:
		cmp := bytecode.Op(in.B)
		switch cmp {
		case bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
			bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
		default:
			return "a jump on " + cmp.String()
		}
		if uint8(cmp)&bytecode.RCmpTrue != 0 {
			return "a comparison past 7 bits"
		}
		be := t.stack[len(t.stack)-1]
		if imm, ok := immOK(be); ok {
			t.stack = t.stack[:len(t.stack)-1]
			a := t.pop()
			t.placeAll()
			t.jump(bytecode.MakeR(bytecode.RJmpCmpI, uint8(cmp), a, uint16(int16(imm)), 0), int(in.A), true)
			break
		}
		b := t.pop()
		a := t.pop()
		t.placeAll()
		t.jump(bytecode.MakeR(bytecode.RJmpCmp, uint8(cmp), a, b, 0), int(in.A), true)

	case bytecode.OpReturn:
		v := t.pop()
		t.emit(bytecode.MakeR(bytecode.RRet, 0, v, 0, 0))
		*reachable = false
	case bytecode.OpReturnUndef:
		t.emit(bytecode.MakeR(bytecode.RRetUndef, 0, 0, 0, 0))
		*reachable = false

	default:
		return "operation " + in.Op.String()
	}
	return ""
}

// pushLocalNoFlush pushes local r's value without first putting a pending
// result in its home: an operator's right operand, read after the left.
func (t *regTranslator) pushLocalNoFlush(r uint16) {
	if int(r) < len(t.shared) && t.shared[r] {
		t.flushPending()
		d := len(t.stack)
		t.emit(bytecode.MakeR(bytecode.RMove, 0, t.home(d), r, 0))
		t.push(rentry{kind: eHome})
		return
	}
	t.push(rentry{kind: eLocal, reg: r})
}

// pushLocalAfterStore pushes, after put_local r stored e, the value it
// stored: local r's, unless a closure may change r first.
func (t *regTranslator) pushLocalAfterStore(r uint16, e rentry) {
	if int(r) < len(t.shared) && t.shared[r] {
		d := len(t.stack)
		t.emit(bytecode.MakeR(bytecode.RMove, 0, t.home(d), r, 0))
		t.push(rentry{kind: eHome})
		return
	}
	t.push(rentry{kind: eLocal, reg: r})
}

// finish resolves the jumps, turns a loop's jump back to its test into the
// test itself, checks the code, and makes the RegCode.
func (t *regTranslator) finish() (*bytecode.RegCode, string) {
	prologue := 0
	if t.thisReg >= 0 {
		prologue = 1
	}
	code := make([]bytecode.RInstr, 0, len(t.code)+prologue)
	src := make([]uint32, 0, len(t.code)+prologue)
	if prologue != 0 {
		code = append(code, bytecode.MakeR(bytecode.RLoadThis, 0, uint16(t.thisReg), 0, 0))
		src = append(src, 0)
	}
	code = append(code, t.code...)
	src = append(src, t.src...)
	for _, f := range t.fixups {
		if !t.done[f.target] {
			return nil, "a jump to code not translated"
		}
		at := f.at + prologue
		target := t.start[f.target] + prologue
		off := int64(target) - int64(at+1)
		if f.short {
			if off < -1<<15 || off >= 1<<15 {
				return nil, "a jump past 16 bits"
			}
			code[at] = code[at].WithSC(int32(off))
		} else {
			code[at] = code[at].WithBC(uint32(int32(off)))
		}
	}
	// A loop's jump back to a test that is its header's first instruction
	// becomes the test reversed, where the loop's exit is the instruction
	// after the jump: one dispatch a turn fewer.
	for pc, w := range code {
		if w.Op() != bytecode.RJmp {
			continue
		}
		target := pc + 1 + int(int32(w.BC()))
		if target < 0 || target >= len(code) || target > pc {
			continue
		}
		c := code[target]
		if c.Op() != bytecode.RJmpCmp && c.Op() != bytecode.RJmpCmpI || c.X()&bytecode.RCmpTrue != 0 {
			continue
		}
		if exit := target + 1 + int(c.SC()); exit != pc+1 {
			continue
		}
		back := int64(target+1) - int64(pc+1)
		if back < -1<<15 {
			continue
		}
		code[pc] = (c | bytecode.RInstr(bytecode.RCmpTrue)<<8).WithSC(int32(back))
		src[pc] = src[target]
	}
	// The locals, the temporaries, the this register and the scratch ones.
	n := t.locals + t.stackMax + 1 + t.maxScr
	if n >= regMax {
		return nil, "too many registers"
	}
	rc := &bytecode.RegCode{Code: code, NumRegs: n, SrcPC: src, SiteName: t.siteName, This: t.thisReg}
	if why := VerifyRegisterCode(rc, t.fn); why != "" {
		return nil, why
	}
	return rc, ""
}

// VerifyRegisterCode checks what the register interpreter takes on trust,
// reading registers and code without bounds checks: every register an
// instruction names is below NumRegs, a call's window too, every jump lands
// in the code, and the last instruction does not run on past the end. It
// returns what is wrong, or "".
func VerifyRegisterCode(rc *bytecode.RegCode, fn *bytecode.Function) string {
	n := rc.NumRegs
	code := rc.Code
	if len(code) == 0 || len(rc.SrcPC) != len(code) {
		return "no code"
	}
	reg := func(r uint16) bool { return int(r) < n }
	inCode := func(pc int, off int64) bool {
		t := int64(pc) + 1 + off
		return t >= 0 && t < int64(len(code))
	}
	for pc, w := range code {
		a, b, c := w.A(), w.B(), w.C()
		ok := true
		switch w.Op() {
		case bytecode.RMove, bytecode.RInc, bytecode.RDec, bytecode.RUnary:
			ok = reg(a) && reg(b)
		case bytecode.RLoadInt, bytecode.RLoadUndef, bytecode.RLoadNull, bytecode.RLoadTrue,
			bytecode.RLoadFalse, bytecode.RLoadEmpty, bytecode.RLoadUninit, bytecode.RLoadThis,
			bytecode.RRet, bytecode.RSetGlobal, bytecode.RGetGlobal, bytecode.RGetUpval, bytecode.RSetUpval:
			ok = reg(a)
		case bytecode.RCloseUpvals:
			ok = int(a) <= fn.LocalCount
		case bytecode.RCheckRef:
			ok = reg(a) && int(b) < len(fn.Locals)
		case bytecode.RLoadConst, bytecode.RClosure:
			ok = reg(a) && int(w.BC()) < len(fn.Constants)
		case bytecode.RGetProp, bytecode.RGetMethod:
			ok = reg(a) && reg(b) && int(c) < len(rc.SiteName)
		case bytecode.RSetProp:
			ok = reg(a) && reg(b) && int(c) < len(rc.SiteName)
		case bytecode.RDefineField:
			ok = reg(a) && reg(b) && int(c) < len(fn.Names)
		case bytecode.RGetIndex, bytecode.RSetIndex, bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin,
			bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr:
			ok = reg(a) && reg(b) && reg(c)
		case bytecode.RAddI, bytecode.RSubI, bytecode.RBinI, bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI:
			ok = reg(a) && reg(b)
		case bytecode.RNewObject:
			ok = reg(a)
		case bytecode.RNewArray:
			ok = reg(a) && int(b)+int(c) <= n
		case bytecode.RCall, bytecode.RNew:
			ok = reg(b) && int(a)+int(w.X())+1 <= n
		case bytecode.RCallW:
			ok = reg(b) && int(a)+int(w.X())+2 <= n
		case bytecode.RCallMethod:
			ok = reg(b) && int(a)+int(w.X())+2 <= n
		case bytecode.RJmp:
			ok = inCode(pc, int64(int32(w.BC())))
		case bytecode.RJmpF, bytecode.RJmpT:
			ok = reg(a) && inCode(pc, int64(int32(w.BC())))
		case bytecode.RJmpCmp:
			ok = reg(a) && reg(b) && inCode(pc, int64(w.SC()))
		case bytecode.RJmpCmpI, bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI,
			bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI, bytecode.RJSEqI, bytecode.RJSNeI:
			ok = reg(a) && inCode(pc, int64(w.SC()))
		case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt,
			bytecode.RJGe, bytecode.RJNGe, bytecode.RJSEq, bytecode.RJSNe:
			ok = reg(a) && reg(b) && inCode(pc, int64(w.SC()))
		case bytecode.RRetUndef:
		default:
			ok = false
		}
		if !ok {
			return "instruction " + rc.Format(pc) + " is out of range"
		}
		if int(rc.SrcPC[pc]) >= len(fn.Code) {
			return "a position out of the code"
		}
	}
	switch code[len(code)-1].Op() {
	case bytecode.RJmp, bytecode.RRet, bytecode.RRetUndef:
	default:
		return "the code runs past its end"
	}
	return ""
}
