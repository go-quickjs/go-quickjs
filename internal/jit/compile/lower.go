// Package compile lowers VM bytecode into the JIT's slot IR. It is separate
// from executable-memory support and does not modify shared function templates.
package compile

import (
	"fmt"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// maxSlots bounds a program's slots -- its locals, the receiver's and the
// globals' cells among them, and its operands -- by what names a slot in
// native code's records and exits (abi.MaxSlot). It is no budget: a
// compile's work grows with the slots and the instructions together, which
// the VM bounds before it asks for one (its jitCompiles).
const maxSlots = abi.MaxSlot

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

// hostOnly is the reason LowerReg refuses a function that does nothing
// native code saves time on (HostOnly).
const hostOnly = "host operations without native loop or array work"

// GeneratorResumable reports whether a loop's step may resume a generator
// of fn by a native call of its code (the VM's jitRegGenCallable, ssa.Func's
// GenEntry): a synchronous generator whose body has no yield*
// (bytecode.Function's HasYieldStar, which the compiler records) and so
// no await, which only an async function's has, whose locals no closure
// captures -- its frame's locals are then the generator's alone, which no
// cell holds -- and no call in tail position, which a generator's frame
// does not give up.
func GeneratorResumable(fn *bytecode.Function) bool {
	if fn == nil || !fn.Generator || fn.Async || fn.HasTailCall || fn.HasDirectEval || fn.HasYieldStar {
		return false
	}
	for _, l := range fn.Locals {
		if l.Captured {
			return false
		}
	}
	return true
}

// HostOnly reports whether err is LowerReg's refusal of a function for
// having no loop and no array work, which LowerRegHot may lower.
func HostOnly(err error) bool {
	r, ok := err.(*Refusal)
	return ok && r.Reason == hostOnly
}

// Hints are what the VM has seen its function's code do that its bytecode
// does not say, which a lowering for the new pipeline takes (LowerReg*With).
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

// lowering is how a function is lowered: for which entry points, and for
// which pipeline.
type lowering struct {
	callee, ssa, forward, hot bool
	hints                     Hints
}

// argumentsOwn reports whether fn's own code may read its arguments and
// make its arguments object by Go, at host exits, on the frame of the
// VM's it was entered with. An unmapped object is a copy of the
// arguments. A mapped one -- a sloppy function's of simple parameters --
// aliases the parameters, which native code holds as values, written to
// the frame only at its exits: so only where it has none, or where the
// function never lets the object out, its arguments read by index and
// counted alone (arguments_index, arguments_length) -- which read what the
// parameters were given, or, where the code or a closure assigns them,
// the parameters as they are (ir.Program's ArgumentParams). The object Go
// makes at an exit there -- for a key past the arguments -- the
// interpreter reads at the exits after it, its frame written.
func argumentsOwn(fn *bytecode.Function) bool {
	if rc := fn.RegCompiled(); rc != nil && rc.Leaf == bytecode.LeafForward || fn.ForwardsArguments && fn.MakesArguments {
		// A forwarding function's -- f.apply(x, arguments), its object
		// made for nothing else (bytecode.Function's ForwardsArguments),
		// which hands the arguments on (apply_arguments, which
		// MakesArguments says) -- whose apply_arguments native code makes
		// itself, a native call of f with the arguments its context holds
		// (ir.Program's Forwards), as V8's CallForwardVarargs copies its
		// caller's arguments: where it may (forwardsNatively). Its callers
		// inline it where they know what it forwards to
		// (LowerRegForwardWith). One that only reads its arguments by
		// index, which the compiler marks so too, is its own.
		return forwardsNatively(fn)
	}
	if !fn.MappedArguments || fn.ParamCount == 0 {
		return true
	}
	if fn.MakesArguments {
		// The object made and let out -- to a call, to a store through
		// it, which writes the parameter it aliases -- while native code
		// holds the parameters as values. The compiler records it of the
		// code.
		return false
	}
	// A store to a parameter, by any instruction that stores to a local
	// (bytecode.WrittenLocals) -- a[++i]'s update is to its key's slot,
	// in B, not to A, its object's -- or a closure's of a parameter it
	// captured: arguments[k], which reads the parameter k aliases as it is
	// now, is given the parameters (ir.Program's ArgumentParams), up to
	// what a call of Go takes past its two operands.
	return fn.ParamCount+2 <= abi.MaxGoArgs
}

// forwardsNatively reports whether fn's apply_arguments may hand on its
// call's arguments as they were given (ir.Program's Forwards): not where a
// mapped arguments object would alias a parameter the code, or a closure,
// assigns, whose value it then hands on (the VM's regApplyArgs), and not
// a generator's or an async function's, whose frame a resumption makes of
// its own, no native call's.
func forwardsNatively(fn *bytecode.Function) bool {
	if fn.Generator || fn.Async {
		return false
	}
	return !fn.MappedArguments || fn.ParamCount == 0 || !fn.AssignsParams && !paramCaptured(fn)
}

// ForwardsNatively reports whether fn's apply_arguments, where its code
// has one, hands on its call's arguments natively (ir.Program's Forwards):
// for the VM, whose feedback makes such a call a native call or Go's
// (ssa.CallSite's Forward) only in code that does.
func ForwardsNatively(fn *bytecode.Function) bool { return forwardsNatively(fn) }

// paramCaptured reports whether a closure fn makes captures one of its
// parameters.
func paramCaptured(fn *bytecode.Function) bool {
	for i := 0; i < fn.ParamCount && i < len(fn.Locals); i++ {
		if fn.Locals[i].Captured {
			return true
		}
	}
	return false
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
	// On the stack for a function of up to 256 slots, as nearly all are.
	var room [256]uint8
	needed := room[:]
	if n := p.Locals + p.StackSize; n > len(room) {
		needed = make([]uint8, n)
	}
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
		case ir.FieldDefine, ir.Coercible, ir.StringFreeze:
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
			if (in.Window || in.Frame || in.Whole) && in.Dest >= 0 && in.Dest < p.Locals {
				// Register code's call, or an instruction Go runs whole,
				// writes its result to a register, below the scratch slots
				// cleared next: what reads it after reads the result, not
				// what the register held before -- a function table's
				// element, called and its result ORed with 0, had been read
				// as a number, which it is not, and deoptimized at.
				take(in.Dest)
			}
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
	// str is what is taken for a string, which decides who makes an addition
	// and is right if it is wrong; sure what cannot be anything else, which
	// is what a conversion may be dropped for: an addition that has met a
	// string once (Hints' StringAdds) may be given two numbers now.
	str := make([]bool, p.Locals+p.StackSize)
	sure := make([]bool, p.Locals+p.StackSize)
	targets := make([]bool, len(p.Code)+1)
	for _, in := range p.Code {
		if in.Op == ir.Jump || in.Op == ir.Branch {
			targets[in.Target] = true
		}
	}
	slot := func(o ir.Operand) bool { return o.Slot >= 0 && o.Slot < len(str) && str[o.Slot] }
	proven := func(o ir.Operand) bool { return o.Slot >= 0 && o.Slot < len(sure) && sure[o.Slot] }
	set := func(i int, v bool) {
		if i >= 0 && i < len(str) {
			str[i], sure[i] = v, v
		}
	}
	// madeBy is set for the result at i of an addition of l and r.
	madeBy := func(i int, l, r ir.Operand) {
		// Before the write: i is the left operand's slot, as often as not.
		known := proven(l) || proven(r)
		set(i, true)
		if i >= 0 && i < len(sure) {
			sure[i] = known
		}
	}
	for pc, in := range p.Code {
		if targets[pc] {
			clear(str)
			clear(sure)
		}
		if p.Maps[pc].Depth < 0 {
			continue
		}
		switch in.Op {
		case ir.StringConst:
			set(in.Dest, true)
		case ir.Copy:
			taken, known := slot(in.Left), proven(in.Left)
			set(in.Dest, taken)
			if in.Dest >= 0 && in.Dest < len(sure) {
				sure[in.Dest] = known
			}
		case ir.Binary:
			if in.Operator == ir.Add && (slot(in.Left) || slot(in.Right)) {
				// A register instruction's component keeps its Generic
				// (ir.Program's Origin).
				p.Code[pc] = ir.Instruction{Op: ir.StringAdd, Left: in.Left, Right: in.Right, Dest: in.Dest, Generic: in.Generic}
				madeBy(in.Dest, in.Left, in.Right)
				continue
			}
			set(in.Dest, false)
		case ir.StringConcat:
			if in.Extra == 1 && proven(ir.Slot(in.Dest)) {
				// A string's conversion, which leaves it as it is.
				p.Code[pc] = ir.Instruction{Op: ir.Nop}
				continue
			}
			set(in.Dest, true)
		case ir.StringAdd:
			madeBy(in.Dest, in.Left, in.Right)
		case ir.ParamInit, ir.ParamDefault:
			set(in.Dest, false)
		case ir.ParamsDead:
			for i := 0; i < int(in.Key); i++ {
				set(i, false)
			}
		case ir.ArrayRead:
			set(in.Dest, in.Reference && slices.Contains(reads, int32(pc)))
		case ir.Host, ir.Call:
			// Its results, past what it leaves below them.
			if pc+1 < len(p.Maps) && p.Maps[pc+1].Depth > 0 {
				clear(str[min(len(str), p.Locals+p.Maps[pc+1].Depth-1):])
				clear(sure[min(len(sure), p.Locals+p.Maps[pc+1].Depth-1):])
			} else {
				clear(str[p.Locals:])
				clear(sure[p.Locals:])
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

// typeNames are the strings typeof answers, each type's name, which a
// comparison with typeof tells natively; it is false for any other string
// (ir.TypeNone).
var typeNames = map[string]int{"number": ir.TypeNumber, "string": ir.TypeString, "boolean": ir.TypeBoolean,
	"undefined": ir.TypeUndefined, "function": ir.TypeFunction, "object": ir.TypeObject, "symbol": ir.TypeSymbol,
	"bigint": ir.TypeBigInt}

// GoOperation reports whether op is one the new pipeline has Go make,
// called from native code, where its operands are the top of the stack and
// its one result replaces them: the VM's GoCall of an operation
// (abi.GoCallOp), which makes it without a frame where that runs no script
// -- an object's own data properties copied, a dense array's elements
// spread, a key that is a primitive converted -- and with the code's frame
// where it may run some, a getter, a proxy's trap, an iterator's next, a
// key's toString, a call: object spread and rest, array spread and spread
// calls, an element appended to an array literal, a literal's __proto__,
// its computed keys and its fields where native code does not define them
// along the shapes' transitions (ir.FieldDefine), a tagged template's
// strings and a function's own object.
func GoOperation(op bytecode.Op) bool {
	switch op {
	case bytecode.OpCopyDataProps, bytecode.OpArraySpread, bytecode.OpSpreadIter, bytecode.OpArrayPush, bytecode.OpSetProtoOf,
		bytecode.OpObjectRest, bytecode.OpCallSpread, bytecode.OpNewSpread, bytecode.OpTemplateObject, bytecode.OpPushCallee,
		bytecode.OpDefineIndex, bytecode.OpToPropertyKey, bytecode.OpDefineField:
		return true
	}
	return false
}

// GoOperands is how many operands an operation Go makes (GoOperation)
// takes, op and its operand a, whose one result replaces them.
func GoOperands(op bytecode.Op, a uint32) int {
	switch op {
	case bytecode.OpCopyDataProps, bytecode.OpArraySpread, bytecode.OpSpreadIter, bytecode.OpArrayPush, bytecode.OpSetProtoOf,
		bytecode.OpNewSpread, bytecode.OpDefineField:
		return 2
	case bytecode.OpObjectRest:
		return int(a) + 1
	case bytecode.OpCallSpread, bytecode.OpDefineIndex:
		return 3
	case bytecode.OpToPropertyKey:
		return 1
	}
	return 0
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

// markBools sets the Bools of p's instructions (ir.Instruction's): the
// operands a comparison or a Not wrote in the same block, through
// instructions between that write slots alone, and through copies. It
// goes forward over each block, knowing of each slot whether what it holds
// is such a result, a copy of one, or neither -- what walking back from
// each operand found, a walk as long as its block, which had made a block
// of n instructions take n² steps.
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
	// Whether slot s holds a comparison's or a Not's result, written in
	// the block since its start or since an instruction that does more
	// than write slots: s's entry, where its stamp is the epoch.
	var stamp []uint32
	var is []bool
	epoch := uint32(1)
	held := func(s int) bool { return s >= 0 && s < len(stamp) && stamp[s] == epoch && is[s] }
	hold := func(s int, b bool) {
		if s < 0 {
			return
		}
		if s >= len(stamp) {
			n := max(2*len(stamp), s+1, p.Locals+p.StackSize)
			stamp, is = append(stamp, make([]uint32, n-len(stamp))...), append(is, make([]bool, n-len(is))...)
		}
		stamp[s], is[s] = epoch, b
	}
	for pc := range p.Code {
		if starts[pc] {
			epoch++
		}
		in := &p.Code[pc]
		switch {
		case in.Op == ir.Binary && in.Operator != ir.Eq && in.Operator != ir.Ne:
			if held(in.Left.Slot) {
				in.Bools |= 1
			}
			if held(in.Right.Slot) {
				in.Bools |= 2
			}
		case in.Op == ir.Unary && in.Operator != ir.Not:
			if held(in.Left.Slot) {
				in.Bools |= 1
			}
		}
		switch in.Op {
		case ir.Nop:
		case ir.Copy:
			hold(in.Dest, held(in.Left.Slot))
		case ir.CopyPair, ir.StoreLoad:
			d := held(in.Left.Slot)
			r := in.Right.Slot
			if in.Op == ir.StoreLoad && r == in.Dest {
				// Read after Dest is written: set_local_get's.
				r = in.Left.Slot
			}
			e := held(r)
			if in.Extra != in.Dest {
				hold(in.Extra, e)
			}
			hold(in.Dest, d)
		case ir.Binary, ir.Unary:
			hold(in.Dest, boolean(in))
		default:
			epoch++
		}
	}
}
