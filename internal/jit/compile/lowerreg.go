package compile

import (
	"fmt"
	"maps"
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
	// lits are the regexp_lit calls lowered as the stack form's
	// (regexpLit), by register pc; litHosts those lowered as Go's whole,
	// their window's registers live past them.
	lits     []int
	litHosts map[int]bool
}

// emit appends in, of the current register instruction's origin; a
// Target it has is a register pc where jump is set.
func (l *regLowering) emit(in ir.Instruction, jump bool) {
	l.code = append(l.code, in)
	l.origin = append(l.origin, uint32(l.pc))
	l.patch = append(l.patch, jump)
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
	l := &regLowering{rc: rc, fn: fn, m: m, scratch: rc.NumRegs + len(fn.Upvalues), litHosts: litHosts}
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
		case bytecode.RCallWT, bytecode.RCallMWT:
			// The interpreter's call in tail position: a call all the same.
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
		// Go's: the interpreter suspends the frame (the VM's regWhole runs
		// no yield), and its resumption enters the code past it.
		l.emit(ir.Instruction{Op: ir.Host}, false)
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
	case bytecode.RRestParam, bytecode.RNewTarget:
		// Go's, from the frame (ir's Frame).
		l.emit(ir.Instruction{Op: ir.Host, Frame: true, Dest: int(a)}, false)
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
		bytecode.RCheckThis, bytecode.RDeleteSuper:
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
				reads, writes, all := RegAccess(rc, int(p.Origin[i]))
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
				if ins.Op == ir.IterStep && !ins.When {
					// A loop's step writes its value only where it goes on:
					// at the loop's end the register is what it was.
					a = -1
				}
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
	// An exit anywhere in a register instruction's lowering runs it again
	// from its start (ir.Program's Origin): what is live at each of its
	// instructions is what is live at its first, which the interpreter
	// reads -- new_array's elements, copied to scratch slots, are read
	// there, not where the literal is made.
	for i := 1; i < len(p.Code); i++ {
		if p.Origin[i] == p.Origin[i-1] {
			copy(at(i), at(i-1))
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
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW, bytecode.RCallWT, bytecode.RCallMWT:
		return span(a, 2+int(w.X())), []int{b}, false
	case bytecode.RGetPrivate:
		return []int{b, c}, []int{a}, false
	case bytecode.RSetPrivate:
		return []int{a, b, c}, nil, false
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
	case bytecode.RToPropertyKey:
		return []int{a}, []int{a}, false
	case bytecode.RAssignConst, bytecode.RThrowRef, bytecode.RCheckThis, bytecode.RDeleteSuper, bytecode.RCloseRegs:
		return nil, nil, false
	case bytecode.RSetUpvalCheck:
		return []int{a}, nil, false
	}
	return nil, nil, true
}
