package compile

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// The lowering of register code (bytecode.RegCode) into the slot IR, the
// new pipeline's front end from the register interpreter's code
// (docs/register-interpreter.md, Stage 3, section 3). A register is a slot:
// the program's first Regs slots are the frame's registers, its states
// have no operands (every Depth 0), and most register instructions are one
// IR instruction with their operands named. A register instruction that
// does several things is several IR instructions of the same origin
// (ir.Program's Origin), which keep the atomicity rule: what may leave does
// so before anything of the instruction has committed, and what follows a
// commit is Generic. Past the registers are the captured bindings' slots
// (UpvalueBase) and the expansions' scratch slots, which no state holds.
//
// Nothing runs it yet but tests: the VM's JIT lowers the stack form
// (LowerSSA), which stays the default until Stage 3a-3's gate.

// LowerReg lowers fn's register code rc for the new pipeline's entry, as
// LowerSSA lowers its stack form: the same functions, the same reasons. What
// it does not lower yet it refuses, with the reason by its kind.
func LowerReg(rc *bytecode.RegCode, fn *bytecode.Function) (*ir.Program, error) {
	return lowerRegRecovered(rc, fn, lowering{ssa: true})
}

// LowerRegHot is LowerReg for a function its callers have shown to be hot
// (LowerSSAHot).
func LowerRegHot(rc *bytecode.RegCode, fn *bytecode.Function) (*ir.Program, error) {
	return lowerRegRecovered(rc, fn, lowering{ssa: true, hot: true})
}

// LowerRegInline is LowerReg for a native callee or an inlined one
// (LowerSSAInline).
func LowerRegInline(rc *bytecode.RegCode, fn *bytecode.Function) (*ir.Program, error) {
	return lowerRegRecovered(rc, fn, lowering{ssa: true, callee: true})
}

// LowerRegForwardWith is LowerRegInline, with hints, for a callee that
// hands its arguments on, f.apply(x, arguments) (apply_args), inlined
// where its caller knows what it forwards to, as LowerSSAForward lowers
// the stack form's: its apply_args is the inliner's to make the call of f
// with the call's own arguments (ssa.InlineSite's Forward), not one that
// reads them from the frame's context (ir.Program's Forwards).
func LowerRegForwardWith(rc *bytecode.RegCode, fn *bytecode.Function, h Hints) (*ir.Program, error) {
	if fn == nil || !forwardsNatively(fn) {
		return nil, refuse(-1, "not a forwarding function")
	}
	return lowerRegRecovered(rc, fn, lowering{ssa: true, callee: true, forward: true, hints: h})
}

// LowerRegWith is LowerReg with hints, whose PCs are register pcs: what
// the function's native code learned at its exits (the stack form's
// LowerSSAWith's).
func LowerRegWith(rc *bytecode.RegCode, fn *bytecode.Function, h Hints) (*ir.Program, error) {
	return lowerRegRecovered(rc, fn, lowering{ssa: true, hints: h})
}

// LowerRegHotWith is LowerRegHot with hints, as LowerRegWith's.
func LowerRegHotWith(rc *bytecode.RegCode, fn *bytecode.Function, h Hints) (*ir.Program, error) {
	return lowerRegRecovered(rc, fn, lowering{ssa: true, hot: true, hints: h})
}

// LowerRegInlineWith is LowerRegInline with hints, as LowerRegWith's.
func LowerRegInlineWith(rc *bytecode.RegCode, fn *bytecode.Function, h Hints) (*ir.Program, error) {
	return lowerRegRecovered(rc, fn, lowering{ssa: true, callee: true, hints: h})
}

// regHinted reports whether the program's instruction of register pc rpc
// is one of pcs, a hint's register pcs.
func regHinted(pcs []int32, rpc uint32) bool { return slices.Contains(pcs, int32(rpc)) }

// regHints applies h, by register pc, to p as the stack form's lowering
// does by its PCs (lowerFunction): an addition that met a string Go's
// (hintedStringAdds), before the reads it takes are decided; then, after
// them (selectNumericProperties), an element read that met what is no
// number read as it is (ir's Reference). It returns the program pcs of the
// element reads that read strings (Hints' StringReads), whose additions
// stringConcats has Go make. An addition the compiler marked an append to
// an upvalue (RAdd's RAddAppend, the stack form's AddOwned on its
// set_upvalue, which hintedStringAdds reads) may grow the string the
// binding alone holds in place (Owned), as the interpreter's rsAdd does:
// unmarked, a closure's s += x had made a new string at each call.
func regHints(rc *bytecode.RegCode, p *ir.Program, h Hints, after bool) []int32 {
	var strs []int32
	for pc := range p.Code {
		in := &p.Code[pc]
		if p.Maps[pc].Depth < 0 {
			continue
		}
		rpc := p.Origin[pc]
		switch {
		case !after && in.Op == ir.Binary && in.Operator == ir.Add && regHinted(h.StringAdds, rpc):
			w := rc.Code[rpc]
			*in = ir.Instruction{Op: ir.StringAdd, Left: in.Left, Right: in.Right, Dest: in.Dest, Generic: in.Generic,
				Owned: w.Op() == bytecode.RAdd && w.X()&bytecode.RAddAppend != 0}
		case after && in.Op == ir.ArrayRead && (regHinted(h.ElementReads, rpc) || regHinted(h.StringReads, rpc)):
			in.Reference = true
			if regHinted(h.StringReads, rpc) {
				strs = append(strs, int32(pc))
			}
		}
	}
	return strs
}

// lowerRegRecovered turns a panic in the lowering into a refusal, as
// lowerRecovered does.
func lowerRegRecovered(rc *bytecode.RegCode, fn *bytecode.Function, m lowering) (p *ir.Program, err error) {
	defer func() {
		if v := recover(); v != nil {
			p, err = nil, refuse(-1, fmt.Sprintf("internal error: %v", v))
		}
	}()
	return lowerReg(rc, regView(rc, fn), m)
}

// regView is fn as rc's instructions see it: code compiled at the
// function's first call (bytecode.RegLater) keeps its names, constants
// and property sites -- the function's own and those it adds -- in rc
// (RegCode.Names), and leaves the Function's, shared by then, as they
// were.
func regView(rc *bytecode.RegCode, fn *bytecode.Function) *bytecode.Function {
	if rc == nil || fn == nil || rc.Names == nil && rc.Constants == nil && rc.PropSites == 0 {
		return fn
	}
	view := *fn
	if rc.Names != nil {
		view.Names = rc.Names
	}
	if rc.Constants != nil {
		view.Constants = rc.Constants
	}
	if rc.PropSites != 0 {
		view.PropSites = rc.PropSites
	}
	return &view
}

// regLowering is one register function's lowering in progress.
type regLowering struct {
	rc *bytecode.RegCode
	fn *bytecode.Function
	m  lowering
	pc int
	// code and origin are the program's instructions so far, and patch
	// marks those whose Target is a register pc, until it is an index.
	code   []ir.Instruction
	origin []uint32
	patch  []bool
	// scratch is the first scratch slot, used how many the current
	// register instruction has taken, most the most any has.
	scratch, used, most int
	// this is a derived constructor's this slot, past the captured
	// bindings, before the scratch slots, or -1 (ir.Program's This); joins
	// marks, in one, the register pcs more than one way reaches.
	this  int
	joins []bool
	// lits are the regexp_lit calls lowered as the stack form's
	// (regexpLit), by register pc; litHosts those lowered as Go's whole,
	// their window's registers live past them.
	lits     []int
	litHosts map[int]bool
	// ins counts, by register pc, the jumps and handlers that reach it,
	// not the instruction before falling to it (regJumpIns).
	ins []int
	// past is the register pc after the loads of the boolean boolBranch
	// lowered last: those before it are Nops.
	past int
}

// regScratch is what a lowering grows its program's instructions in
// (regLowering's code, origin and patch), which the program is given
// copies of at their lengths: grown an instruction at a time, they had
// been twice what the program keeps. It is kept from one lowering for the
// next (regScratchPool); an instruction holds no pointer.
type regScratch struct {
	code   []ir.Instruction
	origin []uint32
	patch  []bool
}

// regScratchPool holds the scratch no lowering has. Separate runtimes
// lower at once, each lowering with one of its own.
var regScratchPool = sync.Pool{New: func() any { return new(regScratch) }}

// emit appends in, of the current register instruction's origin; a
// Target it has is a register pc where jump is set.
func (l *regLowering) emit(in ir.Instruction, jump bool) {
	l.code = append(l.code, in)
	l.origin = append(l.origin, uint32(l.pc))
	l.patch = append(l.patch, jump)
}

// loopTest finds the test of the loop the jump at pc enters: register
// code's loops test at their end -- jmp to the test, the body, the test
// and its branch back to the body, which the interpreter runs one branch
// of each turn -- and the interpreter's back edge, where native code is
// entered by on-stack replacement, is that branch's, at the body's first
// instruction. As it is, that loop has two ways in, the test's from the
// jump and the body's from the entry, and no header that every turn
// passes but the body's, which the jump bypasses: the builder's loop of
// the body's header (ssa's back edges, by pc) reached back to the
// function's start, and the values the loop carries were phis of two
// blocks, the test's not a header's -- not unboxed past the loop, boxed at
// every turn for a use after it, and no loop pass saw them as the
// stack form's, whose loops test at their head, has them. So the test is
// also lowered in place of the jump (lowerLoopTest), its branch the other
// way, to the loop's end, as compilers rotate a loop: the body's first
// instruction is then the header of a loop with one way in, the entry's
// and the copy's, as the stack form's loops have.
//
// t is the test's first register pc, b its branch back to pc+1. Every
// instruction from t to b is reachable, in the jump's handler, goes on to
// the next and jumps only forward, and none suspends or is a step Go may
// take elsewhere (an iterator's).
func (l *regLowering) loopTest(pc int, reach []bool) (t, b int, ok bool) {
	rc := l.rc
	w := rc.Code[pc]
	if w.Op() != bytecode.RJmp {
		return 0, 0, false
	}
	t = regTarget(pc, int32(w.BC()))
	if t <= pc+1 || t >= len(rc.Code) {
		return 0, 0, false
	}
	h := rc.HandlerAt(pc)
	for k := t; k < len(rc.Code); k++ {
		x := rc.Code[k]
		if !reach[k] || rc.HandlerAt(k) != h || regGoBranches(x) {
			return 0, 0, false
		}
		switch x.Op() {
		case bytecode.RIterNext, bytecode.RAwait, bytecode.RYield, bytecode.RYieldStar, bytecode.REndParams, bytecode.RGenEntry, bytecode.RIterResume:
			return 0, 0, false
		}
		succ := regSuccessors(rc, k, x)
		if len(succ) == 2 && succ[0] == k+1 && succ[1] == pc+1 {
			return t, k, true
		}
		if !slices.Contains(succ, k+1) {
			return 0, 0, false
		}
		for _, s := range succ {
			if s <= k {
				return 0, 0, false
			}
		}
	}
	return 0, 0, false
}

// lowerLoopTest lowers the jump at pc into the loop whose test is t to b
// (loopTest) as a copy of the test: a Nop of the jump's, then the test's
// instructions, of their own origins, their jumps inside it to the copy's,
// and its branch back to the body made the other way, to the loop's end,
// past b, so that it goes on into the body, which is next. Where the
// branch's lowering does not end in a branch to the body, nothing is
// copied and it reports false.
func (l *regLowering) lowerLoopTest(pc, t, b int) bool {
	mark, lits := len(l.code), len(l.lits)
	l.emit(ir.Instruction{Op: ir.Nop}, false)
	at := make([]int, b-t+1)
	for k := t; k <= b; k++ {
		at[k-t] = len(l.code)
		l.pc, l.used = k, 0
		l.lower(k)
	}
	l.pc, l.used = pc, 0
	last := len(l.code) - 1
	if in := &l.code[last]; in.Op != ir.Branch || !l.patch[last] || in.Target != pc+1 || l.origin[last] != uint32(b) {
		l.code, l.origin, l.patch, l.lits = l.code[:mark], l.origin[:mark], l.patch[:mark], l.lits[:lits]
		return false
	}
	l.code[last].When, l.code[last].Target = !l.code[last].When, b+1
	for i := mark; i < len(l.code); i++ {
		if x := l.code[i].Target; l.patch[i] && x > t && x <= b {
			l.code[i].Target, l.patch[i] = at[x-t], false
		}
	}
	return true
}

// regJumpIns counts, by register pc, the jumps of rc's reachable code
// and the handlers that go there.
func regJumpIns(rc *bytecode.RegCode, reach []bool) []int {
	ins := make([]int, len(rc.Code)+1)
	for pc, w := range rc.Code {
		if !reach[pc] {
			continue
		}
		for _, t := range regSuccessors(rc, pc, w) {
			if t != pc+1 && t >= 0 && t < len(ins) {
				ins[t]++
			}
		}
	}
	for _, h := range rc.Handlers {
		if int(h.To) < len(ins) {
			ins[h.To]++
		}
	}
	return ins
}

// boolBranch lowers, at pc, the register code a comparison's boolean
// value compiles to -- a compare-and-branch to pc+3, load_false or
// load_true to a register, a jump to pc+4, the other boolean to the
// register at pc+3, which nothing else jumps into -- as the comparison
// it is: the comparison's Binary to the register, negated where the
// branch's way makes it so, and a jump past the loads, which are left
// unreached. Lowered as the branches, the boolean was a phi of two
// constants that a test of it, a callback's result's, branched on again;
// as the stack form's lt or strict_eq makes it, it is the comparison,
// which a branch on it takes as its condition. An exit at the comparison
// runs the compare-and-branch again, which the interpreter goes on from
// to the loads. The loads and the jump are Nops (past), which native code
// falls through to pc+4, so that the comparison, the register's copies
// and what reads it are one block, as the stack form's are: an operator
// of the boolean takes its number natively (ir.Instruction's Bools,
// markBools), where a jump to pc+4 had begun a block there and the
// operator, not knowing it a boolean, deoptimized at it -- Emscripten's
// code ORs and ANDs comparisons, and Octane's zlib's largest function
// deoptimized at one after another, each compile of it a second's.
// Where the branch's lowering is not a Branch to pc+3 of an operator a
// Binary has, nothing is lowered and it reports false.
func (l *regLowering) boolBranch(pc int, reach []bool) bool {
	rc := l.rc
	if pc+4 >= len(rc.Code) {
		return false
	}
	w := rc.Code[pc]
	succ := regSuccessors(rc, pc, w)
	if len(succ) != 2 || succ[0] != pc+1 || succ[1] != pc+3 {
		return false
	}
	x, j, y := rc.Code[pc+1], rc.Code[pc+2], rc.Code[pc+3]
	loads := func(a, b bytecode.ROp) bool {
		return a == bytecode.RLoadTrue && b == bytecode.RLoadFalse || a == bytecode.RLoadFalse && b == bytecode.RLoadTrue
	}
	if !loads(x.Op(), y.Op()) || x.A() != y.A() || j.Op() != bytecode.RJmp || regTarget(pc+2, int32(j.BC())) != pc+4 {
		return false
	}
	for q := pc + 1; q <= pc+4; q++ {
		if !reach[q] {
			return false
		}
	}
	if l.ins[pc+1] != 0 || l.ins[pc+2] != 0 || l.ins[pc+3] != 1 {
		return false
	}
	mark, lits := len(l.code), len(l.lits)
	l.pc, l.used = pc, 0
	l.lower(pc)
	last := len(l.code) - 1
	in := l.code[last]
	switch {
	case last < mark || in.Op != ir.Branch || !l.patch[last] || in.Target != pc+3:
	case in.Operator == ir.Eq || in.Operator == ir.Lt || in.Operator == ir.Le || in.Operator == ir.Gt || in.Operator == ir.Ge:
		r := int(x.A())
		if in.When != (y.Op() == bytecode.RLoadTrue) {
			// Negated: the comparison to a scratch slot, which commits
			// nothing, and its negation to the register, the origin's one
			// write and its last -- the comparison written to the register
			// first, the Not, which may leave, came after a write the
			// interpreter would make again (the atomicity rule).
			t := l.temp()
			l.code[last] = ir.Instruction{Op: ir.Binary, Operator: in.Operator, Left: in.Left, Right: in.Right, Dest: t, Strict: in.Strict}
			l.patch[last] = false
			l.emit(ir.Instruction{Op: ir.Unary, Operator: ir.Not, Left: ir.Slot(t), Dest: r}, false)
		} else {
			l.code[last] = ir.Instruction{Op: ir.Binary, Operator: in.Operator, Left: in.Left, Right: in.Right, Dest: r, Strict: in.Strict}
			l.patch[last] = false
		}
		l.past = pc + 4
		return true
	}
	l.code, l.origin, l.patch, l.lits = l.code[:mark], l.origin[:mark], l.patch[:mark], l.lits[:lits]
	return false
}

// regexpLit lowers regexp_lit at pc, w, that calls -- /x/.exec(s) or
// /x/.test(s), RLitCall -- as the stack form's code is lowered: the
// literal's RegExp from its pool (ObjectLiteral), its method read
// (ReferenceRead), both in scratch slots, the argument copied past them,
// and the call of the method in that window (a Window Host), its value to
// R[B] -- nothing committed before the call, so that an exit anywhere runs
// the instruction whole again, as the interpreter makes it. The window's
// registers, R[A] and R[A+1], are not written: where they are live past
// the instruction (litHosts, lowerReg's check), or the method's name is
// not the function's, it reports false and the instruction is Go's whole.
func (l *regLowering) regexpLit(pc int, w bytecode.RInstr) bool {
	x := w.X()
	if l.litHosts[pc] {
		return false
	}
	name := "exec"
	if x&bytecode.RLitTest != 0 {
		name = "test"
	}
	key := slices.Index(l.fn.Names, name)
	if key < 0 {
		return false
	}
	a := w.A()
	if x&bytecode.RLitCall == 0 {
		// One that does not call: the literal and its method to the
		// window, R[A+1] and R[A], for the call_method_w after it --
		// the read first, which may run a getter, then only copies.
		method, lit := l.temp(), l.temp()
		l.emit(ir.Instruction{Op: ir.ObjectLiteral, Dest: lit}, false)
		l.emit(ir.Instruction{Op: ir.ReferenceRead, Left: ir.Slot(lit), Dest: method, Key: uint32(key)}, false)
		l.emit(ir.Instruction{Op: ir.Copy, Dest: int(a) + 1, Left: ir.Slot(lit)}, false)
		l.emit(ir.Instruction{Op: ir.Copy, Dest: int(a), Left: ir.Slot(method)}, false)
		return true
	}
	method, lit, arg := l.temp(), l.temp(), l.temp()
	l.emit(ir.Instruction{Op: ir.ObjectLiteral, Dest: lit}, false)
	l.emit(ir.Instruction{Op: ir.ReferenceRead, Left: ir.Slot(lit), Dest: method, Key: uint32(key)}, false)
	l.emit(ir.Instruction{Op: ir.Copy, Dest: arg, Left: reg(a + 2)}, false)
	l.emit(ir.Instruction{Op: ir.Host, Extra: 1, When: true, Window: true, Left: ir.Slot(method), Dest: int(w.B())}, false)
	l.lits = append(l.lits, pc)
	return true
}

// litsLive are the register pcs of l's regexpLit calls whose window's
// registers -- R[A] and R[A+1], but the one the value goes to -- are live
// past them in p, whose instructions of register pc q start at start[q].
func (l *regLowering) litsLive(p *ir.Program, start []int) []int {
	var live []int
	for _, pc := range l.lits {
		w := l.rc.Code[pc]
		a, b := int(w.A()), int(w.B())
		next := start[pc+1]
		if next >= len(p.Code) {
			continue
		}
		for _, r := range [2]int{a, a + 1} {
			if r != b && p.LiveAt(next, r) {
				live = append(live, pc)
				break
			}
		}
	}
	return live
}

// temp is a scratch slot of the current register instruction's.
func (l *regLowering) temp() int {
	s := l.scratch + l.used
	l.used++
	l.most = max(l.most, l.used)
	return s
}

// name is the index in fn.Names of the name operand n of the instruction
// at pc (bytecode.RNameFar).
func (l *regLowering) name(n uint16) uint32 {
	if n == bytecode.RNameFar && l.pc < len(l.rc.FarNames) {
		return l.rc.FarNames[l.pc]
	}
	return uint32(n)
}

// siteName is the name of property site c.
func (l *regLowering) siteName(c uint16) uint32 {
	if int(c) < len(l.rc.SiteName) {
		return l.rc.SiteName[c]
	}
	panic(fmt.Sprintf("property site %d past the code's names", c))
}

func reg(n uint16) ir.Operand { return ir.Slot(int(n)) }

func number(n float64) ir.Operand { return ir.Literal(ir.Float(n)) }

// regTarget is where a jump of the instruction at pc by off goes.
func regTarget(pc int, off int32) int { return pc + 1 + int(off) }

// regTypes are the types RJTypeof's X names, in the VM's typeofNames'
// order, as ir's TypeTest names them.
var regTypes = [8]uint32{ir.TypeUndefined, ir.TypeObject, ir.TypeBoolean, ir.TypeNumber, ir.TypeString, ir.TypeSymbol, ir.TypeBigInt, ir.TypeFunction}

// regBranch is a compare-and-branch's operator and the way it jumps.
func regBranch(op bytecode.ROp) (ir.Operator, bool, bool) {
	switch op {
	case bytecode.RJLt, bytecode.RJLtI, bytecode.RJLtU:
		return ir.Lt, true, true
	case bytecode.RJNLt, bytecode.RJNLtI, bytecode.RJNLtU:
		return ir.Lt, false, true
	case bytecode.RJLe, bytecode.RJLeI, bytecode.RJLeU:
		return ir.Le, true, true
	case bytecode.RJNLe, bytecode.RJNLeI, bytecode.RJNLeU:
		return ir.Le, false, true
	case bytecode.RJGt, bytecode.RJGtI:
		return ir.Gt, true, true
	case bytecode.RJNGt, bytecode.RJNGtI:
		return ir.Gt, false, true
	case bytecode.RJGe, bytecode.RJGeI:
		return ir.Ge, true, true
	case bytecode.RJNGe, bytecode.RJNGeI:
		return ir.Ge, false, true
	case bytecode.RJEq, bytecode.RJEqI, bytecode.RJSEq, bytecode.RJSEqI:
		return ir.Eq, true, true
	case bytecode.RJNe, bytecode.RJNeI, bytecode.RJSNe, bytecode.RJSNeI:
		return ir.Eq, false, true
	}
	return 0, false, false
}

// regGoBranches reports whether the instruction w is one Go runs that may
// go on past the instruction after it, a jump it skips (the register
// form's iter_resume, iter_unpack_delegate, iter_result and
// async_iter_close): native code has an entry there as at the jump.
func regGoBranches(w bytecode.RInstr) bool {
	switch w.Op() {
	case bytecode.RIterResume, bytecode.RIterUnpackDelegate, bytecode.RIterResult, bytecode.RAsyncIterClose:
		return true
	}
	return false
}

// regDefinition reports whether cold operation op makes a class's or an
// object literal's member, or a private one's, which a callee's code
// leaves to Go (as lowerFunction's isDefinition and isPrivate): of those,
// what reads only its operands and the frame (frameDefinition), and the
// operations Go makes from native code (GoOperation), a callee keeps.
func regDefinition(op bytecode.ROp) bool {
	switch op {
	case bytecode.RDefineGetterIndex, bytecode.RDefineSetterIndex, bytecode.RClassGetterIndex,
		bytecode.RClassSetterIndex,
		bytecode.RAddPrivateMethod, bytecode.RAddPrivateGetter, bytecode.RAddPrivateSetter,
		bytecode.RDefinePrivate, bytecode.RDefinePrivateMethod, bytecode.RDefinePrivateGetter,
		bytecode.RDefinePrivateSetter:
		// A key in a register, a computed name's or a private name's: the
		// frame Go makes from native code does not show the registers.
		return true
	}
	return false
}

// regRefusal is the reason the instruction at pc is not lowered, or "".
func regRefusal(rc *bytecode.RegCode, fn *bytecode.Function, m lowering, pc int) string {
	w := rc.Code[pc]
	op := w.Op()
	switch op {
	case bytecode.RBin:
		raw := uint32(w.X())
		if _, ok := operator(raw); !ok && raw != uint32(bytecode.OpMod) && raw != uint32(bytecode.OpPow) {
			return "unsupported operator " + bytecode.Op(w.X()).String()
		}
	case bytecode.RUnary:
		switch bytecode.Op(w.X()) {
		case bytecode.OpNeg, bytecode.OpPos, bytecode.OpNot, bytecode.OpBitNot, bytecode.OpTypeOf, bytecode.OpToNumber, bytecode.OpToNumeric:
		default:
			return "unsupported operator " + bytecode.Op(w.X()).String()
		}
	case bytecode.RApplyArgs:
		if m.callee && !forwardsNatively(fn) {
			// As lowerFunction refuses apply_arguments in a callee.
			return "unsupported opcode apply_arguments"
		}
	case bytecode.RSwitch:
		for _, c := range rc.Switches[w.BC()] {
			if c.Kind == bytecode.SwitchStr && regStringConst(fn, c.Str) < 0 {
				return "switch case of no constant"
			}
		}
	case bytecode.RDirectEval:
		return "direct eval"
	case bytecode.RWithGet, bytecode.RWithGetThis, bytecode.RWithDelete, bytecode.RWithSet,
		bytecode.RWithResolve, bytecode.RWithRead, bytecode.RWithWrite, bytecode.RWithPush, bytecode.RWithPop,
		bytecode.RDebug:
		// The stack form's with and debug_stmt, which lowerFunction
		// refuses (describe).
		return "unsupported opcode " + op.String()
	case bytecode.RMovFar, bytecode.RMovToFar, bytecode.RFarInit:
		// Registers past 16 bits, past what native code's records name.
		return "slot budget"
	}
	if m.callee && regDefinition(op) {
		return "unsupported opcode " + op.String()
	}
	return ""
}

// regStringConst is the index of a string constant of fn's whose value is
// s, or -1.
func regStringConst(fn *bytecode.Function, s string) int {
	for i, c := range fn.Constants {
		if c.Kind == bytecode.ConstString && c.Str == s {
			return i
		}
	}
	return -1
}

func lowerReg(rc *bytecode.RegCode, fn *bytecode.Function, m lowering) (*ir.Program, error) {
	return lowerRegLits(rc, fn, m, nil)
}

// lowerRegLits is lowerReg, the regexp_lit calls at the register pcs in
// litHosts Go's whole (regexpLit).
func lowerRegLits(rc *bytecode.RegCode, fn *bytecode.Function, m lowering, litHosts map[int]bool) (*ir.Program, error) {
	switch {
	case fn == nil || rc == nil:
		return nil, refuse(-1, "nil function")
	case !rc.Wide:
		return nil, refuse(-1, "no register code")
	case (fn.TopLevel || fn.IsModule) && m.callee:
		return nil, refuse(-1, "top-level or module code")
	case (fn.Async || fn.Generator) && m.callee:
		return nil, refuse(-1, "async or generator function")
	case fn.Kind != bytecode.KindNormal && fn.Kind != bytecode.KindArrow && fn.Kind != bytecode.KindMethod && fn.Kind != bytecode.KindConstructor &&
		fn.Kind != bytecode.KindGetter && fn.Kind != bytecode.KindSetter && fn.Kind != bytecode.KindDerivedConstructor:
		return nil, refuse(-1, "special function kind")
	case fn.HasDirectEval || len(fn.EvalScopes) != 0:
		return nil, refuse(-1, "direct eval")
	case (fn.UsesArguments || fn.MappedArguments) && !argumentsOwn(fn):
		return nil, refuse(-1, "arguments object")
	case len(rc.Code) == 0:
		return nil, refuse(-1, "no instructions")
	case rc.NumRegs < 2 || rc.NumRegs > maxSlots || len(fn.Upvalues) > maxSlots || rc.NumRegs+len(fn.Upvalues) > maxSlots || rc.Params < 0 || 2+rc.Params > rc.NumRegs:
		return nil, refuse(-1, "slot budget")
	}
	n := len(rc.Code)
	for pc := range n {
		if why := regRefusal(rc, fn, m, pc); why != "" {
			return nil, refuse(pc, why)
		}
	}
	reach, err := regReachable(rc)
	if err != nil {
		return nil, err
	}
	l := &regLowering{rc: rc, fn: fn, m: m, scratch: rc.NumRegs + len(fn.Upvalues), this: -1, litHosts: litHosts, ins: regJumpIns(rc, reach)}
	ls := regScratchPool.Get().(*regScratch)
	l.code, l.origin, l.patch = ls.code[:0], ls.origin[:0], ls.patch[:0]
	defer func() {
		ls.code, ls.origin, ls.patch = l.code[:0], l.origin[:0], l.patch[:0]
		regScratchPool.Put(ls)
	}()
	derived := fn.Kind == bytecode.KindDerivedConstructor
	if derived {
		// A derived constructor's this binding, past the captured
		// bindings (ir.Program's This): the context's This, which its
		// super() binds, as the stack form's this slot.
		l.this, l.scratch = l.scratch, l.scratch+1
		l.joins = regJoins(rc, reach)
	}
	start := make([]int, n+1)
	for pc := range n {
		start[pc] = len(l.code)
		l.pc, l.used = pc, 0
		if !reach[pc] || pc < l.past {
			l.emit(ir.Instruction{Op: ir.Nop}, false)
			continue
		}
		if t, b, ok := l.loopTest(pc, reach); ok && l.lowerLoopTest(pc, t, b) {
			continue
		}
		if l.boolBranch(pc, reach) {
			continue
		}
		l.lower(pc)
	}
	start[n] = len(l.code)
	if l.scratch+l.most > maxSlots {
		return nil, refuse(-1, "slot budget")
	}
	for i := range l.code {
		if l.patch[i] {
			t := l.code[i].Target
			if t < 0 || t >= n || !reach[t] {
				return nil, refuse(int(l.origin[i]), "branch target out of bounds")
			}
			l.code[i].Target = start[t]
		}
	}
	starts := make([]int32, n+1)
	for pc, i := range start {
		starts[pc] = int32(i)
	}
	p := &ir.Program{Code: slices.Clone(l.code), Origin: slices.Clone(l.origin), Starts: starts, Maps: make([]ir.StateMap, len(l.code)),
		Regs: rc.NumRegs, Locals: l.scratch + l.most, ParamBase: 2, Params: rc.Params,
		UpvalueBase: rc.NumRegs, Upvalues: len(fn.Upvalues),
		ArrowThis:        fn.Kind == bytecode.KindArrow && fn.UsesThis,
		ClassConstructor: fn.Kind == bytecode.KindConstructor,
		TailOwn: fn.Strict && (fn.Kind == bytecode.KindNormal || fn.Kind == bytecode.KindMethod || fn.Kind == bytecode.KindArrow ||
			fn.Kind == bytecode.KindGetter || fn.Kind == bytecode.KindSetter),
		This: derived, DerivedConstructor: derived,
	}
	for i := range p.Maps {
		p.Maps[i] = ir.StateMap{PC: l.origin[i]}
		if !reach[l.origin[i]] {
			p.Maps[i].Depth = -1
		}
	}
	l.handlers(p, start, reach)
	for pc := range n {
		w := rc.Code[pc]
		var to []int
		switch {
		case w.Op() == bytecode.RIterNext:
			// Go may take the loop's end (goBranch).
			to = append(to, regTarget(pc, w.SC()))
		case regGoBranches(w):
			to = append(to, pc+1, pc+2)
		case w.Op() == bytecode.RGenEntry && pc == 0:
			// A first run enters past it, which does nothing there (the
			// VM's regGenRun).
			to = append(to, pc+1)
		case w.Op() == bytecode.REndParams && (fn.Async || fn.Generator):
			// The first resumption enters past the parameters.
			to = append(to, pc+1)
		case w.Op() == bytecode.RAwait:
			// An await's resumption enters after it (the VM's regGenRun).
			to = append(to, pc+1)
		case w.Op() == bytecode.RYield && pc+1 < n && rc.Code[pc+1].Op() == bytecode.RJmp:
			// A yield's resumption by next enters past the jump after it,
			// as regGenResume goes on; its return's, at the return path
			// the jump goes past.
			to = append(to, regTarget(pc+1, int32(rc.Code[pc+1].BC())), pc+2)
		}
		for _, t := range to {
			if reach[pc] && t < n && reach[t] && !slices.Contains(p.Entries, start[t]) {
				p.Entries = append(p.Entries, start[t])
			}
		}
	}
	if regHostOnly(p, rc, fn, m) {
		return nil, refuse(-1, hostOnly)
	}
	regHints(rc, p, m.hints, false)
	selectNumericProperties(p, true)
	stringConcats(p, regHints(rc, p, m.hints, true))
	markBools(p)
	p.FrameArgs = fn.HasRest || fn.UsesArguments || fn.MappedArguments || fn.SuperForwards
	p.FrameCallee = fn.FrameCallee
	if fn.MappedArguments && fn.ParamCount > 0 && (fn.AssignsParams || paramCaptured(fn)) {
		p.ArgumentParams = min(fn.ParamCount, rc.Params)
	}
	if forwardsNatively(fn) && !m.forward && slices.ContainsFunc(rc.Code, func(w bytecode.RInstr) bool { return w.Op() == bytecode.RApplyArgs }) {
		p.Forwards = true
	}
	regCaptured(p, rc, fn)
	liveRegs(p, rc)
	regStringParts(p)
	if live := l.litsLive(p, start); len(live) != 0 {
		// Their window's registers read past them, which the lowering
		// does not write: Go's whole.
		hosts := maps.Clone(litHosts)
		if hosts == nil {
			hosts = map[int]bool{}
		}
		for _, pc := range live {
			hosts[pc] = true
		}
		return lowerRegLits(rc, fn, m, hosts)
	}
	return p, nil
}

// regStringParts makes each conversion of a part where it is -- to_string's
// or str_part's, a StringConcat of one part -- whose register is read by a
// concat of more parts and by nothing before it, a StringPart, as the
// stack form's stringParts does: the concat converts a primitive that
// calls nothing the same, so the part needs only checking to be one, not a
// call of Go of its own. Register code's concat reads its parts where they
// are, or copies of them in scratch slots (lowerReg's block): a copy of
// the part's register into the concat's run is the concat's read. Where
// anything else reads or writes the register first, a jump lands between
// them, the code goes elsewhere, or the register is live past the concat
// -- which would read it unconverted -- it stays as it is. An exit between
// them leaves the register the primitive it was, which the interpreter's
// concat converts the same. It runs on p's liveness (liveRegs).
func regStringParts(p *ir.Program) {
	targets := make([]bool, len(p.Code)+1)
	for _, in := range p.Code {
		if in.Op == ir.Jump || in.Op == ir.Branch || in.Op == ir.IterStep {
			targets[in.Target] = true
		}
	}
	for _, e := range p.Entries {
		targets[e] = true
	}
	for pc := range p.Code {
		in := p.Code[pc]
		if in.Op != ir.StringConcat || in.Extra != 1 || p.Maps[pc].Depth < 0 || in.Dest < 0 || in.Dest >= p.Regs {
			continue
		}
		d := in.Dest
		copies := []int{d}
		reads := func(c ir.Instruction) bool {
			return c.Op == ir.StringConcat && c.Extra > 1 && slices.ContainsFunc(copies, func(s int) bool { return c.Dest <= s && s < c.Dest+c.Extra })
		}
		for k := pc + 1; k < len(p.Code); k++ {
			c := p.Code[k]
			if targets[k] || p.Maps[k].Depth < 0 {
				break
			}
			if reads(c) {
				// Live past the concat's origin, whose instructions are
				// all as live as its first.
				next := k + 1
				for next < len(p.Code) && p.Origin[next] == p.Origin[k] {
					next++
				}
				if next < len(p.Code) && !p.LiveAt(next, d) {
					p.Code[pc] = ir.Instruction{Op: ir.StringPart, Left: ir.Slot(d), Dest: d}
				}
				break
			}
			if c.Op == ir.Copy && c.Left.Slot == d && c.Dest >= p.Regs {
				// Into the concat's run of scratch slots.
				copies = append(copies, c.Dest)
				continue
			}
			if c.Op == ir.Jump || c.Op == ir.Branch || c.Op == ir.Return || c.Op == ir.Rethrow || c.Op == ir.Host && !c.Whole && !c.Window ||
				c.Left.Slot == d || c.Right.Slot == d || c.Third.Slot == d || c.Dest == d || c.Extra == d && c.Op != ir.StringConcat && c.Op != ir.ArrayLiteral ||
				c.Op == ir.StringConcat && c.Dest <= d && d < c.Dest+c.Extra || c.Window && c.Left.Slot <= d && d < c.Left.Slot+2+c.Extra {
				break
			}
		}
	}
}

// regReachable is which register instructions the code's start reaches,
// by its jumps, its fall from one to the next and its handlers.
func regReachable(rc *bytecode.RegCode) ([]bool, error) {
	n := len(rc.Code)
	reach := make([]bool, n)
	queue := []int{0}
	reach[0] = true
	add := func(from, t int) error {
		if t < 0 || t >= n {
			if t == n && from >= 0 {
				return refuse(from, "fallthrough past function")
			}
			return refuse(from, "branch target out of bounds")
		}
		if !reach[t] {
			reach[t] = true
			queue = append(queue, t)
		}
		return nil
	}
	for changed := true; changed; {
		changed = false
		for len(queue) > 0 {
			pc := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			w := rc.Code[pc]
			for _, t := range regSuccessors(rc, pc, w) {
				if err := add(pc, t); err != nil {
					return nil, err
				}
			}
		}
		for _, h := range rc.Handlers {
			if reach[h.To] {
				continue
			}
			for j := 0; j+1 < len(h.Ranges); j += 2 {
				for pc := h.Ranges[j]; pc < h.Ranges[j+1] && int(pc) < n; pc++ {
					if reach[pc] {
						if err := add(-1, int(h.To)); err != nil {
							return nil, err
						}
						changed = true
						break
					}
				}
				if reach[h.To] {
					break
				}
			}
		}
	}
	return reach, nil
}

// RegSuccessors is where rc's code may go on from w, the instruction at
// pc: its jumps' targets and the next instruction it falls to.
func RegSuccessors(rc *bytecode.RegCode, pc int, w bytecode.RInstr) []int {
	return regSuccessors(rc, pc, w)
}

// regSuccessors is where the code may go on from w, the instruction at pc.
func regSuccessors(rc *bytecode.RegCode, pc int, w bytecode.RInstr) []int {
	switch op := w.Op(); op {
	case bytecode.RJmp:
		return []int{regTarget(pc, int32(w.BC()))}
	case bytecode.RRet, bytecode.RRetUndef, bytecode.RThrow:
		return nil
	case bytecode.RJmpF, bytecode.RJmpT:
		return []int{pc + 1, regTarget(pc, int32(w.BC()))}
	case bytecode.RSwitch, bytecode.RSwitchTypeof:
		out := []int{pc + 1}
		for _, c := range rc.Switches[w.BC()] {
			out = append(out, int(c.To))
		}
		return out
	case bytecode.RJTypeof, bytecode.RJNull, bytecode.RJNotNull, bytecode.RIterNext:
		return []int{pc + 1, regTarget(pc, w.SC())}
	default:
		if _, _, ok := regBranch(op); ok {
			return []int{pc + 1, regTarget(pc, w.SC())}
		}
		if regGoBranches(w) {
			return []int{pc + 1, pc + 2}
		}
	}
	return []int{pc + 1}
}

// regJoins marks the register pcs of rc a jump or a handler reaches: those
// the instruction before is not the only way to.
func regJoins(rc *bytecode.RegCode, reach []bool) []bool {
	joins := make([]bool, len(rc.Code)+1)
	for pc, w := range rc.Code {
		if !reach[pc] {
			continue
		}
		for _, t := range regSuccessors(rc, pc, w) {
			if t != pc+1 && t >= 0 && t < len(joins) {
				joins[t] = true
			}
		}
	}
	for _, h := range rc.Handlers {
		if int(h.To) < len(joins) {
			joins[h.To] = true
		}
	}
	return joins
}

// undefinedAt reports whether register r certainly holds undefined at
// register pc, as the instructions straight before it, which nothing
// jumps between, leave it: load_undef's, or moved from one.
func (l *regLowering) undefinedAt(pc, r int) bool {
	for q := pc - 1; q >= 0 && !l.joins[q+1]; q-- {
		w := l.rc.Code[q]
		switch w.Op() {
		case bytecode.RLoadUndef:
			if int(w.A()) == r {
				return true
			}
			continue
		case bytecode.RMove:
			if int(w.A()) == r {
				r = int(w.B())
			}
			continue
		case bytecode.RNop:
			continue
		}
		return false
	}
	return false
}

// handlers gives p the code's handlers: rc's, innermost first, in the
// order ir's table has them, an outer one before those inside it; and
// each instruction's innermost.
func (l *regLowering) handlers(p *ir.Program, start []int, reach []bool) {
	rc := l.rc
	if len(rc.Handlers) == 0 {
		return
	}
	k := len(rc.Handlers)
	// rc's handler i is the table's k-1-i.
	covers := func(h *bytecode.RHandler, pc uint32) bool {
		for j := 0; j+1 < len(h.Ranges); j += 2 {
			if pc >= h.Ranges[j] && pc < h.Ranges[j+1] {
				return true
			}
		}
		return false
	}
	p.Handlers = make([]ir.Handler, k)
	for i := range rc.Handlers {
		h := &rc.Handlers[i]
		outer := int32(0)
		if len(h.Ranges) > 0 {
			// The innermost of those after it in rc's order -- outside it
			// -- that holds its first instruction.
			for j := i + 1; j < k; j++ {
				if covers(&rc.Handlers[j], h.Ranges[0]) {
					outer = int32(k - j)
					break
				}
			}
		}
		p.Handlers[k-1-i] = ir.Handler{PC: uint32(start[h.To]), Outer: outer, Finally: !h.Catches}
	}
	p.HandlerAt = make([]int32, len(p.Code))
	for i := range p.Code {
		if h := rc.HandlerAt(int(l.origin[i])); h != nil {
			p.HandlerAt[i] = int32(k - handlerIndex(rc.Handlers, h))
		}
	}
	for _, h := range rc.Handlers {
		if reach[h.To] && !slices.Contains(p.Entries, start[h.To]) {
			// A throw Go catches goes on natively at its handler.
			p.Entries = append(p.Entries, start[h.To])
		}
	}
}

// handlerIndex is h's index in hs.
func handlerIndex(hs []bytecode.RHandler, h *bytecode.RHandler) int {
	for i := range hs {
		if &hs[i] == h {
			return i
		}
	}
	panic("handler not in the code's")
}

// regCaptured marks the registers closures the code makes capture
// (Program's Captured), and of those the ones a closure assigns.
func regCaptured(p *ir.Program, rc *bytecode.RegCode, fn *bytecode.Function) {
	for pc, w := range rc.Code {
		if w.Op() != bytecode.RClosureW || p.Maps[p.At(uint32(pc))].Depth < 0 {
			continue
		}
		k := w.BC()
		if int(k) >= len(rc.Caps) || int(k) >= len(fn.Constants) || fn.Constants[k].Fn == nil {
			continue
		}
		ups := fn.Constants[k].Fn.Upvalues
		for i, r := range rc.Caps[k] {
			if i >= len(ups) || !ups[i].FromParent || int(r) >= p.Regs {
				continue
			}
			if p.Captured == nil {
				p.Captured = make([]bool, p.Regs)
			}
			p.Captured[r] = true
			if ups[i].Assigned {
				if p.Assigned == nil {
					p.Assigned = make([]bool, p.Regs)
				}
				p.Assigned[r] = true
			}
		}
	}
	// In cells of their own (ir.Program's Cells), as the stack form's.
	p.Cells = p.Captured != nil
}

// regHostOnly reports whether the code does nothing native code saves time
// on, as lowerFunction finds it of the stack form: host operations without
// a loop or an element's work -- but for a callee, one shown to be hot that
// makes calls, and a generator a loop's step resumes.
func regHostOnly(p *ir.Program, rc *bytecode.RegCode, fn *bytecode.Function, m lowering) bool {
	host, loop, indexed, calls := false, false, false, false
	for i, in := range p.Code {
		if p.Maps[i].Depth < 0 {
			continue
		}
		switch in.Op {
		case ir.Nop, ir.Copy, ir.CopyPair, ir.StoreLoad, ir.Swap, ir.Unary, ir.Update, ir.Jump, ir.Branch, ir.Return,
			ir.ArrayRead, ir.ArrayWrite, ir.ArrayLength, ir.ArrayKey, ir.ArrayUpdate, ir.UpvalueRead, ir.UpvalueWrite,
			ir.Coercible, ir.StringMethod, ir.StringCode, ir.Rethrow, ir.ArgumentRead, ir.StringFreeze, ir.StringAdd:
		case ir.Binary:
			host = host || in.Operator == ir.Mod || in.Operator == ir.Pow
		default:
			host = true
		}
		if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= i {
			loop = true
		}
	}
	for _, w := range rc.Code {
		switch w.Op() {
		case bytecode.RGetIndex, bytecode.RGetIndexI, bytecode.RSetIndex, bytecode.RSetIndexI, bytecode.RGetIndexPre, bytecode.RGetIndexPost,
			bytecode.RAddIndex, bytecode.RAddIndexK, bytecode.RAddIndexI:
			indexed = true
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW, bytecode.RApplyArgs:
			calls = true
		case bytecode.RCallWT, bytecode.RCallMWT:
			// A call in tail position may be the function's own, which
			// native code makes a jump to its start deep in the stack
			// (ssa.OpTailSelf): a loop, as the stack form's.
			calls, loop = true, true
		case bytecode.RSuperCall, bytecode.RSuperForward:
			// A super(): a call all the same.
			calls = true
		}
	}
	return host && !loop && !indexed && !m.callee && !(m.hot && calls) && !GeneratorResumable(fn)
}

// lower appends the IR of the register instruction at pc.
func (l *regLowering) lower(pc int) {
	rc, fn := l.rc, l.fn
	w := rc.Code[pc]
	a, b, c := w.A(), w.B(), w.C()
	host := func() { l.emit(l.hostAt(pc), false) }
	copyTo := func(dest int, src ir.Operand) { l.emit(ir.Instruction{Op: ir.Copy, Dest: dest, Left: src}, false) }
	binary := func(op ir.Operator, left, right ir.Operand, strict bool) {
		l.emit(ir.Instruction{Op: ir.Binary, Operator: op, Left: left, Right: right, Dest: int(a), Strict: strict}, false)
	}
	branch := func(op ir.Operator, left, right ir.Operand, when, strict bool, target int) {
		l.emit(ir.Instruction{Op: ir.Branch, Operator: op, Left: left, Right: right, When: when, Strict: strict, Target: target}, true)
	}
	// block lowers an operation of the n registers from first, a literal's
	// elements or a concat's parts, to dest: in place where they start at
	// dest, else copied to scratch slots first, the result copied out.
	block := func(dest, first, n int, op ir.Instruction) {
		base := first
		if dest != first {
			base = l.scratch + l.used
			for range n {
				l.temp()
			}
			if n == 0 {
				base = l.temp()
			}
			for i := range n {
				copyTo(base+i, ir.Slot(first+i))
			}
		}
		op.Dest, op.Extra = base, n
		l.emit(op, false)
		if base != dest {
			copyTo(dest, ir.Slot(base))
		}
	}
	switch op := w.Op(); op {
	case bytecode.RNop:
		l.emit(ir.Instruction{Op: ir.Nop}, false)
	case bytecode.RMove:
		copyTo(int(a), reg(b))
	case bytecode.RLoadInt:
		copyTo(int(a), number(float64(int32(w.BC()))))
	case bytecode.RLoadConst:
		k := w.BC()
		switch fn.Constants[k].Kind {
		case bytecode.ConstNumber:
			copyTo(int(a), number(fn.Constants[k].Num))
		case bytecode.ConstString:
			// Read from a cell the VM keeps it in (ssa.ConstantFeedback).
			l.emit(ir.Instruction{Op: ir.StringConst, Dest: int(a), Key: k}, false)
		case bytecode.ConstBigInt:
			l.emit(ir.Instruction{Op: ir.BigIntConst, Dest: int(a), Key: k}, false)
		default:
			host()
		}
	case bytecode.RLoadUndef, bytecode.RLoadNull, bytecode.RLoadTrue, bytecode.RLoadFalse, bytecode.RLoadUninit:
		v := ir.Value{Kind: ir.Undefined}
		switch op {
		case bytecode.RLoadNull:
			v.Kind = ir.Null
		case bytecode.RLoadTrue:
			v = ir.Bool(true)
		case bytecode.RLoadFalse:
			v = ir.Bool(false)
		case bytecode.RLoadUninit:
			v.Kind = ir.Uninitialized
		}
		copyTo(int(a), ir.Literal(v))
	case bytecode.RLoadEmpty:
		// The empty string, a constant the VM keeps a cell of, as the
		// stack form's push_empty_string.
		l.emit(ir.Instruction{Op: ir.StringConst, Dest: int(a)}, false)
	case bytecode.RLoadThis:
		// The frame's this, which register code reads from register 1;
		// what this instruction reads is the frame's.
		host()
	case bytecode.RCheckRef:
		l.emit(ir.Instruction{Op: ir.Copy, Dest: int(a), Left: reg(a), Check: true, CheckSlot: int(a)}, false)
	case bytecode.RGetUpval:
		l.emit(ir.Instruction{Op: ir.UpvalueRead, Dest: int(a), Key: w.BC()}, false)
	case bytecode.RSetUpval:
		l.emit(ir.Instruction{Op: ir.UpvalueWrite, Left: reg(a), Key: w.BC()}, false)
	case bytecode.RGetGlobal:
		// No operand: native code reads the binding where the global object,
		// or a script's lexical bindings, have it (as LowerSSA's).
		l.emit(ir.Instruction{Op: ir.BindingRead, Left: ir.Literal(ir.Value{Kind: ir.Undefined}), Dest: int(a), Key: l.name(b)}, false)
	case bytecode.RSetGlobal:
		l.emit(ir.Instruction{Op: ir.BindingWrite, Left: reg(a), Key: l.name(b)}, false)
	case bytecode.RGetProp:
		l.emit(ir.Instruction{Op: ir.PropertyRead, Left: reg(b), Dest: int(a), Key: l.siteName(c)}, false)
	case bytecode.RGetMethod:
		l.emit(ir.Instruction{Op: ir.ReferenceRead, Left: reg(b), Dest: int(a), Key: l.siteName(c)}, false)
	case bytecode.RGetMethodW:
		// The method, then the receiver beside it in the call's window: the
		// read first, which may run a getter -- after it only copies, which
		// do not leave. A receiver in register A is read before A is written.
		t := l.temp()
		l.emit(ir.Instruction{Op: ir.ReferenceRead, Left: reg(b), Dest: t, Key: l.siteName(c)}, false)
		if a+1 != b {
			copyTo(int(a)+1, reg(b))
		}
		copyTo(int(a), ir.Slot(t))
	case bytecode.RSetProp:
		l.emit(ir.Instruction{Op: ir.PropertyWrite, Left: reg(a), Right: reg(b), Key: l.siteName(c)}, false)
	case bytecode.RGetIndex:
		l.emit(ir.Instruction{Op: ir.ArrayRead, Left: reg(b), Right: reg(c), Dest: int(a)}, false)
	case bytecode.RGetIndexI:
		l.emit(ir.Instruction{Op: ir.ArrayRead, Left: reg(b), Right: number(float64(c)), Dest: int(a)}, false)
	case bytecode.RSetIndex:
		l.emit(ir.Instruction{Op: ir.ArrayWrite, Left: reg(a), Right: reg(b), Third: reg(c)}, false)
	case bytecode.RSetIndexI:
		l.emit(ir.Instruction{Op: ir.ArrayWrite, Left: reg(a), Right: number(float64(b)), Third: reg(c)}, false)
	case bytecode.RGetIndexPre, bytecode.RGetIndexPost:
		// x[++i], x[i++]: the step and the read into scratch slots, either
		// of which may leave, and then both written, as the interpreter
		// writes neither until both are known. The object is register B's
		// before the step.
		step, elem := l.temp(), l.temp()
		l.emit(ir.Instruction{Op: ir.Binary, Operator: ir.Add, Left: reg(c), Right: number(float64(int8(w.X()))), Dest: step}, false)
		key := reg(c)
		if op == bytecode.RGetIndexPre {
			key = ir.Slot(step)
		}
		l.emit(ir.Instruction{Op: ir.ArrayRead, Left: reg(b), Right: key, Dest: elem}, false)
		copyTo(int(c), ir.Slot(step))
		copyTo(int(a), ir.Slot(elem))
	case bytecode.RDefineField:
		l.emit(ir.Instruction{Op: ir.FieldDefine, Left: reg(a), Right: reg(b), Key: l.name(c)}, false)
	case bytecode.RNewObject:
		l.emit(ir.Instruction{Op: ir.ObjectLiteral, Dest: int(a)}, false)
	case bytecode.RNewArray:
		if int(c) > ir.MaxArrayLiteral {
			host()
			break
		}
		block(int(a), int(b), int(c), ir.Instruction{Op: ir.ArrayLiteral})
	case bytecode.RNewArrayE:
		l.emit(ir.Instruction{Op: ir.ArrayLiteral, Dest: int(a)}, false)
	case bytecode.RIterNext:
		l.emit(ir.Instruction{Op: ir.IterStep, Left: reg(b), Dest: int(a), Target: regTarget(pc, w.SC())}, true)
	case bytecode.RAdd:
		binary(ir.Add, reg(b), reg(c), false)
	case bytecode.RSub:
		binary(ir.Sub, reg(b), reg(c), false)
	case bytecode.RMul:
		binary(ir.Mul, reg(b), reg(c), false)
	case bytecode.RBin:
		raw := uint32(w.X())
		switch raw {
		case uint32(bytecode.OpMod):
			binary(ir.Mod, reg(b), reg(c), false)
		case uint32(bytecode.OpPow):
			binary(ir.Pow, reg(b), reg(c), false)
		default:
			o, _ := operator(raw)
			binary(o, reg(b), reg(c), strictOp(raw))
		}
	case bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr:
		binary(ir.BitAnd+ir.Operator(op-bytecode.RBitAnd), reg(b), reg(c), false)
	case bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI:
		if op == bytecode.ROrI && w.SC() == 0 {
			// x | 0.
			l.emit(ir.Instruction{Op: ir.Unary, Operator: ir.Int32, Left: reg(b), Dest: int(a)}, false)
			break
		}
		binary(ir.BitAnd+ir.Operator(op-bytecode.RAndI), reg(b), number(float64(w.SC())), false)
	case bytecode.RAddI:
		binary(ir.Add, reg(b), number(float64(w.SC())), false)
	case bytecode.RSubI:
		binary(ir.Sub, reg(b), number(float64(w.SC())), false)
	case bytecode.RInc, bytecode.RDec:
		o := ir.Add
		if op == bytecode.RDec {
			o = ir.Sub
		}
		l.emit(ir.Instruction{Op: ir.Update, Operator: o, Left: reg(b), Dest: int(a), Extra: -1}, false)
	case bytecode.RPostInc, bytecode.RPostDec:
		// R[A] = R[B], then R[B] = R[A] + 1: the old value at A, the new
		// at B, which wins where they are one register.
		o := ir.Add
		if op == bytecode.RPostDec {
			o = ir.Sub
		}
		extra := int(a)
		if a == b {
			extra = -1
		}
		l.emit(ir.Instruction{Op: ir.Update, Operator: o, Left: reg(b), Dest: int(b), Extra: extra, Postfix: true}, false)
	case bytecode.RUnary:
		var o ir.Operator
		switch bytecode.Op(w.X()) {
		case bytecode.OpNeg:
			o = ir.Neg
		case bytecode.OpNot:
			o = ir.Not
		case bytecode.OpBitNot:
			o = ir.BitNot
		case bytecode.OpTypeOf:
			host()
			return
		default:
			o = ir.Pos
		}
		l.emit(ir.Instruction{Op: ir.Unary, Operator: o, Left: reg(b), Dest: int(a)}, false)
	case bytecode.RTypeof:
		// Go's, from native code where the VM says so (ir's Window).
		l.emit(ir.Instruction{Op: ir.Host, Window: true, Left: reg(b), Dest: int(a)}, false)
	case bytecode.RJmp:
		l.emit(ir.Instruction{Op: ir.Jump, Target: regTarget(pc, int32(w.BC()))}, true)
	case bytecode.RJmpF, bytecode.RJmpT:
		branch(ir.Truth, reg(a), ir.Operand{}, op == bytecode.RJmpT, false, regTarget(pc, int32(w.BC())))
	case bytecode.RJNull, bytecode.RJNotNull:
		// == null, document.all's stand-in included, which ?? does not
		// take for null (ir's Nullish): the loose equality.
		branch(ir.Eq, reg(a), ir.Literal(ir.Value{Kind: ir.Null}), op == bytecode.RJNull, false, regTarget(pc, w.SC()))
	case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
		bytecode.RJEq, bytecode.RJNe, bytecode.RJSEq, bytecode.RJSNe:
		o, when, _ := regBranch(op)
		branch(o, reg(a), reg(b), when, op == bytecode.RJSEq || op == bytecode.RJSNe, regTarget(pc, w.SC()))
	case bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI,
		bytecode.RJEqI, bytecode.RJNeI, bytecode.RJSEqI, bytecode.RJSNeI:
		o, when, _ := regBranch(op)
		branch(o, reg(a), number(float64(w.SB())), when, op == bytecode.RJSEqI || op == bytecode.RJSNeI, regTarget(pc, w.SC()))
	case bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU:
		// The bound read from its cell into a scratch slot, which commits
		// nothing, then the compare.
		t := l.temp()
		l.emit(ir.Instruction{Op: ir.UpvalueRead, Dest: t, Key: uint32(b)}, false)
		o, when, _ := regBranch(op)
		branch(o, reg(a), ir.Slot(t), when, false, regTarget(pc, w.SC()))
	case bytecode.RJTypeof:
		t := l.temp()
		l.emit(ir.Instruction{Op: ir.TypeTest, Left: reg(a), Dest: t, Key: regTypes[w.X()&^bytecode.RCmpTrue&7], When: true}, false)
		branch(ir.Truth, ir.Slot(t), ir.Operand{}, w.X()&bytecode.RCmpTrue != 0, false, regTarget(pc, w.SC()))
	case bytecode.RSwitch:
		// The cases' strict equalities in order, a branch to each body;
		// the next instruction where none holds. Nothing commits.
		for _, sc := range rc.Switches[w.BC()] {
			var v ir.Operand
			switch sc.Kind {
			case bytecode.SwitchNum:
				v = number(sc.Num)
			case bytecode.SwitchTrue:
				v = ir.Literal(ir.Bool(true))
			case bytecode.SwitchFalse:
				v = ir.Literal(ir.Bool(false))
			case bytecode.SwitchNull:
				v = ir.Literal(ir.Value{Kind: ir.Null})
			case bytecode.SwitchStr:
				t := l.temp()
				l.emit(ir.Instruction{Op: ir.StringConst, Dest: t, Key: uint32(regStringConst(fn, sc.Str))}, false)
				v = ir.Slot(t)
			default:
				panic(fmt.Sprintf("switch case of kind %d", sc.Kind))
			}
			branch(ir.Eq, reg(a), v, true, true, int(sc.To))
		}
	case bytecode.RSwitchTypeof:
		t := l.temp()
		for _, sc := range rc.Switches[w.BC()] {
			kind, ok := typeNames[sc.Str]
			if sc.Kind != bytecode.SwitchStr || !ok {
				kind = ir.TypeNone
			}
			l.emit(ir.Instruction{Op: ir.TypeTest, Left: reg(a), Dest: t, Key: uint32(kind), When: true}, false)
			branch(ir.Truth, ir.Slot(t), ir.Operand{}, true, false, int(sc.To))
		}
	case bytecode.RRet:
		l.emit(ir.Instruction{Op: ir.Return, Left: reg(a)}, false)
	case bytecode.RRetUndef:
		l.emit(ir.Instruction{Op: ir.Return, Left: ir.Literal(ir.Value{Kind: ir.Undefined})}, false)
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
		// A call, made natively where the VM knows its callee (the VM's
		// feedback, by the call's register pc), by Go otherwise.
		// Not marked Tail: the new pipeline reads a tail call's operands
		// from the operand stack's top, which register code has none of;
		// the register form's tail calls are Stage 3a-3's.
		l.emit(ir.Instruction{Op: ir.Host, Extra: int(w.X()), When: op == bytecode.RCallMW, Window: true, Left: reg(a), Dest: int(b)}, false)
	case bytecode.RClosureW:
		l.emit(ir.Instruction{Op: ir.Closure, Dest: int(a), Key: w.BC()}, false)
	case bytecode.RThrow:
		// A throw a catch clause of the function catches, the innermost
		// handler: its register given the value, and a jump there, as
		// LowerSSA makes the stack form's.
		if h := rc.HandlerAt(pc); h != nil && h.Catches {
			copyTo(int(h.Reg), reg(a))
			l.emit(ir.Instruction{Op: ir.Jump, Target: int(h.To)}, true)
			break
		}
		host()
	case bytecode.RInstanceOf:
		// Go's, from native code where the VM says so (ir's Window).
		l.emit(ir.Instruction{Op: ir.Host, Window: true, Left: reg(b), Right: reg(c), Dest: int(a), Extra: 1}, false)
	case bytecode.RIn:
		// key in obj: Go's, called from native code where that runs nothing.
		l.emit(ir.Instruction{Op: ir.HasProperty, Left: reg(b), Right: reg(c), Dest: int(a)}, false)
	case bytecode.RDelete:
		l.emit(ir.Instruction{Op: ir.DeleteProperty, Left: reg(b), Right: reg(c), Dest: int(a), Strict: fn.Strict}, false)
	case bytecode.RArguments:
		// Go's, from the frame's arguments (ir's Frame).
		l.emit(ir.Instruction{Op: ir.Host, Frame: true, Dest: int(a)}, false)
	case bytecode.RLit:
		if !l.regexpLit(pc, w) {
			host()
		}
	case bytecode.RNewArrayK:
		// An array of constants, its arrays sharing them (the VM's shared
		// literals): natively from its site's pool, its arrays whole as Go
		// makes them (ssa's LiteralSite's Shared), or, where the site has
		// given sharing up, its elements written from the constants loaded
		// to scratch slots -- as the stack form's new_array of the constants
		// it pushes.
		if !l.constArray(int(a), rc.Literals[rc.Aux[pc]]) {
			host()
		}
	case bytecode.RApplyArgs:
		// f.apply(x, arguments): R[B] f, R[B+1] apply, R[B+2] x, a call of
		// f with the frame's arguments where apply is the realm's, natively
		// or by Go (ssa.CallSite's Forward), as the stack form's
		// apply_arguments; anything else the interpreter's.
		l.emit(ir.Instruction{Op: ir.Host, Extra: 2, Window: true, Left: reg(b), Dest: int(a)}, false)
	case bytecode.RAddProp, bytecode.RAddPropI:
		// o.k += v: the read, which may run a getter, so that the addition
		// and the store after it are Generic.
		var v ir.Operand
		if op == bytecode.RAddProp {
			v = reg(b)
		} else {
			v = number(float64(w.SB()))
		}
		key := l.siteName(c)
		old, sum := l.temp(), l.temp()
		l.emit(ir.Instruction{Op: ir.PropertyRead, Left: reg(a), Dest: old, Key: key}, false)
		l.emit(ir.Instruction{Op: ir.Binary, Operator: ir.Add, Left: ir.Slot(old), Right: v, Dest: sum, Generic: true}, false)
		l.emit(ir.Instruction{Op: ir.PropertyWrite, Left: reg(a), Right: ir.Slot(sum), Key: key, Generic: true}, false)
	case bytecode.RAddIndex, bytecode.RAddIndexK, bytecode.RAddIndexI:
		// a[k] += v: the element's read and the addition into scratch
		// slots, which commit nothing, then the store.
		key, v := reg(b), reg(c)
		switch op {
		case bytecode.RAddIndexK:
			key, v = number(float64(c)), reg(b)
		case bytecode.RAddIndexI:
			v = number(float64(w.SC()))
		}
		old, sum := l.temp(), l.temp()
		l.emit(ir.Instruction{Op: ir.ArrayRead, Left: reg(a), Right: key, Dest: old}, false)
		l.emit(ir.Instruction{Op: ir.Binary, Operator: ir.Add, Left: ir.Slot(old), Right: v, Dest: sum}, false)
		l.emit(ir.Instruction{Op: ir.ArrayWrite, Left: reg(a), Right: key, Third: ir.Slot(sum)}, false)
	case bytecode.RGetLength:
		l.emit(ir.Instruction{Op: ir.ArrayLength, Left: reg(b), Dest: int(a)}, false)
	case bytecode.RArgsLen:
		l.emit(ir.Instruction{Op: ir.ArgumentRead, When: true, Left: number(0), Third: reg(b), Dest: int(a)}, false)
	case bytecode.RArgsIndex:
		l.emit(ir.Instruction{Op: ir.ArgumentRead, Left: reg(c), Third: reg(b), Dest: int(a)}, false)
	case bytecode.RConcat:
		block(int(a), int(b), int(c), ir.Instruction{Op: ir.StringConcat})
	case bytecode.RStrPart:
		// The part converted where it is, a concat of one.
		l.emit(ir.Instruction{Op: ir.StringConcat, Dest: int(a), Extra: 1}, false)
	case bytecode.RKeyOfBase:
		l.emit(ir.Instruction{Op: ir.ArrayKey, Left: reg(b), Right: reg(c)}, false)
		if a != c {
			copyTo(int(a), reg(c))
		}
	case bytecode.RNewObjectLit:
		// The object, its fields defined in order, then given to register
		// A: the definitions, on an object nothing else sees yet, commit
		// nothing (ir.Program's CommitsAt), the copy last.
		keys := rc.ObjKeys[rc.Aux[pc]]
		t := l.temp()
		l.emit(ir.Instruction{Op: ir.ObjectLiteral, Dest: t}, false)
		for i, k := range keys {
			l.emit(ir.Instruction{Op: ir.FieldDefine, Left: ir.Slot(t), Right: ir.Slot(int(b) + i), Key: k}, false)
		}
		copyTo(int(a), ir.Slot(t))
	case bytecode.RAppend:
		// A local's append: a string's from the start (the stack form's
		// AddOwned).
		l.emit(ir.Instruction{Op: ir.StringAdd, Left: reg(a), Right: reg(b), Dest: int(a), Owned: true}, false)
	case bytecode.RStrFreeze:
		l.emit(ir.Instruction{Op: ir.StringFreeze, Left: reg(a)}, false)
	case bytecode.RCharCodeAt:
		// The method's permission, then the code unit: neither commits,
		// and anything else leaves for the interpreter's read and call.
		t := l.temp()
		l.emit(ir.Instruction{Op: ir.StringMethod, Left: reg(b), Dest: t}, false)
		l.emit(ir.Instruction{Op: ir.StringCode, Left: ir.Slot(t), Right: reg(b), Third: ir.Slot(int(w.X())), Dest: int(a)}, false)
	case bytecode.RYield:
		// Go's: the interpreter suspends the frame (the VM's regWhole runs
		// no yield), and its resumption enters the code past it. A
		// synchronous generator's, in code a loop's step or a call of
		// next resumed natively, suspends it natively (ir.Instruction's
		// Suspend), its value R[B].
		l.emit(ir.Instruction{Op: ir.Host, Suspend: fn.Generator && !fn.Async, Left: reg(b)}, false)
	case bytecode.RCallWT, bytecode.RCallMWT:
		// A call in tail position: made as the ordinary call while the
		// stack is shallow, natively or by Go, as the interpreter's
		// rhCallWT makes it, native code going on after it (ir's Tail);
		// deeper, where the frame is given to the callee, the interpreter's
		// (regWhole runs none), so that native code keeps no frame a tail
		// call would have given up, and the frame ends after it.
		l.emit(ir.Instruction{Op: ir.Host, Extra: int(w.X()), When: op == bytecode.RCallMWT, Window: true, Tail: true, Left: reg(a), Dest: int(b)}, false)
	case bytecode.RGetPrivate:
		// A private member's read, R[A] = R[B].#R[C]: a property read of
		// the key the register holds (ir.Instruction's Private), as the
		// stack form's get_private, for the shapes its site met; Go's,
		// native code going on after it, wherever the key or the shape is
		// another (the VM's privateProperty).
		l.emit(ir.Instruction{Op: ir.PropertyRead, Left: reg(b), Dest: int(a), Key: l.name(uint16(l.rc.Aux[pc])), Third: reg(w.C()), Private: true}, false)
	case bytecode.RSetPrivate:
		// R[A].#R[C] = R[B], a writable field's write, as the read.
		l.emit(ir.Instruction{Op: ir.PropertyWrite, Left: reg(a), Right: reg(b), Key: l.name(uint16(l.rc.Aux[pc])), Third: reg(w.C()), Private: true}, false)
	default:
		if !op.Cold() {
			panic(fmt.Sprintf("jit: no register lowering for %v", op))
		}
		l.lowerCold(pc, w)
	}
}

// constArray lowers new_array_k's literal of the constants lit to dest:
// each loaded to a scratch slot, the literal made over them (ArrayLiteral),
// its result copied out. It reports false, lowering nothing, for a literal
// longer than an ArrayLiteral takes or of a constant other than a number,
// a string or a BigInt.
func (l *regLowering) constArray(dest int, lit []bytecode.LitElem) bool {
	consts := l.fn.Constants
	if len(lit) > ir.MaxArrayLiteral {
		return false
	}
	for _, x := range lit {
		if x.Kind != bytecode.LitConst {
			continue
		}
		if int(x.K) >= len(consts) {
			return false
		}
		switch consts[x.K].Kind {
		case bytecode.ConstNumber, bytecode.ConstString, bytecode.ConstBigInt:
		default:
			return false
		}
	}
	base := l.scratch + l.used
	for range lit {
		l.temp()
	}
	if len(lit) == 0 {
		base = l.temp()
	}
	for i, x := range lit {
		v := ir.Value{Kind: ir.Undefined}
		switch x.Kind {
		case bytecode.LitConst:
			switch c := consts[x.K]; c.Kind {
			case bytecode.ConstNumber:
				l.emit(ir.Instruction{Op: ir.Copy, Dest: base + i, Left: number(c.Num)}, false)
			case bytecode.ConstString:
				l.emit(ir.Instruction{Op: ir.StringConst, Dest: base + i, Key: x.K}, false)
			case bytecode.ConstBigInt:
				l.emit(ir.Instruction{Op: ir.BigIntConst, Dest: base + i, Key: x.K}, false)
			}
			continue
		case bytecode.LitTrue:
			v = ir.Bool(true)
		case bytecode.LitFalse:
			v = ir.Bool(false)
		case bytecode.LitNull:
			v.Kind = ir.Null
		}
		l.emit(ir.Instruction{Op: ir.Copy, Dest: base + i, Left: ir.Literal(v)}, false)
	}
	l.emit(ir.Instruction{Op: ir.ArrayLiteral, Dest: base, Extra: len(lit)}, false)
	l.emit(ir.Instruction{Op: ir.Copy, Dest: dest, Left: ir.Slot(base)}, false)
	return true
}

// lowerCold appends the IR of operation w at pc, one of no hot loop: the few the stack
// form lowers natively, as LowerSSA lowers them, and Go's the rest.
func (l *regLowering) lowerCold(pc int, w bytecode.RInstr) {
	a, b := w.A(), w.B()
	switch sub := w.Op(); sub {
	case bytecode.RNewRegExp:
		// From a pool the VM fills with its pattern's, as an object
		// literal's (LowerSSA's new_regexp).
		l.emit(ir.Instruction{Op: ir.ObjectLiteral, Dest: int(a)}, false)
	case bytecode.RIterClose:
		l.emit(ir.Instruction{Op: ir.IterClose, Left: reg(b)}, false)
	case bytecode.RGetGlobalOpt:
		l.emit(ir.Instruction{Op: ir.BindingRead, Left: ir.Literal(ir.Value{Kind: ir.Undefined}), Dest: int(a), Key: l.name(b)}, false)
	case bytecode.RIterStep, bytecode.RIterRest:
		l.emit(ir.Instruction{Op: ir.IterStep, Left: reg(b), Dest: int(a), When: true, Rest: sub == bytecode.RIterRest}, false)
	case bytecode.RCheckCoercible:
		l.emit(ir.Instruction{Op: ir.Coercible, Left: reg(a)}, false)
	case bytecode.RToString:
		l.emit(ir.Instruction{Op: ir.StringConcat, Dest: int(a), Extra: 1}, false)
	case bytecode.RCheckGlobalRef:
		l.emit(ir.Instruction{Op: ir.BindingCheck, Dest: int(a), Key: l.name(b)}, false)
	case bytecode.RGetUpvalCheck:
		l.emit(ir.Instruction{Op: ir.UpvalueRead, Dest: int(a), Key: w.BC(), Check: true}, false)
	case bytecode.RSetUpvalCheck:
		if k := w.BC(); int(k) < len(l.fn.Upvalues) && l.fn.Upvalues[k].Mutable {
			l.emit(ir.Instruction{Op: ir.UpvalueWrite, Left: reg(a), Key: k, Check: true}, false)
			break
		}
		// A constant's assignment, which throws.
		l.emit(ir.Instruction{Op: ir.Host}, false)
	case bytecode.RSuperCall:
		if l.this < 0 {
			l.emit(l.hostAt(pc), false)
			break
		}
		// A derived constructor's super(...) of the B arguments in the
		// registers from C: a call of the parent with the instance as its
		// receiver, natively or by Go (ssa.CallSite's Super), as the stack
		// form's super_call -- the this slot, then the arguments, from the
		// window's first register, Left; the this it binds R[A]'s, and
		// register 1's and the this slot's from here.
		l.emit(ir.Instruction{Op: ir.Host, Window: true, Extra: int(b), Left: reg(w.C()), Dest: int(a)}, false)
	case bytecode.RThis, bytecode.RCheckThis:
		if l.this < 0 {
			l.emit(l.hostAt(pc), false)
			break
		}
		// A derived constructor's this binding, the this slot: checked
		// bound, Go's where not, which throws (ssa's this check), as the
		// stack form's push_this.
		dest := int(a)
		if sub == bytecode.RCheckThis {
			dest = l.temp()
		}
		l.emit(ir.Instruction{Op: ir.Copy, Dest: dest, Left: ir.Slot(l.this), Check: true, CheckSlot: l.this}, false)
	case bytecode.RDerivedRet:
		if l.this < 0 || !l.undefinedAt(pc, int(a)) {
			// Anything but undefined returned -- an object, or what the
			// construction throws for -- is Go's, as the stack form's.
			l.emit(l.hostAt(pc), false)
			break
		}
		// undefined returned: the this binding, checked bound -- Go's
		// where not, which throws -- as the stack form's return.
		l.emit(ir.Instruction{Op: ir.Copy, Dest: int(a), Left: ir.Slot(l.this), Check: true, CheckSlot: l.this}, false)
	case bytecode.RRestParam, bytecode.RNewTarget:
		// Go's, from the frame (ir's Frame).
		l.emit(ir.Instruction{Op: ir.Host, Frame: true, Dest: int(a)}, false)
	case bytecode.RCloseRegs:
		// A scope's end, closing what closures captured of the B
		// registers from A, as the stack form's close_upvalues closes its
		// locals from one on: natively, or by a call of Go on the frame
		// (ssa.GoClose).
		l.emit(ir.Instruction{Op: ir.CloseUpvalues, Key: uint32(a), Extra: int(b)}, false)
	case bytecode.RWrap:
		// x op y, the second argument of the asUintN or asIntN call right
		// after it: the operator alone, the call made after it as any
		// call, which native code makes of BigInts held in their values
		// as it makes BigInt.asUintN(w, x op y) of the stack form's code
		// (ssa/bigint.go) -- the interpreter's wrap's way when the callee
		// is another, never passing the call over, so that native code
		// goes on after it.
		o, ok := regWrapOperator(l.rc, pc)
		if !ok {
			l.emit(l.hostAt(pc), false)
			break
		}
		l.emit(ir.Instruction{Op: ir.Binary, Operator: o, Left: reg(b), Right: reg(w.C()), Dest: int(a)}, false)
	default:
		l.emit(l.hostAt(pc), false)
	}
}

// regWrapOperator is the operator of rc's wrap at pc (its Aux's
// operation's), or false for one it does not know.
func regWrapOperator(rc *bytecode.RegCode, pc int) (ir.Operator, bool) {
	if pc >= len(rc.Aux) {
		return 0, false
	}
	switch op := bytecode.ROp(rc.Aux[pc] & 127); op {
	case bytecode.RAdd:
		return ir.Add, true
	case bytecode.RSub:
		return ir.Sub, true
	case bytecode.RMul:
		return ir.Mul, true
	case bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr:
		return ir.BitAnd + ir.Operator(op-bytecode.RBitAnd), true
	}
	return 0, false
}

// hostAt is the Host of the register instruction at pc, which Go runs
// whole: called from native code where Go may run it on the frame and the
// registers it reads and writes are known (RegAccess) -- up to three read,
// one written -- the code going on after it (ir.Instruction's Whole); left
// to an exit otherwise. A return, a suspension, a call in tail position
// and a throw, which end the frame's flow or are the interpreter's alone,
// leave.
func (l *regLowering) hostAt(pc int) ir.Instruction {
	switch l.rc.Code[pc].Op() {
	case bytecode.RRet, bytecode.RRetUndef, bytecode.RYield, bytecode.RCallWT, bytecode.RCallMWT, bytecode.RAwait,
		bytecode.RYieldStar, bytecode.REndParams, bytecode.RGenEntry, bytecode.RIterResume, bytecode.RThrow,
		bytecode.RCheckThis, bytecode.RDeleteSuper, bytecode.RSuperCall, bytecode.RSuperForward, bytecode.RSuperSpread, bytecode.RDerivedRet:
		// Or reads the frame's this binding, which no operand names.
		return ir.Instruction{Op: ir.Host}
	}
	reads, writes, all := RegAccess(l.rc, pc)
	if all || len(reads) > 3 || len(writes) > 1 {
		return ir.Instruction{Op: ir.Host}
	}
	in := ir.Instruction{Op: ir.Host, Whole: true, Extra: len(reads), Dest: -1}
	ops := []*ir.Operand{&in.Left, &in.Right, &in.Third}
	for i, r := range reads {
		*ops[i] = ir.Slot(r)
	}
	if len(writes) == 1 {
		in.Dest = writes[0]
	}
	return in
}

// liveRegs fills p's Live (ir.Program's): for each instruction, the
// registers the code from there may read before it writes them, a backward
// dataflow over the program, each Host's registers by its register
// instruction (RegAccess). A throw goes to the handler in force, whatever
// the instruction writes; a register a closure captures is live
// throughout, which the closure may read at any call.
//
// The dataflow runs over the program's blocks, each block's set at its
// start found again from its successors' until none changes, and then each
// instruction's from its block's end: the sets are ir.Liveness's, each the
// one after it with what the instruction writes taken out and what it
// reads put in, which shares the rest. A dense row of every register at
// every instruction had made the liveness of n registers and m
// instructions n·m bits, and each pass over it as long, whatever the
// instructions changed.
func liveRegs(p *ir.Program, rc *bytecode.RegCode) {
	n, m := p.Regs, len(p.Code)
	lb := ir.NewLiveBuilder(n)
	captured := func(r int) bool { return r >= 0 && r < len(p.Captured) && p.Captured[r] }
	var ops []ir.LiveOp
	for r := range min(n, len(p.Captured)) {
		if p.Captured[r] {
			ops = append(ops, ir.LiveOp{Slot: int32(r)})
		}
	}
	always := lb.Apply(lb.Empty(), ops)
	reach := func(i int) bool { return i >= 0 && i < m && p.Maps[i].Depth >= 0 }

	// Each reachable instruction's changes, in order: the registers it
	// writes taken out -- but a captured one, which is live throughout --
	// then those it reads put in; or all of them, where it may read any.
	opAt := make([]int32, m+1)
	all := make([]bool, m)
	ops = ops[:0]
	kill := func(r int) {
		if r >= 0 && r < n && !captured(r) {
			ops = append(ops, ir.LiveOp{Slot: int32(r), Clear: true})
		}
	}
	gen := func(r int) {
		if r >= 0 && r < n {
			ops = append(ops, ir.LiveOp{Slot: int32(r)})
		}
	}
	// The instructions' successors but the next one -- a jump's, and the
	// handler in force -- and whether each goes on to the next.
	falls := make([]bool, m)
	far := make([]int32, 2*m)
	for i := range m {
		opAt[i] = int32(len(ops))
		far[2*i], far[2*i+1] = -1, -1
		if !reach(i) {
			continue
		}
		ins := &p.Code[i]
		switch ins.Op {
		case ir.Jump:
			far[2*i] = int32(ins.Target)
		case ir.Return:
		case ir.Branch, ir.IterStep:
			falls[i] = true
			if ins.Op == ir.Branch || !ins.When {
				far[2*i] = int32(ins.Target)
			}
		default:
			falls[i] = true
			if ins.Op == ir.Host {
				if w := rc.Code[p.Origin[i]]; regGoBranches(w) {
					far[2*i] = int32(p.At(p.Origin[i] + 2))
				}
			}
		}
		if p.HandlerAt != nil && p.HandlerAt[i] != 0 {
			far[2*i+1] = int32(p.Handlers[p.HandlerAt[i]-1].PC)
		}
		if ins.Op == ir.Host {
			reads, writes, a := RegAccess(rc, int(p.Origin[i]))
			for _, r := range writes {
				kill(r)
			}
			if w := rc.Code[p.Origin[i]]; w.Op() == bytecode.RCallW || w.Op() == bytecode.RCallWT {
				// The receiver the plain call writes (RegAccess).
				kill(int(w.A()) + 1)
			}
			all[i] = a
			for _, r := range reads {
				gen(r)
			}
		} else {
			a, b := ins.Writes()
			if ins.Op == ir.IterStep && !ins.When {
				// A loop's step writes its value only where it goes on:
				// at the loop's end the register is what it was.
				a = -1
			}
			kill(a)
			kill(b)
			for _, r := range irReads(ins) {
				gen(r)
			}
			if ins.Check {
				if w := rc.Code[p.Origin[i]]; w.Op() == bytecode.RDerivedRet {
					// derived_ret's copy of the this binding, its operand
					// known undefined (lowerReg's undefinedAt), leaves where
					// the slot is unbound for Go to run the instruction,
					// which reads the operand: live there, its undefined in
					// the exit's state. Dead to the copy, the exit had left
					// the window's stale value, which Go's derived_ret
					// returned -- a derived constructor whose this an
					// arrow's super() bound, which native code's this slot
					// does not see.
					gen(int(w.A()))
				}
			}
		}
	}
	opAt[m] = int32(len(ops))

	// The blocks: an instruction that any but the one before goes to starts
	// one, and one that goes anywhere but to the next ends its own, so that
	// within a block each instruction goes on to the next, but for the
	// handler in force.
	starts := make([]bool, m+1)
	starts[0], starts[m] = true, true
	mark := func(j int32) {
		if j >= 0 && int(j) < m {
			starts[j] = true
		}
	}
	for i := range m {
		mark(far[2*i])
		mark(far[2*i+1])
		if !falls[i] || far[2*i] >= 0 {
			starts[i+1] = true
		}
	}
	var blockStart []int32
	blockOf := make([]int32, m)
	for i := range m {
		if starts[i] {
			blockStart = append(blockStart, int32(i))
		}
		blockOf[i] = int32(len(blockStart) - 1)
	}
	nb := len(blockStart)
	blockStart = append(blockStart, int32(m))
	// Each block's set at its start, from, at a reachable start, every
	// register a closure captures, which the solution has there.
	in := make([]ir.LiveSet, nb)
	for k := range nb {
		if reach(int(blockStart[k])) {
			in[k] = always
		}
	}
	at := func(j int32) ir.LiveSet {
		if !reach(int(j)) {
			return lb.Empty()
		}
		return in[blockOf[j]]
	}
	// The blocks whose sets read a block's: those that go to it, and
	// those with an instruction it is the handler of.
	predAt := make([]int32, nb+1)
	edges := func(add func(int, int32)) {
		for k := range nb {
			last := int(blockStart[k+1]) - 1
			if falls[last] && last+1 < m {
				add(k, blockOf[last+1])
			}
			if j := far[2*last]; j >= 0 && int(j) < m {
				add(k, blockOf[j])
			}
			for i := blockStart[k]; i < blockStart[k+1]; i++ {
				if j := far[2*i+1]; j >= 0 && int(j) < m {
					add(k, blockOf[j])
				}
			}
		}
	}
	edges(func(_ int, to int32) { predAt[to+1]++ })
	for k := range nb {
		predAt[k+1] += predAt[k]
	}
	preds := make([]int32, predAt[nb])
	fill := slices.Clone(predAt[:nb])
	edges(func(from int, to int32) { preds[fill[to]] = int32(from); fill[to]++ })

	// transfer is block k's set at its start, made from its end, each of
	// its instructions' given to set.
	// Its sets but the last are its own, which the next one's operations
	// may change in place, where no instruction's set is kept (set nil),
	// and else within a register instruction's lowering, whose
	// instructions' sets are all its first's (below).
	transfer := func(k int, set func(int, ir.LiveSet)) ir.LiveSet {
		s, e := int(blockStart[k]), int(blockStart[k+1])
		cur := lb.Empty()
		lb.Edit()
		for i := e - 1; i >= s; i-- {
			if set != nil && i < e-1 && p.Origin[i] != p.Origin[i+1] {
				lb.Edit()
			}
			if !reach(i) {
				cur = lb.Empty()
				if set != nil {
					set(i, cur)
				}
				continue
			}
			switch {
			case i == e-1:
				cur = always
				if falls[i] {
					cur = lb.Union(at(int32(i+1)), cur)
				}
				cur = lb.Union(cur, at(far[2*i]))
			case !reach(i + 1):
				// What a reachable instruction's set has, after one that
				// is not.
				cur = always
			}
			if all[i] {
				cur = lb.Full()
			} else {
				cur = lb.Apply(cur, ops[opAt[i]:opAt[i+1]])
			}
			cur = lb.Union(cur, at(far[2*i+1]))
			if set != nil {
				set(i, cur)
			}
		}
		return cur
	}
	dirty := make([]bool, nb)
	for k := range dirty {
		dirty[k] = true
	}
	for changed := true; changed; {
		changed = false
		for k := nb - 1; k >= 0; k-- {
			if !dirty[k] {
				continue
			}
			dirty[k] = false
			if s := transfer(k, nil); !lb.Equal(s, in[k]) {
				in[k] = s
				changed = true
				for _, q := range preds[predAt[k]:predAt[k+1]] {
					dirty[q] = true
				}
			}
		}
	}
	roots := make([]ir.LiveSet, m)
	for k := range nb {
		transfer(k, func(i int, s ir.LiveSet) { roots[i] = s })
	}
	// An exit anywhere in a register instruction's lowering runs it again
	// from its start (ir.Program's Origin): what is live at each of its
	// instructions is what is live at its first, which the interpreter
	// reads -- new_array's elements, copied to scratch slots, are read
	// there, not where the literal is made.
	for i := 1; i < m; i++ {
		if p.Origin[i] == p.Origin[i-1] {
			roots[i] = roots[i-1]
		}
	}
	p.Live = lb.Finish(roots)
}

// irReads is the slots in reads.
func irReads(in *ir.Instruction) []int {
	var out []int
	add := func(o ir.Operand) {
		if o.Slot >= 0 {
			out = append(out, o.Slot)
		}
	}
	if in.Check {
		out = append(out, in.CheckSlot)
	}
	switch in.Op {
	case ir.Copy, ir.Unary, ir.Update, ir.ArrayLength, ir.Return, ir.Rethrow, ir.IterStep, ir.IterClose, ir.Coercible,
		ir.StringFreeze, ir.StringMethod, ir.TypeTest, ir.BindingWrite, ir.StringPart, ir.UpvalueWrite,
		ir.PropertyRead, ir.ReferenceRead, ir.BindingRead:
		add(in.Left)
	case ir.CopyPair, ir.StoreLoad, ir.Binary, ir.StringAdd, ir.ArrayRead, ir.ArrayKey, ir.ArrayUpdate, ir.Resolved,
		ir.HasProperty, ir.DeleteProperty, ir.FieldDefine, ir.PropertyWrite:
		add(in.Left)
		add(in.Right)
	case ir.Swap:
		out = append(out, in.Dest, in.Extra)
	case ir.ArrayWrite, ir.StringCode:
		add(in.Left)
		add(in.Right)
		add(in.Third)
	case ir.ArgumentRead:
		add(in.Left)
		add(in.Third)
	case ir.Branch:
		add(in.Left)
		if in.Operator != ir.Truth && in.Operator != ir.Nullish {
			add(in.Right)
		}
	case ir.ArrayLiteral, ir.StringConcat:
		for k := range in.Extra {
			out = append(out, in.Dest+k)
		}
	}
	if in.Private {
		add(in.Third)
	}
	return out
}

// RegAccess is the registers the register instruction at pc, which Go
// runs (a Host), reads and writes, in the order a whole Host's operands
// name them (ir.Instruction's Whole); all where it may read any.
func RegAccess(rc *bytecode.RegCode, pc int) (reads, writes []int, all bool) {
	w := rc.Code[pc]
	a, b, c := int(w.A()), int(w.B()), int(w.C())
	span := func(from, n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = from + i
		}
		return out
	}
	switch w.Op() {
	case bytecode.RCallW, bytecode.RCallWT:
		// A plain call's receiver register, R[A+1], the call writes --
		// undefined, the global object, an arrow's this (the VM's
		// regCallThis) -- and reads nowhere: a read had kept the register
		// live from the function's start, loaded at its entries and
		// carried round its loops (liveRegs, which kills it).
		return append([]int{a}, span(a+2, int(w.X()))...), []int{b}, false
	case bytecode.RCallMW, bytecode.RNewW, bytecode.RCallMWT:
		return span(a, 2+int(w.X())), []int{b}, false
	case bytecode.RGetPrivate:
		return []int{b, c}, []int{a}, false
	case bytecode.RSetPrivate:
		return []int{a, b, c}, nil, false
	case bytecode.RApplyArgs:
		return append(span(b, 3), c), []int{a}, false
	case bytecode.RInstanceOf:
		return []int{b, c}, []int{a}, false
	case bytecode.RPrivateIn:
		// #x in o, of the object and the key's register.
		return []int{b, c}, []int{a}, false
	case bytecode.RSuperGet, bytecode.RSuperBase:
		// super's base is its home object's prototype, its receiver this,
		// register 1 -- where a derived constructor's binding is not, the
		// frame's.
		return []int{1}, []int{a}, false
	case bytecode.RSuperSet:
		return []int{a, 1}, nil, false
	case bytecode.RSuperGetIndex:
		return []int{b, c, 1}, []int{a}, false
	case bytecode.RDerivedRet:
		// What a derived constructor returns: its operand, or the frame's
		// this binding.
		return []int{a}, []int{a}, false
	case bytecode.RTypeof:
		return []int{b}, []int{a}, false
	case bytecode.RUnary:
		return []int{b}, []int{a}, false
	case bytecode.RThrow:
		return []int{a}, nil, false
	case bytecode.RArguments, bytecode.RNewArrayK, bytecode.RLoadThis:
		return nil, []int{a}, false
	case bytecode.RNewArray:
		return span(b, c), []int{a}, false
	case bytecode.RYield:
		return []int{b}, []int{a}, false
	case bytecode.RLit:
		if w.X()&bytecode.RLitCall != 0 {
			return []int{a + 2}, []int{a, a + 1, b}, false
		}
		return nil, []int{a, a + 1}, false
	case bytecode.RLoadConst:
		return nil, []int{a}, false
	case bytecode.RForOfStart, bytecode.RForInStart, bytecode.RIterRest:
		return []int{b}, []int{a}, false
	case bytecode.RObjectRest:
		return append([]int{b}, span(c, int(rc.Aux[pc]))...), []int{a}, false
	case bytecode.RCopyDataProps, bytecode.RSetProtoOf, bytecode.RArrayPush, bytecode.RArraySpread, bytecode.RNewClass,
		bytecode.RSetHomeObject, bytecode.RDefineMethod, bytecode.RSetFieldInit:
		return []int{a, b}, nil, false
	case bytecode.RDefineIndex:
		return []int{a, b, c}, nil, false
	case bytecode.RCallSpread:
		return span(a, 3), []int{b}, false
	case bytecode.RIterCloseThrow:
		return []int{b}, nil, false
	case bytecode.RRestParam, bytecode.RNewTarget, bytecode.RTemplateObject, bytecode.RThis:
		return nil, []int{a}, false
	case bytecode.RSuperCall:
		// Register 1 too, in a derived constructor, which it is not
		// counted as writing: what is live there stays so.
		return span(c, b), []int{a}, false
	case bytecode.RToPropertyKey:
		return []int{a}, []int{a}, false
	case bytecode.RAssignConst, bytecode.RThrowRef, bytecode.RCheckThis, bytecode.RDeleteSuper, bytecode.RCloseRegs:
		// close_regs reads and writes no register's value: what it
		// changes is what the closures' cells hold.
		return nil, nil, false
	case bytecode.RSetUpvalCheck:
		return []int{a}, nil, false
	case bytecode.RNewDispose:
		// A using scope's stack of resources, made; a resource added to
		// it; its resources disposed of, a throw leaving the scope among
		// what it reads where C is 1.
		return nil, []int{a}, false
	case bytecode.RAddDisposable:
		return []int{a, b}, nil, false
	case bytecode.RDispose:
		if c == 1 {
			return []int{a, b}, nil, false
		}
		return []int{a}, nil, false
	}
	return nil, nil, true
}
