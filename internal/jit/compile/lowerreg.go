package compile

import (
	"fmt"
	"slices"

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
}

// emit appends in, of the current register instruction's origin; a
// Target it has is a register pc where jump is set.
func (l *regLowering) emit(in ir.Instruction, jump bool) {
	l.code = append(l.code, in)
	l.origin = append(l.origin, uint32(l.pc))
	l.patch = append(l.patch, jump)
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
	if w.Op() != bytecode.RCold {
		return false
	}
	switch bytecode.RColdOp(w.X()) {
	case bytecode.RColdIterResume, bytecode.RColdIterUnpackDelegate, bytecode.RColdIterResult, bytecode.RColdAsyncIterClose:
		return true
	}
	return false
}

// regDefinition reports whether cold operation op makes a class's or an
// object literal's member, or a private one's, which a callee's code
// leaves to Go (as lowerFunction's isDefinition and isPrivate): of those,
// what reads only its operands and the frame (frameDefinition), and the
// operations Go makes from native code (GoOperation), a callee keeps.
func regDefinition(op bytecode.RColdOp) bool {
	switch op {
	case bytecode.RColdDefineGetterIndex, bytecode.RColdDefineSetterIndex, bytecode.RColdClassGetterIndex,
		bytecode.RColdClassSetterIndex,
		bytecode.RColdAddPrivateMethod, bytecode.RColdAddPrivateGetter, bytecode.RColdAddPrivateSetter,
		bytecode.RColdDefinePrivate, bytecode.RColdDefinePrivateMethod, bytecode.RColdDefinePrivateGetter,
		bytecode.RColdDefinePrivateSetter:
		// A key in a register, a computed name's or a private name's: the
		// frame Go makes from native code does not show the registers.
		return true
	}
	return false
}

// regRefusal is the reason the instruction at pc is not lowered, or "".
func regRefusal(rc *bytecode.RegCode, fn *bytecode.Function, m lowering, pc int) string {
	w := rc.Code[pc]
	switch op := w.Op(); op {
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
	case bytecode.RCold:
		sub := bytecode.RColdOp(w.X())
		switch sub {
		case bytecode.RColdDirectEval:
			return "direct eval"
		case bytecode.RColdWithGet, bytecode.RColdWithGetThis, bytecode.RColdWithDelete, bytecode.RColdWithSet,
			bytecode.RColdWithResolve, bytecode.RColdWithRead, bytecode.RColdWithWrite, bytecode.RColdWithPush, bytecode.RColdWithPop,
			bytecode.RColdDebug:
			// The stack form's with and debug_stmt, which lowerFunction
			// refuses (describe).
			return "unsupported opcode " + sub.String()
		case bytecode.RColdMovFar, bytecode.RColdMovToFar, bytecode.RColdFarInit:
			// Registers past 16 bits, past what native code's records name.
			return "slot budget"
		}
		if m.callee && regDefinition(sub) {
			return "unsupported opcode " + sub.String()
		}
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
	l := &regLowering{rc: rc, fn: fn, m: m, scratch: rc.NumRegs + len(fn.Upvalues)}
	start := make([]int, n+1)
	for pc := range n {
		start[pc] = len(l.code)
		l.pc, l.used = pc, 0
		if !reach[pc] {
			l.emit(ir.Instruction{Op: ir.Nop}, false)
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
	p := &ir.Program{Code: l.code, Origin: l.origin, Maps: make([]ir.StateMap, len(l.code)),
		Regs: rc.NumRegs, Locals: l.scratch + l.most, ParamBase: 2, Params: rc.Params,
		UpvalueBase: rc.NumRegs, Upvalues: len(fn.Upvalues),
		ArrowThis:        fn.Kind == bytecode.KindArrow && fn.UsesThis,
		ClassConstructor: fn.Kind == bytecode.KindConstructor,
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
		case w.Op() == bytecode.RCold && bytecode.RColdOp(w.X()) == bytecode.RColdEndParams && (fn.Async || fn.Generator):
			// The first resumption enters past the parameters.
			to = append(to, pc+1)
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
	selectNumericProperties(p, true)
	stringConcats(p, nil)
	markBools(p)
	p.FrameArgs = fn.HasRest || fn.UsesArguments || fn.MappedArguments || fn.SuperForwards
	p.FrameCallee = fn.FrameCallee
	if fn.MappedArguments && fn.ParamCount > 0 && (fn.AssignsParams || paramCaptured(fn)) {
		p.ArgumentParams = min(fn.ParamCount, rc.Params)
	}
	if forwardsNatively(fn) && slices.ContainsFunc(rc.Code, func(w bytecode.RInstr) bool { return w.Op() == bytecode.RApplyArgs }) {
		p.Forwards = true
	}
	regCaptured(p, rc, fn)
	liveRegs(p, rc)
	return p, nil
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
	for pc, w := range rc.Code {
		switch w.Op() {
		case bytecode.RGetIndex, bytecode.RGetIndexI, bytecode.RSetIndex, bytecode.RSetIndexI, bytecode.RGetIndexPre, bytecode.RGetIndexPost,
			bytecode.RAddIndex, bytecode.RAddIndexK, bytecode.RAddIndexI:
			indexed = true
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
			calls = true
			// A call in tail position may be the function's own, which
			// native code makes a jump to its start: a loop.
			loop = loop || pc < len(rc.Marks) && rc.Marks[pc]&bytecode.RMarkTail != 0
		}
	}
	return host && !loop && !indexed && !m.callee && !(m.hot && calls) && !GeneratorResumable(fn)
}

// lower appends the IR of the register instruction at pc.
func (l *regLowering) lower(pc int) {
	rc, fn := l.rc, l.fn
	w := rc.Code[pc]
	a, b, c := w.A(), w.B(), w.C()
	host := func() { l.emit(ir.Instruction{Op: ir.Host}, false) }
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
		host()
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
		l.emit(ir.Instruction{Op: ir.Host, Extra: int(w.X()), When: op == bytecode.RCallMW}, false)
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
		host()
	case bytecode.RIn:
		// key in obj: Go's, called from native code where that runs nothing.
		l.emit(ir.Instruction{Op: ir.HasProperty, Left: reg(b), Right: reg(c), Dest: int(a)}, false)
	case bytecode.RDelete:
		l.emit(ir.Instruction{Op: ir.DeleteProperty, Left: reg(b), Right: reg(c), Dest: int(a), Strict: fn.Strict}, false)
	case bytecode.RArguments:
		// Go's, from the frame's arguments (ir's Frame).
		l.emit(ir.Instruction{Op: ir.Host, Frame: true, Dest: int(a)}, false)
	case bytecode.RNewArrayK, bytecode.RApplyArgs, bytecode.RLit:
		host()
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
		// As the stack form's yield: a synchronous generator's, which code
		// a loop's step resumed natively makes itself (Suspend).
		l.emit(ir.Instruction{Op: ir.Host, Suspend: fn.Generator && !fn.Async}, false)
	case bytecode.RCold:
		l.lowerCold(pc, w)
	default:
		panic(fmt.Sprintf("jit: no register lowering for %v", op))
	}
}

// lowerCold appends the IR of cold operation w at pc: the few the stack
// form lowers natively, as LowerSSA lowers them, and Go's the rest.
func (l *regLowering) lowerCold(pc int, w bytecode.RInstr) {
	a, b := w.A(), w.B()
	switch sub := bytecode.RColdOp(w.X()); sub {
	case bytecode.RColdNewRegExp:
		// From a pool the VM fills with its pattern's, as an object
		// literal's (LowerSSA's new_regexp).
		l.emit(ir.Instruction{Op: ir.ObjectLiteral, Dest: int(a)}, false)
	case bytecode.RColdIterClose:
		l.emit(ir.Instruction{Op: ir.IterClose, Left: reg(b)}, false)
	case bytecode.RColdGetGlobalOpt:
		l.emit(ir.Instruction{Op: ir.BindingRead, Left: ir.Literal(ir.Value{Kind: ir.Undefined}), Dest: int(a), Key: l.name(b)}, false)
	case bytecode.RColdIterStep, bytecode.RColdIterRest:
		l.emit(ir.Instruction{Op: ir.IterStep, Left: reg(b), Dest: int(a), When: true, Rest: sub == bytecode.RColdIterRest}, false)
	case bytecode.RColdCheckCoercible:
		l.emit(ir.Instruction{Op: ir.Coercible, Left: reg(a)}, false)
	case bytecode.RColdToString:
		l.emit(ir.Instruction{Op: ir.StringConcat, Dest: int(a), Extra: 1}, false)
	case bytecode.RColdCheckGlobalRef:
		l.emit(ir.Instruction{Op: ir.BindingCheck, Dest: int(a), Key: l.name(b)}, false)
	case bytecode.RColdGetUpvalCheck:
		l.emit(ir.Instruction{Op: ir.UpvalueRead, Dest: int(a), Key: w.BC(), Check: true}, false)
	case bytecode.RColdSetUpvalCheck:
		if k := w.BC(); int(k) < len(l.fn.Upvalues) && l.fn.Upvalues[k].Mutable {
			l.emit(ir.Instruction{Op: ir.UpvalueWrite, Left: reg(a), Key: k, Check: true}, false)
			break
		}
		// A constant's assignment, which throws.
		l.emit(ir.Instruction{Op: ir.Host}, false)
	case bytecode.RColdRestParam, bytecode.RColdNewTarget:
		// Go's, from the frame (ir's Frame).
		l.emit(ir.Instruction{Op: ir.Host, Frame: true, Dest: int(a)}, false)
	default:
		l.emit(ir.Instruction{Op: ir.Host}, false)
	}
}

// liveRegs fills p's Live (ir.Program's): for each instruction, the
// registers the code from there may read before it writes them, a backward
// dataflow over the program, each Host's registers by its register
// instruction (regAccess). A throw goes to the handler in force, whatever
// the instruction writes; a register a closure captures is live
// throughout, which the closure may read at any call.
func liveRegs(p *ir.Program, rc *bytecode.RegCode) {
	n := p.Regs
	words := (n + 63) / 64
	live := make([]uint64, len(p.Code)*words)
	at := func(i int) []uint64 { return live[i*words : (i+1)*words] }
	set := func(s []uint64, r int) {
		if r >= 0 && r < n {
			s[r/64] |= 1 << (r % 64)
		}
	}
	kill := func(s []uint64, r int) {
		if r >= 0 && r < n {
			s[r/64] &^= 1 << (r % 64)
		}
	}
	always := make([]uint64, words)
	for r, c := range p.Captured {
		if c {
			set(always, r)
		}
	}
	in := make([]uint64, words)
	for changed := true; changed; {
		changed = false
		for i := len(p.Code) - 1; i >= 0; i-- {
			if p.Maps[i].Depth < 0 {
				continue
			}
			ins := &p.Code[i]
			clear(in)
			succ := func(j int) {
				if j >= 0 && j < len(p.Code) {
					for k, x := range at(j) {
						in[k] |= x
					}
				}
			}
			switch ins.Op {
			case ir.Jump:
				succ(ins.Target)
			case ir.Return:
			case ir.Branch, ir.IterStep:
				succ(i + 1)
				if ins.Op == ir.Branch || !ins.When {
					succ(ins.Target)
				}
			default:
				succ(i + 1)
				if ins.Op == ir.Host {
					if w := rc.Code[p.Origin[i]]; regGoBranches(w) {
						succ(p.At(p.Origin[i] + 2))
					}
				}
			}
			if ins.Op == ir.Host {
				reads, writes, all := regAccess(rc, int(p.Origin[i]))
				for _, r := range writes {
					kill(in, r)
				}
				if all {
					for k := range in {
						in[k] = ^uint64(0)
					}
				}
				for _, r := range reads {
					set(in, r)
				}
			} else {
				a, b := ins.Writes()
				kill(in, a)
				kill(in, b)
				for _, r := range irReads(ins) {
					set(in, r)
				}
			}
			if p.HandlerAt != nil && p.HandlerAt[i] != 0 {
				succ(int(p.Handlers[p.HandlerAt[i]-1].PC))
			}
			for k, x := range always {
				in[k] |= x
			}
			if n%64 != 0 {
				in[words-1] &= 1<<(n%64) - 1
			}
			if s := at(i); !slices.Equal(s, in) {
				copy(s, in)
				changed = true
			}
		}
	}
	p.Live, p.LiveLocals, p.LiveWords = live, n, words
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

// regAccess is the registers the register instruction at pc, which Go
// runs (a Host), reads and writes; all where it may read any.
func regAccess(rc *bytecode.RegCode, pc int) (reads, writes []int, all bool) {
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
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
		return span(a, 2+int(w.X())), []int{b}, false
	case bytecode.RApplyArgs:
		return append(span(b, 3), c), []int{a}, false
	case bytecode.RInstanceOf:
		return []int{b, c}, []int{a}, false
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
	case bytecode.RCold:
		switch bytecode.RColdOp(w.X()) {
		case bytecode.RColdForOfStart, bytecode.RColdForInStart:
			return []int{b}, []int{a}, false
		case bytecode.RColdIterCloseThrow:
			return []int{b}, nil, false
		case bytecode.RColdRestParam, bytecode.RColdNewTarget, bytecode.RColdTemplateObject, bytecode.RColdThis:
			return nil, []int{a}, false
		case bytecode.RColdToPropertyKey:
			return []int{a}, []int{a}, false
		case bytecode.RColdAssignConst, bytecode.RColdThrowRef, bytecode.RColdCheckThis, bytecode.RColdDeleteSuper:
			return nil, nil, false
		case bytecode.RColdSetUpvalCheck:
			return []int{a}, nil, false
		}
	}
	return nil, nil, true
}
