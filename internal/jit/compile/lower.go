// Package compile lowers VM bytecode into the JIT's slot IR. It is separate
// from executable-memory support and does not modify shared function templates.
package compile

import (
	"fmt"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Compiler work and scratch size are bounded even for permanently refused
// functions. Runtime code/metadata ownership is handled by the optional VM tier.
const (
	// MaxInstructions bounds control-flow analysis and IR allocation.
	MaxInstructions = ir.MaxInstructions
	// MaxSlots bounds the locals and operand scratch storage together.
	MaxSlots = ir.MaxSlots
)

// Refusal identifies the first unsupported construct or invalid state map.
// PC is -1 for a function-wide refusal.
type Refusal struct {
	PC     int
	Reason string
}

func (e *Refusal) Error() string {
	if e.PC < 0 {
		return "jit: " + e.Reason
	}
	return fmt.Sprintf("jit: pc %d: %s", e.PC, e.Reason)
}

func refuse(pc int, why string) error { return &Refusal{PC: pc, Reason: why} }

// hostOnly is the reason LowerSSA refuses a function that does nothing
// native code saves time on (HostOnly).
const hostOnly = "host operations without native loop or array work"

// HostOnly reports whether err is LowerSSA's refusal of a function for
// having no loop and no array work, which LowerSSAHot may lower.
func HostOnly(err error) bool {
	r, ok := err.(*Refusal)
	return ok && r.Reason == hostOnly
}

// Lower checks eligibility, validates control-flow stack depths, and produces
// one atomic IR instruction and pre-instruction map per bytecode instruction:
// the slot IR without the new pipeline's additions (LowerSSA), which the SSA
// package's tests run both ways, compiled and through the slot IR's own
// evaluator (ir.Evaluate). No executable memory is allocated. Unsupported
// code, even when unreachable, is refused.
func Lower(fn *bytecode.Function) (*ir.Program, error) {
	return lowerRecovered(fn, lowering{})
}

// LowerSSA lowers for the new pipeline (internal/jit/ssa), whose costs are
// not the slot IR emitters': it reads properties and the receiver in place
// where the VM's caches know the shape, so it keeps every property
// operation and the receiver, which Lower keeps only in loops worth a view's
// preparation; and it reads globals where the VM found them, with no
// binding-view slots.
func LowerSSA(fn *bytecode.Function) (*ir.Program, error) {
	return lowerRecovered(fn, lowering{ssa: true})
}

// Hints are what the VM has seen its function's code do that its bytecode
// does not say, which a lowering for the new pipeline takes (Lower*With).
// StringAdds are the PCs of additions that have met a string: each is Go's
// (ir.StringAdd), its operands read as they are, not as numbers -- as V8's
// code follows an addition's feedback. A loop's `token += c`, whose token
// is a string, had each character read as a number and leave native code.
// ElementReads are the PCs of element reads that have met a receiver whose
// elements are no numbers, a string, or an object read by a name: each is
// read as it is (ir's
// Reference), whatever its uses take it for -- its first use that takes a
// number then exits, and is a hint of its own. StringReads are those whose
// element was a string -- a string's, or an array's of strings -- read as
// they are too, the additions their results reach Go's, as StringAdds'
// (stringConcats): learned one at a time, a read and then each addition of
// it, a function's compiles for what failed had run out before its last,
// btoa's, of a table of characters, which then left native code at every
// call.
type Hints struct {
	StringAdds, ElementReads, StringReads []int32
}

// LowerSSAWith is LowerSSA with hints.
func LowerSSAWith(fn *bytecode.Function, h Hints) (*ir.Program, error) {
	return lowerRecovered(fn, lowering{ssa: true, hints: h})
}

// LowerSSAHotWith is LowerSSAHot with hints.
func LowerSSAHotWith(fn *bytecode.Function, h Hints) (*ir.Program, error) {
	return lowerRecovered(fn, lowering{ssa: true, hot: true, hints: h})
}

// LowerSSAInlineWith is LowerSSAInline with hints.
func LowerSSAInlineWith(fn *bytecode.Function, h Hints) (*ir.Program, error) {
	return lowerRecovered(fn, lowering{ssa: true, callee: true, hints: h})
}

// LowerSSAForwardWith is LowerSSAForward with hints.
func LowerSSAForwardWith(fn *bytecode.Function, h Hints) (*ir.Program, error) {
	if fn == nil || fn.Leaf != bytecode.LeafForward && !fn.ForwardsArguments {
		return nil, refuse(-1, "not a forwarding constructor")
	}
	return lowerRecovered(fn, lowering{ssa: true, callee: true, forward: true, hints: h})
}

// LowerSSAHot is LowerSSA for a function its callers have shown to be hot
// -- called enough times, for its size, that V8 would have optimized it
// (the VM's jitHotWork) -- which LowerSSA refuses for having no loop and no
// array work (HostOnly): one that makes calls is lowered, as they are made
// natively where their callees have code, a recursion or a run of calls
// that native code makes, however little the function does itself.
func LowerSSAHot(fn *bytecode.Function) (*ir.Program, error) {
	return lowerRecovered(fn, lowering{ssa: true, hot: true})
}

// LowerSSAInline is LowerSSA for a callee the new pipeline inlines into its
// caller (ssa.InlineSite): it lowers what a function called alone would not
// be worth compiling for, host operations without a loop.
func LowerSSAInline(fn *bytecode.Function) (*ir.Program, error) {
	return lowerRecovered(fn, lowering{ssa: true, callee: true})
}

// LowerSSAForward is LowerSSAInline for a constructor that forwards its
// arguments to a method of its object, `this.k.apply(this, arguments)`
// (bytecode.LeafForward), or one that uses its arguments for nothing but
// such calls (bytecode's ForwardsArguments), inlined where its caller
// knows what it forwards to: apply_arguments is left to Go in its program,
// and the inliner makes it the call of the method with the construction's
// own arguments, as V8 eliminates the arguments object. Its frame, made by
// Go at an exit, has the construction's arguments only where the inliner
// keeps them for it (ssa.InlineSite's Arguments); else only an inliner
// that restarts the construction at an exit before the call may take it.
func LowerSSAForward(fn *bytecode.Function) (*ir.Program, error) {
	if fn == nil || fn.Leaf != bytecode.LeafForward && !fn.ForwardsArguments {
		return nil, refuse(-1, "not a forwarding constructor")
	}
	return lowerRecovered(fn, lowering{ssa: true, callee: true, forward: true})
}

// makesArrow reports whether fn makes an arrow, which shares its this
// binding: one an ordinary function it makes makes shares that function's.
func makesArrow(fn *bytecode.Function) bool {
	for _, k := range fn.Constants {
		if k.Kind == bytecode.ConstFunction && k.Fn != nil && k.Fn.Kind == bytecode.KindArrow {
			return true
		}
	}
	return false
}

// closureAssigned is, by local, whether a closure fn makes, or one a
// closure of it makes in turn, assigns the local it captured; or nil if
// none does. A nested function with a direct eval may assign any.
func closureAssigned(fn *bytecode.Function) []bool {
	var out []bool
	var assigns func(t *bytecode.Function, up int) bool
	assigns = func(t *bytecode.Function, up int) bool {
		if t.HasDirectEval {
			return true
		}
		for _, in := range t.Code {
			switch in.Op {
			case bytecode.OpSetUpvalue, bytecode.OpSetUpvalueCheck, bytecode.OpInitUpvalue:
				if int(in.A) == up {
					return true
				}
			}
		}
		for _, c := range t.Constants {
			if c.Kind != bytecode.ConstFunction || c.Fn == nil {
				continue
			}
			for j, d := range c.Fn.Upvalues {
				if !d.FromParent && int(d.Index) == up && assigns(c.Fn, j) {
					return true
				}
			}
		}
		return false
	}
	for _, c := range fn.Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		for j, d := range c.Fn.Upvalues {
			if d.FromParent && int(d.Index) < fn.LocalCount && assigns(c.Fn, j) {
				if out == nil {
					out = make([]bool, fn.LocalCount)
				}
				out[d.Index] = true
			}
		}
	}
	return out
}

// lowering is how a function is lowered: for which entry points, and for
// which pipeline.
type lowering struct {
	calls, callee, ssa, forward, hot bool
	hints                            Hints
}

// lowerRecovered turns a panic in analysis into a refusal: an optional tier
// must never take down its host, and the function then runs in the existing
// tiers.
func lowerRecovered(fn *bytecode.Function, m lowering) (p *ir.Program, err error) {
	defer func() {
		if v := recover(); v != nil {
			p, err = nil, refuse(-1, fmt.Sprintf("internal error: %v", v))
		}
	}()
	return lowerImpl(fn, m)
}

// lowerImpl is lowerFunction, or a test's replacement for it.
var lowerImpl = lowerFunction

func lowerFunction(fn *bytecode.Function, m lowering) (*ir.Program, error) {
	if fn == nil {
		return nil, refuse(-1, "nil function")
	}
	switch {
	case (fn.TopLevel || fn.IsModule) && (!m.ssa || m.callee):
		// A script's, a module's or an eval's code is the new pipeline's,
		// entered at its loops: nothing calls it, and its frame is the
		// VM's, as V8 enters top-level code only by on-stack replacement.
		return nil, refuse(-1, "top-level or module code")
	case (fn.Async || fn.Generator) && (!m.ssa || m.callee):
		// A generator's or an async function's body is the new pipeline's,
		// in its own code: a call of one makes its object, and inlining
		// its body would run it as a call does.
		return nil, refuse(-1, "async or generator function")
	case fn.Kind != bytecode.KindNormal && fn.Kind != bytecode.KindArrow && fn.Kind != bytecode.KindMethod && fn.Kind != bytecode.KindConstructor &&
		fn.Kind != bytecode.KindGetter && fn.Kind != bytecode.KindSetter && (fn.Kind != bytecode.KindDerivedConstructor || !m.ssa || m.forward):
		// A base class's constructor is a function's body, constructed as
		// one is (the VM's jitConstructs), and called by no native call:
		// a call without new throws. A getter's and a setter's are a
		// method's, called with the receiver as their this by a read or a
		// write (ssa.AccessorSite). A derived class's is the new
		// pipeline's, in its own code, entered from Go as its construction
		// runs it or constructed natively (ir.Program's
		// DerivedConstructor): its super() binds its this, which Go makes
		// (OpSuperCall). A class's field initializers are not.
		return nil, refuse(-1, "special function kind")
	case fn.Kind == bytecode.KindDerivedConstructor && m.callee && makesArrow(fn):
		// An arrow shares the constructor's this binding, which a frame
		// native code constructs has none of until Go makes its frame.
		return nil, refuse(-1, "derived constructor making an arrow, for native callers")
	case fn.HasDirectEval || len(fn.EvalScopes) != 0:
		return nil, refuse(-1, "direct eval")
	case (fn.UsesArguments || fn.MappedArguments) && !m.forward:
		return nil, refuse(-1, "arguments object")
	case !fn.HasSimpleParams && !fn.TopLevel || fn.HasRest || fn.ParamsAreLexical:
		// Top-level code has no parameter list, simple or not.
		return nil, refuse(-1, "non-simple parameters")
	case len(fn.Code) == 0 || len(fn.Code) > MaxInstructions:
		return nil, refuse(-1, "instruction budget")
	case fn.LocalCount < 0 || fn.MaxStack < 0 || fn.LocalCount > MaxSlots || fn.MaxStack > MaxSlots || len(fn.Upvalues) > MaxSlots || fn.LocalCount+fn.MaxStack+len(fn.Upvalues) > MaxSlots:
		return nil, refuse(-1, "slot budget")
	case fn.ParamCount < 0 || fn.ParamCount > fn.LocalCount || len(fn.Locals) != fn.LocalCount:
		return nil, refuse(-1, "invalid local layout")
	}
	captured := false
	for _, local := range fn.Locals {
		if local.Captured {
			if !m.ssa || m.forward {
				// Its frame is the VM's, which the old pipeline's code,
				// and a forwarding constructor's inlined frame, have none
				// of. A native callee's frame is made in the VM's stack, and
				// Go makes and closes its closures there (jitCalleeFrame);
				// a callee of captured locals is not inlined (ssa's
				// InlineCalls).
				return nil, refuse(-1, "captured local")
			}
			captured = true
		}
	}
	effects := make([]effect, len(fn.Code))
	host, loop, indexed, property, bitwise, this, handlers, iters := false, false, false, false, false, false, false, false
	calls := false
	for pc, in := range fn.Code {
		e, err := describe(fn, pc, in)
		if err != nil {
			return nil, err
		}
		if in.Op == bytecode.OpSuperCall && in.B == bytecode.SuperForward && m.callee {
			// super(...arguments) hands on the call's arguments, which a
			// frame native code constructs has none of (the VM's f.args):
			// the constructor a class without one has, which a
			// construction skips (the VM's jitDerivedBase).
			return nil, refuse(pc, "super of the call's arguments, for native callers")
		}
		effects[pc] = e
		if in.Op == bytecode.OpNew && !m.ssa || (in.Op == bytecode.OpSetGlobal || in.Op == bytecode.OpSetGlobalStrict ||
			in.Op == bytecode.OpCheckGlobalRef || in.Op == bytecode.OpAssertResolved ||
			in.Op == bytecode.OpNewObject || in.Op == bytecode.OpNewRegExp || in.Op == bytecode.OpDefineField || in.Op == bytecode.OpPushEmptyString ||
			in.Op == bytecode.OpConcat || in.Op == bytecode.OpSetUpvalue || in.Op == bytecode.OpSetUpvalueCheck || in.Op == bytecode.OpInitUpvalue) && !m.ssa {
			// The new pipeline constructs natively (mir's native calls),
			// and assigns to a global natively where the VM knows it, Go
			// otherwise; the old one does neither.
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		str := in.Op == bytecode.OpPushConst && (fn.Constants[in.A].Kind == bytecode.ConstString || fn.Constants[in.A].Kind == bytecode.ConstBigInt)
		if str && !m.ssa {
			// The new pipeline has Go push a string constant (a host
			// exit), which a function's cold paths -- a message it throws
			// -- often have; the old one refuses it.
			return nil, refuse(pc, "non-number or invalid constant")
		}
		if in.Op == bytecode.OpTypeOf && !m.ssa {
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		if in.Op == bytecode.OpInstanceOf && !m.ssa {
			// The new pipeline makes it natively where the VM knows its
			// constructor (ssa.InstanceOfSite), Go otherwise.
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		if (in.Op == bytecode.OpIn || in.Op == bytecode.OpDeleteProp) && !m.ssa {
			// The new pipeline has Go answer in and make delete, called
			// from native code (ir.HasProperty, ir.DeleteProperty).
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		if (in.Op == bytecode.OpClosure || in.Op == bytecode.OpCloseUpvalues) && (!m.ssa || m.forward) {
			// Go makes a closure, and closes what one captured, on the
			// frame (jitHost): a function's own, or a native callee's in
			// the VM's stack (jitCalleeFrame), in the new pipeline; not an
			// inlined callee's (ssa's InlineCalls).
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		if (in.Op == bytecode.OpApplyArguments || in.Op == bytecode.OpArgumentsIndex) && !m.forward {
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		if (isPrivate(in.Op) || isDefinition(in.Op)) && !m.ssa {
			// The new pipeline reads and writes a private member as a
			// property, its key checked; Go makes the rest.
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		if m.callee && (isDefinition(in.Op) || isPrivate(in.Op) && in.Op != bytecode.OpGetPrivate && in.Op != bytecode.OpSetPrivate && in.Op != bytecode.OpPrivateIn) {
			// What Go makes of them -- a class's or a literal's member, a
			// private member's definition -- leaves native code, after which
			// Go goes on natively in a function's own code, at the entry
			// after it, but finishes a callee in the interpreter, which has
			// none: a callee's every call would leave, and Go is the one to
			// call it.
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		if (in.Op == bytecode.OpTailCall || in.Op == bytecode.OpTailCallMethod) && (!m.ssa || m.callee) {
			// A tail call gives up its frame deep enough in the stack: a
			// function's own, in the new pipeline, which a callee inlined
			// has none of.
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		if (in.Op == bytecode.OpPushCatch || in.Op == bytecode.OpPushFinally || in.Op == bytecode.OpPopCatch || in.Op == bytecode.OpRethrow) && (!m.ssa || m.callee) {
			// Exception handlers are the new pipeline's, in a function's
			// own code: a callee inlined has no frame of its own for Go to
			// give them to.
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		switch in.Op {
		case bytecode.OpForOfStart, bytecode.OpForInStart, bytecode.OpIterNextOrJump, bytecode.OpIterClose, bytecode.OpIterCloseNormal,
			bytecode.OpIterResume, bytecode.OpIterUnpackDelegate, bytecode.OpYieldStar, bytecode.OpIterStep, bytecode.OpIterRest:
			if !m.ssa || m.callee {
				// A loop's cursor is Go's, in the function's own frame.
				return nil, refuse(pc, "unsupported opcode "+in.Op.String())
			}
			iters = true
		}
		handlers = handlers || in.Op == bytecode.OpPushCatch || in.Op == bytecode.OpPushFinally
		if (in.Op == bytecode.OpJumpIfNullish || in.Op == bytecode.OpJumpIfNotNullish) && !m.ssa {
			// The new pipeline tests a value for null and undefined.
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		if in.Op == bytecode.OpThrow && !m.ssa {
			// The new pipeline leaves a throw to the interpreter, which
			// throws it, as V8's code calls the runtime to throw.
			return nil, refuse(pc, "unsupported opcode "+in.Op.String())
		}
		host = host || str || iters || isPrivate(in.Op) || isDefinition(in.Op) || in.Op == bytecode.OpClosure || in.Op == bytecode.OpCloseUpvalues || in.Op == bytecode.OpImport || in.Op == bytecode.OpImportMeta || in.Op == bytecode.OpYield || in.Op == bytecode.OpAwait || in.Op == bytecode.OpThrow ||
			in.Op == bytecode.OpNewDisposeCapability || in.Op == bytecode.OpAddDisposable || in.Op == bytecode.OpDispose || in.Op == bytecode.OpInstanceOf || in.Op == bytecode.OpIn || in.Op == bytecode.OpDeleteProp || in.Op == bytecode.OpTypeOf || in.Op == bytecode.OpApplyArguments || in.Op == bytecode.OpNew || in.Op == bytecode.OpSuperCall || in.Op == bytecode.OpSetGlobal || in.Op == bytecode.OpSetGlobalStrict || in.Op == bytecode.OpCheckGlobalRef || in.Op == bytecode.OpNewObject || in.Op == bytecode.OpNewRegExp || in.Op == bytecode.OpDefineField || in.Op == bytecode.OpPushEmptyString || in.Op == bytecode.OpConcat || in.Op == bytecode.OpNewArray || in.Op == bytecode.OpCall || in.Op == bytecode.OpCallMethod || in.Op == bytecode.OpTailCall || in.Op == bytecode.OpTailCallMethod || in.Op == bytecode.OpGetGlobal || in.Op == bytecode.OpGetGlobalOpt || in.Op == bytecode.OpGetPropThis || in.Op == bytecode.OpPushThis || in.Op == bytecode.OpGetProp || in.Op == bytecode.OpSetProp
		property = property || in.Op == bytecode.OpPushThis || in.Op == bytecode.OpGetProp || in.Op == bytecode.OpSetProp
		// A derived constructor's this slot is what it returns, bound by
		// its super() (ir.Program's DerivedConstructor).
		this = this || in.Op == bytecode.OpPushThis || fn.Kind == bytecode.KindDerivedConstructor
		raw := uint32(in.Op)
		switch in.Op {
		case bytecode.OpBinLocal, bytecode.OpBinImm:
			raw = in.B
		case bytecode.OpLocalBinImm:
			raw = in.A >> 24
		}
		op, _ := operator(raw)
		host = host || raw == uint32(bytecode.OpMod) || raw == uint32(bytecode.OpPow)
		bitwise = bitwise || op >= ir.BitAnd && op <= ir.UShr || in.Op == bytecode.OpBitNot
		// A call in tail position may be the function's own, which native
		// code makes a jump to its start deep in the stack: a loop.
		loop = loop || e.branch && e.target(in) <= pc || in.Op == bytecode.OpTailCall || in.Op == bytecode.OpTailCallMethod
		calls = calls || in.Op == bytecode.OpCall || in.Op == bytecode.OpCallMethod || in.Op == bytecode.OpNew
		indexed = indexed || in.Op == bytecode.OpGetIndex || in.Op == bytecode.OpGetIndexThis || in.Op == bytecode.OpSetIndex || in.Op == bytecode.OpGetLocalIndex || in.Op == bytecode.OpGetLocalIndexUpdate
	}
	// Tiny host-only wrappers pay the bridge overhead without enough native
	// work -- but for one shown to be hot that makes calls (LowerSSAHot).
	if host && !loop && !indexed && !m.callee && !(m.hot && calls) {
		return nil, refuse(-1, hostOnly)
	}
	if property && !indexed && !bitwise && !m.callee && !m.ssa {
		return nil, refuse(-1, "property operations without native array or bitwise work")
	}
	maps := make([]ir.StateMap, len(fn.Code))
	for pc := range maps {
		maps[pc] = ir.StateMap{PC: uint32(pc), Depth: -1}
	}
	maps[0].Depth = 0
	queue := make([]int, 1, len(fn.Code))
	queue[0] = 0
	// The handlers in force at each instruction, as Program's HandlerAt has
	// them, where the function has any: the code is structured, so every way
	// into an instruction has the same ones, as the tree tier's builder
	// finds them (enterHandlers).
	var chains []int32
	var table []ir.Handler
	var pushed []int32
	if handlers {
		chains, pushed = make([]int32, len(fn.Code)), make([]int32, len(fn.Code))
	}
	join := func(pc, depth int, chain int32) error {
		if pc >= len(maps) {
			return refuse(pc-1, "fallthrough past function")
		}
		if maps[pc].Depth >= 0 {
			if maps[pc].Depth != depth {
				return refuse(pc, "inconsistent stack depth at join")
			}
			if chains != nil && chains[pc] != chain {
				return refuse(pc, "inconsistent handlers at join")
			}
			return nil
		}
		maps[pc].Depth = depth
		if chains != nil {
			chains[pc] = chain
		}
		queue = append(queue, pc)
		return nil
	}
	for head := 0; head < len(queue); head++ {
		pc := queue[head]
		e, depth := effects[pc], maps[pc].Depth
		var chain int32
		if chains != nil {
			chain = chains[pc]
			switch in := fn.Code[pc]; in.Op {
			case bytecode.OpPushCatch, bytecode.OpPushFinally:
				if pushed[pc] == 0 {
					table = append(table, ir.Handler{PC: in.A, Depth: int32(depth), Outer: chain, Finally: in.Op == bytecode.OpPushFinally})
					pushed[pc] = int32(len(table))
					// The handler is entered as the interpreter enters it:
					// the stack cut to its depth, the thrown value -- for a
					// finally clause, a completion record -- pushed, and
					// the handler dropped.
					n := 1
					if in.Op == bytecode.OpPushFinally {
						n = 2
					}
					if err := join(int(in.A), depth+n, chain); err != nil {
						return nil, err
					}
				}
				chain = pushed[pc]
			case bytecode.OpPopCatch:
				if chain == 0 {
					return nil, refuse(pc, "no handler to drop")
				}
				chain = table[chain-1].Outer
			}
		}
		if depth < e.need {
			return nil, refuse(pc, "operand stack underflow")
		}
		nextDepth := depth + e.delta
		if nextDepth > fn.MaxStack {
			return nil, refuse(pc, "operand stack exceeds MaxStack")
		}
		if e.branch {
			targetDepth := nextDepth
			switch {
			case e.keep:
				targetDepth = depth
			case e.jumps:
				targetDepth = depth + e.jump
			}
			if err := join(e.target(fn.Code[pc]), targetDepth, chain); err != nil {
				return nil, err
			}
		}
		if !e.terminal {
			if err := join(pc+1, nextDepth, chain); err != nil {
				return nil, err
			}
		}
	}
	p := &ir.Program{Locals: fn.LocalCount + len(fn.Upvalues), StackSize: fn.MaxStack, This: this, Maps: maps, Code: make([]ir.Instruction, len(fn.Code)),
		ArrowThis:          fn.Kind == bytecode.KindArrow && fn.UsesThis,
		ClassConstructor:   fn.Kind == bytecode.KindConstructor,
		DerivedConstructor: fn.Kind == bytecode.KindDerivedConstructor,
		Handlers:           table, HandlerAt: chains}
	for pc, in := range fn.Code {
		if to, ok := goBranch(in); ok && maps[pc].Depth >= 0 && !slices.Contains(p.Entries, to) {
			p.Entries = append(p.Entries, to)
		}
	}
	for _, h := range table {
		// A throw Go catches goes on natively at its handler.
		if !slices.Contains(p.Entries, int(h.PC)) {
			p.Entries = append(p.Entries, int(h.PC))
		}
	}
	if fn.Async || fn.Generator {
		// The first resumption enters past the parameters, which ran when
		// the generator was made.
		for pc, in := range fn.Code {
			if in.Op == bytecode.OpEndParams && pc+1 < len(fn.Code) && maps[pc+1].Depth >= 0 {
				p.Entries = append(p.Entries, pc+1)
				break
			}
		}
	}
	if this {
		if p.Locals+p.StackSize == MaxSlots {
			if p.DerivedConstructor {
				return nil, refuse(-1, "no slot for a derived constructor's this")
			}
			p.This = false
		} else {
			p.Locals++
		}
	}
	// The old pipeline finds the calls of charCodeAt by the name of the
	// method they call; the new one by their target (ssa's
	// CharCodeFeedback): a call whose method is read anyhow, and none
	// that calls another.
	codes := make([]bool, len(fn.Code))
	if !m.ssa {
		codes = charCodeCalls(fn, maps)
	}
	for pc, in := range fn.Code {
		switch {
		case maps[pc].Depth < 0:
		case iters && (in.Op == bytecode.OpReturn && maps[pc].Depth > 1 || in.Op == bytecode.OpReturnUndef && maps[pc].Depth > 0):
			// A return with operands beneath its value, which may be the
			// cursors of the loops it leaves: Go's, as the interpreter
			// closes them (closeIteratorsReturning).
			p.Code[pc] = ir.Instruction{Op: ir.Host}
		case m.ssa && in.Op == bytecode.OpThrow && chains != nil && chains[pc] != 0 &&
			!table[chains[pc]-1].Finally && int32(maps[pc].Depth-1) == table[chains[pc]-1].Depth:
			// A throw a catch clause of the function catches, its value the
			// one operand above the handler's depth: the frame is already as
			// the interpreter's unwinding leaves it -- the stack at the
			// handler's depth, the value pushed, the handler dropped (its
			// chain at the handler, the outer one) -- so it is a jump there,
			// as the tree tier's throwToHandler makes it, no exception made.
			// Each had left native code for Go to throw, which snapshot the
			// frames for an uncaught one's stack, five times the
			// interpreter's time. A finally clause, which takes a completion
			// record, and a throw with operands beneath its value, which may
			// be loops' cursors to close, Go still throws.
			p.Code[pc] = ir.Instruction{Op: ir.Jump, Target: int(table[chains[pc]-1].PC)}
		default:
			p.Code[pc] = lower(fn, in, p.Locals+maps[pc].Depth, p.This, codes[pc])
		}
	}
	if m.ssa {
		hintedStringAdds(p, m.hints.StringAdds)
	}
	selectNumericProperties(p, m.ssa)
	if m.ssa {
		for _, pcs := range [][]int32{m.hints.ElementReads, m.hints.StringReads} {
			for _, pc := range pcs {
				if pc >= 0 && int(pc) < len(p.Code) && p.Code[pc].Op == ir.ArrayRead {
					p.Code[pc].Reference = true
				}
			}
		}
		liveLocals(fn, p, effects)
		typeTests(fn, p, effects)
		if p.Live != nil && chains != nil {
			// A throw resumes the interpreter at a handler, which native
			// code never runs, so what the handler reads is not known:
			// every local is live where one is in force.
			for pc, h := range chains {
				if h != 0 {
					for w := range p.LiveWords {
						p.Live[pc*p.LiveWords+w] = ^uint64(0)
					}
				}
			}
		}
	}
	if m.ssa {
		for pc, in := range p.Code {
			if in.Op != ir.Host || p.Maps[pc].Depth < 0 {
				continue
			}
			// s.charCodeAt, in any function: the new pipeline finds the
			// intrinsic in every context (the VM's jitShared), not at each
			// entry, so a helper with no loop -- Crypto's intAt -- is worth
			// it too (stringKernel). The call is made by its target.
			sp := p.Locals + p.Maps[pc].Depth
			if b := fn.Code[pc]; b.Op == bytecode.OpGetPropThis && fn.Names[b.A] == "charCodeAt" {
				p.Code[pc] = ir.Instruction{Op: ir.StringMethod, Left: ir.Slot(sp - 1), Dest: sp}
			}
		}
		for pc, in := range p.Code {
			if in.Op == ir.Host && p.Maps[pc].Depth >= 0 {
				// The new pipeline computes a remainder; the old one leaves
				// it to Go.
				if mod, ok := lowerMod(fn.Code[pc], p.Locals+p.Maps[pc].Depth); ok {
					p.Code[pc] = mod
				}
				if fn.Code[pc].Op == bytecode.OpPow {
					// So is a power, ** (ir.Pow).
					sp := p.Locals + p.Maps[pc].Depth
					p.Code[pc] = ir.Instruction{Op: ir.Binary, Operator: ir.Pow, Dest: sp - 2, Left: ir.Slot(sp - 2), Right: ir.Slot(sp - 1)}
				}
			}
			if b := fn.Code[pc]; in.Op == ir.Host && (b.Op == bytecode.OpPushConst && fn.Constants[b.A].Kind == bytecode.ConstString || b.Op == bytecode.OpPushEmptyString) && p.Maps[pc].Depth >= 0 {
				// A string constant: the new pipeline reads it from a cell
				// the VM keeps it in, where the VM gives one.
				p.Code[pc] = ir.Instruction{Op: ir.StringConst, Dest: p.Locals + p.Maps[pc].Depth, Key: b.A}
			}
			if b := fn.Code[pc]; in.Op == ir.Host && b.Op == bytecode.OpPushConst && fn.Constants[b.A].Kind == bytecode.ConstBigInt && p.Maps[pc].Depth >= 0 {
				// A BigInt constant, read as a string's is.
				p.Code[pc] = ir.Instruction{Op: ir.BigIntConst, Dest: p.Locals + p.Maps[pc].Depth, Key: b.A}
			}
			if b := fn.Code[pc]; p.Maps[pc].Depth >= 0 {
				// Captured bindings read and written in their cells: not
				// slots read once at an entry, which a native callee's
				// assignment would leave stale.
				sp := p.Locals + p.Maps[pc].Depth
				switch b.Op {
				case bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck:
					p.Code[pc] = ir.Instruction{Op: ir.UpvalueRead, Dest: sp, Key: b.A, Check: b.Op == bytecode.OpGetUpvalueCheck}
				case bytecode.OpSetUpvalue, bytecode.OpInitUpvalue:
					p.Code[pc] = ir.Instruction{Op: ir.UpvalueWrite, Left: ir.Slot(sp - 1), Key: b.A}
				case bytecode.OpSetUpvalueCheck:
					if fn.Upvalues[b.A].Mutable {
						p.Code[pc] = ir.Instruction{Op: ir.UpvalueWrite, Left: ir.Slot(sp - 1), Key: b.A, Check: true}
					}
				}
			}
			if b := fn.Code[pc]; in.Op == ir.Host && b.Op == bytecode.OpNewArray && b.A <= ir.MaxArrayLiteral && p.Maps[pc].Depth >= 0 {
				// An array literal: the new pipeline makes it from its
				// site's pool where the VM knows the site.
				n := int(b.A)
				p.Code[pc] = ir.Instruction{Op: ir.ArrayLiteral, Dest: p.Locals + p.Maps[pc].Depth - n, Extra: n}
			}
			if b := fn.Code[pc]; in.Op == ir.Host && b.Op == bytecode.OpConcat && b.A <= ir.MaxStringConcat && p.Maps[pc].Depth >= 0 {
				// A template's or a string chain's parts joined: the new
				// pipeline has Go join them, called from native code.
				n := int(b.A)
				p.Code[pc] = ir.Instruction{Op: ir.StringConcat, Dest: p.Locals + p.Maps[pc].Depth - n, Extra: n}
			}
			if in.Op == ir.BindingRead {
				// No operand: native code reads the binding where the
				// global object, or a script's lexical bindings, have it,
				// through its context.
				p.Code[pc].Left = ir.Literal(ir.Value{Kind: ir.Undefined})
			}
			if b := fn.Code[pc]; in.Op == ir.Host && b.Op == bytecode.OpGetPropThis && p.Maps[pc].Depth >= 0 {
				// A method's read, which keeps its receiver below it: the
				// new pipeline reads it where the site's cache found it,
				// on a prototype too, and carries it by its cell to the call.
				sp := p.Locals + p.Maps[pc].Depth
				p.Code[pc] = ir.Instruction{Op: ir.ReferenceRead, Left: ir.Slot(sp - 1), Dest: sp, Key: b.A}
			}
		}
		stringConcats(p, m.hints.StringReads)
		markBools(p)
		p.UpvalueBase, p.Upvalues, p.Params = fn.LocalCount, len(fn.Upvalues), fn.ParamCount
		if captured {
			p.Captured = make([]bool, fn.LocalCount)
			for i, local := range fn.Locals {
				p.Captured[i] = local.Captured
			}
			p.Assigned = closureAssigned(fn)
			// In cells of their own (ir.Program's Cells), as V8 allocates
			// every function's context.
			p.Cells = true
		}
		return p, nil
	}
	selectPropertyLoops(fn, p, m.calls, m.callee, codes)
	selectGlobalSlots(fn, p, codes)
	return p, nil
}

// Numeric global reads borrow live cells, rather than snapshotting their values.
// Reserve one handle per name; excess bindings retain the original host path.
func selectGlobalSlots(fn *bytecode.Function, p *ir.Program, codes []bool) {
	base := p.Locals
	for pc, in := range p.Code {
		if in.Op != ir.BindingRead {
			continue
		}
		i := 0
		for i < len(p.Globals) && p.Globals[i] != in.Key {
			i++
		}
		if i == len(p.Globals) {
			if base+p.StackSize+len(p.Globals) == MaxSlots {
				p.Code[pc] = ir.Instruction{Op: ir.Host}
				continue
			}
			p.Globals = append(p.Globals, in.Key)
		}
		in.Left = ir.Slot(base + i)
		p.Code[pc] = in
	}
	if len(p.Globals) == 0 {
		return
	}
	p.Locals += len(p.Globals)
	for pc, old := range p.Code {
		if p.Maps[pc].Depth < 0 || old.Op == ir.Host {
			continue
		}
		in := lower(fn, fn.Code[pc], p.Locals+p.Maps[pc].Depth, p.This, codes[pc])
		if old.Op == ir.BindingRead {
			in.Left = old.Left
		}
		if old.Op == ir.ReferenceRead {
			in.Op = ir.ReferenceRead
		}
		p.Code[pc] = in
	}
}

// Borrowing object tables and another receiver root costs something on every
// entry. Keep mixed loops on their existing bridge until their remaining host
// operations are covered; a native field loop must amortize that preparation.
func selectPropertyLoops(fn *bytecode.Function, p *ir.Program, calls, callee bool, codes []bool) bool {
	// Preparing a reference graph cannot amortize a read performed only once
	// before the loop. Keep those entries on the existing cheap Go bridge.
	loopReference := false
	for pc, in := range p.Code {
		if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc {
			for _, body := range p.Code[in.Target : pc+1] {
				loopReference = loopReference || body.Op == ir.ReferenceRead
			}
		}
	}
	if !loopReference {
		for pc, in := range p.Code {
			if in.Op == ir.ReferenceRead {
				p.Code[pc] = ir.Instruction{Op: ir.Host}
			}
		}
	}
	hosts := make([]int, len(p.Code)+1)
	fields, loop, mixed, fieldCount := false, false, false, 0
	for pc, in := range p.Code {
		hosts[pc+1] = hosts[pc]
		if in.Op == ir.Host {
			hosts[pc+1]++
		}
		fields = fields || in.Op == ir.PropertyRead || in.Op == ir.PropertyWrite || in.Op == ir.ReferenceRead
		if in.Op == ir.PropertyRead || in.Op == ir.PropertyWrite || in.Op == ir.ReferenceRead {
			fieldCount++
		}
	}
	for pc, in := range p.Code {
		if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc {
			loop = true
			mixed = mixed || hosts[pc+1] != hosts[in.Target]
		}
	}
	if callee || calls && mixed {
		for pc, in := range p.Code {
			if in.Op == ir.ReferenceRead {
				p.Code[pc] = ir.Instruction{Op: ir.Host}
			}
		}
	}
	fields = fields && (loop || callee) && (!mixed || calls)
	if p.This && !p.DerivedConstructor && !fields && hosts[len(p.Code)]+fieldCount != 0 {
		p.This = false
		p.Locals--
		for pc, in := range fn.Code {
			if p.Maps[pc].Depth >= 0 {
				p.Code[pc] = lower(fn, in, p.Locals+p.Maps[pc].Depth, false, codes[pc])
			}
		}
		selectNumericProperties(p, false)
	}
	if !fields {
		for pc, in := range p.Code {
			if in.Op == ir.PropertyRead || in.Op == ir.PropertyWrite || in.Op == ir.ReferenceRead {
				p.Code[pc] = ir.Instruction{Op: ir.Host}
			}
		}
	}
	return loop && !mixed
}

// This bounded backward scan chooses field reads whose results feed numeric
// operations or reference receivers. It is a profitability hint; every native
// read still checks its live type and permissions before committing. With
// cells, the new pipeline carries any value a read finds by where it found
// it, so a read no native operation consumes -- a call's callee or argument,
// which Go takes from the frame -- stays native there, as a global's cell or
// a reference, rather than add an exit.
func selectNumericProperties(p *ir.Program, cells bool) {
	const number, reference uint8 = 1, 2
	var needed [MaxSlots]uint8
	read := func(o ir.Operand, kind uint8) {
		if o.Slot >= 0 {
			needed[o.Slot] |= kind
		}
	}
	take := func(slot int) uint8 { k := needed[slot]; needed[slot] = 0; return k }
	for pc := len(p.Code) - 1; pc >= 0; pc-- {
		if p.Maps[pc].Depth < 0 {
			continue
		}
		in := p.Code[pc]
		switch in.Op {
		case ir.PropertyRead, ir.BindingRead:
			kind := take(in.Dest)
			if kind&number == 0 && !(cells && in.Op == ir.BindingRead) {
				if in.Op == ir.PropertyRead && (kind&reference != 0 || cells) {
					in.Op = ir.ReferenceRead
					p.Code[pc] = in
				} else {
					p.Code[pc] = ir.Instruction{Op: ir.Host}
				}
			}
			if in.Op != ir.BindingRead {
				read(in.Left, reference)
			}
		case ir.Copy:
			read(in.Left, take(in.Dest))
		case ir.Resolved:
			read(in.Right, take(in.Dest))
		case ir.BindingCheck, ir.ObjectLiteral, ir.StringConst, ir.BigIntConst, ir.UpvalueRead, ir.Closure:
			take(in.Dest)
		case ir.FieldDefine:
			read(in.Left, reference)
		case ir.ArgumentRead:
			// Go takes the key as it is.
			take(in.Dest)
			read(in.Left, reference)
		case ir.HasProperty, ir.DeleteProperty:
			// Go takes the key and the object as they are.
			take(in.Dest)
			read(in.Left, reference)
			read(in.Right, reference)
		case ir.CopyPair:
			left, right := take(in.Dest), take(in.Extra)
			read(in.Left, left)
			read(in.Right, right)
		case ir.StoreLoad:
			read(in.Right, take(in.Extra))
			read(in.Left, take(in.Dest))
		case ir.Swap:
			needed[in.Dest], needed[in.Extra] = needed[in.Extra], needed[in.Dest]
		case ir.Binary:
			take(in.Dest)
			if !(cells && (in.Operator == ir.Eq || in.Operator == ir.Ne)) {
				// The new pipeline compares any value: equality takes a read
				// as it finds it, as a reference if it is one.
				read(in.Left, number)
				read(in.Right, number)
			}
		case ir.Unary, ir.Update:
			take(in.Dest)
			if in.Op == ir.Update && in.Extra >= 0 {
				take(in.Extra)
			}
			if in.Operator != ir.Not {
				read(in.Left, number)
			}
		case ir.ArrayRead, ir.ArrayUpdate, ir.ArrayLength:
			if kind := take(in.Dest); cells && in.Op != ir.ArrayLength && kind&number == 0 {
				in.Reference = true
				p.Code[pc] = in
			}
			if in.Op == ir.ArrayUpdate {
				take(in.Extra)
			}
			read(in.Left, reference)
			if in.Op != ir.ArrayLength {
				read(in.Right, number)
			}
		case ir.ArrayWrite, ir.ArrayKey:
			read(in.Left, reference)
			read(in.Right, number)
			if in.Op == ir.ArrayWrite {
				read(in.Third, number)
			}
		case ir.PropertyWrite:
			read(in.Left, reference)
			if !cells {
				// The new pipeline stores any value.
				read(in.Right, number)
			}
		case ir.Branch:
			if in.Operator != ir.Truth && in.Operator != ir.Nullish && !(cells && (in.Operator == ir.Eq || in.Operator == ir.Ne)) {
				read(in.Left, number)
				read(in.Right, number)
			}
		case ir.Host:
			clear(needed[p.Locals:])
		case ir.Insert2, ir.Insert3:
			n, last := in.Dest, 2
			if in.Op == ir.Insert2 {
				last = 1
			}
			kind := needed[n] | needed[n+last+1]
			for i := 0; i < last; i++ {
				needed[n+i] = needed[n+i+1]
			}
			needed[n+last], needed[n+last+1] = kind, 0
		}
	}
}

// hintedStringAdds makes the additions at pcs, which have met a string,
// Go's (Hints.StringAdds), before the reads they take are decided
// (selectNumericProperties): an operand is then read as it is, a string's
// element included (ir's Reference), not as a number.
func hintedStringAdds(p *ir.Program, pcs []int32) {
	for _, pc := range pcs {
		if pc < 0 || int(pc) >= len(p.Code) || p.Maps[pc].Depth < 0 {
			continue
		}
		if in := p.Code[pc]; in.Op == ir.Binary && in.Operator == ir.Add {
			p.Code[pc] = ir.Instruction{Op: ir.StringAdd, Left: in.Left, Right: in.Right, Dest: in.Dest}
		}
	}
}

// stringConcats has Go make an addition an operand of which is a string
// constant, such an addition's result -- 'key ' + tag + ' in leaf' -- or
// the result of an element read at one of reads, which have read strings
// (Hints' StringReads), rather than speculate it adds numbers: a string
// constant read natively
// (ir.StringConst) made the code take every such operand for a number, at
// the entry too, and leave there at every call. What a slot holds is
// followed forward through straight code: a jump's target, a host
// operation's results and anything else written forget it.
func stringConcats(p *ir.Program, reads []int32) {
	str := make([]bool, p.Locals+p.StackSize)
	targets := make([]bool, len(p.Code)+1)
	for _, in := range p.Code {
		if in.Op == ir.Jump || in.Op == ir.Branch {
			targets[in.Target] = true
		}
	}
	slot := func(o ir.Operand) bool { return o.Slot >= 0 && o.Slot < len(str) && str[o.Slot] }
	set := func(i int, v bool) {
		if i >= 0 && i < len(str) {
			str[i] = v
		}
	}
	for pc, in := range p.Code {
		if targets[pc] {
			clear(str)
		}
		if p.Maps[pc].Depth < 0 {
			continue
		}
		switch in.Op {
		case ir.StringConst:
			set(in.Dest, true)
		case ir.Copy:
			set(in.Dest, slot(in.Left))
		case ir.Binary:
			if in.Operator == ir.Add && (slot(in.Left) || slot(in.Right)) {
				p.Code[pc] = ir.Instruction{Op: ir.StringAdd, Left: in.Left, Right: in.Right, Dest: in.Dest}
				set(in.Dest, true)
				continue
			}
			set(in.Dest, false)
		case ir.StringAdd, ir.StringConcat:
			set(in.Dest, true)
		case ir.ArrayRead:
			set(in.Dest, in.Reference && slices.Contains(reads, int32(pc)))
		case ir.Host, ir.Call:
			// Its results, past what it leaves below them.
			if pc+1 < len(p.Maps) && p.Maps[pc+1].Depth > 0 {
				clear(str[min(len(str), p.Locals+p.Maps[pc+1].Depth-1):])
			} else {
				clear(str[p.Locals:])
			}
		case ir.Swap, ir.CopyPair, ir.StoreLoad, ir.Insert2, ir.Insert3, ir.Update, ir.ArrayUpdate:
			set(in.Dest, false)
			set(in.Extra, false)
			for i := in.Dest; i >= 0 && i <= in.Dest+3; i++ {
				set(i, false)
			}
		case ir.Branch, ir.Jump, ir.Return, ir.Nop, ir.ArrayWrite, ir.PropertyWrite, ir.BindingWrite, ir.FieldDefine, ir.UpvalueWrite, ir.Rethrow, ir.CloseUpvalues:
		default:
			set(in.Dest, false)
		}
	}
}

// typeNames are the strings typeof compared with tells natively.
var typeNames = map[string]int{"number": ir.TypeNumber, "string": ir.TypeString, "boolean": ir.TypeBoolean,
	"undefined": ir.TypeUndefined, "function": ir.TypeFunction}

// typeTests has typeof compared with a string constant, typeof x ===
// "number", tell the type natively, as V8 folds it into a check of the
// value: the typeof becomes the test (ir.TypeTest), its boolean where the
// typeof's string was; the constant, a number pushed in its place, unread;
// the comparison nothing, or a branch on the boolean. Nothing may jump to
// the constant or the comparison, whose states would hold the boolean.
func typeTests(fn *bytecode.Function, p *ir.Program, effects []effect) {
	targets := map[int]bool{}
	for pc, in := range fn.Code {
		if effects[pc].branch {
			targets[int(in.A)] = true
		}
	}
	for pc := 0; pc+2 < len(fn.Code); pc++ {
		in, k, cmp := fn.Code[pc], fn.Code[pc+1], fn.Code[pc+2]
		if in.Op != bytecode.OpTypeOf || p.Maps[pc].Depth < 1 || k.Op != bytecode.OpPushConst || targets[pc+1] || targets[pc+2] ||
			p.Maps[pc+1].Depth < 0 || p.Maps[pc+2].Depth < 0 || uint64(k.A) >= uint64(len(fn.Constants)) || fn.Constants[k.A].Kind != bytecode.ConstString {
			continue
		}
		kind, ok := typeNames[fn.Constants[k.A].Str]
		if !ok {
			continue
		}
		op := uint32(cmp.Op)
		if cmp.Op == bytecode.OpJumpIfCmpFalse {
			op = cmp.B
		}
		var equal bool
		switch op {
		case uint32(bytecode.OpStrictEq), uint32(bytecode.OpEq):
			equal = true
		case uint32(bytecode.OpStrictNe), uint32(bytecode.OpNe):
		default:
			continue
		}
		slot := p.Locals + p.Maps[pc].Depth - 1
		p.Code[pc] = ir.Instruction{Op: ir.TypeTest, Left: ir.Slot(slot), Dest: slot, Key: uint32(kind), When: equal}
		p.Code[pc+1] = ir.Instruction{Op: ir.Copy, Dest: slot + 1, Left: ir.Literal(ir.Float(0))}
		p.Code[pc+2] = ir.Instruction{Op: ir.Nop}
		if cmp.Op == bytecode.OpJumpIfCmpFalse {
			p.Code[pc+2] = ir.Instruction{Op: ir.Branch, Operator: ir.Truth, Target: int(cmp.A), Left: ir.Slot(slot)}
		}
	}
}

// liveLocals fills p's Live: for each reachable instruction, the locals the
// code from there may read before writing them, as a backward dataflow
// over the instructions, each a branch to its A, a fall to the next, or
// both. Only the instructions describe takes reach here; of those, the
// ones below name locals, in A and B, and no other reads or writes one by
// index -- a host operation runs the instruction itself, so a local it
// names is read as the instruction reads it. describe takes no exception
// handler: one that it comes to take must be a successor here of every
// instruction it guards, or a catch would find its locals unwritten.
func liveLocals(fn *bytecode.Function, p *ir.Program, effects []effect) {
	n := fn.LocalCount
	if n == 0 {
		return
	}
	words := (n + 63) / 64
	live := make([]uint64, len(fn.Code)*words)
	at := func(pc int) []uint64 { return live[pc*words : (pc+1)*words] }
	read := func(s []uint64, i uint32) { s[i/64] |= 1 << (i % 64) }
	kill := func(s []uint64, i uint32) { s[i/64] &^= 1 << (i % 64) }
	in := make([]uint64, words)
	for changed := true; changed; {
		changed = false
		for pc := len(fn.Code) - 1; pc >= 0; pc-- {
			if p.Maps[pc].Depth < 0 {
				continue
			}
			clear(in)
			e, ins := effects[pc], fn.Code[pc]
			if !e.terminal && pc+1 < len(fn.Code) {
				for w, x := range at(pc + 1) {
					in[w] |= x
				}
			}
			if e.branch {
				for w, x := range at(e.target(ins)) {
					in[w] |= x
				}
			}
			switch ins.Op {
			case bytecode.OpSetLocal, bytecode.OpInitLocal, bytecode.OpPutLocal, bytecode.OpClearLocal:
				kill(in, ins.A)
			case bytecode.OpSetLocalGet:
				// The write, then the read, which sees it if they are one.
				kill(in, ins.A)
				if ins.B != ins.A {
					read(in, ins.B)
				}
			case bytecode.OpGetLocal, bytecode.OpGetLocalCheck, bytecode.OpSetLocalCheck, bytecode.OpIncLocal,
				bytecode.OpDecLocal, bytecode.OpUpdateLocal, bytecode.OpBinLocal:
				// set_local_check reads the binding first: its dead zone.
				read(in, ins.A)
			case bytecode.OpGetLocal2, bytecode.OpGetLocalIndex:
				read(in, ins.A)
				read(in, ins.B)
			case bytecode.OpGetLocalIndexUpdate:
				read(in, ins.A)
				read(in, ins.B>>2)
			case bytecode.OpLocalBinImm:
				read(in, ins.A&(1<<24-1))
			}
			if s := at(pc); !slices.Equal(s, in) {
				copy(s, in)
				changed = true
			}
		}
	}
	p.Live, p.LiveLocals, p.LiveWords = live, n, words
}

type effect struct {
	need, delta int
	branch      bool
	keep        bool
	terminal    bool
	// targetB says the branch's target is the instruction's B, not its A.
	targetB bool
	// jumps says the branch's target has jump operands more than the
	// instruction had, rather than as many as after it (or, keep, before).
	jumps bool
	jump  int
}

// StartOnly reports whether fn's code is entered at its first instruction
// only at its start: no branch of it goes there, nor anywhere Go goes on
// (goBranch). Its code then makes the frame's locals past its parameters
// undefined at its start (ssa.Func's StartOnly), which its native callers
// leave to it, as V8's callee makes its own frame: each call had written
// every one, the callee's every other local an immediate pair at each of
// its calls.
func StartOnly(fn *bytecode.Function) bool {
	for pc, in := range fn.Code {
		e, err := describe(fn, pc, in)
		if err != nil {
			return false
		}
		if e.branch && e.target(in) == 0 {
			return false
		}
		if to, ok := goBranch(in); ok && to == 0 {
			return false
		}
	}
	return true
}

// goBranch is where Go may go on at, past in, an instruction it runs
// (jitHost) and that may jump: a loop's end, where its cursor is done; a
// delegation's; an await using scope's disposal's, where nothing is left
// to await. Native code has an entry there (ir.Program's Entries).
func goBranch(in bytecode.Instr) (int, bool) {
	switch in.Op {
	case bytecode.OpIterNextOrJump, bytecode.OpIterUnpackDelegate:
		return int(in.A), true
	case bytecode.OpIterResume:
		return int(in.B), true
	case bytecode.OpDispose:
		if in.B&bytecode.DisposeStep != 0 && in.B&bytecode.DisposeFinish == 0 {
			return int(in.A), true
		}
	}
	return 0, false
}

// target is the instruction in's branch target, where e says it branches.
func (e effect) target(in bytecode.Instr) int {
	if e.targetB {
		return int(in.B)
	}
	return int(in.A)
}

func describe(fn *bytecode.Function, pc int, in bytecode.Instr) (effect, error) {
	bad := func(why string) (effect, error) { return effect{}, refuse(pc, why) }
	local := func(index uint32) bool { return uint64(index) < uint64(fn.LocalCount) }
	switch in.Op {
	case bytecode.OpGetLocal, bytecode.OpSetLocal, bytecode.OpPutLocal,
		bytecode.OpInitLocal, bytecode.OpGetLocalCheck, bytecode.OpSetLocalCheck,
		bytecode.OpClearLocal, bytecode.OpIncLocal, bytecode.OpDecLocal,
		bytecode.OpUpdateLocal, bytecode.OpBinLocal:
		if !local(in.A) {
			return bad("local index out of bounds")
		}
	case bytecode.OpGetLocal2, bytecode.OpSetLocalGet, bytecode.OpGetLocalIndex:
		if !local(in.A) || !local(in.B) {
			return bad("local index out of bounds")
		}
	case bytecode.OpLocalBinImm:
		if !local(in.A & (1<<24 - 1)) {
			return bad("local index out of bounds")
		}
	}
	if in.Op == bytecode.OpGetLocalIndexUpdate && (!local(in.A) || !local(in.B>>2)) {
		return bad("local index out of bounds")
	}
	switch in.Op {
	case bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck, bytecode.OpSetUpvalue, bytecode.OpSetUpvalueCheck, bytecode.OpInitUpvalue:
		if uint64(in.A) >= uint64(len(fn.Upvalues)) {
			return bad("upvalue index out of bounds")
		}
	}
	switch in.Op {
	case bytecode.OpNewArray:
		if in.A > MaxSlots {
			return bad("array literal budget")
		}
		return effect{need: int(in.A), delta: 1 - int(in.A)}, nil
	case bytecode.OpConcat:
		// The parts of a template or of a string chain, joined into one.
		if in.A == 0 || in.A > MaxSlots {
			return bad("concat part count")
		}
		return effect{need: int(in.A), delta: 1 - int(in.A)}, nil
	case bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck, bytecode.OpGetLocalIndex, bytecode.OpGetLocalIndexUpdate:
		return effect{delta: 1}, nil
	case bytecode.OpSetUpvalue, bytecode.OpSetUpvalueCheck, bytecode.OpInitUpvalue:
		return effect{need: 1, delta: -1}, nil
	case bytecode.OpGetIndex:
		return effect{need: 2, delta: -1}, nil
	case bytecode.OpGetIndexThis:
		// The element, past the object, its receiver: a method called by
		// its index, as Emscripten's code calls through its function
		// table, __FUNCTION_TABLE__[i](...).
		return effect{need: 2}, nil
	case bytecode.OpSetIndex:
		return effect{need: 3, delta: -3}, nil
	case bytecode.OpGetLength:
		return effect{need: 1}, nil
	case bytecode.OpToPropertyKeyOfBase:
		return effect{need: 2}, nil
	case bytecode.OpInsert2:
		return effect{need: 2, delta: 1}, nil
	case bytecode.OpInsert3:
		return effect{need: 3, delta: 1}, nil
	case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew, bytecode.OpTailCall, bytecode.OpTailCallMethod:
		if in.A > MaxSlots {
			return bad("argument budget")
		}
		n := int(in.A) + 1
		if in.Op == bytecode.OpCallMethod || in.Op == bytecode.OpTailCallMethod {
			n++
		}
		return effect{need: n, delta: 1 - n}, nil
	case bytecode.OpGetGlobal, bytecode.OpGetPropThis, bytecode.OpGetProp, bytecode.OpSetProp, bytecode.OpSetGlobal, bytecode.OpSetGlobalStrict,
		bytecode.OpCheckGlobalRef:
		if uint64(in.A) >= uint64(len(fn.Names)) || in.B == 0 || in.B > fn.PropSites {
			return bad("invalid property site")
		}
		if in.Op == bytecode.OpSetProp {
			return effect{need: 2, delta: -2}, nil
		}
		if in.Op == bytecode.OpSetGlobal || in.Op == bytecode.OpSetGlobalStrict {
			return effect{need: 1, delta: -1}, nil
		}
		if in.Op == bytecode.OpGetPropThis {
			return effect{need: 1, delta: 1}, nil
		}
		if in.Op == bytecode.OpGetProp {
			return effect{need: 1}, nil
		}
		return effect{delta: 1}, nil
	case bytecode.OpPushThis, bytecode.OpPushEmptyString:
		return effect{delta: 1}, nil
	case bytecode.OpGetPrivate, bytecode.OpSetPrivate, bytecode.OpPrivateIn,
		bytecode.OpDefinePrivate, bytecode.OpDefinePrivateMethod, bytecode.OpDefinePrivateGetter, bytecode.OpDefinePrivateSetter,
		bytecode.OpAddPrivateMethod, bytecode.OpAddPrivateGetter, bytecode.OpAddPrivateSetter:
		// A is the name; B where its key is: a local, or, its low bit
		// set, a captured binding.
		if uint64(in.A) >= uint64(len(fn.Names)) || in.B&1 == 0 && !local(in.B>>1) || in.B&1 != 0 && uint64(in.B>>1) >= uint64(len(fn.Upvalues)) {
			return bad("invalid private name")
		}
		switch in.Op {
		case bytecode.OpGetPrivate, bytecode.OpPrivateIn:
			return effect{need: 1}, nil
		case bytecode.OpSetPrivate, bytecode.OpAddPrivateMethod, bytecode.OpAddPrivateGetter, bytecode.OpAddPrivateSetter:
			return effect{need: 2, delta: -2}, nil
		}
		// A definition: the value popped, its object kept.
		return effect{need: 2, delta: -1}, nil
	case bytecode.OpPrivateName, bytecode.OpNewPrivateMethods:
		if uint64(in.A) >= uint64(len(fn.Names)) {
			return bad("invalid name")
		}
		return effect{delta: 1}, nil
	case bytecode.OpNewClass, bytecode.OpSetFieldInit:
		// The constructor kept, its parent or its fields' initializer
		// popped.
		return effect{need: 2, delta: -1}, nil
	case bytecode.OpSetHomeObject:
		// The method, its home object A slots below it.
		if in.A > MaxSlots {
			return bad("invalid home object")
		}
		return effect{need: int(in.A) + 1}, nil
	case bytecode.OpDefineMethod, bytecode.OpDefineGetter, bytecode.OpDefineSetter:
		// The function popped, its object kept; A the name.
		if uint64(in.A) >= uint64(len(fn.Names)) {
			return bad("invalid name")
		}
		return effect{need: 2, delta: -1}, nil
	case bytecode.OpDefineIndex, bytecode.OpDefineGetterIndex, bytecode.OpDefineSetterIndex:
		// The key and the value popped, the object kept.
		return effect{need: 3, delta: -2}, nil
	case bytecode.OpSetFuncName:
		// The function named after the key below it.
		return effect{need: 2}, nil
	case bytecode.OpToPropertyKey:
		return effect{need: 1}, nil
	case bytecode.OpInstallPrivateMethods:
		// The class's private methods, popped, onto the receiver.
		return effect{need: 1, delta: -1}, nil
	case bytecode.OpGetGlobalOpt:
		// typeof's read of a name, which has no site of its own.
		if uint64(in.A) >= uint64(len(fn.Names)) {
			return bad("invalid name")
		}
		return effect{delta: 1}, nil
	case bytecode.OpAssertResolved, bytecode.OpDefineField:
		if uint64(in.A) >= uint64(len(fn.Names)) {
			return bad("invalid name")
		}
		return effect{need: 2, delta: -1}, nil
	case bytecode.OpNewObject:
		if in.B == 0 || in.B > fn.PropSites {
			return bad("invalid literal site")
		}
		return effect{delta: 1}, nil
	case bytecode.OpNewRegExp:
		if uint64(in.A) >= uint64(len(fn.Constants)) || fn.Constants[in.A].Kind != bytecode.ConstRegExp {
			return bad("invalid regular expression literal")
		}
		return effect{delta: 1}, nil
	case bytecode.OpNop, bytecode.OpEndParams, bytecode.OpClearLocal:
		return effect{}, nil
	case bytecode.OpPushConst:
		if uint64(in.A) >= uint64(len(fn.Constants)) {
			return bad("non-number or invalid constant")
		}
		if k := fn.Constants[in.A].Kind; k != bytecode.ConstNumber && k != bytecode.ConstString && k != bytecode.ConstBigInt {
			return bad("non-number or invalid constant")
		}
		return effect{delta: 1}, nil
	case bytecode.OpPushInt, bytecode.OpPushUndef, bytecode.OpPushNull,
		bytecode.OpPushTrue, bytecode.OpPushFalse, bytecode.OpPushUninitialized,
		bytecode.OpGetLocal, bytecode.OpGetLocalCheck:
		return effect{delta: 1}, nil
	case bytecode.OpGetLocal2:
		return effect{delta: 2}, nil
	case bytecode.OpSetLocal, bytecode.OpInitLocal:
		return effect{need: 1, delta: -1}, nil
	case bytecode.OpSetLocalCheck:
		if !fn.Locals[in.A].Mutable {
			return bad("assignment to immutable local")
		}
		return effect{need: 1, delta: -1}, nil
	case bytecode.OpPutLocal, bytecode.OpSetLocalGet, bytecode.OpSwap:
		need := 1
		if in.Op == bytecode.OpSwap {
			need = 2
		}
		return effect{need: need}, nil
	case bytecode.OpDup:
		return effect{need: 1, delta: 1}, nil
	case bytecode.OpDup2:
		return effect{need: 2, delta: 2}, nil
	case bytecode.OpDrop:
		return effect{need: 1, delta: -1}, nil
	case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
		bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod, bytecode.OpPow,
		bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
		bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
		return effect{need: 2, delta: -1}, nil
	case bytecode.OpNeg, bytecode.OpPos, bytecode.OpInc, bytecode.OpDec,
		bytecode.OpToNumber, bytecode.OpToNumeric, bytecode.OpNot, bytecode.OpBitNot:
		return effect{need: 1}, nil
	case bytecode.OpIncLocal, bytecode.OpDecLocal:
		return effect{}, nil
	case bytecode.OpUpdateLocal:
		if in.B & ^uint32(bytecode.UpdateDec|bytecode.UpdatePostfix) != 0 {
			return bad("invalid update flags")
		}
		return effect{delta: 1}, nil
	case bytecode.OpBinImm, bytecode.OpBinLocal, bytecode.OpLocalBinImm:
		raw := in.B
		if in.Op == bytecode.OpLocalBinImm {
			raw = in.A >> 24
		}
		op, ok := operator(raw)
		if raw != uint32(bytecode.OpMod) && (!ok || op > ir.Mul && (op < ir.BitAnd || op > ir.UShr)) {
			return bad("unsupported fused arithmetic operator")
		}
		if in.Op == bytecode.OpLocalBinImm {
			return effect{delta: 1}, nil
		}
		return effect{need: 1}, nil
	case bytecode.OpJumpIfNullish, bytecode.OpJumpIfNotNullish:
		// ?.: the value kept on either path; ??: kept where it jumps, the
		// value not nullish, and popped where it goes on to the default.
		if uint64(in.A) >= uint64(len(fn.Code)) {
			return bad("branch target out of bounds")
		}
		if in.Op == bytecode.OpJumpIfNotNullish {
			return effect{branch: true, need: 1, delta: -1, keep: true}, nil
		}
		return effect{branch: true, need: 1}, nil
	case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue,
		bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfCmpFalse:
		if uint64(in.A) >= uint64(len(fn.Code)) {
			return bad("branch target out of bounds")
		}
		e := effect{branch: true, need: 1, delta: -1}
		if in.Op == bytecode.OpJump {
			e.need, e.delta, e.terminal = 0, 0, true
		} else if in.Op == bytecode.OpJumpIfFalseKeep || in.Op == bytecode.OpJumpIfTrueKeep {
			e.keep = true
		} else if in.Op == bytecode.OpJumpIfCmpFalse {
			op, ok := operator(in.B)
			if !ok || op < ir.Lt || op > ir.Ne {
				return bad("unsupported fused comparison operator")
			}
			e.need, e.delta = 2, -2
		}
		return e, nil
	case bytecode.OpReturn:
		return effect{need: 1, delta: -1, terminal: true}, nil
	case bytecode.OpReturnUndef:
		return effect{terminal: true}, nil
	case bytecode.OpThrow:
		return effect{need: 1, delta: -1, terminal: true}, nil
	case bytecode.OpInstanceOf, bytecode.OpIn, bytecode.OpDeleteProp:
		return effect{need: 2, delta: -1}, nil
	case bytecode.OpTypeOf:
		return effect{need: 1}, nil
	case bytecode.OpPushCatch, bytecode.OpPushFinally:
		if uint64(in.A) >= uint64(len(fn.Code)) {
			return bad("handler out of bounds")
		}
		return effect{}, nil
	case bytecode.OpPopCatch:
		return effect{}, nil
	case bytecode.OpRethrow:
		// A finally clause's completion record: its value, then its kind.
		return effect{need: 2, delta: -2}, nil
	case bytecode.OpForOfStart, bytecode.OpForInStart:
		// The iterable popped, its loop's cursor pushed.
		return effect{need: 1}, nil
	case bytecode.OpSuperCall:
		// A derived constructor's super(...): its arguments popped -- the A
		// on the stack, or an array of them -- and the this it binds
		// pushed.
		if fn.Kind != bytecode.KindDerivedConstructor {
			return bad("super() outside a derived constructor")
		}
		switch in.B {
		case bytecode.SuperArgs:
			return effect{need: int(in.A), delta: 1 - int(in.A)}, nil
		case bytecode.SuperSpread:
			return effect{need: 1}, nil
		case bytecode.SuperForward:
			return effect{delta: 1}, nil
		}
		return bad("invalid super call")
	case bytecode.OpIterNextOrJump:
		// The cursor kept; its next value pushed, or, done, a jump to A,
		// which Go takes (jitHost).
		if uint64(in.A) >= uint64(len(fn.Code)) {
			return bad("branch target out of bounds")
		}
		return effect{need: 1, delta: 1, branch: true, keep: true}, nil
	case bytecode.OpIterClose:
		return effect{need: 1}, nil
	case bytecode.OpIterStep, bytecode.OpIterRest:
		// A destructuring's: the cursor, A below the top, kept; its next
		// value, or what is left, pushed.
		if in.A > MaxSlots {
			return bad("invalid cursor")
		}
		return effect{need: int(in.A) + 1, delta: 1}, nil
	case bytecode.OpIterResume:
		// yield*'s step of its delegate: the cursor, the value sent and
		// the kind of resumption, for the delegate's result -- with which,
		// the delegation ended, Go jumps to B.
		if !fn.Generator || uint64(in.B) >= uint64(len(fn.Code)) {
			return bad("invalid delegation")
		}
		return effect{need: 3, delta: -1, branch: true, targetB: true}, nil
	case bytecode.OpIterUnpackDelegate:
		// The delegate's result for what yield* hands out, or, done, its
		// value, with which Go jumps to A.
		if !fn.Generator || uint64(in.A) >= uint64(len(fn.Code)) {
			return bad("invalid delegation")
		}
		return effect{need: 1, branch: true}, nil
	case bytecode.OpYieldStar:
		// The value handed out; a resumption sends a value and its kind.
		if !fn.Generator {
			return bad("suspension outside a generator")
		}
		return effect{need: 1, delta: 1}, nil
	case bytecode.OpImportMeta:
		return effect{delta: 1}, nil
	case bytecode.OpClosure:
		if uint64(in.A) >= uint64(len(fn.Constants)) || fn.Constants[in.A].Kind != bytecode.ConstFunction {
			return bad("invalid closure template")
		}
		return effect{delta: 1}, nil
	case bytecode.OpCloseUpvalues:
		if uint64(in.A) > uint64(fn.LocalCount) {
			return bad("local index out of bounds")
		}
		return effect{}, nil
	case bytecode.OpImport:
		// The specifier, and with B 2 the options, for the import's
		// promise.
		if in.B != 1 && in.B != 2 {
			return bad("invalid import")
		}
		return effect{need: int(in.B), delta: 1 - int(in.B)}, nil
	case bytecode.OpNipUnder:
		// The top stays; the A beneath it go.
		if in.A > MaxSlots {
			return bad("invalid nip")
		}
		return effect{need: int(in.A) + 1, delta: -int(in.A)}, nil
	case bytecode.OpIterCloseNormal:
		return effect{need: 1, delta: -1}, nil
	case bytecode.OpNewDisposeCapability:
		return effect{delta: 1}, nil
	case bytecode.OpAddDisposable:
		// The capability popped, the resource beneath it kept.
		return effect{need: 2, delta: -1}, nil
	case bytecode.OpDispose:
		// The capability popped; with a record, a finally clause's,
		// beneath it, read; with DisposeAsync, the disposal's promise
		// pushed. A step of an await using scope's disposal, which jumps
		// where Go says, is the tree tier's.
		if in.B&bytecode.DisposeStep != 0 {
			// A step of an await using scope's disposal: the capability
			// popped, and with DisposeFail what the await before it threw;
			// with DisposeStart and DisposeRecord a finally clause's record
			// beneath read. A value to await pushed; or, none left, a jump
			// to A, which Go takes (jitHost), but at the end.
			if !fn.Async {
				return bad("invalid disposal step")
			}
			e := effect{need: 1}
			if in.B&bytecode.DisposeFail != 0 {
				e.need, e.delta = 2, -1
			} else if in.B&bytecode.DisposeStart != 0 && in.B&bytecode.DisposeRecord != 0 {
				e.need = 3
			}
			if in.B&bytecode.DisposeFinish != 0 {
				e.delta--
				return e, nil
			}
			if uint64(in.A) >= uint64(len(fn.Code)) {
				return bad("branch target out of bounds")
			}
			e.branch, e.jumps, e.jump = true, true, e.delta-1
			return e, nil
		}
		if in.B != 0 {
			return bad("unsupported opcode " + in.Op.String())
		}
		e := effect{need: 1, delta: -1}
		if in.A&bytecode.DisposeRecord != 0 {
			e.need = 3
		}
		if in.A&bytecode.DisposeAsync != 0 {
			e.delta = 0
		}
		return e, nil
	case bytecode.OpYield, bytecode.OpAwait:
		// The value handed out, and the one a resumption sends in its
		// place.
		if !fn.Generator && !fn.Async {
			return bad("suspension outside a generator")
		}
		return effect{need: 1}, nil
	case bytecode.OpApplyArguments:
		// f, apply and the receiver, for f's result.
		if !local(in.A) {
			return bad("invalid arguments slot")
		}
		return effect{need: 3, delta: -2}, nil
	case bytecode.OpArgumentsIndex:
		// The key, for the element.
		if !local(in.A) {
			return bad("invalid arguments slot")
		}
		return effect{need: 1}, nil
	case bytecode.OpCheckGlobalVar, bytecode.OpCheckGlobalLex, bytecode.OpDefineGlobalVar, bytecode.OpDeclareGlobalLex, bytecode.OpDeclareModuleLex:
		// A top-level declaration's: its check, or its binding made.
		if !fn.TopLevel {
			return bad("declaration outside top-level code")
		}
		return effect{}, nil
	case bytecode.OpDefineGlobalFunc, bytecode.OpInitGlobalLex, bytecode.OpInitModuleLex:
		// The function, or the binding's first value, popped.
		if !fn.TopLevel {
			return bad("declaration outside top-level code")
		}
		return effect{need: 1, delta: -1}, nil
	}
	return bad("unsupported opcode " + in.Op.String())
}

// isPrivate reports whether op is a private member's (bytecode's private
// class members), which only the new pipeline compiles.
func isPrivate(op bytecode.Op) bool {
	switch op {
	case bytecode.OpGetPrivate, bytecode.OpSetPrivate, bytecode.OpPrivateIn,
		bytecode.OpDefinePrivate, bytecode.OpDefinePrivateMethod, bytecode.OpDefinePrivateGetter, bytecode.OpDefinePrivateSetter,
		bytecode.OpAddPrivateMethod, bytecode.OpAddPrivateGetter, bytecode.OpAddPrivateSetter,
		bytecode.OpPrivateName, bytecode.OpNewPrivateMethods, bytecode.OpInstallPrivateMethods:
		return true
	}
	return false
}

// isDefinition reports whether op makes a class's or an object literal's
// member, which only the new pipeline compiles, as Go's (jitHost).
func isDefinition(op bytecode.Op) bool {
	switch op {
	case bytecode.OpNewClass, bytecode.OpSetHomeObject, bytecode.OpSetFieldInit, bytecode.OpDefineMethod,
		bytecode.OpDefineIndex, bytecode.OpDefineGetter, bytecode.OpDefineSetter, bytecode.OpSetFuncName,
		bytecode.OpDefineGetterIndex, bytecode.OpDefineSetterIndex, bytecode.OpToPropertyKey:
		return true
	}
	return false
}

// privateKeySlot is the slot of the binding a private instruction's B
// names, which holds its key: a local, or, its low bit set, a captured
// binding, whose slots follow the locals.
func privateKeySlot(fn *bytecode.Function, ref uint32) ir.Operand {
	if ref&1 != 0 {
		return ir.Slot(fn.LocalCount + int(ref>>1))
	}
	return ir.Slot(int(ref >> 1))
}

// strictOp reports whether a raw comparison opcode is === or !==.
func strictOp(raw uint32) bool {
	return raw == uint32(bytecode.OpStrictEq) || raw == uint32(bytecode.OpStrictNe)
}

func operator(raw uint32) (ir.Operator, bool) {
	switch raw {
	case uint32(bytecode.OpBitAnd):
		return ir.BitAnd, true
	case uint32(bytecode.OpBitOr):
		return ir.BitOr, true
	case uint32(bytecode.OpBitXor):
		return ir.BitXor, true
	case uint32(bytecode.OpShl):
		return ir.Shl, true
	case uint32(bytecode.OpShr):
		return ir.Shr, true
	case uint32(bytecode.OpUShr):
		return ir.UShr, true
	case uint32(bytecode.OpAdd):
		return ir.Add, true
	case uint32(bytecode.OpSub):
		return ir.Sub, true
	case uint32(bytecode.OpMul):
		return ir.Mul, true
	case uint32(bytecode.OpDiv):
		return ir.Div, true
	case uint32(bytecode.OpLt):
		return ir.Lt, true
	case uint32(bytecode.OpLe):
		return ir.Le, true
	case uint32(bytecode.OpGt):
		return ir.Gt, true
	case uint32(bytecode.OpGe):
		return ir.Ge, true
	case uint32(bytecode.OpEq), uint32(bytecode.OpStrictEq):
		return ir.Eq, true
	case uint32(bytecode.OpNe), uint32(bytecode.OpStrictNe):
		return ir.Ne, true
	}
	return 0, false
}

// lowerMod is a remainder's instruction, in each form the bytecode has it:
// the operator, with a local, with an immediate, and a local's with an
// immediate.
func lowerMod(in bytecode.Instr, sp int) (ir.Instruction, bool) {
	mod := uint32(bytecode.OpMod)
	top := ir.Slot(sp - 1)
	switch {
	case in.Op == bytecode.OpMod:
		return ir.Instruction{Op: ir.Binary, Operator: ir.Mod, Dest: sp - 2, Left: ir.Slot(sp - 2), Right: top}, true
	case in.Op == bytecode.OpBinLocal && in.B == mod:
		return ir.Instruction{Op: ir.Binary, Operator: ir.Mod, Dest: sp - 1, Left: top, Right: ir.Slot(int(in.A))}, true
	case in.Op == bytecode.OpBinImm && in.B == mod:
		return ir.Instruction{Op: ir.Binary, Operator: ir.Mod, Dest: sp - 1, Left: top, Right: ir.Literal(ir.Float(float64(int32(in.A))))}, true
	case in.Op == bytecode.OpLocalBinImm && in.A>>24 == mod:
		return ir.Instruction{Op: ir.Binary, Operator: ir.Mod, Dest: sp, Left: ir.Slot(int(in.A & (1<<24 - 1))), Right: ir.Literal(ir.Float(float64(int32(in.B))))}, true
	}
	return ir.Instruction{}, false
}

// lower is instruction in of fn, sp its stack's top past the locals; this
// whether the receiver has a slot, and charCode whether in is a call of
// String.prototype.charCodeAt's (charCodeCalls).
func lower(fn *bytecode.Function, in bytecode.Instr, sp int, this, charCode bool) ir.Instruction {
	copyTo := func(dest int, source ir.Operand) ir.Instruction {
		return ir.Instruction{Op: ir.Copy, Dest: dest, Left: source}
	}
	number := func(n int32) ir.Operand { return ir.Literal(ir.Float(float64(n))) }
	top := ir.Slot(sp - 1)
	switch in.Op {
	case bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck:
		slot := fn.LocalCount + int(in.A)
		out := copyTo(sp, ir.Slot(slot))
		out.Check, out.CheckSlot = in.Op == bytecode.OpGetUpvalueCheck, slot
		return out
	case bytecode.OpGetIndex:
		return ir.Instruction{Op: ir.ArrayRead, Dest: sp - 2, Left: ir.Slot(sp - 2), Right: top}
	case bytecode.OpGetIndexThis:
		return ir.Instruction{Op: ir.ArrayRead, Dest: sp - 1, Left: ir.Slot(sp - 2), Right: top}
	case bytecode.OpGetLocalIndex:
		return ir.Instruction{Op: ir.ArrayRead, Dest: sp, Left: ir.Slot(int(in.A)), Right: ir.Slot(int(in.B))}
	case bytecode.OpGetLocalIndexUpdate:
		return ir.Instruction{Op: ir.ArrayUpdate, Dest: sp, Extra: int(in.B >> 2), Left: ir.Slot(int(in.A)), Right: ir.Slot(int(in.B >> 2)), Postfix: in.B&bytecode.UpdatePostfix != 0, Operator: ir.Operator(in.B & bytecode.UpdateDec)}
	case bytecode.OpSetIndex:
		return ir.Instruction{Op: ir.ArrayWrite, Left: ir.Slot(sp - 3), Right: ir.Slot(sp - 2), Third: top}
	case bytecode.OpGetLength:
		return ir.Instruction{Op: ir.ArrayLength, Left: top, Dest: sp - 1}
	case bytecode.OpToPropertyKeyOfBase:
		return ir.Instruction{Op: ir.ArrayKey, Left: ir.Slot(sp - 2), Right: top}
	case bytecode.OpInsert2:
		return ir.Instruction{Op: ir.Insert2, Dest: sp - 2}
	case bytecode.OpInsert3:
		return ir.Instruction{Op: ir.Insert3, Dest: sp - 3}
	case bytecode.OpCallMethod:
		if charCode && stringKernel(fn) {
			return ir.Instruction{Op: ir.StringCode, Left: ir.Slot(sp - 2), Right: ir.Slot(sp - 3), Third: top, Dest: sp - 3}
		}
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpGetPropThis:
		if fn.Names[in.A] == "charCodeAt" && stringKernel(fn) {
			return ir.Instruction{Op: ir.StringMethod, Left: top, Dest: sp}
		}
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpNewArray, bytecode.OpCall, bytecode.OpMod, bytecode.OpPow, bytecode.OpNew:
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpGetPrivate:
		// A property read, of the key the binding holds (Private).
		return ir.Instruction{Op: ir.PropertyRead, Left: top, Dest: sp - 1, Key: in.A, Third: privateKeySlot(fn, in.B), Private: true}
	case bytecode.OpSetPrivate:
		return ir.Instruction{Op: ir.PropertyWrite, Left: ir.Slot(sp - 2), Right: top, Key: in.A, Third: privateKeySlot(fn, in.B), Private: true}
	case bytecode.OpPrivateIn:
		// #x in o: a read of whether it is there (When).
		return ir.Instruction{Op: ir.PropertyRead, Left: top, Dest: sp - 1, Key: in.A, Third: privateKeySlot(fn, in.B), Private: true, When: true}
	case bytecode.OpDefinePrivate, bytecode.OpDefinePrivateMethod, bytecode.OpDefinePrivateGetter,
		bytecode.OpDefinePrivateSetter, bytecode.OpAddPrivateMethod, bytecode.OpAddPrivateGetter, bytecode.OpAddPrivateSetter,
		bytecode.OpPrivateName, bytecode.OpNewPrivateMethods, bytecode.OpInstallPrivateMethods:
		// Go's (jitHost): a class's members made and given to an
		// instance as it is constructed, and #x in o.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpNewClass, bytecode.OpSetHomeObject, bytecode.OpSetFieldInit, bytecode.OpDefineMethod,
		bytecode.OpDefineIndex, bytecode.OpDefineGetter, bytecode.OpDefineSetter, bytecode.OpSetFuncName,
		bytecode.OpDefineGetterIndex, bytecode.OpDefineSetterIndex, bytecode.OpToPropertyKey:
		// Go's (jitHost): a class or an object literal's members made.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpTailCall, bytecode.OpTailCallMethod:
		// A call, made as an ordinary one where the stack is shallow, as
		// the interpreter makes it (tailCallDepth).
		return ir.Instruction{Op: ir.Host, Tail: true, Extra: int(in.A), When: in.Op == bytecode.OpTailCallMethod}
	case bytecode.OpConcat:
		// Go converts the parts and joins them, as the interpreter does.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpSetGlobal, bytecode.OpSetGlobalStrict:
		// Native where the VM knows the binding (ssa's), Go's otherwise.
		return ir.Instruction{Op: ir.BindingWrite, Left: top, Key: in.A}
	case bytecode.OpCheckGlobalRef:
		return ir.Instruction{Op: ir.BindingCheck, Dest: sp, Key: in.A}
	case bytecode.OpAssertResolved:
		return ir.Instruction{Op: ir.Resolved, Left: ir.Slot(sp - 2), Right: top, Dest: sp - 2, Key: in.A}
	case bytecode.OpNewObject, bytecode.OpNewRegExp:
		// A regular expression literal's object is made as an object
		// literal's is, from a pool the VM fills with its pattern's.
		return ir.Instruction{Op: ir.ObjectLiteral, Dest: sp}
	case bytecode.OpDefineField:
		return ir.Instruction{Op: ir.FieldDefine, Left: ir.Slot(sp - 2), Right: top, Key: in.A}
	case bytecode.OpGetGlobal, bytecode.OpGetGlobalOpt:
		// typeof's read is a read where the name is there, Go's where it
		// is not.
		return ir.Instruction{Op: ir.BindingRead, Left: ir.Literal(ir.Value{Kind: ir.Opaque}), Dest: sp, Key: in.A}
	case bytecode.OpPushThis:
		if !this {
			return ir.Instruction{Op: ir.Host}
		}
		slot := fn.LocalCount + len(fn.Upvalues)
		return ir.Instruction{Op: ir.Copy, Left: ir.Slot(slot), Dest: sp, Check: true, CheckSlot: slot}
	case bytecode.OpGetProp:
		return ir.Instruction{Op: ir.PropertyRead, Left: top, Dest: sp - 1, Key: in.A}
	case bytecode.OpSetProp:
		return ir.Instruction{Op: ir.PropertyWrite, Left: ir.Slot(sp - 2), Right: top, Key: in.A}
	case bytecode.OpThrow, bytecode.OpPushEmptyString, bytecode.OpSetUpvalue, bytecode.OpSetUpvalueCheck, bytecode.OpInitUpvalue:
		// The interpreter throws it (jitHost leaves it); Go pushes '' and
		// assigns a captured binding, which the new pipeline does itself.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpInstanceOf:
		// Made natively where the VM knows the constructor
		// (ssa.InstanceOfSite), by Go otherwise.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpIn:
		// key in obj, the key below the object: Go's, called from native
		// code where that runs nothing (ssa.GoHas).
		return ir.Instruction{Op: ir.HasProperty, Left: ir.Slot(sp - 2), Right: top, Dest: sp - 2}
	case bytecode.OpDeleteProp:
		// delete obj[key], the key above the object: Go's, called from
		// native code where that runs nothing (ssa.GoDelete), and strict
		// as the function is, whose deletion of what cannot be deleted
		// throws.
		return ir.Instruction{Op: ir.DeleteProperty, Left: ir.Slot(sp - 2), Right: top, Dest: sp - 2, Strict: fn.Strict}
	case bytecode.OpTypeOf:
		// Go's, but compared with a string constant (typeTests).
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpSuperCall:
		// Go's, which binds the frame's this: from native code where it
		// can be (ssa.CallSite's Super), which the this slot takes.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpApplyArguments:
		// The inliner makes it a call (LowerSSAForward).
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpArgumentsIndex:
		// The inliner reads the call's argument where it can (LowerSSAForward).
		return ir.Instruction{Op: ir.ArgumentRead, Left: top, Dest: sp - 1}
	case bytecode.OpPushConst:
		if fn.Constants[in.A].Kind != bytecode.ConstNumber {
			// A string or a BigInt, which Go pushes.
			return ir.Instruction{Op: ir.Host}
		}
		return copyTo(sp, ir.Literal(ir.Float(fn.Constants[in.A].Num)))
	case bytecode.OpPushInt:
		return copyTo(sp, number(int32(in.A)))
	case bytecode.OpPushUndef, bytecode.OpPushNull, bytecode.OpPushUninitialized, bytecode.OpPushTrue, bytecode.OpPushFalse:
		v := ir.Value{Kind: ir.Undefined}
		switch in.Op {
		case bytecode.OpPushNull:
			v.Kind = ir.Null
		case bytecode.OpPushUninitialized:
			v.Kind = ir.Uninitialized
		case bytecode.OpPushTrue:
			v = ir.Bool(true)
		case bytecode.OpPushFalse:
			v = ir.Bool(false)
		}
		return copyTo(sp, ir.Literal(v))
	case bytecode.OpGetLocal, bytecode.OpGetLocalCheck:
		out := copyTo(sp, ir.Slot(int(in.A)))
		out.Check, out.CheckSlot = in.Op == bytecode.OpGetLocalCheck, int(in.A)
		return out
	case bytecode.OpGetLocal2:
		return ir.Instruction{Op: ir.CopyPair, Dest: sp, Extra: sp + 1, Left: ir.Slot(int(in.A)), Right: ir.Slot(int(in.B))}
	case bytecode.OpSetLocal, bytecode.OpInitLocal, bytecode.OpPutLocal, bytecode.OpSetLocalCheck:
		out := copyTo(int(in.A), top)
		out.Check, out.CheckSlot = in.Op == bytecode.OpSetLocalCheck, int(in.A)
		return out
	case bytecode.OpSetLocalGet:
		return ir.Instruction{Op: ir.StoreLoad, Dest: int(in.A), Extra: sp - 1, Left: top, Right: ir.Slot(int(in.B))}
	case bytecode.OpClearLocal:
		return copyTo(int(in.A), ir.Literal(ir.Value{Kind: ir.Undefined}))
	case bytecode.OpDup:
		return copyTo(sp, top)
	case bytecode.OpDup2:
		return ir.Instruction{Op: ir.CopyPair, Dest: sp, Extra: sp + 1, Left: ir.Slot(sp - 2), Right: top}
	case bytecode.OpSwap:
		return ir.Instruction{Op: ir.Swap, Dest: sp - 2, Extra: sp - 1}
	case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
		bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv,
		bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
		bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
		op, _ := operator(uint32(in.Op))
		return ir.Instruction{Op: ir.Binary, Operator: op, Dest: sp - 2, Left: ir.Slot(sp - 2), Right: top, Strict: strictOp(uint32(in.Op))}
	case bytecode.OpBinLocal:
		if in.B == uint32(bytecode.OpMod) {
			return ir.Instruction{Op: ir.Host}
		}
		op, _ := operator(in.B)
		return ir.Instruction{Op: ir.Binary, Operator: op, Dest: sp - 1, Left: top, Right: ir.Slot(int(in.A)), Strict: strictOp(in.B)}
	case bytecode.OpBinImm:
		if in.B == uint32(bytecode.OpMod) {
			return ir.Instruction{Op: ir.Host}
		}
		if in.B == uint32(bytecode.OpBitOr) && in.A == 0 {
			return ir.Instruction{Op: ir.Unary, Operator: ir.Int32, Left: top, Dest: sp - 1}
		}
		op, _ := operator(in.B)
		return ir.Instruction{Op: ir.Binary, Operator: op, Dest: sp - 1, Left: top, Right: number(int32(in.A)), Strict: strictOp(in.B)}
	case bytecode.OpLocalBinImm:
		if in.A>>24 == uint32(bytecode.OpMod) {
			return ir.Instruction{Op: ir.Host}
		}
		if in.A>>24 == uint32(bytecode.OpBitOr) && in.B == 0 {
			return ir.Instruction{Op: ir.Unary, Operator: ir.Int32, Left: ir.Slot(int(in.A & (1<<24 - 1))), Dest: sp}
		}
		op, _ := operator(in.A >> 24)
		return ir.Instruction{Op: ir.Binary, Operator: op, Dest: sp, Left: ir.Slot(int(in.A & (1<<24 - 1))), Right: number(int32(in.B)), Strict: strictOp(in.A >> 24)}
	case bytecode.OpNeg, bytecode.OpPos, bytecode.OpToNumber, bytecode.OpToNumeric, bytecode.OpNot, bytecode.OpBitNot:
		op := ir.Pos
		if in.Op == bytecode.OpNeg {
			op = ir.Neg
		} else if in.Op == bytecode.OpBitNot {
			op = ir.BitNot
		} else if in.Op == bytecode.OpNot {
			op = ir.Not
		}
		return ir.Instruction{Op: ir.Unary, Operator: op, Dest: sp - 1, Left: top}
	case bytecode.OpInc, bytecode.OpDec, bytecode.OpIncLocal, bytecode.OpDecLocal, bytecode.OpUpdateLocal:
		dest, extra := sp-1, -1
		if in.Op == bytecode.OpIncLocal || in.Op == bytecode.OpDecLocal || in.Op == bytecode.OpUpdateLocal {
			dest = int(in.A)
		}
		if in.Op == bytecode.OpUpdateLocal {
			extra = sp
		}
		op := ir.Add
		if in.Op == bytecode.OpDec || in.Op == bytecode.OpDecLocal || in.Op == bytecode.OpUpdateLocal && in.B&bytecode.UpdateDec != 0 {
			op = ir.Sub
		}
		return ir.Instruction{Op: ir.Update, Operator: op, Dest: dest, Extra: extra, Left: ir.Slot(dest), Postfix: in.Op == bytecode.OpUpdateLocal && in.B&bytecode.UpdatePostfix != 0}
	case bytecode.OpJump:
		return ir.Instruction{Op: ir.Jump, Target: int(in.A)}
	case bytecode.OpJumpIfTrue, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfFalseKeep:
		return ir.Instruction{Op: ir.Branch, Operator: ir.Truth, Target: int(in.A), Left: top, When: in.Op == bytecode.OpJumpIfTrue || in.Op == bytecode.OpJumpIfTrueKeep}
	case bytecode.OpJumpIfNullish, bytecode.OpJumpIfNotNullish:
		return ir.Instruction{Op: ir.Branch, Operator: ir.Nullish, Target: int(in.A), Left: top, When: in.Op == bytecode.OpJumpIfNullish}
	case bytecode.OpJumpIfCmpFalse:
		op, _ := operator(in.B)
		return ir.Instruction{Op: ir.Branch, Operator: op, Target: int(in.A), Left: ir.Slot(sp - 2), Right: top, Strict: strictOp(in.B)}
	case bytecode.OpReturn:
		return ir.Instruction{Op: ir.Return, Left: top}
	case bytecode.OpReturnUndef:
		return ir.Instruction{Op: ir.Return, Left: ir.Literal(ir.Value{Kind: ir.Undefined})}
	case bytecode.OpNop, bytecode.OpEndParams, bytecode.OpDrop:
		// A pop writes no slot: the next map's depth is the whole effect.
		return ir.Instruction{Op: ir.Nop}
	case bytecode.OpPushCatch, bytecode.OpPushFinally, bytecode.OpPopCatch:
		// The handlers in force are known at each instruction (Program's
		// HandlerAt), and Go gives the frame them where native code
		// leaves it: native code registers none.
		return ir.Instruction{Op: ir.Nop}
	case bytecode.OpRethrow:
		return ir.Instruction{Op: ir.Rethrow, Left: top}
	case bytecode.OpYield, bytecode.OpAwait:
		// Go suspends the frame where native code leaves it, and a
		// resumption enters native code again after it.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpNewDisposeCapability, bytecode.OpAddDisposable, bytecode.OpDispose:
		// A using declaration's resources, Go's (jitHost).
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpIterNextOrJump:
		// A call of Go from native code where it can, which branches on
		// its answer; an exit otherwise.
		return ir.Instruction{Op: ir.IterStep, Left: top, Dest: sp, Target: int(in.A)}
	case bytecode.OpIterStep:
		// A destructuring's step, as a loop's, but going on with undefined
		// once done (When).
		return ir.Instruction{Op: ir.IterStep, Left: ir.Slot(sp - 1 - int(in.A)), Dest: sp, When: true}
	case bytecode.OpIterRest:
		// Go's (jitHost): the array of what is left.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpIterCloseNormal, bytecode.OpIterClose:
		// Native where there is nothing to close, Go's otherwise: a loop's
		// cursor done, or an array's walked in place, as its loop ends --
		// iter_close keeps the cursor, which the drop after it takes.
		return ir.Instruction{Op: ir.IterClose, Left: top}
	case bytecode.OpForOfStart, bytecode.OpForInStart,
		bytecode.OpIterResume, bytecode.OpIterUnpackDelegate, bytecode.OpYieldStar:
		// A for...of or for...in loop's cursor, or yield*'s delegate, Go's
		// (jitHost), which sets where native code goes on: past a step
		// that has a value, or, done, where the loop or the delegation
		// ends; and yield*'s suspension, as yield's.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpNipUnder:
		return copyTo(sp-1-int(in.A), top)
	case bytecode.OpImport, bytecode.OpImportMeta:
		// Go's (jitHost): a dynamic import's promise, the module's
		// import.meta.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpCheckGlobalVar, bytecode.OpCheckGlobalLex, bytecode.OpDefineGlobalVar, bytecode.OpDefineGlobalFunc,
		bytecode.OpDeclareGlobalLex, bytecode.OpInitGlobalLex, bytecode.OpDeclareModuleLex, bytecode.OpInitModuleLex:
		// The interpreter's (jitHost leaves it): top-level code's
		// declarations run before its loops, where native code enters it.
		return ir.Instruction{Op: ir.Host}
	case bytecode.OpClosure:
		// Go's: a closure made on the frame, capturing its locals where
		// the frame has them.
		return ir.Instruction{Op: ir.Closure, Dest: sp, Key: in.A}
	case bytecode.OpCloseUpvalues:
		// Go's: what closures captured closed as a scope ends.
		return ir.Instruction{Op: ir.CloseUpvalues, Key: in.A}
	}
	// describe accepted an opcode this switch does not translate. Becoming a
	// Nop with describe's stack effect would be a silent miscompile; the panic
	// is a refusal (lowerRecovered).
	panic(fmt.Sprintf("jit: no lowering for %v", in.Op))
}

// charCodeCalls marks the calls of one argument in fn whose function is the
// method a read of "charCodeAt" took (get_prop_this), which the old
// pipeline lowers as String.prototype.charCodeAt's (ir.StringCode), the new
// one making them by their target: the instruction the call's
// stack, two below its depth, was last at, the stack none lower since --
// s.charCodeAt(i), s.charCodeAt(i + 1), s.charCodeAt(c ? i : j). Every
// call of one argument in a function that read "charCodeAt" anywhere was
// lowered so: its check that the function is the intrinsic failed at every
// call of another, which left native code, and it was never a call native
// code makes (PDF.js's Type1Font_wrap, its cff.push(field.charCodeAt(i)),
// at every character). A call wrongly marked is still checked.
func charCodeCalls(fn *bytecode.Function, maps []ir.StateMap) []bool {
	codes := make([]bool, len(fn.Code))
	// at[d] is the last instruction the stack was at depth d before, none
	// lower since; -1 for none.
	var at []int
	for pc, in := range fn.Code {
		d := maps[pc].Depth
		if d < 0 {
			at = at[:0]
			continue
		}
		if in.Op == bytecode.OpCallMethod && in.A == 1 && d >= 2 && d-2 < len(at) {
			if q := at[d-2]; q >= 0 && fn.Code[q].Op == bytecode.OpGetPropThis && fn.Names[fn.Code[q].A] == "charCodeAt" {
				codes[pc] = true
			}
		}
		at = at[:min(len(at), d)]
		for len(at) < d {
			at = append(at, -1)
		}
		at = append(at, pc)
	}
	return codes
}

// An intrinsic snapshot and an extra root must amortize their entry cost.
// One-off character helpers retain the existing call path.
func stringKernel(fn *bytecode.Function) bool {
	if len(fn.Code) >= 64 {
		return true
	}
	for pc, in := range fn.Code {
		switch in.Op {
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue,
			bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfCmpFalse:
			if in.A <= uint32(pc) {
				return true
			}
		}
	}
	return false
}

// markBools sets the Bools of p's instructions (ir.Instruction's): the
// operands a comparison or a Not wrote in the same block, found by walking
// back over instructions that write slots alone, and through copies.
func markBools(p *ir.Program) {
	starts := make([]bool, len(p.Code)+1)
	starts[0] = true
	for _, e := range p.Entries {
		if e >= 0 && e < len(starts) {
			starts[e] = true
		}
	}
	for _, h := range p.Handlers {
		if int(h.PC) < len(starts) {
			starts[h.PC] = true
		}
	}
	for pc, in := range p.Code {
		switch in.Op {
		case ir.Jump, ir.Branch:
			if in.Target >= 0 && in.Target < len(starts) {
				starts[in.Target] = true
			}
			starts[pc+1] = true
		case ir.Return:
			starts[pc+1] = true
		}
	}
	boolean := func(in *ir.Instruction) bool {
		return in.Op == ir.Binary && in.Operator >= ir.Lt && in.Operator <= ir.Ne || in.Op == ir.Unary && in.Operator == ir.Not
	}
	// written reports whether slot s, read at pc, was written last, in its
	// block, by a comparison or a Not, or a copy of what one wrote, with
	// only instructions between that write slots alone: Binary, Unary and
	// the copies.
	written := func(pc, s int) bool {
		for q := pc - 1; q >= 0 && !starts[q+1] && s >= 0; q-- {
			in := &p.Code[q]
			switch in.Op {
			case ir.Nop:
			case ir.Copy:
				if in.Dest == s {
					s = in.Left.Slot
				}
			case ir.CopyPair, ir.StoreLoad:
				switch s {
				case in.Dest:
					s = in.Left.Slot
				case in.Extra:
					s = in.Right.Slot
					if in.Op == ir.StoreLoad && s == in.Dest {
						// Read after Dest is written: set_local_get's.
						s = in.Left.Slot
					}
				}
			case ir.Binary, ir.Unary:
				if in.Dest == s {
					return boolean(in)
				}
			default:
				return false
			}
		}
		return false
	}
	for pc := range p.Code {
		in := &p.Code[pc]
		switch {
		case in.Op == ir.Binary && in.Operator != ir.Eq && in.Operator != ir.Ne:
			if in.Left.Slot >= 0 && written(pc, in.Left.Slot) {
				in.Bools |= 1
			}
			if in.Right.Slot >= 0 && written(pc, in.Right.Slot) {
				in.Bools |= 2
			}
		case in.Op == ir.Unary && in.Operator != ir.Not:
			if in.Left.Slot >= 0 && written(pc, in.Left.Slot) {
				in.Bools |= 1
			}
		}
	}
}
