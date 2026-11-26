package ssa

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// ErrUnsupported reports a program with an operation the SSA builder does
// not yet translate. The function stays in the existing tiers.
var ErrUnsupported = errors.New("ssa: unsupported operation")

// Build translates a validated slot IR program into SSA. Its entries are
// PC 0, each loop header, and each PC a host exit resumes at.
func Build(p *ir.Program) (*Func, error) { return BuildWith(p, nil) }

// Feedback is what the VM knows of a function's sites, which Build would
// otherwise leave to Go.
type Feedback interface {
	// Property is the property a PropertyRead or PropertyWrite at pc names,
	// or false to leave the site to Go.
	Property(pc int) (PropertySite, bool)
	// Global is where the global a BindingRead at pc names was last found,
	// or false to leave the site to Go.
	Global(pc int) (GlobalSite, bool)
	// Generic reports an operation at pc whose speculation failed before,
	// which is built so that what it did not expect goes to Go, which
	// resumes after it, rather than deoptimizing: an element read by its
	// cell, an arithmetic operation or a comparison of what are not numbers
	// by Go, a property read for a number (ir.PropertyRead) that met
	// another value by its cell. EntryGeneric reports an entry whose speculation failed,
	// by its PC in the function's own code (FrameState.PC): the slots it
	// loads are not taken for numbers there.
	Generic(pc int) bool
	EntryGeneric(pc int) bool
	// Inline is the call at pc to inline, if the VM has seen it call one
	// function it may be: see InlineSite.
	Inline(pc int) (InlineSite, bool)
	// NativeCalls are the functions the call at pc may call natively, if
	// any: those the VM has seen it call, at most a few (see CallSite).
	// The answer is read-only and may be shared by repeated builds.
	NativeCalls(pc int) []CallSite
}

// Intrinsic is a call of a built-in the VM knows, which native code makes
// itself, as V8 reduces Math.sqrt(x) to a square root in its code: the
// function object, which the call checks it calls (the VM keeps it
// alive), and what it computes of its argument, a number, or of its two
// for Math.pow (OpPowF64), Math.imul (OpMulI32) and Math.atan2 and
// Math.hypot, or of any number from one for Math.max and Math.min --
// another leaves the call to Go, which makes it. Math.clz32 is an
// instruction of its own (OpClz32), as Math.imul is.
type Intrinsic struct {
	Callee uintptr
	Op     Op
	// Args is how many arguments the call passes, Math.max's or
	// Math.min's (OpMaxF64, OpMinF64); 0 for the op's own count.
	Args int
	// Go and Fn, for OpMathCall, are the call of Go that computes it and,
	// for GoMath, which of the VM's functions of one number.
	Go GoOp
	Fn int
	// Random, for OpRandomF64, is the runtime's Math.random cache, which
	// the code takes its numbers from (Encoding's RandomCount and
	// RandomNumbers).
	Random uintptr
	// Plain marks a call that is no method's, of no receiver: BigInt(x)'s
	// (OpBigOfF64), or one of Math's functions kept in a binding, P(x) of
	// P = Math.clz32. BigInt.asUintN(n, x) and asIntN's are OpZextW's and
	// OpSextW's, of the width n, Fn, from 0 to 64, a constant the call
	// is made with, and x (bigint.go).
	Plain bool
}

// maxIntrinsicArgs is the most arguments an intrinsic call is made with.
const maxIntrinsicArgs = 8

// InstanceOfSite is an instanceof operator's constructor, as the VM knows
// it, which native code answers for itself, as V8 lowers the operator to
// a walk along the prototypes: the function object, which the operator
// checks it has (the VM keeps it alive); where its Symbol.hasInstance is,
// on Function.prototype, and the realm's function there, which it checks
// is still what that read finds; and where its own prototype property is.
type InstanceOfSite struct {
	Ctor          uintptr
	HasInstance   PropertySite
	HasInstanceFn uintptr
	Prototype     PropertySite
}

// AccessorSite is a property read or write (ir.PropertyRead,
// PropertyWrite) whose site found an accessor, as V8's code calls a getter
// or setter its feedback knows: Property is where -- the receiver's shape,
// the prototypes up to the holder with theirs, and the accessor's index in
// the holder's table, as a read found on a prototype has them -- and
// Function the getter, or, Setter, the setter, its function object, which
// the VM keeps alive. The read is a call of the getter with the receiver
// as its this and no arguments; the write a call of the setter with the
// value, its result dropped. Inline is the callee to inline, if it may be
// (InlineSite: its program and feedback, Argc 0 or 1, Method), else Call
// the native call to make, through its link (CallSite, with Accessor),
// else Go makes the read or the write, called from native code (GoCall).
// Any other receiver, or an accessor whose function is no longer
// Function, Go reads or writes at an exit.
type AccessorSite struct {
	Property PropertySite
	Function uintptr
	Setter   bool
	Inline   *InlineSite
	Call     *CallSite
}

// AccessorFeedback is Feedback that knows the property reads and writes
// whose sites found accessors (AccessorSite).
type AccessorFeedback interface {
	Accessor(pc int) (AccessorSite, bool)
}

// InstanceOfFeedback is Feedback that knows instanceof's constructors.
type InstanceOfFeedback interface {
	InstanceOf(pc int) (InstanceOfSite, bool)
}

// ForEachSite is a call of Array.prototype.forEach, the realm's, at Fn,
// that the VM has seen make on an array (ForEachFeedback) -- or of reduce,
// map, filter, some, every, find or findIndex, its Kind (abi.LoopForEach
// and those after it) -- and Callback the native call of the callback it
// met: a method call of three arguments, the element, its index and the
// array, its receiver the call's second argument or undefined; reduce's,
// of four, the accumulator first, its receiver undefined. One of
// %TypedArray%.prototype's methods of those names has abi.LoopTyped in
// Kind, with its element type, and Proto the prototype of the typed arrays
// it was seen on, which the loop checks.
//
// Inline, if the VM gives one, is the callback to inline in the loop, as
// V8's JSCallReducer inlines a forEach's callback into the loop it makes:
// the closure the site's callback has been (InlineSite's Callee, which the
// loop checks it is given), or, where Made says the caller makes the
// callback just before the call, a closure of its function -- Callee 0,
// since each call is given a new one -- which, its call inlined, is never
// made: Made says where, and what it would capture.
type ForEachSite struct {
	Fn       uintptr
	Kind     int
	Proto    uintptr
	Callback CallSite
	Inline   *InlineSite
	Made     *MadeCallback
}

// MadeCallback is a callback a forEach, or another loop ForEachSite names,
// is given that the caller's own code makes just before the call (an
// ir.Closure at PC), a closure of the function ForEachSite's Inline is:
// Template is the address of the function's template, a VM value, which a
// frame state holds in the closure's place, as a cell (OpConstCell), for
// Go to make the closure from at an exit, as V8's deoptimizer materializes
// an object escape analysis removed; Closure is the template's own address,
// the inlined frame's closure an exit names (InlineState's Closure); and
// Captures are what the closure would capture, in order: index<<1|1 the
// maker's local, index<<1 the maker's own captured binding.
type MadeCallback struct {
	PC       int
	Template uintptr
	Closure  uintptr
	Captures []int32
}

// ForEachFeedback is Feedback that knows the forEach, or another of the
// methods ForEachSite names, a method call at a PC makes: native code makes
// it a loop of its own.
type ForEachFeedback interface {
	ForEach(pc int) (ForEachSite, bool)
}

// loopLocals is how many of a loop's slots (forEachCall) are its own, which
// Go goes on from at an exit (abi.LoopForEach's): the array, the callback,
// this -- reduce's accumulator -- the index, the length, the array map or
// filter makes, the element filter or find gave the callback, and the
// callback's result. The callback's call's operands follow them:
// loopSlots in all.
const loopLocals = 8

// The loop's slots, past its first.
const (
	loopArray = iota
	loopCallback
	loopThis
	loopIndex
	loopLength
	loopOut
	loopElem
	loopResult
)

// loopSlots is how many slots past the function's a loop of kind takes:
// its locals and its callback's call's operands -- this, the callback and
// its arguments, three, or four, reduce's.
func loopSlots(kind int) int {
	switch kind & abi.LoopKindMask {
	case abi.LoopReduce:
		return loopLocals + 6
	case abi.LoopSort:
		// sortCall's locals, and the comparator's call's operands: this,
		// the comparator and its two arguments.
		return sortLocals + 4
	}
	return loopLocals + 5
}

// loopLocalsOf is how many of a loop of kind's slots are its own, past
// which its callback's call's operands are.
func loopLocalsOf(kind int) int {
	if kind&abi.LoopKindMask == abi.LoopSort {
		return sortLocals
	}
	return loopLocals
}

// loopKeeps is how many keep cells a loop of kind takes: its callback's
// result's, and map's and filter's array's.
func loopKeeps(kind int) int {
	if makesArray(kind) || kind&abi.LoopKindMask == abi.LoopSort {
		return 2
	}
	return 1
}

// makesArray reports whether a loop of kind makes an array, its result:
// map's and filter's.
func makesArray(kind int) bool {
	kind &= abi.LoopKindMask
	return kind == abi.LoopMap || kind == abi.LoopFilter
}

// forEachPlan is the forEach, or another loop, at a PC that planning made a
// loop of: its site, its slots' first, and the keep cell its callback's
// result is in, the array it makes in the next.
type forEachPlan struct {
	site       ForEachSite
	base, keep int
	// inline is the callback inlined in the loop (ForEachSite's Inline),
	// if planning inlined it, and made says the caller's code never makes
	// it (ForEachSite's Made).
	inline *frame
	made   bool
}

// forEachAt is the forEach, or another loop, the method call at pc of the
// root, one Go makes (a GoCall site), makes on an array, if its feedback
// has one (ForEachFeedback): one argument, the callback, or two -- its
// this, reduce's initial value -- too. One that makes an array needs Go
// to make it (GoNewArray).
func (b *builder) forEachAt(pc int) (ForEachSite, bool) {
	calls := b.callsAt(pc)
	if b.fb == nil || b.cur != b.root || b.p.Code[pc].Op != ir.Host || len(calls) != 1 || calls[0].Go != GoCall || !calls[0].Method ||
		calls[0].Argc != 1 && calls[0].Argc != 2 || b.fb.Generic(pc) || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) {
		return ForEachSite{}, false
	}
	if cs, ok := b.callSlotsOf(pc, calls[0].Argc, true); !ok || b.p.Code[pc].Tail || b.p.IsCaptured(cs.dest) {
		// Register code's result in a register a closure captures, which
		// the loop's many writes of it would store through its cell: Go's.
		return ForEachSite{}, false
	}
	f, ok := b.fb.(ForEachFeedback)
	if !ok {
		return ForEachSite{}, false
	}
	site, ok := f.ForEach(pc)
	typed := site.Kind&abi.LoopTyped != 0
	if base := site.Kind & abi.LoopKindMask; !ok || base <= 0 || base >= abi.LoopKinds || (makesArray(base) || base == abi.LoopSort) && !b.goCalls() ||
		base == abi.LoopSort && site.Inline == nil || typed && site.Proto == 0 {
		return ForEachSite{}, false
	}
	return site, true
}

// planCallback plans the callback of fp, the loop at pc, inlined in the
// loop's body, if its feedback has one to (ForEachSite's Inline) and
// inlining's rules and bounds allow it, as any callee's: a method call of
// three arguments, or reduce's four, whose this, if it reads one, is the
// call's as it is -- not a sloppy function's, which would coerce it --
// and one that returns natively, the loop going on after it. Where the
// root's code makes the callback just before the call (ForEachSite's
// Made, madeAt), the closure is not made: the inlined code reads and
// assigns what it captured as the root's own slots (InlineSite's
// Captured), and an exit's frame names its template where it would be,
// for Go to make it there. It reports whether it planned it so.
func (b *builder) planCallback(pc int, fp *forEachPlan, total *int) bool {
	if fp.site.Inline == nil || fp.site.Inline.Program == nil || b.cur.nest >= maxInlineDepth {
		return false
	}
	if f, ok := b.siteFrequency(pc); ok && f < minInliningFrequency {
		// Its call made too seldom, as V8's call reducer gives the
		// callback's call the frequency of the call of the built-in,
		// whatever the callback's size.
		return false
	}
	s := *fp.site.Inline
	made := fp.site.Made
	if made != nil && !b.madeAt(pc, made) {
		made = nil
	}
	argc := 3
	switch fp.site.Kind & abi.LoopKindMask {
	case abi.LoopReduce:
		argc = 4
	case abi.LoopSort:
		argc = 2
	}
	if made == nil && s.Callee == 0 || s.Construct || s.Restart || s.Forward || s.Arguments || !s.Method || s.Argc != argc || s.Params < 0 ||
		s.Coerce && s.ThisSlot >= 0 {
		return false
	}
	if made != nil {
		// Its captured bindings, the root's slots: a local of its own,
		// which a closure captures, or a binding it captured itself.
		p := b.root.p
		s.Callee, s.Closure, s.Upvalues = 0, made.Closure, 0
		s.Captured = make([]int, len(made.Captures))
		for i, c := range made.Captures {
			k := int(c >> 1)
			switch {
			case c&1 != 0 && k < p.UpvalueBase && p.IsCaptured(k):
				s.Captured[i] = k
			case c&1 == 0 && k < p.Upvalues:
				s.Captured[i] = p.UpvalueBase + k
			default:
				return false
			}
		}
	}
	calls, ok := b.inlineCallsOf(s.Program)
	if !ok || !inlineFits(*total, inlineSize(s.Program)) || !b.typedInline(s) {
		return false
	}
	nslots, before, keeps, inlined := b.nslots, *total, b.f.Keeps, len(b.f.Inlined)
	fr, ok := b.inlineFrame(s, calls, nil, total, fp.base+loopLocalsOf(fp.site.Kind), pc)
	if ok && !b.returnsNatively(fr) {
		b.nslots, *total, b.f.Keeps = nslots, before, keeps
		b.f.Inlined = b.f.Inlined[:inlined]
		ok = false
	}
	if !ok {
		return false
	}
	fp.inline, fp.made = fr, made != nil
	b.f.Callbacks++
	if made != nil {
		b.f.Unmade++
		if b.made == nil {
			b.made = map[int]uintptr{}
		}
		b.made[made.PC] = made.Template
	}
	return true
}

// madeAt reports whether made, the callback of the loop at pc, is a closure
// the root's code makes just before the call and gives only to it: its
// instruction at made.PC, made natively (host), writes the callback's
// operand, and the instructions between -- the loop's this, if it has
// one -- are copies that neither read nor write that slot, which nothing
// jumps into, nor a handler catches at, nor an entry enters: what the
// slot holds there is the closure, never read but by the call, and dead
// after it. Only an exit's frame state names it then.
func (b *builder) madeAt(pc int, made *MadeCallback) bool {
	p := b.p
	pc0 := made.PC
	if b.cur != b.root || pc0 < 0 || pc0 >= pc || !reachable(p, pc0) || p.Code[pc0].Op != ir.Closure || b.host(pc0) || made.Template == 0 || made.Closure == 0 {
		return false
	}
	// The call's first argument, where its form has it (callSlotsOf).
	cs, ok := b.callSlotsOf(pc, b.callsAt(pc)[0].Argc, true)
	slot := cs.arg
	if !ok || p.Code[pc0].Dest != slot {
		return false
	}
	for q := pc0 + 1; q < pc; q++ {
		if in := p.Code[q]; in.Op != ir.Copy || in.Dest == slot || in.Left.Slot == slot {
			return false
		}
	}
	into := func(target int) bool { return target > pc0 && target <= pc }
	for q, in := range p.Code {
		if (in.Op == ir.Jump || in.Op == ir.Branch || in.Op == ir.IterStep && !in.When) && into(in.Target) && reachable(p, q) {
			return false
		}
	}
	for _, h := range p.Handlers {
		if into(int(h.PC)) {
			return false
		}
	}
	return !slices.ContainsFunc(p.Entries, into)
}

// forEachCall makes the forEach, or another loop, at pc a loop of the
// code's own, as V8's JSCallReducer reduces each (forEachPlan): its
// function, its callback being a function and its array checked and its
// length read once -- anything else Go's call -- then, for each index
// below the length, the element, where the array has it -- find's and
// findIndex's wherever, a hole undefined -- given to the callback, called
// natively (ForEachSite's Callback) with its index and the array, its
// this the call's second argument; reduce's with the accumulator first --
// its second argument, or else the first element, the loop then starting
// at the next -- its result the next accumulator, and the reduce's the
// last. map's result is an array Go makes (GoNewArray) where the array's
// species is Array, room for the length in it, each callback's result
// appended to it (OpElemStore); filter's one Go makes of no length, each
// element whose callback's result is true appended. some, every, find and
// findIndex leave the loop at the first callback's result that is true --
// every's, false -- their result true, false, the element or its index,
// or, the loop done, false, true, undefined or -1.
//
// A hole or an index past the array's end now leaves, and so does
// anything the loop's checks find: Go goes on from that index (abi.Context's
// Loop, which the loop sets as it starts and clears as it ends), as V8's
// builtin continuation frames go on in the builtin, then the code after
// the call -- after the callback's result, where the loop has it, from
// what that result is to do. Its slots (loopSlots) are the frame's past
// its operands -- its locals (loopLocals), then the call's operands -- in
// every state inside the loop, the call's below them.
func (b *builder) forEachCall(blk *Block, pc int, fp forEachPlan) {
	f := b.f
	mark, kind := fp.site.Kind, fp.site.Kind&abi.LoopKindMask
	typed := mark&abi.LoopTyped != 0
	reduce := kind == abi.LoopReduce
	argc := b.callsAt(pc)[0].Argc
	// Its operands where the call's form has them: register code's in its
	// window, a slot program's on its operand stack (callSlots).
	cs, _ := b.callSlotsOf(pc, argc, true)
	dest := cs.dest
	recv, fn, cb := b.read(cs.recv, blk), b.read(cs.fn, blk), b.read(cs.arg, blk)
	// Constants, made where they are used: constIn puts its constant first
	// in its block, before the phis an operand's read makes there.
	constant := func(blk *Block, c ir.Value) *Value {
		v := f.newValue(blk, OpConst, Tagged)
		v.Const = c
		return v
	}
	undefined := func(blk *Block) *Value { return constant(blk, ir.Value{Kind: ir.Undefined}) }
	none := func(blk *Block) *Value { return constant(blk, ir.Value{Kind: ir.Uninitialized}) }
	call := b.rootState()
	at := func(blk *Block, st *FrameState, op Op, t Type, args ...*Value) *Value {
		v := f.newValue(blk, op, t, args...)
		v.Aux, v.State = int(ir.HostExit), st
		st.addUse()
		return v
	}
	object := at(blk, call, OpObjectOf, Ptr, fn)
	same := at(blk, call, OpSameObject, None, object)
	same.Const = ir.Value{Bits: uint64(fp.site.Fn)}
	switch fr := fp.inline; {
	case fr != nil && fp.made:
		// A closure the code would have made just before (ForEachSite's
		// Made): a function, and the one inlined.
	case fr != nil:
		// The closure inlined, which a function is: any other, Go's call.
		is := at(blk, call, OpSameObject, None, at(blk, call, OpObjectOf, Ptr, cb))
		is.Const = ir.Value{Bits: uint64(fr.site.Callee)}
	default:
		// The callback a function, which each method checks before it
		// reads an element: Go's call throws its TypeError otherwise.
		callable := at(blk, call, OpTypeIs, Bool, cb)
		callable.Index = int(ir.TypeFunction)
		at(blk, call, OpCheckTrue, None, f.newValue(blk, OpBoxBool, Tagged, callable))
	}
	var array *Value
	if typed {
		// A typed array of the prototype the loop was seen with, its
		// elements read as numbers (OpElemRead): one whose view is out of
		// its buffer, Go's call, which throws.
		is := at(blk, call, OpSameObject, None, at(blk, call, OpObjectOf, Ptr, recv))
		is.Const, is.Index = ir.Value{Bits: uint64(fp.site.Proto)}, 1
		array = at(blk, call, OpIndexedOf, Ptr, recv)
	} else {
		array = at(blk, call, OpArrayOf, Ptr, recv)
	}
	n := at(blk, call, OpLength, Float64, recv)
	// Room past the operands for the loop's slots, which an exit inside it
	// writes there.
	room := at(blk, call, OpFrameRoom, None)
	room.Index = fp.base - b.root.p.Locals + loopSlots(kind)
	if fr := fp.inline; fr != nil {
		// And past them for the callback's frame, which an exit inside it
		// writes there, and a context for Go to make it from.
		room := at(blk, call, OpFrameRoom, None)
		room.Index = fr.base - b.root.p.Locals + fr.p.Locals + fr.p.StackSize
		room.Const = ir.Value{Bits: uint64(fr.depth)}
	}
	boxF := func(blk *Block, x *Value) *Value { return f.newValue(blk, OpBoxF64, Tagged, x) }
	// The element at k: a typed array's a number, an array's from its cell
	// -- where it has one, but find's and findIndex's -- whatever it is.
	finds := kind == abi.LoopFind || kind == abi.LoopFindIndex
	// A BigInt typed array's element is a BigInt held in its value, read
	// as its type's word (OpBigElemRead) and boxed.
	big := None
	switch mark >> abi.LoopTypedShift {
	case abi.ElemBigInt64:
		big = Int64
	case abi.ElemBigUint64:
		big = Uint64
	}
	element := func(blk *Block, st *FrameState, k *Value, present bool) *Value {
		if typed && big != None {
			return b.bigBoxed(blk, at(blk, st, OpBigElemRead, big, array, k))
		}
		if typed {
			return boxF(blk, at(blk, st, OpElemRead, Float64, array, k))
		}
		cell := at(blk, st, OpElemCell, Source, array, k)
		if present {
			cell.Index = ElemPresent
		}
		elem := f.newValue(blk, OpLoadCell, Tagged, cell)
		elem.Shadow = cell
		return elem
	}
	// map's and filter's array, as their ArraySpeciesCreate makes it, of
	// the length's room, map's: Go makes it where the array's species is
	// Array's, its constructor as it was made, and refuses otherwise,
	// leaving the call to Go. A typed array's map's is a typed array of
	// the length, as its TypedArraySpeciesCreate makes it, where the
	// species is the type's own constructor as it was made; a typed
	// array's filter's an Array its kept elements are appended to, of
	// which Go makes the typed array after the loop (GoNewArray's
	// NewArrayFill), when the specification makes it, of the count kept.
	out := undefined(blk)
	if makesArray(kind) {
		mk := at(blk, call, OpCall, Tagged, recv, boxF(blk, n))
		site := &CallSite{Go: GoNewArray, ThisSlot: -1}
		if kind == abi.LoopMap {
			site.Argc = NewArrayRoom
		}
		if typed {
			site.Argc |= NewArrayTyped
		}
		f.cache(mk).Calls, mk.Index = []*CallSite{site}, fp.keep+1
		cell := f.newValue(blk, OpCallCell, Source, mk)
		out = f.newValue(blk, OpKept, Tagged, cell, mk)
		out.Shadow = cell
	}
	from := f.newValue(blk, OpConstF64, Float64)
	from.Const = ir.Float(0)
	// this, or reduce's first accumulator: its initial value, or else the
	// first element -- an empty array's, or a hole's there, Go's call,
	// which throws or finds the first.
	third := undefined(blk)
	switch {
	case argc == 2:
		third = b.read(cs.arg+1, blk)
	case reduce:
		third = element(blk, call, from, true)
		from = f.newValue(blk, OpConstF64, Float64)
		from.Const = ir.Float(1)
	}
	base := fp.base
	for i, v := range []*Value{recv, cb, third, boxF(blk, from), boxF(blk, n), out, undefined(blk), none(blk)} {
		b.write(base+i, blk, v)
	}
	// The result if the loop runs to its end, but reduce's, its last
	// accumulator: the frame's after the call is its, Go's at an exit.
	var whole *Value
	switch kind {
	case abi.LoopMap, abi.LoopFilter:
		whole = out
	case abi.LoopSome:
		whole = constant(blk, ir.Bool(false))
	case abi.LoopEvery:
		whole = constant(blk, ir.Bool(true))
	case abi.LoopFindIndex:
		whole = constant(blk, ir.Float(-1))
	case abi.LoopForEach, abi.LoopFind:
		whole = undefined(blk)
	}
	if whole != nil {
		b.assign(dest, blk, whole)
	}
	loop := f.newValue(blk, OpLoopMark, None)
	loop.Const = ir.Value{Bits: uint64(mark)<<32 | uint64(base-b.root.p.Locals+1)}
	// The state inside the loop: the call's, and the loop's slots, depth of
	// its operands.
	state := func(blk *Block, depth int) *FrameState {
		s := f.newState(FrameState{PC: call.PC, Depth: call.Depth, Site: call.Site})
		s.shareSlots(call)
		s.n = base + loopLocals + depth
		f.openEdit()
		for i := base; i < s.n; i++ {
			f.setSlot(s, i, b.read(i, blk))
		}
		return s
	}
	head, body, cont := b.loop(blk)
	head.Header = state(head, 0)
	head.Header.Site = -1
	k := at(head, state(head, 0), OpUnboxF64, Float64, b.read(base+loopIndex, head))
	more := f.newValue(head, OpCmpF64, Bool, k, n)
	more.Aux = int(ir.Lt)
	head.Control = more
	more.Uses++
	elem := element(body, state(body, 0), k, !finds)
	// The element, for what the callback's result does with it.
	keepsElem := kind == abi.LoopFilter || kind == abi.LoopFind
	if keepsElem {
		b.write(base+loopElem, body, elem)
	}
	ops := []*Value{b.read(base+loopThis, body), cb, elem, boxF(body, k), recv}
	if reduce {
		ops = []*Value{undefined(body), cb, b.read(base+loopThis, body), elem, boxF(body, k), recv}
	}
	for i, v := range ops {
		b.write(base+loopLocals+i, body, v)
	}
	var v *Value
	if fr := fp.inline; fr != nil {
		// The callback inlined: its result the value its returns give, and
		// the body goes on after it. Its assignments of captured bindings
		// are its own.
		v, body = b.inlineCallback(body, fr, ops, base+loopLocals+2, state(body, len(ops)))
	} else {
		site := fp.site.Callback
		c := at(body, state(body, len(ops)), OpCall, Tagged, ops...)
		f.cache(c).Calls, c.Index = []*CallSite{&site}, fp.keep
		// The callback may have assigned a captured binding, as a callee may.
		if b.hasCells() {
			b.reloadAfterCall(body)
		}
		if kind != abi.LoopForEach {
			// Its result, kept where the call left it.
			kept := f.newValue(body, OpCallCell, Source, c)
			v = f.newValue(body, OpKept, Tagged, kept, c)
			v.Shadow = kept
		}
	}
	step := body
	if kind != abi.LoopForEach {
		// Its result: the loop's, which an exit before the next index gives
		// Go to finish the index with.
		b.write(base+loopResult, body, v)
		switch kind {
		case abi.LoopReduce:
			b.write(base+loopThis, body, v)
		case abi.LoopMap:
			st := state(body, 0)
			if typed && big != None {
				// A BigInt typed array's: the result's low 64 bits, a
				// BigInt held in its value (OpBigLow), stored at the
				// index as its type's word; anything else leaves for Go.
				w := at(body, st, OpBigElemWrite, None, at(body, st, OpIndexedOf, Ptr, out), k, at(body, st, OpBigLow, Uint64, v))
				if big == Uint64 {
					w.Index = 1
				}
				break
			}
			if typed {
				// Stored at the index of the typed array of the length,
				// as a number, converted to its type as a store converts
				// one (OpElemWrite); anything else -- a BigInt, an object
				// whose valueOf runs -- leaves for Go to store, and go on.
				at(body, st, OpElemWrite, None, at(body, st, OpIndexedOf, Ptr, out), k, at(body, st, OpUnboxF64, Float64, v))
				break
			}
			// Appended at the index, which is the array's length: the
			// elements before it all appended.
			at(body, st, OpElemStore, None, at(body, st, OpArrayOf, Ptr, out), k, v).Index = StoreFresh
		default:
			test := at(body, state(body, 0), OpTruth, Bool, v)
			if kind == abi.LoopEvery {
				test = f.newValue(body, OpNot, Bool, test)
			}
			var yes *Block
			yes, step = b.loopTest(body, cont, test, kind != abi.LoopFilter)
			switch kind {
			case abi.LoopFilter:
				// The element appended at the array's length.
				st := state(yes, 0)
				to := at(yes, st, OpLength, Float64, out)
				at(yes, st, OpElemStore, None, at(yes, st, OpArrayOf, Ptr, out), to, b.read(base+loopElem, yes)).Index = StoreFresh
			case abi.LoopSome:
				b.assign(dest, yes, constant(yes, ir.Bool(true)))
			case abi.LoopEvery:
				b.assign(dest, yes, constant(yes, ir.Bool(false)))
			case abi.LoopFind:
				b.assign(dest, yes, b.read(base+loopElem, yes))
			case abi.LoopFindIndex:
				b.assign(dest, yes, boxF(yes, k))
			}
		}
	}
	one := f.newValue(step, OpConstF64, Float64)
	one.Const = ir.Float(1)
	next := f.newValue(step, OpAddF64, Float64, k, one)
	b.write(base+loopIndex, step, boxF(step, next))
	if kind != abi.LoopForEach {
		b.write(base+loopResult, step, none(step))
	}
	if keepsElem {
		b.write(base+loopElem, step, undefined(step))
	}
	b.seal(head)
	if typed && kind == abi.LoopFilter {
		// The typed array of the elements kept, which Go makes of them as
		// %TypedArray%.prototype.filter does after its loop, where the
		// species is the type's own constructor as it was made; an exit
		// at its refusal has Go make it from the loop's end, as the
		// method would.
		st := state(cont, 0)
		mk := at(cont, st, OpCall, Tagged, recv, out)
		f.cache(mk).Calls, mk.Index = []*CallSite{{Go: GoNewArray, Argc: NewArrayTyped | NewArrayFill, ThisSlot: -1}}, fp.keep
		cell := f.newValue(cont, OpCallCell, Source, mk)
		made := f.newValue(cont, OpKept, Tagged, cell, mk)
		made.Shadow = cell
		b.assign(dest, cont, made)
	}
	done := f.newValue(cont, OpLoopMark, None)
	done.Const = ir.Value{}
	if reduce {
		b.assign(dest, cont, b.read(base+loopThis, cont))
	}
}

// sortLocals is how many of a sort's slots (sortCall) are its own: the
// array and the comparator, which Go sorts again from at an exit, and the
// merge sort's run width, the half it reads from, a pass's run, the next
// index of each of the two runs a merge reads, the next index it writes,
// and the value it writes there.
const sortLocals = 9

// A sort's slots, past its first.
const (
	sortArray = iota
	sortComparator
	sortWidth
	sortFrom
	sortRun
	sortLeft
	sortRight
	sortTo
	sortValue
)

// sortCall makes x.sort(cmp), the realm's %TypedArray%.prototype.sort on
// a typed array of a number type or Array.prototype.sort on an Array (a
// ForEachSite of abi.LoopSort), a sort of the code's own: the comparator
// inlined in it, as V8's code inlines a builtin's callback, and no call of
// Go for any comparison. Go makes a Float64Array of twice the array's
// length whose first half holds its elements, as numbers -- for an Array
// only where each is a number it has, no hole, nothing undefined, nothing
// a getter answers (GoNewArray's NewArraySort) -- and refuses anything
// else, which leaves the call to Go before anything has run. The code
// sorts that copy, bottom-up, by merges of runs of one, two, four
// elements and on between its two halves, taking the right run's element
// first only where the comparator's result for it and the left's, as
// ToNumber makes it, is below zero: an equal one, NaN, -0, keeps the
// left's first, which makes the sort stable, as the specification asks,
// and no answer the comparator gives can take an index past its run's
// end. So a comparator that is not consistent sorts in some order and
// nothing more -- one the specification leaves to the implementation,
// which need not be the order Go's sort, of other merges, would give.
// Go then writes the copy's sorted half back to the array
// (NewArrayFill), the call's result the array.
//
// Any exit inside -- the comparator's result neither a number nor an
// oddball, which ToNumber would have Go convert; anything else the code
// does not make -- has Go sort the array again from its first element, as
// the built-in sorts it (jitRegLoopFinish). Nothing shows that it did: the
// comparator is a function the VM has found free of effects (its body
// reads its parameters and constants, which are numbers, and computes,
// compares and branches on them, jitRegSortComparator), given the array's
// numbers, so that the calls made of it before the exit did nothing the
// calls made again could tell apart, and the array is the same until the
// copy is written back, in one call of Go after the last comparison.
func (b *builder) sortCall(blk *Block, pc int, fp forEachPlan) {
	f := b.f
	mark := fp.site.Kind
	typed := mark&abi.LoopTyped != 0
	argc := b.callsAt(pc)[0].Argc
	// Its operands where the call's form has them, as forEachCall's.
	cs, _ := b.callSlotsOf(pc, argc, true)
	dest := cs.dest
	recv, fn, cb := b.read(cs.recv, blk), b.read(cs.fn, blk), b.read(cs.arg, blk)
	fr := fp.inline
	call := b.rootState()
	at := func(blk *Block, st *FrameState, op Op, t Type, args ...*Value) *Value {
		v := f.newValue(blk, op, t, args...)
		v.Aux, v.State = int(ir.HostExit), st
		st.addUse()
		return v
	}
	num := func(blk *Block, x float64) *Value {
		v := f.newValue(blk, OpConstF64, Float64)
		v.Const = ir.Float(x)
		return v
	}
	boxF := func(blk *Block, x *Value) *Value { return f.newValue(blk, OpBoxF64, Tagged, x) }
	add := func(blk *Block, x, y *Value) *Value { return f.newValue(blk, OpAddF64, Float64, x, y) }
	less := func(blk *Block, x, y *Value) *Value {
		c := f.newValue(blk, OpCmpF64, Bool, x, y)
		c.Aux = int(ir.Lt)
		return c
	}
	object := at(blk, call, OpObjectOf, Ptr, fn)
	same := at(blk, call, OpSameObject, None, object)
	same.Const = ir.Value{Bits: uint64(fp.site.Fn)}
	if !fp.made {
		// The closure inlined, which a function is.
		is := at(blk, call, OpSameObject, None, at(blk, call, OpObjectOf, Ptr, cb))
		is.Const = ir.Value{Bits: uint64(fr.site.Callee)}
	}
	if typed {
		is := at(blk, call, OpSameObject, None, at(blk, call, OpObjectOf, Ptr, recv))
		is.Const, is.Index = ir.Value{Bits: uint64(fp.site.Proto)}, 1
		at(blk, call, OpIndexedOf, Ptr, recv)
	} else {
		at(blk, call, OpArrayOf, Ptr, recv)
	}
	n := at(blk, call, OpLength, Float64, recv)
	room := at(blk, call, OpFrameRoom, None)
	room.Index = fp.base - b.root.p.Locals + loopSlots(mark)
	{
		// And past them for the comparator's frame, which an exit inside it
		// writes there, and a context for Go to make it from.
		room := at(blk, call, OpFrameRoom, None)
		room.Index = fr.base - b.root.p.Locals + fr.p.Locals + fr.p.StackSize
		room.Const = ir.Value{Bits: uint64(fr.depth)}
	}
	// The copy of the elements, its first half, the second the merges'.
	mk := at(blk, call, OpCall, Tagged, recv, boxF(blk, n))
	form := NewArraySort
	if typed {
		form |= NewArrayTyped
	}
	f.cache(mk).Calls, mk.Index = []*CallSite{{Go: GoNewArray, Argc: form, ThisSlot: -1}}, fp.keep+1
	cell := f.newValue(blk, OpCallCell, Source, mk)
	scratch := f.newValue(blk, OpKept, Tagged, cell, mk)
	scratch.Shadow = cell
	base := fp.base
	zero := boxF(blk, num(blk, 0))
	for i, v := range []*Value{recv, cb, boxF(blk, num(blk, 1)), zero, zero, zero, zero, zero, zero} {
		b.write(base+i, blk, v)
	}
	// The call's result, the array.
	b.assign(dest, blk, recv)
	loop := f.newValue(blk, OpLoopMark, None)
	loop.Const = ir.Value{Bits: uint64(mark)<<32 | uint64(base-b.root.p.Locals+1)}
	state := func(blk *Block, depth int) *FrameState {
		s := f.newState(FrameState{PC: call.PC, Depth: call.Depth, Site: call.Site})
		s.shareSlots(call)
		s.n = base + sortLocals + depth
		f.openEdit()
		for i := base; i < s.n; i++ {
			f.setSlot(s, i, b.read(i, blk))
		}
		return s
	}
	get := func(blk *Block, slot int) *Value {
		return at(blk, state(blk, 0), OpUnboxF64, Float64, b.read(base+slot, blk))
	}
	set := func(blk *Block, slot int, x *Value) { b.write(base+slot, blk, boxF(blk, x)) }
	read := func(blk *Block, k *Value) *Value {
		st := state(blk, 0)
		return at(blk, st, OpElemRead, Float64, at(blk, st, OpIndexedOf, Ptr, scratch), k)
	}
	write := func(blk *Block, k, x *Value) {
		st := state(blk, 0)
		at(blk, st, OpElemWrite, None, at(blk, st, OpIndexedOf, Ptr, scratch), k, x)
	}
	// while is a loop from blk, of cond's test at its head, of the blocks
	// body makes from the loop's first, given the loop's end: it returns
	// the block the code goes on in after the loop.
	while := func(blk *Block, cond func(*Block) *Value, body func(*Block, *Block) *Block) *Block {
		head, first, cont := b.loop(blk)
		head.Header = state(head, 0)
		head.Header.Site = -1
		c := cond(head)
		head.Control = c
		c.Uses++
		body(first, cont)
		b.seal(head)
		return cont
	}
	// copyRun copies the elements from the slot's index up to end from the
	// half at from to the one at to, from the slot sortTo's index on.
	copyRun := func(blk *Block, slot int, end, from, to *Value) *Block {
		return while(blk, func(h *Block) *Value { return less(h, get(h, slot), end) }, func(body, _ *Block) *Block {
			k, t := get(body, slot), get(body, sortTo)
			write(body, add(body, to, t), read(body, add(body, from, k)))
			set(body, slot, add(body, k, num(body, 1)))
			set(body, sortTo, add(body, t, num(body, 1)))
			return body
		})
	}
	end := while(blk, func(h *Block) *Value { return less(h, get(h, sortWidth), n) }, func(pass, _ *Block) *Block {
		w, from := get(pass, sortWidth), get(pass, sortFrom)
		to := f.newValue(pass, OpSubF64, Float64, n, from)
		w2 := add(pass, w, w)
		set(pass, sortRun, num(pass, 0))
		runs := while(pass, func(h *Block) *Value { return less(h, get(h, sortRun), n) }, func(run, _ *Block) *Block {
			lo := get(run, sortRun)
			mid := f.newValue(run, OpMinF64, Float64, add(run, lo, w), n)
			hi := f.newValue(run, OpMinF64, Float64, add(run, lo, w2), n)
			set(run, sortLeft, lo)
			set(run, sortRight, mid)
			set(run, sortTo, lo)
			merged := while(run, func(h *Block) *Value { return less(h, get(h, sortLeft), mid) }, func(body, cont *Block) *Block {
				j := get(body, sortRight)
				// The right run used up: the left's rest is copyRun's.
				_, step := b.loopTest(body, cont, f.newValue(body, OpNot, Bool, less(body, j, hi)), true)
				i := get(step, sortLeft)
				x, y := read(step, add(step, from, j)), read(step, add(step, from, i))
				ops := []*Value{f.newValue(step, OpConst, Tagged), cb, boxF(step, x), boxF(step, y)}
				ops[0].Const = ir.Value{Kind: ir.Undefined}
				for k, v := range ops {
					b.write(base+sortLocals+k, step, v)
				}
				res, after := b.inlineCallback(step, fr, ops, base+sortLocals+2, state(step, len(ops)))
				// The result as ToNumber makes it, of a number, a boolean,
				// undefined or null (OpOddballF64): NaN is no answer below
				// zero. Anything else is Go's to convert.
				r := at(after, state(after, 0), OpOddballF64, Float64, res)
				// The left's element, unless the right's sorts before it.
				set(after, sortValue, y)
				set(after, sortLeft, add(after, i, num(after, 1)))
				yes, next := b.loopTest(after, cont, less(after, r, num(after, 0)), false)
				set(yes, sortValue, x)
				set(yes, sortLeft, i)
				set(yes, sortRight, add(yes, j, num(yes, 1)))
				t := get(next, sortTo)
				write(next, add(next, to, t), get(next, sortValue))
				set(next, sortTo, add(next, t, num(next, 1)))
				return next
			})
			left := copyRun(merged, sortLeft, mid, from, to)
			right := copyRun(left, sortRight, hi, from, to)
			set(right, sortRun, add(right, lo, w2))
			return right
		})
		// The next pass reads what this one wrote.
		set(runs, sortFrom, to)
		set(runs, sortWidth, w2)
		return runs
	})
	// The sorted elements in the first half, where Go reads them.
	from := get(end, sortFrom)
	set(end, sortTo, num(end, 0))
	copied := while(end, func(h *Block) *Value { return less(h, get(h, sortTo), from) }, func(body, _ *Block) *Block {
		t := get(body, sortTo)
		write(body, t, read(body, add(body, from, t)))
		set(body, sortTo, add(body, t, num(body, 1)))
		return body
	})
	st := state(copied, 0)
	back := at(copied, st, OpCall, Tagged, recv, scratch)
	f.cache(back).Calls, back.Index = []*CallSite{{Go: GoNewArray, Argc: form | NewArrayFill, ThisSlot: -1}}, fp.keep
	done := f.newValue(copied, OpLoopMark, None)
	done.Const = ir.Value{}
	b.next = copied
}

// IndexOfFeedback is Feedback that knows a method call at a PC calls one
// of the realm's searches, String.prototype's or Array.prototype's indexOf,
// lastIndexOf or includes, at the address it gives -- which, its kind --
// on a string or an array: native code makes it a call of Go of its own
// (indexOfCall).
type IndexOfFeedback interface {
	IndexOf(pc int) (fn uintptr, kind int, ok bool)
}

// indexOfAt is the search the method call at pc of the root, one Go makes
// (a GoCall site) of one argument or two, calls (IndexOfFeedback).
func (b *builder) indexOfAt(pc int) (fn uintptr, kind int, ok bool) {
	if b.fb == nil || b.cur != b.root || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) || b.fb.Generic(pc) || !b.goCalls() {
		return 0, 0, false
	}
	calls := b.callsAt(pc)
	if len(calls) != 1 || calls[0].Go != GoCall || !calls[0].Method || calls[0].Argc != 1 && calls[0].Argc != 2 {
		return 0, 0, false
	}
	if _, ok := b.callSlotsOf(pc, calls[0].Argc, true); !ok {
		return 0, 0, false
	}
	f, ok := b.fb.(IndexOfFeedback)
	if !ok {
		return 0, 0, false
	}
	return f.IndexOf(pc)
}

// indexOfCall makes x.indexOf(t), or x.indexOf(t, i), or another search,
// the call at pc of the function at fn, of the VM's kind: checked to call
// it, a call of Go (GoIndexOf) whose result is the call's, kept where the
// call's would be. Go refuses a receiver or an argument the built-in would
// run anything for; those leave for Go to make the call after an exit.
func (b *builder) indexOfCall(blk *Block, pc int, fn uintptr, kind int, gd *guarder) {
	f := b.f
	argc := b.callsAt(pc)[0].Argc
	cs, _ := b.callSlotsOf(pc, argc, true)
	recv, callee := b.read(cs.recv, blk), b.read(cs.fn, blk)
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, callee)
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	args := []*Value{recv}
	for k := range argc {
		args = append(args, b.read(cs.arg+k, blk))
	}
	site := &CallSite{Go: GoIndexOf, Argc: kind << 1, ThisSlot: -1}
	if argc == 2 {
		site.Argc |= IndexOfFrom
	}
	call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
	f.cache(call).Calls, call.Index = []*CallSite{site}, b.callKeepAt(pc)
	kept := f.newValue(blk, OpCallCell, Source, call)
	r := f.newValue(blk, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.assign(cs.dest, blk, r)
}

// LeafFeedback is Feedback that knows the call at a PC, a method call or a
// plain one, calls a leaf, a built-in the VM calls without a frame where
// it runs no script, at the address it gives: native code makes it a call
// of Go of its own (leafCall).
type LeafFeedback interface {
	Leaf(pc int) (fn uintptr, ok bool)
}

// ViaLeafFeedback is Feedback that knows the method call at a PC, register
// code's, is G.call(x, ...) through the realm's Function.prototype.call, at
// call, of a leaf G at fn, as V8's call reducer reduces a call of
// Function.prototype.call whose target it knows: native code checks the
// method is call and its receiver G, and makes G's call of x and the rest
// as a leaf's call of Go (windowViaLeaf) -- hasOwnProperty.call(o, k).
type ViaLeafFeedback interface {
	ViaLeaf(pc int) (call, fn uintptr, ok bool)
}

// LeafKindFeedback is LeafFeedback that knows which of the leaves native
// code makes itself where it can (LeafKind) the function at fn is, as V8's
// call reducer knows a builtin by its id: leafCall makes those natively
// first, the leaf's call made where native code says nothing.
type LeafKindFeedback interface {
	LeafKind(fn uintptr) LeafKind
}

// LeafKind is a leaf native code makes itself where it can: the
// realm's String.prototype.codePointAt (LeafCodePointAt) and
// String.prototype.at (LeafAt), each of one argument, made of a flat
// string's code units (OpStringCodePointOr, OpStringAtOr); Number.isNaN,
// isFinite, isInteger and isSafeInteger (LeafNumberIsNaN...), of any value,
// which convert nothing (OpNumberIs). LeafOther is any other.
type LeafKind uint8

const (
	LeafOther LeafKind = iota
	LeafCodePointAt
	LeafAt
	LeafNumberIsNaN
	LeafNumberIsFinite
	LeafNumberIsInteger
	LeafNumberIsSafeInteger
)

// reads reports whether the leaf's answer is of its receiver's: a method
// it makes only of a method call.
func (k LeafKind) reads() bool { return k == LeafCodePointAt || k == LeafAt }

// callSlots are where the operands of a call are, as the form of its code
// has them: register code's in its window (ir.Instruction's Window), the
// function first, the receiver after it, the arguments from the third
// register on, its result to the instruction's register (Dest); a slot
// program's (ir.Program's Origin nil) on the operand stack, the receiver
// below the function, the arguments above it, its result where the first
// of them was.
type callSlots struct {
	fn, recv, arg, dest int
}

// callSlotsOf is where the operands of the call at pc of the frame being
// translated are, of argc arguments, a method's where method says (recv is
// -1 otherwise), or false where its instruction does not have them so.
func (b *builder) callSlotsOf(pc, argc int, method bool) (callSlots, bool) {
	p, in := b.p, &b.p.Code[pc]
	if in.Window {
		// In the frame being translated, an inlined callee's too: its
		// registers past its base, as its instructions are read (shifted).
		if in.Extra != argc {
			return callSlots{}, false
		}
		a := b.cur.base + in.Left.Slot
		s := callSlots{fn: a, recv: -1, arg: a + 2, dest: b.cur.base + in.Dest}
		if method {
			s.recv = a + 1
		}
		return s, true
	}
	n := argc + 1
	if method {
		n++
	}
	if pc+1 >= len(p.Maps) || p.Maps[pc].Depth < n || p.Maps[pc+1].Depth != p.Maps[pc].Depth-n+1 {
		return callSlots{}, false
	}
	base := b.cur.base + p.Locals + p.Maps[pc].Depth - n
	s := callSlots{fn: base, recv: -1, arg: base + 1, dest: base}
	if method {
		s.recv, s.fn, s.arg = base, base+1, base+2
	}
	return s, true
}

// leafAt is the leaf the call at pc of the root, one Go makes (a GoCall
// site) of at most abi.MaxGoArgs-2 arguments, calls (LeafFeedback).
func (b *builder) leafAt(pc int) (uintptr, bool) {
	if b.fb == nil || b.cur != b.root || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) || b.fb.Generic(pc) || !b.goCalls() {
		return 0, false
	}
	calls := b.callsAt(pc)
	if len(calls) != 1 || calls[0].Go != GoCall || calls[0].New || calls[0].Argc > abi.MaxGoArgs-2 {
		return 0, false
	}
	n := calls[0].Argc + 1
	if calls[0].Method {
		n++
	}
	if b.p.Maps[pc].Depth < n || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-n+1 {
		return 0, false
	}
	f, ok := b.fb.(LeafFeedback)
	if !ok {
		return 0, false
	}
	return f.Leaf(pc)
}

// leafCall makes the call at pc of the leaf at fn: checked to call it, a
// call of Go (GoLeaf) of the function, the receiver of a method call and
// the arguments, whose result is the call's, kept where the call's would
// be.
func (b *builder) leafCall(blk *Block, pc int, fn uintptr, gd *guarder) {
	c := b.callsAt(pc)[0]
	sp := b.p.Locals + b.p.Maps[pc].Depth
	base := sp - c.Argc - 1
	if c.Method {
		base--
	}
	at := base
	var recv *Value
	if c.Method {
		recv, at = b.read(base, blk), base+1
	}
	callee := b.read(at, blk)
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, callee)
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	args := []*Value{callee}
	if c.Method {
		args = append(args, recv)
	}
	for k := range c.Argc {
		args = append(args, b.read(at+1+k, blk))
	}
	site := &CallSite{Go: GoLeaf, Argc: c.Argc, Method: c.Method, ThisSlot: -1, Leaf: fn, Unused: b.resultUnused(pc, base)}
	b.createPool(pc, fn, site)
	b.leafMade(blk, pc, fn, c, args, site, gd, func(blk *Block, v *Value) { b.write(base, blk, v) })
}

// ExecReuseFeedback is Feedback that knows, for register code's call at a
// PC of the leaf at fn, the realm's exec, whether its array may be made
// again in the memory of the call's last (the VM's regexpreuse.go): the
// VM's description of the call's site, its low bit set where the array's
// pieces may be made again too, nothing reading the local that holds one
// past the call before it is written; 0 for no such call (CallSite's
// Reuse).
type ExecReuseFeedback interface {
	ExecReuse(pc int, fn uintptr) uint32
}

// CreatePoolFeedback is Feedback that knows, for a call at a PC of the
// leaf at fn, the abi.ObjectPool of the objects native code makes the
// call's of where the leaf is the realm's Object.create, called with a
// receiver and one argument: an object of the pool's prototype (its
// Proto), the pool's next, as the leaf would make it, Go's leaf making any
// other and filling the pool again (CallSite's Pool and Give, mir's
// createMake), as V8's code allocates the object CreateObjectWithoutProperties
// would. 0 for any other.
type CreatePoolFeedback interface {
	CreatePool(pc int, fn uintptr) uintptr
}

// createPool gives site, the call at pc of the leaf at fn, its pool of
// objects where the feedback has one (CreatePoolFeedback).
func (b *builder) createPool(pc int, fn uintptr, site *CallSite) {
	f, ok := b.fb.(CreatePoolFeedback)
	if !ok || !site.Method || site.Argc != 1 {
		return
	}
	if pool := f.CreatePool(pc, fn); pool != 0 {
		site.Pool, site.Give = pool, pool
	}
}

// leafMade makes the call at pc of the leaf at fn, c its call's site, args
// its operands -- the function, the receiver of a method call, the
// arguments -- the function checked to be the leaf: natively where native
// code makes the leaf itself (leafKind), a call of Go (GoLeaf, site)
// otherwise, its result given to put, in the block it is made in.
func (b *builder) leafMade(blk *Block, pc int, fn uintptr, c *CallSite, args []*Value, site *CallSite, gd *guarder, put func(*Block, *Value)) {
	f := b.f
	if kind := b.leafKind(fn); kind != LeafOther && c.Argc == 1 && (c.Method || !kind.reads()) {
		if kind >= LeafNumberIsNaN {
			// Of the one argument alone, whatever it is: no call.
			is := f.newValue(blk, OpNumberIs, Bool, args[len(args)-1])
			is.Aux = int(kind - LeafNumberIsNaN)
			put(blk, f.newValue(blk, OpBoxBool, Tagged, is))
			return
		}
		b.leafNative(blk, pc, kind, args, site, put)
		return
	}
	call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
	f.cache(call).Calls, call.Index = []*CallSite{site}, b.callKeepAt(pc)
	kept := f.newValue(blk, OpCallCell, Source, call)
	r := f.newValue(blk, OpKept, Tagged, kept, call)
	r.Shadow = kept
	put(blk, r)
}

// windowLeaf makes register code's call at pc, in, of the leaf at fn, c
// its call's site, as leafCall makes a slot program's: its operands the
// call's window's registers from in.Left -- the function, the receiver,
// the arguments -- the function checked to be the leaf (a host exit, where
// it is not, has Go run the instruction whole), its result to the call's
// register.
func (b *builder) windowLeaf(blk *Block, pc int, in ir.Instruction, c *CallSite, fn uintptr, gd *guarder) {
	a := in.Left.Slot
	callee := b.read(a, blk)
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, callee)
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	args := []*Value{callee}
	if c.Method {
		args = append(args, b.read(a+1, blk))
	}
	for k := range c.Argc {
		args = append(args, b.read(a+2+k, blk))
	}
	site := &CallSite{Go: GoLeaf, Argc: c.Argc, Method: c.Method, ThisSlot: -1, Leaf: fn}
	// Nothing reads the result but its truth: exec makes no array, replace
	// no string (abi.GoCallState's Unused).
	site.Unused = b.windowResultUnused(pc)
	if f, ok := b.fb.(ExecReuseFeedback); ok && !site.Unused && c.Method {
		site.Reuse = f.ExecReuse(pc, fn)
	}
	if c.Method {
		// regexp_lit's: its RegExp, the pool's, given back to it.
		site.Give = c.Give
	}
	b.createPool(pc, fn, site)
	b.leafMade(blk, pc, fn, c, args, site, gd, func(blk *Block, v *Value) { b.assign(in.Dest, blk, v) })
}

// windowViaLeaf makes register code's method call at pc, in, c its call's
// site, G.call(x, ...) of the leaf at fn through the realm's
// Function.prototype.call at call (ViaLeafFeedback): the method, its
// window's register 0, checked to be call, and its receiver, register 1,
// G -- where either is not, a host exit has Go run the instruction whole --
// then G's call, its receiver x, register 2, and the arguments from
// register 3, made as windowLeaf makes a method call of the leaf: a call of
// Go of its own (GoLeaf), or natively where native code makes the leaf
// itself (leafMade), its result to the call's register.
func (b *builder) windowViaLeaf(blk *Block, pc int, in ir.Instruction, c *CallSite, call, fn uintptr, gd *guarder) {
	a := in.Left.Slot
	method := gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(a, blk))
	isCall := gd.guard(OpSameObject, None, ir.HostExit, method)
	isCall.Const = ir.Value{Bits: uint64(call)}
	g := b.read(a+1, blk)
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, g)
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	args := []*Value{g, b.read(a+2, blk)}
	for k := 1; k < c.Argc; k++ {
		args = append(args, b.read(a+2+k, blk))
	}
	leaf := &CallSite{Argc: c.Argc - 1, Method: true}
	site := &CallSite{Go: GoLeaf, Argc: c.Argc - 1, Method: true, ThisSlot: -1, Leaf: fn}
	site.Unused = b.windowResultUnused(pc)
	b.leafMade(blk, pc, fn, leaf, args, site, gd, func(blk *Block, v *Value) { b.assign(in.Dest, blk, v) })
}

// windowResultUnused is resultUnused of register code's call at pc, whose
// result is its register (Dest), which no instruction pops: by the
// program's liveness (Live), nothing reads the register after the call --
// a statement's call -- or the instruction after it tests its truth or
// negates it and nothing reads it after that, but where a branch jumps
// where it is falsy, which keeps it, as && keeps it. Not a register a
// closure captures, whose cell its closures read.
func (b *builder) windowResultUnused(pc int) bool {
	p := b.p
	d := p.Code[pc].Dest
	if d < 0 || p.IsCaptured(d) || pc+2 >= len(p.Code) {
		return false
	}
	dead := func(at int) bool { return at >= 0 && at < len(p.Code) && !p.LiveAt(at, d) }
	if dead(pc + 1) {
		return true
	}
	switch in := p.Code[pc+1]; in.Op {
	case ir.Branch:
		return in.Operator == ir.Truth && in.Left == ir.Slot(d) && dead(pc+2) && (!in.When || dead(in.Target))
	case ir.Unary:
		return in.Operator == ir.Not && in.Left == ir.Slot(d) && (in.Dest == d || dead(pc+2))
	}
	return false
}

// resultUnused reports whether nothing reads the result of the call at pc,
// in slot at, but, at most, whether it is truthy (CallSite's Unused): the
// next instruction pops it (a drop, a Nop the depth falls by one at),
// branches on its truth and pops it -- or keeps it where it is falsy
// alone, as && keeps it -- or negates it in its slot.
func (b *builder) resultUnused(pc, at int) bool {
	p := b.p
	if pc+2 >= len(p.Code) || pc+2 >= len(p.Maps) {
		return false
	}
	d := p.Maps[pc+1].Depth
	if d < 1 || p.Locals+d-1 != at {
		return false
	}
	top := ir.Slot(at)
	switch in := p.Code[pc+1]; in.Op {
	case ir.Nop:
		return p.Maps[pc+2].Depth == d-1
	case ir.Branch:
		if in.Operator != ir.Truth || in.Left != top || p.Maps[pc+2].Depth != d-1 {
			return false
		}
		// Where it jumps: popped, or, jumping where it is falsy, kept.
		return !in.When || in.Target >= 0 && in.Target < len(p.Maps) && p.Maps[in.Target].Depth == d-1
	case ir.Unary:
		return in.Operator == ir.Not && in.Left == top && in.Dest == at
	}
	return false
}

// leafKind is which of the leaves native code makes itself the function at
// fn is (LeafKindFeedback), or LeafOther.
func (b *builder) leafKind(fn uintptr) LeafKind {
	if f, ok := b.fb.(LeafKindFeedback); ok {
		return f.LeafKind(fn)
	}
	return LeafOther
}

// leafNative makes the call at pc of a leaf native code makes itself where
// it can (kind), its function checked, args the leaf's call's operands --
// the function, the receiver, the one argument -- as V8 reduces a call of
// a builtin it knows: the result made of the receiver's code units where
// native code says what it is (OpStringCodePointOr, OpStringAtOr), and
// the leaf's call of Go (GoLeaf, site) made on a branch of its own where
// it does not (split), native code going on after either, the result
// where the call's would be.
func (b *builder) leafNative(blk *Block, pc int, kind LeafKind, args []*Value, site *CallSite, put func(*Block, *Value)) {
	f := b.f
	recv, key := args[1], args[2]
	var made *Value
	var miss, hit *Block
	st := b.state(blk, pc)
	switch kind {
	case LeafCodePointAt:
		code := f.newValue(blk, OpStringCodePointOr, Float64, recv, key)
		zero := f.newValue(blk, OpConstF64, Float64)
		zero.Const = ir.Float(0)
		none := f.newValue(blk, OpCmpF64, Bool, code, zero)
		none.Aux = int(ir.Lt)
		miss, hit, _ = b.split(blk, none, true)
		made = f.newValue(hit, OpBoxF64, Tagged, code)
	default:
		cell := f.newValue(blk, OpStringAtOr, Source, recv, key)
		none := f.newValue(blk, OpCellNone, Bool, cell)
		miss, hit, _ = b.split(blk, none, true)
		made = f.newValue(hit, OpLoadCell, Tagged, cell)
		made.Shadow = cell
	}
	put(hit, made)
	call := f.newValue(miss, OpCall, Tagged, args...)
	f.cache(call).Calls = []*CallSite{site}
	call.Aux, call.State, call.Index = int(ir.HostExit), st, b.callKeepAt(pc)
	st.addUse()
	kept := f.newValue(miss, OpCallCell, Source, call)
	r := f.newValue(miss, OpKept, Tagged, kept, call)
	r.Shadow = kept
	put(miss, r)
}

// BigStringFeedback is Feedback that knows a method call at a PC, of no
// argument or one, calls the realm's BigInt.prototype.toString, at the
// address it gives, on a BigInt: native code makes it a call of Go of its
// own (bigStringCall), as V8's code calls its BigInt-to-string builtin.
type BigStringFeedback interface {
	BigString(pc int) (fn uintptr, ok bool)
}

// bigStringAt is BigInt.prototype.toString if the method call at pc of the
// root, one Go makes (a GoCall site) of no argument or one, calls it
// (BigStringFeedback).
func (b *builder) bigStringAt(pc int) (uintptr, bool) {
	if b.fb == nil || b.cur != b.root || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) || b.fb.Generic(pc) || !b.goCalls() {
		return 0, false
	}
	calls := b.callsAt(pc)
	if len(calls) != 1 || calls[0].Go != GoCall || !calls[0].Method || calls[0].Argc > 1 {
		return 0, false
	}
	if _, ok := b.callSlotsOf(pc, calls[0].Argc, true); !ok {
		return 0, false
	}
	f, ok := b.fb.(BigStringFeedback)
	if !ok {
		return 0, false
	}
	return f.BigString(pc)
}

// bigStringCall makes x.toString() or x.toString(radix), the call at pc of
// the function at fn: checked to call it, a call of Go (GoBigString) whose
// result is the call's, kept where the call's would be.
func (b *builder) bigStringCall(blk *Block, pc int, fn uintptr, gd *guarder) {
	f := b.f
	argc := b.callsAt(pc)[0].Argc
	cs, _ := b.callSlotsOf(pc, argc, true)
	recv, callee := b.read(cs.recv, blk), b.read(cs.fn, blk)
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, callee)
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	args := []*Value{recv}
	for k := range argc {
		args = append(args, b.read(cs.arg+k, blk))
	}
	call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
	f.cache(call).Calls, call.Index = []*CallSite{{Go: GoBigString, Argc: argc, ThisSlot: -1}}, b.callKeepAt(pc)
	kept := f.newValue(blk, OpCallCell, Source, call)
	r := f.newValue(blk, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.assign(cs.dest, blk, r)
}

// ParseIntFeedback is Feedback that knows a plain call at a PC of one
// argument calls the realm's parseInt, at the address it gives, with a
// number: native code makes it parseInt of a number, as V8's JSCallReducer
// reduces it (parseIntCall).
type ParseIntFeedback interface {
	ParseInt(pc int) (fn uintptr, ok bool)
}

// parseIntAt is the realm's parseInt the call at pc of the root, one Go
// makes (a GoCall site) of one argument, calls (ParseIntFeedback).
func (b *builder) parseIntAt(pc int) (uintptr, bool) {
	if b.fb == nil || b.cur != b.root || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) || b.fb.Generic(pc) {
		return 0, false
	}
	calls := b.callsAt(pc)
	if len(calls) != 1 || calls[0].Go != GoCall || calls[0].Method || calls[0].New || calls[0].Argc != 1 {
		return 0, false
	}
	if _, ok := b.callSlotsOf(pc, 1, false); !ok {
		return 0, false
	}
	f, ok := b.fb.(ParseIntFeedback)
	if !ok {
		return 0, false
	}
	return f.ParseInt(pc)
}

// parseIntCall makes parseInt(x), the call at pc of the realm's parseInt at
// fn, of a number, as V8's JSCallReducer reduces it: its integer part,
// where ToString writes the number in decimal with no exponent -- its
// magnitude below 1e21, and 1 or more, or 0, or at least 1e-6 where it is
// positive -- the integer part of a zero or of a fraction +0, as the
// string "0.5" or "0" parses. Any other number -- NaN, an infinity, one
// of an exponent, one in (-1, 0), which parses as -0 -- and any other
// argument or function, are Go's call.
func (b *builder) parseIntCall(blk *Block, pc int, fn uintptr, gd *guarder, boxF func(*Value) *Value) {
	f := b.f
	cs, _ := b.callSlotsOf(pc, 1, false)
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(cs.fn, blk))
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	x := gd.guard(OpUnboxF64, Float64, ir.HostExit, b.read(cs.arg, blk))
	constant := func(c float64) *Value {
		v := f.newValue(blk, OpConstF64, Float64)
		v.Const = ir.Float(c)
		return v
	}
	cmp := func(op ir.Operator, l, r *Value) *Value {
		v := f.newValue(blk, OpCmpF64, Bool, l, r)
		v.Aux = int(op)
		return v
	}
	number := func(test *Value) *Value { return f.newValue(blk, OpBoolToF64, Float64, test) }
	a := f.newValue(blk, OpAbsF64, Float64, x)
	gd.guard(OpCheckTrue, None, ir.HostExit, f.newValue(blk, OpBoxBool, Tagged, cmp(ir.Lt, a, constant(1e21))))
	some := f.newValue(blk, OpMaxF64, Float64, number(cmp(ir.Ge, a, constant(1))),
		f.newValue(blk, OpMaxF64, Float64, number(cmp(ir.Eq, x, constant(0))), number(cmp(ir.Ge, x, constant(1e-6)))))
	gd.guard(OpCheckTrue, None, ir.HostExit, f.newValue(blk, OpBoxBool, Tagged, cmp(ir.Gt, some, constant(0))))
	// The integer part, a zero's +0: the only number left whose integer
	// part is -0 is -0 itself.
	r := f.newValue(blk, OpAddF64, Float64, f.newValue(blk, OpTruncF64, Float64, x), constant(0))
	b.assign(cs.dest, blk, boxF(r))
}

// SubstringFeedback is Feedback that knows a method call at a PC calls
// String.prototype.substring, slice, substr or charAt, the realm's, at the
// address it gives, and which (OpSubstringChar's Aux): s.substring(i, j),
// by its target, whatever read it.
type SubstringFeedback interface {
	Substring(pc int) (fn uintptr, kind int, ok bool)
}

// CharCodeFeedback is Feedback that knows a method call of one argument at
// a PC calls String.prototype.charCodeAt, the realm's (abi.Context's
// CharCodeAt): s.charCodeAt(i), by its target, whatever read it.
type CharCodeFeedback interface {
	CharCode(pc int) bool
}

// FromCharCodeFeedback is Feedback that knows a method call of one argument
// at a PC calls String.fromCharCode, the realm's, at the address it
// returns.
type FromCharCodeFeedback interface {
	FromCharCode(pc int) (uintptr, bool)
}

// IntrinsicFeedback is Feedback that knows the calls at a PC of an
// intrinsic (Intrinsic): a method call of one argument, Math.sqrt(x), or
// two, Math.pow(x, y).
type IntrinsicFeedback interface {
	Intrinsic(pc int) (Intrinsic, bool)
}

// ConstantFeedback is Feedback that keeps string constants in cells: the
// address of the cell the constant pushed at pc is in, which the VM keeps
// alive and unchanged (ir.StringConst).
type ConstantFeedback interface {
	StringCell(pc int) (uintptr, bool)
}

// StringContentFeedback is ConstantFeedback that says what the string
// constant pushed at pc is, too: its WTF-8, which the cell's string holds
// flat. Native code compares a string with it by its length and its bytes
// (OpConstCell's ConstString).
type StringContentFeedback interface {
	StringContent(pc int) (string, bool)
}

// TypeofFeedback is Feedback that keeps the runtime's strings typeof
// answers in a table of cells, eight values in the order of TypeofUndefined
// and the rest, which the VM keeps alive and unchanged: its address
// (OpTypeofCell), or false where it keeps none.
type TypeofFeedback interface {
	TypeofCells() (uintptr, bool)
}

// The types' places in TypeofFeedback's table.
const (
	TypeofUndefined = iota
	TypeofObject
	TypeofBoolean
	TypeofNumber
	TypeofString
	TypeofSymbol
	TypeofBigInt
	TypeofFunction
)

// BigIntConstFeedback is Feedback that keeps BigInt constants in cells, as
// ConstantFeedback keeps strings: the address of the cell the constant
// pushed at pc is in, and, for one held in its value (kind
// BigConstInt64 or BigConstUint64, not BigConstHeap), its bits, which
// the code's arithmetic takes as a constant (OpConstI64).
type BigIntConstFeedback interface {
	BigIntCell(pc int) (cell uintptr, bits uint64, kind int, ok bool)
}

// LiteralSite is an object literal's site: the address of the
// abi.ObjectPool its objects come from, made as the VM makes the literal's,
// which the VM keeps alive.
type LiteralSite struct {
	Pool uintptr
	// Shared marks an array literal whose arrays the pool holds whole, its
	// elements constants they share (the VM's objSharedElems): the take
	// writes no element, and the literal's operands are not the code's.
	Shared bool
}

// StringFeedback is Feedback that knows the property reads whose receivers
// have been strings (StringProperty): a method's, which String.prototype
// has as its own data property, by the shape it has (PropertySite's Shape)
// and the property's index in its table. A read whose receivers have been
// another primitive (PropertySite's Primitive) -- BigInts, numbers,
// booleans -- is known so where the prototype the realm fixed for that
// kind, the site's first holder, has the method.
type StringFeedback interface {
	StringProperty(pc int) (PropertySite, bool)
}

// Primitive is a kind of primitive a property read's receivers have been
// (StringFeedback), whose properties are the prototype's the realm fixed
// for the kind: String.prototype, BigInt.prototype, Number.prototype,
// Boolean.prototype.
type Primitive uint8

// The kinds of Primitive.
const (
	PrimitiveString Primitive = iota
	PrimitiveBigInt
	PrimitiveNumber
	PrimitiveBoolean
)

// primitiveType is the type typeof answers for a primitive of kind k
// (OpTypeIs's Index).
func primitiveType(k Primitive) int {
	switch k {
	case PrimitiveBigInt:
		return ir.TypeBigInt
	case PrimitiveNumber:
		return ir.TypeNumber
	case PrimitiveBoolean:
		return ir.TypeBoolean
	}
	return ir.TypeString
}

// ElementFeedback is Feedback that knows the element reads whose receivers
// have been typed arrays (TypedRead): a read whose result no native
// operation takes as a number (ir.Instruction's Reference) is made there
// as a typed array's, its number boxed, rather than by an element's cell,
// which a typed array has none of -- as V8's keyed loads follow their
// feedback's elements kind. Any other receiver Go reads.
type ElementFeedback interface {
	TypedRead(pc int) bool
}

// BigElementFeedback is Feedback that knows the element reads, read as
// they are, that have met BigInt64Arrays, or BigUint64Arrays (unsigned),
// whose elements native code reads as BigInts (OpBigElemRead), boxed
// where a use needs a value, as V8's code reads a BigInt64Array's element
// as a word.
type BigElementFeedback interface {
	TypedBigRead(pc int) (unsigned, ok bool)
}

// typedBigRead is BigElementFeedback's TypedBigRead, the Type the element
// is read as: Int64, or Uint64 for a BigUint64Array's.
func (b *builder) typedBigRead(pc int) (Type, bool) {
	f, ok := b.fb.(BigElementFeedback)
	if !ok {
		return None, false
	}
	unsigned, ok := f.TypedBigRead(pc)
	if unsigned {
		return Uint64, ok
	}
	return Int64, ok
}

// KeyedFeedback is Feedback that knows the element reads, read as they
// are (ir.Instruction's Reference), whose receivers have been objects
// read by a key native code does not find -- obj[name], a property by its
// name's string -- which Go makes from native code (GoIndex).
type KeyedFeedback interface {
	KeyedRead(pc int) bool
}

// KeyedWriteFeedback is Feedback that knows the element writes whose
// receivers have been objects native code does not store to -- an
// ordinary object's property by an index or a name, obj[i] = v -- which
// Go makes from native code (GoKeyedWrite).
type KeyedWriteFeedback interface {
	KeyedWrite(pc int) bool
}

// keyedWrite reports whether the element write at pc is made by Go from
// native code (KeyedWriteFeedback, GoKeyedWrite).
func (b *builder) keyedWrite(pc int) bool {
	if b.p.Code[pc].Op != ir.ArrayWrite || !b.goCalls() {
		return false
	}
	f, ok := b.fb.(KeyedWriteFeedback)
	return ok && f.KeyedWrite(pc)
}

// KeyedStoreFeedback is Feedback that knows the keyed stores' sites: an
// element's write (ir.ArrayWrite), o[k] = v, or a computed key's define,
// { [k]: v } or a class's field (a whole ir.Host of three operands: the
// object, the key, the value), whose cache remembers the keys it stored
// and, for each, the shapes it stored to (the VM's keyed store cache), as
// V8's keyed store inline caches remember the names and maps they met.
type KeyedStoreFeedback interface {
	KeyedStore(pc int) (KeyedStoreSite, bool)
}

// KeyedStoreSite is a keyed store's site: for each key it met, a string
// the VM's table of key strings names by its atom (Func's KeyAtoms), the
// write of that key as a named write's site has it -- Key its atom, the
// properties objects of Shape and of Cases' shapes have, the additions
// Add and Adds make, a define's marked so (PropertyAdd's Define, whose
// Key is the key's) where Define says the site is one.
type KeyedStoreSite struct {
	Keys   []PropertySite
	Define bool
}

// keyedStore is the site of the keyed store at pc where native code can
// tell its keys -- the VM's table of key strings is the code's to read --
// and the store is the root frame's, whose block it splits (keyedStores).
func (b *builder) keyedStore(pc int) (KeyedStoreSite, bool) {
	kf, ok := b.fb.(KeyedStoreFeedback)
	if !ok || b.cur != b.root || b.f.KeyAtoms == 0 {
		return KeyedStoreSite{}, false
	}
	s, ok := kf.KeyedStore(pc)
	return s, ok && len(s.Keys) != 0
}

// keyedStores makes the stores of value to obj by key that site knows,
// ahead of the store the instruction makes otherwise: a branch for each of
// its keys on whether key is a string naming the key's atom (OpKeyIs), to
// the key's write of the shapes the site met (OpPropWrite, as a named
// write's site makes it), as V8's keyed store inline caches check the
// name, then the map. An object none of the key's shapes deoptimizes, as
// a named write's map check does, and so does a string none of the keys
// is that the VM's table of key strings names an atom by, the
// interpreter's store teaching the site's cache the pair for the code
// compiled next. Once the site has deoptimized (Generic), such an object
// exits to Go, which stores and goes on natively, and such a key is stored
// as any other key is -- a string the table does not know, made anew for
// each store, no string at all: in the block keyedStores returns, where
// the instruction makes its store as it would have. The instructions
// after go on where all meet.
func (b *builder) keyedStores(blk *Block, site KeyedStoreSite, obj, key, value *Value, generic bool, state func() *FrameState) *Block {
	f := b.f
	var cont *Block
	kind := ir.GuardExit
	if generic {
		kind = ir.HostExit
	}
	at := func(blk *Block, op Op, t Type, args ...*Value) *Value {
		v := f.newValue(blk, op, t, args...)
		v.Aux, v.State = int(kind), state()
		v.State.addUse()
		return v
	}
	for _, ks := range site.Keys {
		is := f.newValue(blk, OpKeyIs, Bool, key)
		is.Const = ir.Value{Bits: uint64(ks.Key)}
		yes, no, c := b.split(blk, is, true)
		if cont == nil {
			cont = c
		}
		object := at(yes, OpObjectOf, Ptr, obj)
		w := at(yes, OpPropWrite, None, object, value)
		w.Const, w.Index, w.Key = ir.Value{Bits: uint64(ks.Shape)}, int(ks.Index), ks.Key
		f.setProperty(w, nil, ks.Cases, ks.Add, ks.Adds)
		blk = no
	}
	if !generic {
		// A key the site did not know, by its atom.
		known := f.newValue(blk, OpKeyIs, Bool, key)
		known.Index = KeyIsAny
		at(blk, OpCheckTrue, None, f.newValue(blk, OpBoxBool, Tagged, f.newValue(blk, OpNot, Bool, known)))
	}
	b.next = cont
	return blk
}

// StringElementFeedback is Feedback that knows the element reads whose
// receivers have been strings (OpStringChar): such a read is compiled for
// a string, which native code makes the element of, and any other
// receiver exits.
type StringElementFeedback interface {
	StringRead(pc int) bool
}

// stringReceiver reports whether the element read at pc is compiled for a
// string's (StringElementFeedback).
func (b *builder) stringReceiver(pc int) bool {
	f, ok := b.fb.(StringElementFeedback)
	return ok && f.StringRead(pc)
}

// typedReceiver reports whether the element read at pc, of recv, is
// compiled to read a typed array's number: one that has met typed arrays
// (typedRead), or one of what a global read found holding a typed array
// (GlobalSite's Typed).
func (b *builder) typedReceiver(pc int, recv *Value) bool {
	return b.typedArrays[recv] != 0 || b.typedRead(pc)
}

// typedRead reports whether the element read at pc has met typed arrays
// (ElementFeedback).
func (b *builder) typedRead(pc int) bool {
	f, ok := b.fb.(ElementFeedback)
	return ok && f.TypedRead(pc)
}

// TypedKindFeedback is Feedback that knows, for an element read or write
// of a number at a PC, the element types of the typed arrays it has met
// (TypedSite): one that met a single type native code reads and writes
// is compiled for it, inline, the type checked, an Array's element out of
// line -- as V8's keyed loads and stores follow their feedback's elements
// kind -- and one that met none, or several, tells them apart as it runs.
type TypedKindFeedback interface {
	TypedKind(pc int) (TypedSite, bool)
}

// TypedSite is what an element read or write of a number has met of typed
// arrays: Kinds has bit 1<<k for each element type k (abi's Elem
// constants) it met. Cell is the address of a word, the VM's, that native
// code reads Kinds from as it runs and that Go sets another bit of
// (GoTypedSeen), which the code is compiled again for: a site's kinds are
// learned as its code runs, native code being the first to see most of
// them.
type TypedSite struct {
	Kinds uint16
	Cell  uintptr
}

// typedSite gives v, an element read or write of a number at pc (OpElemRead,
// OpElemWrite, OpElemStore), what its site has met of typed arrays
// (TypedKindFeedback): its Const the site's cell, as an Opaque constant,
// and its Key the element type plus one where that is a single type native
// code makes (TypedKindOf).
func (b *builder) typedSite(pc int, v *Value) {
	f, ok := b.fb.(TypedKindFeedback)
	if !ok {
		return
	}
	s, ok := f.TypedKind(pc)
	if !ok || s.Cell == 0 {
		return
	}
	v.Const = ir.Value{Kind: ir.Opaque, Bits: uint64(s.Cell)}
	if s.Kinds == 0 {
		// A site that has learned nothing, of what a global read, or a
		// captured binding's, found holding a typed array: compiled for
		// its type, which is the one to learn if the site meets another.
		if g := b.typedArrays[v.Args[0].Args[0]]; g != 0 && g != typedAny {
			s.Kinds = 1 << (g - 1)
		}
	}
	if k := s.Kinds; k != 0 && k&(k-1) == 0 {
		if kind := uint32(bits.TrailingZeros16(k)); nativeTyped(kind) {
			v.Key = kind + 1
		}
	}
}

// typedBigWrite reports whether the element write at pc is compiled to
// store a BigInt's low 64 bits (OpBigElemWrite): its site has met typed
// arrays of one element type, BigInt64Array's, or BigUint64Array's
// (unsigned), as native code learned them (TypedKindFeedback). Native
// code's store of any value (OpElemStore) stores none of their elements:
// Go stored each, at a call from native code, which made a BigInt64Array
// filled in a loop five times as slow with the JIT as without.
func (b *builder) typedBigWrite(pc int) (unsigned, ok bool) {
	f, ok := b.fb.(TypedKindFeedback)
	if !ok {
		return false, false
	}
	s, ok := f.TypedKind(pc)
	switch {
	case !ok:
		return false, false
	case s.Kinds == 1<<abi.ElemBigInt64:
		return false, true
	case s.Kinds == 1<<abi.ElemBigUint64:
		return true, true
	}
	return false, false
}

// typedAny is a builder's typedArrays' mark of a typed array whose element
// type the global read's site did not say.
const typedAny = 0xFF

// UpvalueTypedFeedback is Feedback that knows, for a read of a captured
// binding at a PC (ir.UpvalueRead), whether the binding holds a typed
// array as the code is compiled, and its element type plus one, or 0 where
// it is not one native code makes: an element read or write of what the
// read found is compiled as one of what a global read found holding a
// typed array is (GlobalSite's Typed and TypedKind). Emscripten's asm.js
// modules hold their heap's views in bindings their functions capture,
// which they read at every branch: learned as native code ran, site by
// site (TypedKindFeedback), each such function was compiled once more for
// it, and some more than once.
type UpvalueTypedFeedback interface {
	UpvalueTyped(pc int) (kind uint8, typed bool)
}

// typedUpvalue marks v, what the read of a captured binding at pc found,
// as a typed array where the binding holds one (UpvalueTypedFeedback), as
// what a global read found is marked.
func (b *builder) typedUpvalue(pc int, v *Value) {
	f, ok := b.fb.(UpvalueTypedFeedback)
	if !ok {
		return
	}
	kind, typed := f.UpvalueTyped(pc)
	if !typed {
		return
	}
	if kind == 0 {
		kind = typedAny
	}
	if b.typedArrays == nil {
		b.typedArrays = map[*Value]uint8{}
	}
	b.typedArrays[v] = kind
}

// nativeTyped reports whether native code reads and writes elements of
// type kind (abi's Elem constants) as numbers: every type but Float16's
// and the BigInt ones.
func nativeTyped(kind uint32) bool {
	return kind <= abi.ElemFloat64 && kind != abi.ElemFloat16
}

// TypedKindOf is the element type an element read or write of a number
// (OpElemRead, OpElemWrite, OpElemStore) is compiled for (typedSite), or
// -1 for any: the type its site has met alone.
func TypedKindOf(v *Value) int32 {
	return int32(v.Key) - 1
}

// TypedCellOf is the address of the cell an element read or write of a
// number learns the element types of its typed arrays in (TypedSite's
// Cell), or 0 for one that learns none.
func TypedCellOf(v *Value) uintptr {
	if v.Const.Kind != ir.Opaque {
		return 0
	}
	return uintptr(v.Const.Bits)
}

// keyedRead reports whether the element read at pc is made by Go from
// native code (KeyedFeedback, GoIndex).
func (b *builder) keyedRead(pc int) bool {
	in := b.p.Code[pc]
	if in.Op != ir.ArrayRead || !in.Reference || !b.goCalls() {
		return false
	}
	f, ok := b.fb.(KeyedFeedback)
	return ok && f.KeyedRead(pc)
}

// stringProperty is the feedback for a method read at pc whose receivers
// have been strings, or another primitive (PropertySite's Primitive), if
// any.
func (b *builder) stringProperty(pc int) (PropertySite, bool) {
	f, ok := b.fb.(StringFeedback)
	if !ok || b.p.Code[pc].Op != ir.ReferenceRead {
		return PropertySite{}, false
	}
	a := b.answer(pc)
	if a.asked&askedString == 0 {
		s, ok := f.StringProperty(pc)
		a = b.answer(pc)
		a.str, a.strOK, a.asked = s, ok && s.Shape != 0, a.asked|askedString
	}
	return a.str, a.strOK
}

// LiteralFeedback is Feedback that knows object literals' sites (Literal)
// and what each of their fields adds (Define): the shape the literal's
// object has before it and the one after, a PropertyAdd with Define.
type LiteralFeedback interface {
	Literal(pc int) (LiteralSite, bool)
	Define(pc int) (*PropertyAdd, bool)
}

// CallSite is a function the VM has seen a call call whose native code a
// caller's may call (mir's native calls): the function object's address,
// which the call checks it calls; its closure's, for Go to make its frame
// from; the address of the cell its code's entry is in, 0 while it has
// none, and of the count of the calls made to it natively, if any, which
// the VM keeps alive; the call's argument count and whether it
// passes a receiver; the callee's parameters, locals and operand slots;
// its receiver's slot, or -1 if it reads none; and whether a receiver that
// is not an object needs coercing, which only Go does. A construction,
// `new`, has Pool, the address of the abi.ObjectPool its receiver comes
// from, and ProtoIndex and ProtoKey, where in the function's table its
// own prototype property is and its name, which the call checks holds the
// pool's prototype; its result, if not an object, is the receiver. A
// leaf's call of Object.create (GoLeaf) may have Pool too, the objects
// native code makes it of (CreatePoolFeedback). One with
// Alloc is a built-in's with nothing to run, `new Array()`: its result is
// the pool's object, whose prototype the realm fixed. One with Receiver is a
// construction's receiver alone, taken from the pool once its function's
// prototype is checked as for a construction, the constructor then inlined
// (InlineSite's Construct). One with Via calls
// Function.prototype.call, at Via, of the function at Callee, its receiver:
// that function is called, with the first argument as its receiver and
// the others as its arguments. One with Push calls Array.prototype.push,
// the realm's, at Callee, with Argc arguments: on a dense array of its own,
// extensible, its length writable, with room for them all, whose
// prototypes are Protos with their shapes and no elements -- none with an
// indexed property a setter could be asked about -- the arguments are its
// new last elements, in their order, and the result its length. One with Pop calls Array.prototype.pop, the
// realm's, at Callee: on a dense array of the realm's prototype, Protos[0],
// its length writable, whose last element is not a hole, that element is
// the result, its cell undefined and the length one less.
type CallSite struct {
	Callee, Closure, Entry, Count uintptr
	Argc                          int
	Method                        bool
	// New marks a construction Go makes (GoCall at a new), Step a
	// for...of or for...in loop's step (GoCall at an ir.IterStep): its
	// cursor the one operand, the result the next value or, done, the
	// uninitialized value.
	New, Step bool
	// StepResult marks register code's loop's step's answer, of its
	// cursor and the result of its iterator's next, which native code
	// called (iterStep's userStep): Go's iterator protocol on it -- an
	// object, its done, then its value -- but where native code reads it,
	// a result of ResultShape whose done is false or true, its own data
	// properties done and value at the positions DoneAt and ValueAt; the
	// next value, or, done, the uninitialized value, the cursor marked
	// done.
	StepResult                   bool
	ResultShape                  uintptr
	DoneAt, ValueAt              int
	Params, LocalCount, MaxStack int
	ThisSlot                     int
	Coerce                       bool
	Pool                         uintptr
	ProtoIndex                   int
	ProtoKey                     uint32
	Alloc                        bool
	Receiver                     bool
	Via                          uintptr
	Push                         bool
	Protos                       [2]Holder
	Pop                          bool
	// Upvalues is the address of the callee closure's captured bindings'
	// cells, its context's (abi.Context.Upvalues), or 0 for none.
	Upvalues uintptr
	// Closes, for a scope's end (GoClose) in register code, is how many
	// registers from Argc it closes (close_regs's B), those alone; 0
	// closes every one from Argc on, as close_upvalues does.
	Closes int
	// StartOnly says the callee's code makes its locals past its
	// parameters undefined as it starts (Func's StartOnly): the call leaves
	// them to it.
	StartOnly bool
	// Forward marks apply_arguments, f.apply(x, arguments), in code that
	// hands on the arguments its frame was given (ir.Program's Forwards):
	// its operands f, apply and x -- Argc 2 -- the call checks apply is
	// Apply, the realm's Function.prototype.apply, and calls f, Callee's,
	// with x as its receiver and the frame's arguments, as many as its
	// context's Argc says, read from its Args (abi.Context), as V8's
	// CallForwardVarargs copies its caller's arguments to its callee's
	// frame. With Go (GoCall), Go makes it from those, as apply would. A
	// call none of its sites makes -- another function, a receiver Coerce
	// says Go coerces, more arguments than the context holds -- Go makes
	// where the sites are Generic, and leaves for the interpreter
	// otherwise, learning there what to call.
	Forward bool
	Apply   uintptr
	// FrameArgs says the callee's code reads its call's arguments past its
	// parameters (ir.Program's FrameArgs): the call writes all Argc of
	// them, and their count, to its callee's context (abi.Context's Args
	// and Argc), as V8's call pushes them all, where Go finds them for the
	// frames it makes of the callee's level.
	FrameArgs bool
	// Function says the callee's code reads its function object -- it
	// pushes it (push_callee), reads its home object (a super reference)
	// or names it (a mapped arguments object's callee): the call writes it
	// to its callee's context (abi.Context's Function), as a derived
	// construction's does, where Go finds it for the frames it makes of
	// the callee's level, as V8's frame has its function.
	Function bool
	// ReadsFunction, on register code's native call (Window), says the
	// callee's code reads its function object -- as Function's, or by its
	// register 0, a named function expression's own name: the call writes
	// it to the window's register 0, the register frame's place for it. A
	// call none of whose functions reads it writes it nowhere, and Go
	// gives a frame it makes of the callee's level the one its checks
	// found (abi.CallState's Callees).
	ReadsFunction bool
	// Parent, for a construction with a class whose constructor is the
	// one a class without one has, is the class it extends, whose
	// function Closure is: the call checks the callee's prototype is still
	// it, as super() reads it, and constructs as a construction with it
	// does, its receiver from Pool, made as the callee makes one.
	Parent uintptr
	// ByCode, where not 0, has the call check the function the callee's
	// closure runs, the address of its bytecode, and that the closure's
	// environment and realm are Env and Realm, rather than that the callee
	// is the object Callee: the closure, and its captured bindings, are the
	// callee's at run time. A closure an enclosing function makes at each
	// of its calls is a new object each time, the same function, as V8's
	// calls check the function's code, not the closure.
	ByCode, Env, Realm uintptr
	// Literal marks an object literal's object (ir.ObjectLiteral), the
	// pool's, with Alloc: no function, no operands; or an array literal's
	// (ir.ArrayLiteral), whose Argc operands are its elements -- none for
	// Shared's, whose array the pool holds whole (LiteralSite's Shared):
	// never left unmade (escape.go), which would make it empty.
	Literal, Shared bool
	// Fill, with Step, marks a destructuring's step (ir.IterStep's When),
	// whose result is undefined once the iterator is done, not the
	// uninitialized value a loop's step branches on.
	Fill bool
	// MapNexts, with Step, are the realm's Map and Set iterators' next
	// functions (MapStepFeedback): a cursor over an iterator whose next is
	// one of them native code steps itself, as V8 inlines a Map
	// iterator's next. Zero for none.
	MapNexts [2]uintptr
	// Gens, with Step, are the generators' functions the step has met
	// (GenStepFeedback), and GenNext the realm's %GeneratorPrototype%.next:
	// a cursor over a generator of one of them, whose next is GenNext,
	// suspended and not in a yield*, native code resumes by a native call
	// of the generator's code (Func's GenEntry), as V8's code calls a
	// generator's resumption -- the generator's value, or, once it
	// finishes, the step's done. On a method call Go makes (GoCall,
	// Method) of no argument or one, they are the generators the call has
	// met calling GenNext (GenStepFeedback at the call's PC), and Pool the
	// abi.ObjectPool of the result objects, { value, done }, the call
	// makes: a call of GenNext on a generator of one of them native code
	// makes by the same native call, its argument the value sent in, its
	// result the pool's next object, the value and the done written to it
	// (the VM's iterResult's layout) -- the realm's next's result,
	// which nothing could tell from it.
	Gens    []GenStep
	GenNext uintptr
	// GenIter, on a for...of's cursor's making Go makes (ForOf), is the
	// address of the VM's cache of how generators iterate, and GenIterFn
	// the realm's %IteratorPrototype%[Symbol.iterator], with GenNext the
	// realm's generators' next: of a generator with room for its loop's
	// cursor, which iterates as the intrinsics do -- no property of its
	// own, its prototype an ordinary object with none, the chain up from
	// that the cache's, valid, its next and Symbol.iterator the two --
	// native code makes the cursor the one in the generator's allocation.
	// Go makes any other.
	GenIter, GenIterFn uintptr
	// GenMake, on a call Go makes (GoCall), plain or a method's, is the
	// object of a synchronous generator function the call has met, whose
	// generators native code makes: a call of it takes its generator from
	// Pool, made as a call of it with no arguments makes one -- the
	// function's prototype, which the call checks its own property at
	// ProtoIndex, of the key ProtoKey, holds still, the pool's -- and
	// writes the call's receiver, or undefined, and its arguments, up to
	// the function's Params and LocalCount, to it. Go makes any other
	// call, and the call while the collector marks. GenArgAt is the place
	// of the first argument among the generator's values -- its window's
	// register 2 less the registers it does not keep -- and GenThisAt, where
	// not 0, one past the value the receiver is written to besides the
	// generator's this: a register generator's register 1.
	GenMake             uintptr
	GenArgAt, GenThisAt int
	// ForOf marks a for...of's or a destructuring's cursor made of the
	// iterable, its one operand; ForIn a for...in's, of the object.
	ForOf, ForIn bool
	// Rest marks a destructuring's rest element made of its cursor, the
	// one operand: the array of what the cursor has left (ir's IterStep,
	// Rest).
	Rest bool
	// Tail marks a callee whose function makes calls in tail position
	// (OpTailSelf), for which the call sets the callee's context's
	// TailFrame to TailFrame: false for a construction, or an arrow that
	// shares a derived constructor's this.
	Tail, TailFrame bool
	// Go marks a call of Go, not of a function, as V8's code calls a
	// runtime function: what Go does with the call's operands, which
	// native code passes as their words (abi.Context's GoOp).
	Go GoOp
	// Release, for a BigInt's operator Go makes (GoArith, GoAdd), is what
	// of its operands' blocks Go may give back once it has made its result
	// (ReleaseLeft, bigRelease in bigint.go).
	Release uint8
	// Owned, for an addition (GoAdd), says its result may be grown in place
	// (ir's Instruction.Owned).
	Owned bool
	// Big marks a BigInt's addition, subtraction, product, division or
	// remainder Go makes (GoArith; GoAdd at a site that met BigInts), which
	// the VM's leaf makes without Go's call where it can (abi.Encoding's
	// BigFast).
	Big bool
	// Generic, on a call's first site, has a function none of its sites
	// is called natively all the same, through its link, found from its
	// closure's hint (abi.HintTable), where its closure's environment and
	// realm are GenericEnv and GenericRealm, the caller's -- as V8's code
	// makes a call its feedback saw too many functions at, through the
	// code the function object points to.
	Generic                  bool
	GenericEnv, GenericRealm uintptr
	// Table, on a Generic call of typed code through a function table, is
	// the address of the table's TableLen abi.TableEntry: the function
	// none of its sites is is found there by the call's index into the
	// table (OpTableCallee), not by its hint. A call whose index the
	// build does not know is built without it.
	Table    uintptr
	TableLen int
	// ArrowThis marks a callee that is an arrow reading this: the call
	// gives it the this it captured, read from its function object
	// (abi.Encoding's FuncExtra), in place of its receiver, as the
	// interpreter's call does; one that shares a derived constructor's
	// binding the call leaves to Go.
	ArrowThis bool
	// PlainThis marks a plain call, no method's, of a callee that reads its
	// receiver: the call gives it undefined, or, Global not 0, that object,
	// the realm's global object a sloppy callee's this is coerced to, as
	// the interpreter's call gives it (callFunction).
	PlainThis bool
	Global    uintptr
	// Window, where not 0, marks a native call register code makes (its
	// ir.Instruction's Window) of a function that runs register code: one
	// past the caller's register the call's window starts at, the function
	// there, the receiver after it and the arguments from the third. The
	// callee's window is made at the VM's stack's top, past the caller's
	// window and the slots past it: the function in its register 0, the
	// receiver in 1 -- a method call's, or a plain call's undefined, global
	// object (PlainThis, Global) or captured this (ArrowThis) -- and the
	// arguments from 2, undefined in its parameters past them; its code's
	// call entry makes the rest of its registers undefined. Params is then
	// the callee's ParamBase and parameters, LocalCount its Regs, ThisSlot
	// -1: the frame is its window. The room the call checks is its link's
	// (abi.Encoding's LinkFrame), which its code's frame sets.
	Window int
	// Accessor marks a property's read (AccessorGet) or write (AccessorSet)
	// whose site found an accessor (AccessorSite), at its PC: a call of the
	// getter, or of the setter with the value, its result dropped. Its
	// operands are the receiver and, for a write, the value: no function,
	// which the guards before the call found is Callee's. With Go (GoCall),
	// Go makes the read or the write of Key, the property's atom, as the
	// interpreter would, whatever it finds.
	Accessor AccessorKind
	Key      uint32
	// TypeOf and InstanceOf mark a call of Go (GoCall) that is a typeof of
	// its one operand, or an instanceof of its two, the object and the
	// constructor, as V8's code calls its Typeof and InstanceOf builtins:
	// Go makes it, as the interpreter does -- an instanceof may run code,
	// a Symbol.hasInstance method or a proxy's trap.
	TypeOf, InstanceOf bool
	// Instr, where not 0, marks a call of Go (GoCall) that is an operation
	// Go makes, the VM's instruction at the call's PC (compile's
	// GoOperands) -- its opcode in Instr's low byte, its A operand in the
	// 24 bits above -- of its Argc operands, the top of the stack, its
	// result in their place: object spread and rest, array spread, a spread call,
	// a literal's computed key or __proto__ -- as V8's code calls its
	// CloneObject, CopyDataProperties and CallWithSpread builtins. Go makes
	// it without a frame where that runs no script, and with the code's
	// frame where it may.
	Instr int
	// Whole, of a call of Go (GoCall), is register code's instruction at
	// the call's PC that Go runs whole on the register frame (ir's Whole):
	// its operands the registers the instruction reads, in order, its
	// result the one it writes, the code going on after it, as V8's code
	// calls a runtime function for what it has no code of.
	Whole bool
	// StringSelf, of an operation Go makes (Instr) of one operand, says its
	// result is the operand itself where that is a string -- a computed
	// key's conversion, to_property_key -- which native code tests,
	// calling Go for any other.
	StringSelf bool
	// Copies, of an object spread Go makes (Instr, copy_data_props), is
	// the address of the VM's table of spreads by layout (abi's CopyBits),
	// by which native code makes one it finds there, as V8's CloneObjectIC
	// copies a fast object by its map, calling Go for any other.
	Copies uintptr
	// DefineNext, of a literal's field Go defines (Instr, define_field),
	// has native code define it where the object's shape's first
	// transition is by the field's key, Key, with the default attributes,
	// as the VM's appendTransition does -- the object of a layout no plan
	// knows, past a spread or a computed key -- calling Go otherwise.
	DefineNext bool
	// ArrayIter, of an array's spread Go makes (Instr, array_spread or
	// spread_iter), is the address of where the VM's arrays find their
	// iteration's methods (abi's IterCellArray): native code copies the
	// elements of an array that iterates as Array.prototype's intrinsics
	// do, no hole among them, into room the array spread into has, as V8's
	// code spreads a fast array under its ArrayIteratorProtector, calling
	// Go for any other. Append, of an element's (array_push), has native
	// code append it where the array has room.
	ArrayIter uintptr
	Append    bool
	// Super marks a call of Go (GoCall) that is a derived constructor's
	// super(...) of its Argc operands, the arguments -- no function among
	// them -- as V8's code calls the parent constructor: Go makes it on
	// the code's own frame, binding its this, which is the result and
	// which the this slot takes. Its first operand is the this slot, the
	// uninitialized value unless an earlier super() bound it. Register
	// code's (Window) has the same operands, the arguments from its
	// instruction's Left, the result its Dest's, register 1's and the this
	// slot's (superCall); natively, the parent's window is made with the
	// function whose code runs in register 0 and the instance in 1.
	Super bool
	// Derived marks a construction (Pool) whose callee's code is a derived
	// constructor's (ir.Program's DerivedConstructor): the pool's object is
	// the callee's instance, not its receiver -- its this is unbound until
	// its super() binds it (abi.Context's Instance) -- and its result is
	// always an object.
	Derived bool
	// Active, for a super() native code makes (Super, Go 0), is the
	// function object the code was compiled for, whose prototype --
	// Callee, Parent past a class with no constructor of its own -- the
	// super() constructs with, its code at Closure: the call checks the
	// running one (abi.Context's Function) is Active, and its prototype
	// Callee. The parent's code is called as a construction's, its this
	// the instance (abi.Context's Instance), or, a derived parent's,
	// unbound with that instance; the result, an object or else the
	// instance, the this it binds.
	Active uintptr
	// Captures, of a closure Go makes (GoClosure) that has a Pool, says
	// natively: the closure is the pool's next object, made as the VM
	// makes the function, its captured bindings' cells unwritten, and
	// native code writes each in order -- index<<1|1 the code's own cell
	// of a captured local's (abi.Context's Cells), index<<1 a captured
	// binding's of its own (Upvalues) -- as V8's code allocates a closure
	// in its context (FastNewClosure). LexThis marks an arrow, whose this
	// native code writes: the code's receiver, its context's (abi.Context's
	// This), which a native call writes and Go's entry does for such code.
	// Go makes it while the collector marks or the pool is empty -- and
	// fills the pool again. Of an entry's cells (GoCells) that have a
	// Pool, it is the cells' locals (CellsFeedback): native code takes a
	// block of them from the pool, its cells linked, and writes each
	// cell's value -- the argument, |1, or undefined -- as V8's code
	// allocates a function's context, where its context is a native
	// call's; Go makes them otherwise, and fills the pool again. Of a
	// scope's end (GoClose) in such code, they are the same, the function's
	// pool and its cells' locals: native code gives each local from Argc
	// on a fresh cell, its block's, holding what the old one held, which it
	// closes, as V8's code allocates a block's context at each iteration.
	Captures []int32
	LexThis  bool
	// LexThisReg, with LexThis, says the arrow's this is the frame's
	// register 1, where register code has its this -- as the VM's
	// closure_w takes it -- not the context's This.
	LexThisReg bool
	// Leaf, of a leaf's call (GoLeaf), is the address of the function the
	// call checked it calls (OpSameObject's), which the VM keeps alive with
	// the code: Go takes the leaf from there (abi.GoCallState's Leaf), and
	// the call writes no operand for it.
	Leaf uintptr
	// Unused, of a leaf's call (GoLeaf), says that nothing reads its result
	// but, at most, whether it is truthy (resultUnused): the instruction
	// after the call drops it, branches on it -- keeping it only where it
	// is falsy -- or negates it. Go may then give back, for an object the
	// leaf would make, another object that nothing else can reach
	// (abi.GoCallState's Unused).
	Unused bool
	// Reuse, of a leaf's call of exec, is what ExecReuseFeedback says of
	// the call (abi.GoCallState's Reuse): its array made again in the
	// memory of the call's last, by the frame that made the last. It takes
	// the word Unused leaves.
	Reuse uint32
	// Give, of register code's method call of Go at regexp_lit's
	// expansion (Window), is the address of the abi.ObjectPool of the
	// literal whose RegExp, the pool's next object, is its receiver, which
	// nothing but the call reaches: a leaf's call of it (GoLeaf) has Go
	// give the receiver back to the pool once the leaf has returned, as
	// its call of Go of the same call gives the literal its RegExp back
	// (abi.GoCallState's Give). Of a leaf's call of Object.create with
	// Pool (CreatePoolFeedback), it is that pool, which Go fills again as
	// its leaf makes an object of the pool's prototype.
	Give uintptr
}

// ClosureSite is a closure's site that native code makes the closure at
// (ClosureFeedback): the address of the abi.ObjectPool its closures come
// from, which the VM keeps alive, and CallSite's Captures, LexThis and
// LexThisReg.
type ClosureSite struct {
	Pool       uintptr
	Captures   []int32
	LexThis    bool
	LexThisReg bool
}

// ClosureFeedback is Feedback that knows the closures native code may make
// from a pool (CallSite's Captures): the one at pc, of code whose captured
// locals are in cells of their own (ir.Program's Cells).
type ClosureFeedback interface {
	Closure(pc int) (ClosureSite, bool)
}

// CellsFeedback is Feedback that knows where native code takes the cells
// of code whose captured locals are in cells of their own (ir.Program's
// Cells) from, at its entries' GoCells: the address of the abi.ObjectPool
// of the function's cells, which the VM keeps alive, and the cells' locals
// in order, each slot<<1, |1 a parameter's, whose cell starts with the
// argument (CallSite's Captures).
type CellsFeedback interface {
	CellsPool() (pool uintptr, cells []int32, ok bool)
}

// AccessorKind is the kind of a call an accessor's read or write makes
// (CallSite's Accessor): none, a getter's or a setter's.
type AccessorKind uint8

const (
	AccessorNone AccessorKind = iota
	AccessorGet
	AccessorSet
)

// GoOp is what a call of Go does (CallSite's Go).
type GoOp uint8

// GoAdd is +, an operand possibly a string (ir.StringAdd): Go makes it of
// primitives; an object, whose conversion may run anything, or a symbol,
// which throws, it leaves to an exit, after which Go makes it. GoRefill
// fills an object pool that ran out again (abi.ObjectPool's Source), its
// address the first operand's pointer word: a call that takes from one
// makes it, and is made again from its start.
//
// GoCall is a call, a method call or a construction -- the call at the
// state's PC -- that Go makes with the operands, from the function or the
// receiver on, as V8's code calls a function it has no code of its own
// for: Go runs it, the callee's own native code or the interpreter, and
// native code goes on with its result. Go refuses one native code does
// not make at its function's own level (a native call's callee); one that
// threw, or after which the code is to be compiled again, it makes and
// leaves for Go to finish at the call's exit.
//
// GoPow is Math.pow of the two operands' number words where its answer is
// not exact (OpPowF64), as V8's code calls its ieee754 pow: Go writes it
// to the first's number word. GoMath, GoAtan2 and GoHypot are a Math
// function too (OpMathCall): GoMath's of one number, which of the VM's
// the second operand's number word says.
//
// GoMod is JavaScript's % of the two operands' number words where they are
// not integers native code divides (OpModF64), as V8's code calls its
// fmod: Go writes it to the first's number word.
//
// GoPoll is a loop's back edge whose budget ran out: Go gives it another
// and checks for an interrupt, as V8's code calls its StackGuard, and native
// code goes on at the loop's header; Go refuses it, for the code to leave
// there, if one is due.
//
// GoElemWrite is a store of a number to an element native code does not
// make (OpElemWrite): to a hole, or at the array's length, which grows it,
// as V8's code calls its grow stub; or to a typed array's element where
// native code does not find the view in its buffer -- before Go has looked
// at it -- or does not convert to the element's type. The operands are the
// array's pointer word, the key's and the value's number words; Go refuses
// a store that could run a script or throw. Not a write with checks
// (storeChecks): Go's store writes over any element and grows the array,
// moving its elements, under a reference live after it that was read from
// one; such a write exits instead.
//
// GoElemStore is GoElemWrite for any value (OpElemStore): its third
// operand is the value, both its words, and its fourth's pointer word the
// site's cell (TypedCellOf), or 0, in which Go sets the bit of a BigInt
// typed array's element type it stores in, as GoTypedSeen sets a number's,
// for the code to be compiled again to store it natively
// (OpBigElemWrite). One into an array a loop made (StoreFresh, the
// operation's index 1) Go appends at the array's length as
// CreateDataProperty defines it, whatever the prototypes hold.
//
// GoTypedRead is a typed array's element native code does not read
// (OpElemRead): before Go has looked at the view, or of a type native code
// does not convert. Go writes it to the first operand's number word, the
// typed array's pointer word, the key the second's number word; it refuses
// an element that is not a number -- a BigInt, an index the view does not
// have.
//
// GoElemCell is an element read as a reference that native code does not
// find -- past the array's length, or a hole: Go says whether it is
// undefined with nothing to ask, the array's chain holding no element and
// no index, the first operand its pointer word, the key its number word;
// the read is then from Encoding's UndefinedCell.
//
// GoStringAbsent is a string's element read (OpStringChar) at an index
// past the string's end: Go says whether it is undefined with nothing to
// ask, String.prototype and its chain holding no element and no index,
// the first operand the string's pointer word, the key its number word;
// the read is then from Encoding's UndefinedCell, as GoElemCell's is.
//
// GoConcat is concat (ir.StringConcat): Go converts its Argc operands to
// strings and joins them, as GoAdd adds; what could run code or throw it
// leaves to an exit, after which Go makes it.
//
// GoIndex is an element read as it is (ir.Instruction's Reference) of
// any receiver and key, the first operand and the second, as V8's code
// calls its generic keyed load: Go reads an object's property by a string
// key, or its element by an integer, where that runs nothing -- a data
// property, found by the realm's shared property cache, or a dense
// array's or a typed array's element -- and refuses any other, which
// leaves for Go to read after an exit, native code going on after it.
//
// GoAppend is GoAdd of a loop's string builder (stringBuilders): its
// first operand is the string the loop builds, which nothing but its
// appends uses in the loop. Go appends the second operand, a primitive, to
// it in place where it is a building string -- one an append made, which
// nothing has frozen (OpFreeze) or seen since, Go freezing every one
// native code leaves for it with -- and otherwise makes it a building
// string of the two, the first its left side, copied nowhere. Its result
// is the string, the same one each time. What GoAdd refuses it refuses.
//
// GoCompare is a relational comparison, <, <=, > or >= -- ir's Operator
// in Argc -- of two primitives, whose answer is a boolean, as V8's code
// calls its StringLessThan builtin: one whose guard has failed, an operand
// not a number -- a lexer's c >= 'a' of a string's character, which had
// left native code at every turn for Go to compare -- and what Go refuses,
// an operand converted by calling something, leaves for Go after an exit.
//
// GoRandom fills Math.random's cache again (OpRandomF64), which native
// code found empty, as V8's code calls its runtime to: Go writes the next
// number to the first operand's number word.
//
// GoNumber is ToNumber of an arithmetic operation's operand that is no
// number nor an oddball (OpOddballF64 at a site whose guard has failed),
// as V8's code calls its NonNumberToNumber builtin: Go writes the
// first operand's number to its number word -- a string's, parsed -- and
// refuses one whose conversion could run code or throws, an object, a
// symbol, a BigInt, which leaves for Go to make the operation after an
// exit.
//
// GoEqual is == or ===, strictly if the operation's index is 1, of the
// first and second operands where native code does not compare them
// (OpEqTagged): strings it does not compare inline, a number and a string,
// a BigInt and a string -- what equality converts runs nothing -- as V8's
// code calls its Equal builtin. Go writes 1 or 0 to the first operand's
// number word, and refuses an object compared loosely with a primitive
// that is not null or undefined, whose conversion may run code.
//
// GoCells makes the cells of code whose captured locals are in cells of
// their own (ir.Program's Cells), at each of its entries, as V8's code
// allocates a function's context: abi.Context's Cells. It writes no keep
// cell.
//
// GoHas is in (ir.HasProperty), the key the first operand and the object
// the second, as V8's code calls its HasProperty builtin: Go searches the
// object and its chain, as the interpreter does, where that runs nothing
// and throws nothing -- an object none of whose chain is a proxy or a
// module namespace, by a key that is no object -- and writes the boolean
// to the call's keep cell. It refuses the rest: a proxy's has trap, a
// namespace's deferred evaluation, a key whose conversion calls its
// toString or Symbol.toPrimitive, a right operand that is no object,
// which throws; those leave for Go to make after an exit, native code
// going on after it.
//
// GoDelete is delete (ir.DeleteProperty), the object the first operand and
// the key the second, as V8's code calls its DeleteProperty builtin, and
// GoDeleteStrict is the same in strict code: Go deletes the object's own
// property as the interpreter does, where that runs nothing and throws
// nothing -- an ordinary object's or an array's, by a key that is no
// object -- and writes whether it is gone to the call's keep cell. It
// refuses the rest: a proxy's deleteProperty trap, a module namespace's
// deferred evaluation, a typed array's, a string wrapper's or a mapped
// arguments object's, a key whose conversion calls its toString or
// Symbol.toPrimitive, a base that is no object, and, in strict code, a
// property that cannot be deleted, which throws; those leave for Go to
// make after an exit, native code going on after it. A refused deletion
// has changed nothing. What it deletes changes the object's shape, as a
// call of any function may, which every property operation after it
// checks again, and clears the property's cell, which a value used after
// it may have been read from: it is a store of any key to the analyses
// (allocOnly, keepAcrossStores), which keep such a value first.
//
// GoClosure makes the function of constant Argc on the code's own frame
// (ir.Closure), capturing its locals there, and GoClose closes what
// closures captured of locals Argc on (ir.CloseUpvalues): both Go's, on
// the frame Go entered the code with, and refused on any other -- a native
// callee's -- which leaves for Go to make them after an exit.
//
// GoIndexOf is a search, String.prototype's or Array.prototype's indexOf,
// lastIndexOf or includes -- which, the VM's kind of it in Argc's bits past
// IndexOfFrom -- of the first operand, for the second, from the third's
// position if the call gives one (IndexOfFrom), as V8's code calls its
// StringIndexOf and ArrayIndexOf builtins: Go answers it as the built-in
// does where that runs nothing -- a string searched for a primitive that
// is no symbol, an array of the realm's, dense, whose prototypes hold no
// index, a position such a primitive -- and writes the index, or the
// boolean, to the call's keep cell. It refuses the rest, which leaves for
// Go to make the call after an exit.
//
// GoNewArray makes the array a loop of map or filter fills (forEachCall),
// as their ArraySpeciesCreate makes it, the array they are called on the
// first operand and its length the second: an Array of no length, with
// room for that many elements if Argc has NewArrayRoom, map's, where the
// array's species is Array's -- no constructor of its own,
// Array.prototype's and Array[Symbol.species] as they were made -- as
// V8's code checks its ArraySpeciesProtector. It refuses any other, which
// leaves the call for Go to make after an exit. With NewArrayTyped the
// first operand is a typed array: map's is a typed array of its type and
// the length, filter's an Array of no length; with NewArrayFill too, the
// second operand is that Array, and filter's result is made of its
// elements, as %TypedArray%.prototype.filter makes it after its loop --
// each where the typed array's species is its type's own constructor, its
// prototype's constructor and %TypedArray%[Symbol.species] as they were
// made.
//
// GoArith is an arithmetic, bitwise or shift operator -- ir's Operator in
// Argc -- of two BigInts, at a site that met BigInts and whose speculation
// failed (BigIntFeedback), as V8's code calls its BigInt builtins: Go writes
// the result to the call's keep cell where the operation throws nothing --
// no result past BigInt's size limit, no division by zero -- and refuses
// the rest, an operand no BigInt among them, which leave for Go to make the
// operation after an exit.
//
// GoKeyedWrite is an element's write (ir.ArrayWrite) of the third operand
// to any receiver, the first, by any key, the second, as V8's code calls
// its generic keyed store: Go stores where that runs nothing and throws
// nothing -- an ordinary object's own writable data property, or a new
// one where nothing up its chain intercepts it, by an index or a name; an
// array's element as GoElemStore stores one -- and refuses the rest, which
// leaves for Go to store after an exit, native code going on after it.
//
// GoBigString is x.toString() or x.toString(radix) of a BigInt, the realm's
// BigInt.prototype.toString called (BigStringFeedback): Go writes the
// string to the call's keep cell -- of a radix undefined, or an integer
// from 2 to 36 -- and refuses any other receiver or radix, which leave the
// call for Go to make after an exit. Argc is the call's arguments.
//
// GoLeaf is a call of a leaf, a built-in that runs no script where its
// check passes (LeafFeedback): the function the first operand, which the
// call has checked it calls, the receiver the second of a method call
// (Method), the Argc arguments past them. Go applies it as the interpreter
// does, without a frame -- the built-in's own function where its check passes,
// or the leaf's own -- with nothing to find out of the call first, as V8's
// code calls a fast API function with no frame of JavaScript's, and
// writes its result to the call's keep cell; what it refuses leaves for Go
// to make the call after an exit.
//
// GoTypedSeen is an element read or write of a number (OpElemRead,
// OpElemWrite, OpElemStore) that met a typed array of an element type its
// site's cell (TypedSite's Cell), the second operand's pointer word, has
// no bit of, the typed array the first operand: Go sets the type's bit in
// the cell, and has the code compiled again, for that type, once it has
// spent its budget, as V8's baseline code records a keyed access's
// elements kind in its feedback. It never refuses.
//
// GoTypedView is a typed array, the first operand's pointer word, whose
// copy of where its elements are and how many (abi.Encoding's TypedElems
// and TypedLength) is of an epoch before the context's TypedEpoch: a
// buffer's bytes have moved or changed length, or been detached, since.
// Go copies them anew, as of the epoch now, for native code to read them
// again; it refuses only what is no typed array.
//
// GoIterClose closes a cursor OpIterClosed finds open, its one operand,
// where closing it runs no script: a generator's of register code,
// suspended at a yield no handler of its holds -- no finally clause, no
// loop's iterator to close, no disposal -- whose return path returns at
// once, its iterator's return the realm's own. Go finishes the generator
// as the return would, and the cursor is done; it refuses any other, which
// leaves for Go to close after an exit.
//
// GoAwait is what an async function's await of an object, its one
// operand, goes on with in place (OpAwaitReady): a fulfilled promise of
// the realm's own constructor's value, as the interpreter's await finds it
// (the VM's regSettled), which Go writes to the call's keep cell. It runs
// no script, and refuses anything else, which leaves for Go to suspend the
// frame at the await after an exit.
const (
	GoAdd GoOp = 1 + iota
	GoRefill
	GoPow
	GoMath
	GoAtan2
	GoHypot
	GoCall
	GoMod
	GoPoll
	GoElemWrite
	GoElemCell
	GoConcat
	GoClosure
	GoClose
	GoTypedRead
	GoElemStore
	GoIndex
	GoAppend
	GoCompare
	GoRandom
	GoNumber
	GoEqual
	GoCells
	GoHas
	GoDelete
	GoDeleteStrict
	GoNewArray
	GoIndexOf
	GoArith
	GoKeyedWrite
	GoBigString
	GoLeaf
	GoArgument
	GoHost
	GoStringAbsent
	GoIterResult
	GoTypedSeen
	GoTypedView
	GoIterClose
	GoAwait
)

// GoNewArray's flags, in its CallSite's Argc. With NewArraySort, the
// first operand is what a sort is called on, a typed array of a number
// type (NewArrayTyped) or an Array: Go makes the Float64Array of twice
// its length whose first half holds its elements, as numbers, where every
// one is a number the array has, and refuses anything else; with
// NewArrayFill too, the second operand is that Float64Array, whose first
// half Go writes back to the array, in order, its result the array.
const (
	NewArrayRoom = 1 << iota
	NewArrayTyped
	NewArrayFill
	NewArraySort
)

// GoIterResult is the result object of a call of a generator's next
// native code made (CallSite's Gens on a call), { value, done }, of its
// first operand, the value, and its second, the done, a boolean's word:
// Go makes it as the realm's next makes it, and fills the call's pool of
// them, its third operand's pointer word, if it ran out. Go refuses
// nothing.

// GoArgument is arguments[k] in a function's own code (ir.ArgumentRead
// at the root), of two operands, the key and the local the arguments
// object is kept in once made: Go reads the frame's argument k, where the
// object is not made and k is an index of the arguments; with Argc 1, its
// one operand that local, arguments.length, their count. Go refuses the
// rest, and the code leaves for Go to make it there: the object
// made, an index past the arguments, any other key.

// GoHost is an instruction of a function's own code that reads only its
// Argc operands and the frame Go entered the code with (ir.Instruction's
// Frame) -- a super reference's read or write, a rest parameter's array,
// the arguments object -- which Go makes there, as the interpreter does,
// from native code: it may run anything, a getter or a setter, as a call
// Go makes (GoCall) may. Its result, where the instruction pushes one.

// IndexOfFrom is GoIndexOf's Argc's low bit, where the call gives a
// position, its second argument; the bits above it are the VM's kind of
// search (IndexOfFeedback's).
const IndexOfFrom = 1

// RefillsPools reports whether a call takes an object from a pool, which
// a call of Go fills again when it runs out (GoRefill).
func RefillsPools(v *Value) bool {
	for _, c := range v.Calls() {
		if c.Pool != 0 {
			return true
		}
	}
	return false
}

// popsElement reports whether a call is Array.prototype.pop's (CallSite's
// Pop): it writes an element's cell, and its own result's.
func popsElement(v *Value) bool {
	for _, c := range v.Calls() {
		if !c.Pop {
			return false
		}
	}
	return len(v.Calls()) != 0
}

// allocOnly reports whether a call runs nothing that can write the heap
// but past what any value was read from: it makes a pool's object
// (CallSite.Alloc), or adds an element past an array's last (Push).
func allocOnly(v *Value) bool {
	for _, c := range v.Calls() {
		// A call of Go makes what it returns, and writes nothing else --
		// but a call it makes (GoCall), which runs anything, and a
		// deletion (GoDelete), which clears the cell of the property it
		// deletes, one a value used after it may have been read from.
		if !c.Alloc && !c.Receiver && !c.Push && (c.Go == 0 || c.Go == GoCall || c.Go == GoHost || c.Go == GoDelete || c.Go == GoDeleteStrict) {
			return false
		}
	}
	return len(v.Calls()) != 0
}

// InlineSite is a call the VM has seen call one function, whose program,
// lowered for inlining, InlineCalls accepts: that program and its own
// feedback; the function object's address, which the VM keeps alive and
// the call checks it still calls, and its closure's, for Go to make its
// frame from at an exit inside it (InlineState); the call's argument
// count, and whether it passes a receiver, as a method call does; and the
// callee's parameter count, its receiver's slot, or -1 if it reads none,
// and whether a receiver that is not an object needs coercing, which only
// Go does. A construction, `new`, has Construct, and its receiver comes
// from Pool, as a native construction's does (CallSite's Pool, ProtoIndex
// and ProtoKey); the callee returns nothing, so the receiver is the
// result, as V8 inlines a constructor. Restart marks a callee whose frame
// Go cannot make: an exit in its own code has Go make the call over again,
// from the caller's state at it, as one before it changed nothing (a
// forwarding constructor's, bytecode.LeafForward, whose frame would have no
// arguments). Forward is the call of f.apply(this, arguments) in such a
// callee, apply_arguments: f, the function at Callee, is called with the
// callee's receiver, after the check that apply is the realm's own, at
// Apply, with the arguments the callee was given, as V8 eliminates the
// arguments object. Arguments marks a callee that keeps the arguments its
// call gave it for its frame at an exit (InlineState's Args), a forwarding
// constructor whose code does more than forward them once
// (bytecode's ForwardsArguments): Go makes its frame, not the construction
// over again. Upvalues is the address of the callee closure's captured
// bindings' cells, which its reads of them read, the closure the call's
// check of its function fixes; or 0, Go's. ReadsArguments marks a callee
// that reads its arguments by index (ir.ArgumentRead) -- none of them a
// parameter its arguments object aliases: the reads are the call's
// arguments, and its frame keeps them at an exit, as Arguments' does.
// Captured, for a callback its caller makes and never does (ForEachSite's
// Made), are the caller's slots its captured bindings are, by index: its
// reads and assignments of them are the caller's own, in place, as V8's
// inlined code reads a context escape analysis removed. LexThis marks an
// arrow the root makes that reads its this (CalledClosureFeedback), which
// is the root's own: its receiver's slot, LexThisSlot of the root's frame,
// is the callee's this, whoever calls it -- as V8 inlines an arrow, whose
// this is a context slot of its maker's.
type InlineSite struct {
	Program         *ir.Program
	Feedback        Feedback
	Callee, Closure uintptr
	Argc            int
	Method          bool
	Params          int
	ThisSlot        int
	Coerce          bool
	Construct       bool
	Pool            uintptr
	ProtoIndex      int
	ProtoKey        uint32
	Restart         bool
	Forward         bool
	Apply           uintptr
	Arguments       bool
	Upvalues        uintptr
	ReadsArguments  bool
	Captured        []int
	LexThis         bool
	LexThisSlot     int
	// ThisCell, where not 0, is the address of a cell holding the
	// receiver of a plain call of a callee that reads one: the global
	// object a sloppy callee's this is coerced to, or undefined, a strict
	// one's, as the interpreter's call gives it (callFunction).
	ThisCell uintptr
	// Window marks a callee that runs register code, inlined at register
	// code's call (ir.Instruction's Window): its program's slots are its
	// window's registers -- its function in 0, its receiver in 1 (a method
	// call's, ThisCell's, or undefined), its arguments from 2, undefined in
	// the rest -- then its captured bindings, read in their cells
	// (Upvalues), and its scratch slots. Params is 2 plus its parameters,
	// ThisSlot -1, and its result goes to the call's register (Dest).
	Window bool
	// FunctionCell, for such a callee inlined at a property's read or
	// write whose accessor it is (AccessorSite's Inline), is the address
	// of a cell holding its function object, its window's register 0: no
	// operand of the read's is the function.
	FunctionCell uintptr
}

// Inlining's bounds, by size alone, as V8's JSInliningHeuristic has them:
// a callee's instructions (maxInline), all callees' in a function
// (maxInlineTotal), and how deep a callee's calls are inlined in it, as V8
// inlines them (maxInlineDepth). A small callee -- maxInlineSmall
// instructions or fewer, the callees inlined in it counted in -- is
// inlined past the function's budget, up to maxInlineAbsolute, as V8
// inlines one of max_inlined_bytecode_size_small past
// max_inlined_bytecode_size_cumulative, up to
// max_inlined_bytecode_size_absolute: its code is about the call's it
// replaces. V8 counts bytecode bytes, and ours instructions: V8's
// bytecode is 2.6 bytes to an instruction of ours, the median of the 270
// functions of Are We Fast Yet's whose names are unique in both, so that
// its small callee, 27 bytes, is 10 of ours, and its bounds of a callee
// and of a function, 460 and 920 bytes, some 180 and 350 -- ours, 48 and
// 192, are tighter; its absolute bound is five times a function's. No
// count of the calls a function inlines bounds it, as none bounds V8's:
// one of eight had, which AWFY's CD's findIntersection spent six of on
// this.delta() twice, three frames each -- delta, Vector3D's minus, its
// constructor -- its minus() calls after them made natively and their
// objects made, which, inlined, escape analysis does not make.
const (
	maxInline         = 48
	maxInlineTotal    = 192
	maxInlineSmall    = 10
	maxInlineAbsolute = 5 * maxInlineTotal
	maxInlineDepth    = 3
)

// inlineSize is p's size in the unit inlining's bounds count: an
// instruction of the stack bytecode the front end once made, in which they
// were measured against V8's bytecode above. Register code's program does
// more in each instruction -- an operation's operands are registers, not
// copies to and from a stack -- and had 0.85 of that bytecode's
// instructions, the median of Are We Fast Yet's 284 functions whose names
// are unique, measured while both were lowered: V8's 2.6 bytes to a stack
// instruction are 3.1 to a register one. So each of its instructions
// counts 20/17 of one, rounded; a slot program's count one each.
func inlineSize(p *ir.Program) int {
	return inlineSizeOf(len(p.Code), p.Origin != nil)
}

// inlineSizeOf is inlineSize of a program of n instructions, a register
// program's where reg says.
func inlineSizeOf(n int, reg bool) int {
	if reg {
		return (n*20 + 8) / 17
	}
	return n
}

// MayInlineSize reports whether a register program of n instructions or
// more may be inlined, as far as its size says: whether one of n is within
// the bound every inlining of a callee checks (InlineCalls,
// InlineCallsGiven, NeverInlined). A program longer than n is no more
// likely, its size growing with n. The VM's snapshot for a compile in the
// background copies the caches of a callee only where its register code,
// which lowers to at least an instruction for each of its own, passes.
func MayInlineSize(n int) bool {
	return inlineSizeOf(n, true) <= maxInline
}

// inlineFits reports whether a callee of n instructions may be inlined
// where a function has inlined total already: within the function's
// budget, or, if n is small (maxInlineSmall), within the absolute bound.
func inlineFits(total, n int) bool {
	return total+n <= maxInlineTotal || n <= maxInlineSmall && total+n <= maxInlineAbsolute
}

// MaxInline is the most instructions a callee inlined may have, and
// MaxInlineTotal the most a function inlines in all.
const (
	MaxInline      = maxInline
	MaxInlineTotal = maxInlineTotal
)

// maxInlineLoop is the most instructions a callee with a loop may have and
// be inlined, and then only if it leaves nothing to Go (smallLoop). Such a
// callee is a search of a short list, EarleyBoyer's sc_assq -- three
// property reads and a compare a turn, a few turns a call, two million
// calls in forty runs -- whose call, some 75 instructions of native call
// sequence and entry, costs about what its loop does, and which adds
// little code where it is inlined. A larger loop runs more turns a call,
// over which the call's cost is spread, and inlining it saves that one
// call against the code it adds: inlining Crypto's am3, a loop of a few
// dozen turns a call, made its callers' code 60% larger and Crypto 25%
// slower.
const maxInlineLoop = 24

// smallLoop reports whether p, a callee with a loop that leaves n
// operations to Go, may be inlined: it is small (maxInlineLoop) and n is
// 0. A call it leaves to Go in its loop would be made, or its callee
// inlined, at each turn; one outside the loop is refused too, the rule
// kept to what was measured.
func smallLoop(p *ir.Program, n int) bool {
	return n == 0 && inlineSize(p) <= maxInlineLoop
}

// NeverInlined reports whether no site inlines p, whatever any feedback
// says: what InlineCalls and InlineCallsGiven refuse before either reads
// anything of the callee's feedback -- too long, or of globals, captured
// locals, a derived constructor's super() or exception handlers. The VM's
// snapshot for a compile in the background asks nothing ahead of such a
// callee's feedback, which the build never asks.
func NeverInlined(p *ir.Program) bool {
	return p == nil || inlineSize(p) > maxInline || len(p.Globals) != 0 || p.Captured != nil || p.DerivedConstructor || len(p.Handlers) != 0
}

// InlineCalls reports whether a callee may be inlined if the operations it
// leaves to Go, which it returns by PC, are calls inlined in it too: a
// small program that reads and writes properties and elements, computes,
// branches forward -- or back, in a loop smaller still (maxInlineLoop) --
// and returns, which native code can do all of but for those calls. An
// exit inside it makes its frame (InlineState), so that what it did before
// is not done again. A callee with a loop is taken only if it is small and
// leaves nothing to Go (smallLoop).
func InlineCalls(p *ir.Program) ([]int, bool) {
	if p == nil || !p.Checked && p.Validate() != nil || inlineSize(p) > maxInline || len(p.Globals) != 0 || p.Captured != nil || p.DerivedConstructor || len(p.Handlers) != 0 || p.Forwards {
		// A callee whose locals closures capture: an inlined frame's
		// slots have no cells, and Go makes no closure on one. A derived
		// constructor's super() is Go's, on a frame of its own. A callee
		// with exception handlers registers none in an inlined frame,
		// which a throw would unwind past them. One that hands on its
		// arguments from its context (ir.Program's Forwards), which an
		// inlined frame has none of: inlined from its forwarding lowering.
		return nil, false
	}
	var calls []int
	loops := false
	for pc, in := range p.Code {
		if !reachable(p, pc) {
			continue
		}
		switch in.Op {
		case ir.Call, ir.Closure, ir.CloseUpvalues, ir.IterStep, ir.IterClose:
			// A loop's cursor's step or its closing is Go's, which would
			// leave the inlined frame at each.
			return nil, false
		case ir.Host:
			calls = append(calls, pc)
		case ir.Jump, ir.Branch:
			if in.Target <= pc {
				loops = true
			}
		}
	}
	if loops && !smallLoop(p, len(calls)) {
		return nil, false
	}
	return calls, true
}

// InlineCallsGiven is InlineCalls for a callee that calls what it is
// given: of the calls it leaves to Go, those whose callee is one of its
// parameters, read just before the call (given; argc is a call's argument
// count, ok false for a method's or one it does not know), apart from the
// others (calls). A loop does not keep it from being inlined where one of
// those calls is in the loop: inlining the callee where its caller gives
// it a closure the caller makes fixes what the loop calls, which is then
// inlined in its turn and the closure not made (calledClosures), as V8
// inlines Array.prototype.forEach's loop and its callback. Elsewhere a
// call of a callee with a loop is made once for all the loop's turns,
// and inlining it saves only that call's cost, spread over them, against
// the code it adds to its caller: inlining Crypto's am3, a loop of a few
// dozen turns a call, made its callers' code 60% larger and Crypto 25%
// slower. A callee whose loops call none of what it is given is refused,
// but for one InlineCalls takes, small and leaving nothing to Go
// (smallLoop).
func InlineCallsGiven(p *ir.Program, argc func(pc int) (int, bool)) (calls, given []int, ok bool) {
	if p == nil || !p.Checked && p.Validate() != nil || inlineSize(p) > maxInline || len(p.Globals) != 0 || p.Captured != nil || p.DerivedConstructor || len(p.Handlers) != 0 {
		return nil, nil, false
	}
	// Each loop, by its back edge: from its header to the branch.
	type span struct{ from, to int }
	var loops []span
	targets := make([]bool, len(p.Code)+1)
	for pc, in := range p.Code {
		if !reachable(p, pc) {
			continue
		}
		switch in.Op {
		case ir.Call, ir.Closure, ir.CloseUpvalues, ir.IterStep, ir.IterClose:
			return nil, nil, false
		case ir.Jump, ir.Branch:
			targets[in.Target] = true
			if in.Target <= pc {
				loops = append(loops, span{in.Target, pc})
			}
		}
	}
	looped := len(loops) == 0
	for pc, in := range p.Code {
		if !reachable(p, pc) || in.Op != ir.Host {
			continue
		}
		if !givenCall(p, pc, targets, argc) {
			calls = append(calls, pc)
			continue
		}
		given = append(given, pc)
		for _, l := range loops {
			looped = looped || l.from <= pc && pc <= l.to
		}
	}
	if !looped && !smallLoop(p, len(calls)+len(given)) {
		return nil, nil, false
	}
	return calls, given, true
}

// looping reports whether p has a loop: a backward branch.
func looping(p *ir.Program) bool {
	for pc, in := range p.Code {
		if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc && reachable(p, pc) {
			return true
		}
	}
	return false
}

// loopHolds reports whether one of p's loops holds pc: a backward branch
// at or after it to a header at or before it.
func loopHolds(p *ir.Program, pc int) bool {
	for q, in := range p.Code {
		if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc && pc <= q && reachable(p, q) {
			return true
		}
	}
	return false
}

// givenCall reports whether p's call at pc calls one of p's parameters: its
// callee the slot a copy of a parameter wrote, at the depth below the
// call's arguments, in the same block (targets marks the PCs a branch goes
// to) -- a copy of one, or the second of a pair's (get_local2 s f, as
// s = s + f(x) compiles). Which value the call calls the SSA says
// (calledClosures); this only picks the calls that may call a closure the
// caller gives.
func givenCall(p *ir.Program, pc int, targets []bool, argc func(pc int) (int, bool)) bool {
	n, ok := argc(pc)
	if !ok || pc >= len(p.Maps) {
		return false
	}
	if in := p.Code[pc]; in.Window {
		// Register code's: its window's function copied from a parameter's
		// register, the last write of that register before the call.
		a := in.Left.Slot
		for q := pc - 1; q >= 0 && !targets[q+1]; q-- {
			c := p.Code[q]
			if c.Op == ir.CopyPair && (c.Dest == a || c.Extra == a) {
				return false
			}
			if c.Dest == a {
				return c.Op == ir.Copy && c.Left.Slot >= p.ParamBase && c.Left.Slot < p.ParamBase+p.Params
			}
		}
		return false
	}
	depth := p.Maps[pc].Depth - n - 1
	if depth < 0 {
		return false
	}
	slot := p.Locals + depth
	param := func(o ir.Operand) bool { return o.Slot >= 0 && o.Slot < p.Params }
	for q := pc - 1; q >= 0 && !targets[q+1]; q-- {
		in := p.Code[q]
		switch d := p.Maps[q].Depth; {
		case d > depth:
			continue
		case d == depth:
			return (in.Op == ir.Copy || in.Op == ir.CopyPair) && in.Dest == slot && param(in.Left)
		case d == depth-1:
			return in.Op == ir.CopyPair && in.Extra == slot && param(in.Right)
		}
		return false
	}
	return false
}

// InlineExitFeedback is Feedback that keeps a callee inlined with an
// operation it leaves to Go that native code neither makes itself nor
// inlines (InlineCalls): InlineExit reports, of an inlined callee's
// feedback, whether its operation at pc is one seldom run -- a throw, or
// a call the callee has never been seen to make -- at which native code
// may leave the inlined frame, Go making its frame (InlineState) and the
// interpreter the operation, as V8's code inlines a function whose cold
// paths deoptimize or call the runtime to throw. AWFY Towers' pushDisk
// and popDiskFrom, each guarding with throw new Error(...), had been
// called natively at every move, and their caller with them.
type InlineExitFeedback interface {
	InlineExit(pc int) bool
}

// inlineExit reports whether the frame being translated, an inlined
// callee, may leave its operation at pc to an exit (InlineExitFeedback).
func (b *builder) inlineExit(pc int) bool {
	f, ok := b.fb.(InlineExitFeedback)
	return ok && b.cur != b.root && f.InlineExit(pc)
}

// GlobalSite is a global read's site: its name, the VM's atom, and the
// index in the global object's table where the binding was found -- or,
// Lexical, in the table of the script-level lexical bindings', a let's,
// a const's or a class's. Fixed says the binding can never change -- a
// data property neither writable nor configurable, as undefined, NaN and
// Infinity are, or a const that holds its value -- and Constant is its
// value then: the read checks the binding is still where it was, and uses
// the constant.
//
// Holder, for a read, is the address of the object that has the binding,
// where it is not the scope's (abi.Context's Global): the module an import
// names its binding in, or the global object, for a global a module's code
// reads, which its environment, whose bindings are fixed, does not have.
// The VM keeps the object as long as the code. Shadowable says a
// script-level lexical binding of the name would shadow it, as for the
// global object's.
//
// Typed, for a read, says the binding held a typed array when the code was
// compiled: an element read of what the read found, whatever native code
// does with the element, reads a typed array's number (TypedRead's), and
// Go reads any other's, as V8 compiles for a global cell's constant type.
// Emscripten's code reads its heap through such globals, at every branch:
// learned at each read's exit instead, in turn, a function was compiled
// again for each, a dozen times, as each invocation of it came to one
// more. TypedKind is that typed array's element type plus one: an element
// read or write of a number of what the read found, at a site that has
// learned no type of its own (TypedKindFeedback), is compiled for it.
//
// Epoch, for a read of the global object's own binding, is one more than
// the VM's count of changes to the global object's bindings other than
// appends, and of the script-level lexical bindings declared, when the
// code was compiled (abi.Context's GlobalEpoch), and Scope that object:
// while the count is unchanged the binding is where it was, plain data,
// unshadowed, and the read checks the count and the object rather than
// each of those. 0 for none.
type GlobalSite struct {
	Key        uint32
	Index      int32
	Lexical    bool
	Fixed      bool
	Constant   ir.Value
	Holder     uintptr
	Shadowable bool
	Typed      bool
	TypedKind  uint8
	Epoch      uint32
	Scope      uintptr
}

// PropertySite is a property site: its key, the VM's atom; and, if the
// site has met objects of one shape, that shape's address, which the VM
// keeps alive and unchanged, with the index of the property -- a writable
// data property, for a write -- in their tables. Shape is 0 otherwise.
//
// A read may have found the property on a prototype, a method most often:
// Holders then name the objects from the receiver's prototype on, one or
// two, each with the shape it had, and Index is the property's in the last
// one's table. The receiver's shape says it has no such property of its
// own; each holder's, that the one before it has none, and the last's
// where it is.
//
// A read may have met objects of other shapes too, as V8's polymorphic
// inline caches keep up to four: Cases are those, each with its own
// holders and index, which the read checks after Shape's.
type PropertySite struct {
	Key     uint32
	Shape   uintptr
	Index   int32
	Holders [2]Holder
	// More are the holders past Holders', for a chain deeper than the
	// VM's caches hold: a method a class two levels below the one that
	// has it inherits, as DeltaBlue's constraints' addConstraint.
	More  []Holder
	Cases []PropertyCase
	// Private, for a private member's read or write, is the address of
	// its key, the symbol the code checks the binding holds
	// (OpSameSymbol), the VM keeping it.
	Private uintptr
	// Add, for a write whose cache adds the property, says how; Shape is
	// then 0. Adds are the adds a write met objects of other shapes take,
	// as V8's polymorphic stores keep a transition for each map; a write's
	// Cases are the own properties others have.
	Add  *PropertyAdd
	Adds []*PropertyAdd
	// Megamorphic says the site's cache is megamorphic: it has met more
	// than its cases hold, or what no case can hold. A receiver of another
	// shape has its own table searched and is looked for in the
	// megamorphic cache (abi.MegaCache), and leaves for Go only if neither
	// has the property, as V8's megamorphic loads and stores look further
	// rather than deoptimize. At any other site such a receiver, and any
	// receiver at a site that knows no shape, deoptimizes (ir.GuardExit),
	// as V8's map checks do: the interpreter makes the operation, its cache
	// learning the shape, and the code is compiled again for it.
	Megamorphic bool
	// Primitives, for a megamorphic site's read or write, says it has met
	// receivers that are primitives -- strings, numbers, booleans -- whose
	// reads and writes Go makes on the operation's branch of its own
	// (ObjectOfTry), rather than at an exit; a read of undefined or null,
	// which throws, exits still.
	Primitives bool
	// Scan, for a read whose cache is empty though it may have run, has a
	// receiver's own small table searched for
	// the key, plain data, and deoptimizes where it does not have it,
	// rather than at every receiver.
	Scan bool
	// Primitive, for a read StringFeedback knows, is the kind of its
	// receivers: a string's, whose method String.prototype has, or
	// another's, whose method the prototype at Holders[0] has; or, for
	// either, one a prototype of the kind's has, Holders and More the
	// prototypes from the kind's up to that one, which Shape and Index
	// are of (OpStringProp).
	// PrimitiveCases are the other kinds it has met, each a site as this
	// one is, of a kind of its own: the read tells them apart by the
	// receiver's word, as it tells them from the objects' cases.
	Primitive      Primitive
	PrimitiveCases []PropertySite
}

// unfed reports whether the property operation at pc has a site that
// knows nothing to make it natively with -- no shape, no case, no
// addition, its cache not megamorphic -- and so always deoptimizes, as
// V8's code deoptimizes for insufficient feedback: its block ends there,
// with no entry after it.
func (b *builder) unfed(pc int) bool {
	switch b.p.Code[pc].Op {
	case ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead:
	case ir.Binary, ir.Branch, ir.Unary, ir.Update:
		// An operator whose operands met nothing (unseen): left to Go at
		// a host exit, with no entry after it.
		return b.unseen(pc)
	default:
		return false
	}
	if b.p.Code[pc].Private || b.host(pc) {
		return false
	}
	if _, ok := b.stringProperty(pc); ok {
		return false
	}
	site, ok := b.property(pc)
	return ok && unfedSite(site)
}

// unfedSite reports whether site knows nothing to make its operation with.
func unfedSite(site PropertySite) bool {
	return site.Shape == 0 && len(site.Cases) == 0 && site.Add == nil && len(site.Adds) == 0 && site.Holders[0].Object == 0 && !site.Megamorphic && !site.Scan
}

// PropertyAdd is a write that adds its property, as the VM's cache has it
// (its propCache.adds): to an object of shape From that is extensible, whose prototypes are Protos -- each object, with its shape,
// the first's prototype the second, the last's none; an Object of 0 ends
// the chain -- the property goes at the end of its table, if the table
// has room, with Flags, and the object takes the shape Next. Nothing up
// the chain intercepts the write: the cache found so for those shapes.
type PropertyAdd struct {
	From   uintptr
	Next   uintptr
	Flags  uint8
	Protos [2]Holder
	// Define marks a literal's field (ir.FieldDefine), which its
	// prototypes do not intercept: none is looked at, and an object it is
	// not added to exits. Key is then the field's, the VM's atom.
	Define bool
	Key    uint32
	// More are the prototypes past Protos', for a chain deeper than the
	// VM's caches hold, as the VM found it at an exit (DeltaBlue's
	// constraints: their class's prototype, its superclass's,
	// Constraint's, Object's).
	More []Holder
}

// Chain is the add's prototype chain, object by object, its end after the
// last: Protos' up to the first of none, then More.
func (a *PropertyAdd) Chain() []Holder {
	var chain []Holder
	for _, h := range a.Protos {
		if h.Object == 0 {
			return chain
		}
		chain = append(chain, h)
	}
	return append(chain, a.More...)
}

// PropertyCase is one more shape a read's site met (PropertySite.Cases):
// the receiver's shape, the prototypes the property was found on, none if
// it is the receiver's own, and its index in the last one's table.
type PropertyCase struct {
	Shape   uintptr
	Index   int32
	Holders [2]Holder
	// More are the holders past Holders' (PropertySite's More).
	More []Holder
}

// HolderChain is the prototypes a read's site found its property on
// (PropertySite's Holders and More), which the read checks.
type HolderChain struct {
	Holders [2]Holder
	More    []Holder
}

// List is the chain, object by object: Holders' up to the first of none,
// then More.
func (h *HolderChain) List() []Holder {
	var chain []Holder
	for _, x := range h.Holders {
		if x.Object == 0 {
			return chain
		}
		chain = append(chain, x)
	}
	return append(chain, h.More...)
}

// Holder is a prototype a property read was answered by, or passed through,
// at its address, which the VM keeps alive, with the shape it had: 0 for
// none.
type Holder struct{ Object, Shape uintptr }

// BuildWith is Build with what the VM knows of the sites.
func BuildWith(p *ir.Program, fb Feedback) (*Func, error) { return build(nil, p, fb) }

// build builds p, and, where the build finds closures the function makes
// that nothing uses but its calls of them (calledClosures), builds it again
// with those calls inlined and the closures not made, as V8 inlines a
// call whose target its graph knows and its escape analysis then removes
// the closure. The first build stands where the second fails.
//
// Where the root makes a closure, the first build also inlines callees that
// call what they are given (InlineCallsGiven), leaving those calls to
// exits, whose states say whether what a callee calls is a closure the
// root gave it: the second build inlines those, the callees around them
// and their loops, and leaves the closures unmade too (a closure given to
// SOM's Vector.forEach, called in its loop). Such a first build is never
// the code: where nothing was found, or the second build is refused
// (unmadeAlone), the function is built again without them.
//
// The build that stands is asked escape analysis's questions
// (askVirtualRoots): what the optimization needs of the VM is asked here.
func build(w *Workspace, p *ir.Program, fb Feedback) (f *Func, err error) {
	if p.Kinds != nil {
		// Typed code's raw instructions refuse what the feedback does not
		// answer (raw.go).
		defer func() {
			if v := recover(); v != nil {
				e, ok := v.(rawRefusal)
				if !ok {
					panic(v)
				}
				f, err = nil, e
			}
		}()
	}
	f, err = buildChosen(w, p, fb)
	if err == nil {
		askVirtualRoots(f, fb)
	}
	return f, err
}

// buildChosen is build's choice of builds (above).
func buildChosen(w *Workspace, p *ir.Program, fb Feedback) (*Func, error) {
	// Checked once for all its builds (buildCalls), which do not change
	// it, where its maker has not (ir.Program's Checked).
	if !p.Checked {
		if err := p.Validate(); err != nil {
			return nil, err
		}
	}
	f, plan, provisional, err := buildCalls(w, p, fb, nil)
	if err != nil {
		return f, err
	}
	if len(plan.calls) != 0 || len(plan.given) != 0 {
		if g, _, _, err := buildCalls(w, p, fb, &plan); err == nil {
			return g, nil
		}
		if !provisional {
			return f, nil
		}
		if len(plan.given) != 0 && len(plan.calls) != 0 {
			// The root's own calls of its closures, without the callees'.
			if g, _, _, err := buildCalls(w, p, fb, &calledPlan{calls: plan.calls}); err == nil {
				return g, nil
			}
		}
	}
	if !provisional {
		return f, nil
	}
	f, _, _, err = buildCalls(w, p, fb, &calledPlan{})
	return f, err
}

// calledPlan is what a first build found to inline of the closures the
// root makes and only calls (calledClosures): calls, by the PC of each of
// the root's calls of one, the PC of the closure; given, by the path of a
// callee's call of what it is given (frame's path), the PC of the closure
// the root gave it.
type calledPlan struct {
	calls map[int]int
	given map[string]int
}

// buildCalls is a build of p, the calls plan names inlined, the closures
// not made where all their calls are; and, with plan nil, the closures it
// finds so (calledClosures), and whether it inlined callees whose calls of
// what they are given it left to exits (provisional), which makes it no
// code to run. p is checked (ir.Program's Validate) by its caller.
func buildCalls(w *Workspace, p *ir.Program, fb Feedback, plan *calledPlan) (*Func, calledPlan, bool, error) {
	p = referenceReads(p, fb)
	f := &Func{Locals: p.Locals, StackSize: p.StackSize, FrameLocals: p.Locals, ThisSlot: -1, ws: w, Kinds: p.Kinds,
		Handlers: p.Handlers, HandlerAt: p.HandlerAt}
	if kf, ok := fb.(KeepFeedback); ok {
		f.KeepLimit = kf.KeepLimit()
	}
	if af, ok := fb.(AppendFeedback); ok {
		f.ArrayProtos, _ = af.ArrayProtos()
	}
	if hf, ok := fb.(HasCacheFeedback); ok {
		f.HasCache = hf.HasCache()
	}
	if kf, ok := fb.(KeyAtomsFeedback); ok {
		f.KeyAtoms, f.IntStrings = kf.KeyAtoms()
	}
	f.written = f.bools(p.Locals + p.StackSize)
	f.assigned = p.Assigned
	b := &builder{p: p, fb: fb, f: f, nslots: p.Locals + p.StackSize}
	b.root = f.newFrame()
	*b.root = frame{p: p, fb: fb, at: -1, givenPC0: -1}
	b.cur = b.root
	if plan != nil {
		b.called, b.given = plan.calls, plan.given
	} else if _, ok := fb.(CalledClosureFeedback); ok {
		b.provisional = makesClosure(p)
	}
	if err := b.plan(); err != nil {
		return nil, calledPlan{}, false, err
	}
	b.cellsBack()
	b.translate()
	if b.atomicErr != nil {
		return nil, calledPlan{}, false, b.atomicErr
	}
	bigLeafSites(f)
	f.slotCount = b.nslots
	if len(b.inlined) != 0 {
		for _, pc := range sortedKeys(b.inlined) {
			if fr := b.inlined[pc]; !fr.given && p.Code[pc].Op == ir.Host {
				f.InlinedAt = append(f.InlinedAt, pc)
			}
		}
	}
	provisional, found := false, calledPlan{}
	if plan == nil {
		found.calls, found.given = b.calledClosures()
		provisional = b.pendingAny
	} else if err := b.unmadeAlone(); err != nil {
		return nil, calledPlan{}, false, err
	}
	// The shadows and the stores' checks are Optimize's to make, which
	// would remake them, or the first to run or compile f's (Finish).
	return b.f, found, provisional, nil
}

// makesClosure reports whether p makes a closure (ir.Closure) where it
// runs.
func makesClosure(p *ir.Program) bool {
	for pc, in := range p.Code {
		if in.Op == ir.Closure && reachable(p, pc) {
			return true
		}
	}
	return false
}

type builder struct {
	// inlineAsked are InlineCalls' answers, each program's once a build
	// (inlineCallsOf).
	inlineAsked []inlineAnswer
	// stepOn is the register a register loop's step writes, by the block
	// where its loop goes on, which assigns it there (iterStep).
	stepOn map[*Block]stepDef
	// atomic says the root's instruction being built follows one of its
	// register instruction's that committed (atomicInstruction);
	// atomicErr is the exit such a build made.
	atomic    bool
	atomicErr error
	// kindsOwn says the Func's Kinds are its own, not its root program's,
	// which inlined typed callees' kinds extend (frameKinds).
	kindsOwn bool
	// p and fb are the program the block being translated is in, and its
	// feedback: the function's own (root), or an inlined callee's.
	p      *ir.Program
	fb     Feedback
	f      *Func
	nslots int
	// named is where stateSlots lists a state's slots; stating says a
	// state is being made (stateSlots).
	named   []int32
	stating bool

	// root is the function's own frame, cur the one translated now;
	// inlined are the callees, by the PC of their call; frameOf is each
	// block's frame, by block ID, nil for the function's own.
	root, cur *frame
	inlined   map[int]*frame
	// calls are the calls made natively (OpCall), by PC: their blocks go
	// on to the next instruction's.
	calls [][]*CallSite
	// callKeeps are, by PC, the keep cells of the calls' results (calls),
	// plus one: 0 for a PC with none.
	callKeeps []int
	// closureKeeps are, by PC, the keep cells of the closures the root's
	// code makes, or -1 for one Go makes (closureKeep).
	closureKeeps map[int]int
	// forEaches are the forEach calls the root makes a loop of, by PC
	// (forEachAt), decided in planning.
	forEaches map[int]forEachPlan
	// made are the closures the root's code does not make, by PC: the
	// callbacks of loops their calls are inlined in (forEachPlan's made),
	// each its template's address (MadeCallback's Template).
	made map[int]uintptr
	// called are the calls to inline of closures the function makes that
	// nothing else uses (calledClosures), by PC, each the PC of its
	// closure; madeCalls the closures their inlining leaves unmade.
	called    map[int]int
	madeCalls map[int]*MadeCallback
	// typedArrays are the global reads whose bindings held typed arrays,
	// each the element type plus one (GlobalSite's TypedKind), or typedAny
	// (GlobalSite's Typed), and the reads of captured bindings that did
	// (UpvalueTypedFeedback), which element reads of them read as such.
	typedArrays map[*Value]uint8
	frameOf     []*frame
	// stale are the captured bindings' and captured locals' markers
	// (OpUpvalueStale), by slot; here is the block of the instruction
	// being built, at herePC, whose state a read there loads a binding
	// from its cell with (read); rootSt is the state of the root frame's
	// instruction being built, at rootPC in rootBlk, which an inlined
	// callee's leaves as it is, made once, as its guards share it
	// (rootState).
	stale   []*Value
	here    *Block
	herePC  int
	rootSt  *FrameState
	rootBlk *Block
	rootPC  int

	// heaps are the cells of the heaps typed code read (heapOf), each a
	// variable of the slots heapSlot gives; heapMarks their slots' stale
	// markers; heapKills the blocks a call made them stale in.
	heaps     []uintptr
	heapMarks []*Value
	heapKills []*Block
	// tableIdx is, for each function typed code read from a function
	// table, the index it read it at (tableRead).
	tableIdx map[*Value]*Value

	entryPCs []int
	blockAt  []*Block // by the PC a block starts at
	// endOf is the last PC of each block, by block ID.
	endOf []int

	// By block ID, which is unique while the builder runs: each block's
	// definitions by slot (defTable), whether it is sealed and filled, and
	// the phis it made before it was sealed. translating says the blocks
	// are being filled.
	defs        defTable
	translating bool
	// marks mark a sealed block's slot whose predecessors' values are
	// being read (mergeVar), each of the block markBlock, markDepth of
	// them in use; and phiArgs holds those values, each read's after the
	// reads it is inside of.
	marks     []*Value
	markDepth int
	phiArgs   []*Value
	// dirtyLists and mergeDiff are stateSlots' lists of the slots to read
	// again (mergeFrom).
	dirtyLists [][]int32
	mergeDiff  []int32
	last       []lastState
	sealed     []bool
	filled     []bool
	incomplete [][]slotPhi
	// nextBlock is the ID a block an instruction splits off is given
	// (splitOn); next, the block the instructions after the one being
	// built go on in, if it split its own; splits, the blocks the
	// instructions of the block being filled split off.
	nextBlock int
	next      *Block
	splits    []*Block

	// BigInts' low bits (wraps): wrapped holds, by PC, whether the
	// operator there is made on its operands' low 64 bits, 1 if so and 2
	// if not, once asked; words, the words such operators made, by the
	// placeholder each one's slot holds until its operator or call takes
	// it; region, the state their guards exit to, the first one's.
	wrapped []uint8
	words   map[*Value]*Value
	region  *FrameState
	// sole is the locals that hold their values alone, and soleOK whether
	// the code may give BigInts' blocks back (BigIntRecycleFeedback), once
	// asked (soleAsked).
	sole              uint64
	soleOK, soleAsked bool

	// provisional says a first build inlines callees whose calls of what
	// they are given (InlineCallsGiven) it leaves to exits, for its
	// analysis to find the closures the root gives them (calledClosures);
	// givenStates are those exits' states. given are, in a second build,
	// the calls of what a callee is given to inline, by their paths (frame's
	// path), each the PC of the closure the root makes that it calls.
	provisional bool
	pendingAny  bool
	givenStates []givenState
	given       map[string]int
}

// givenState is a first build's exit at a call of what an inlined callee
// is given (frame's pending): the frame, the call's PC and the state.
type givenState struct {
	fr    *frame
	pc    int
	state *FrameState
}

// property is the feedback for a property operation at pc, if any.
func (b *builder) property(pc int) (PropertySite, bool) {
	if op := b.p.Code[pc].Op; b.fb == nil || op != ir.PropertyRead && op != ir.PropertyWrite && op != ir.ReferenceRead {
		return PropertySite{}, false
	}
	a := b.answer(pc)
	if a.asked&askedSite == 0 {
		site, ok := b.fb.Property(pc)
		a = b.answer(pc)
		a.site, a.ok, a.asked = site, ok, a.asked|askedSite
	}
	return a.site, a.ok
}

// host reports whether the instruction at pc is left to Go whole: it ends
// its block, and the next PC is an entry. The plan asks it of each
// instruction at each leader, block's end and entry, and each answer is
// a search of the VM's caches, so the frame keeps its answers (hosts),
// which nothing the plan does changes.
func (b *builder) host(pc int) bool {
	fr := b.cur
	if fr.hosts == nil {
		fr.hosts = b.f.bytes(len(b.p.Code))
	}
	if k := fr.hosts[pc]; k != 0 {
		return k == 2
	}
	h := b.hostOf(pc)
	fr.hosts[pc] = 1
	if h {
		fr.hosts[pc] = 2
	}
	return h
}

// hostOf is host's answer, found.
func (b *builder) hostOf(pc int) bool {
	switch b.p.Code[pc].Op {
	case ir.Host:
		return !b.nativeHost(pc)
	case ir.Call:
		return true
	case ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead:
		if _, ok := b.stringProperty(pc); ok {
			return false
		}
		if _, ok := b.accessor(pc); ok {
			// A call of its getter or setter (AccessorSite), as a call is
			// left to Go but where it is made natively.
			return true
		}
		_, ok := b.property(pc)
		// Register code's private member Go reads or writes from native
		// code where native code does not (whole), which goes on after it.
		return !ok && !(b.p.Code[pc].Private && b.wholeAt(pc))
	case ir.BindingRead:
		_, ok := b.global(pc)
		return !ok
	case ir.BindingWrite:
		site, ok := b.global(pc)
		return !ok || site.Fixed
	case ir.BindingCheck:
		_, ok := b.global(pc)
		return !ok
	case ir.ObjectLiteral, ir.ArrayLiteral:
		_, _, ok := b.literal(pc)
		return !ok
	case ir.StringConst:
		_, ok := b.stringCell(pc)
		return !ok
	case ir.BigIntConst:
		f, ok := b.fb.(BigIntConstFeedback)
		if !ok {
			return true
		}
		_, _, _, ok = f.BigIntCell(pc)
		return !ok
	case ir.UpvalueRead, ir.UpvalueWrite:
		// The function's own captured bindings, which its context has;
		// an inlined callee's where its closure's cells are known
		// (InlineSite's Upvalues), or are the root's own (Captured); the
		// rest Go's.
		return b.cur != b.root && b.cur.site.Upvalues == 0 && b.cur.site.Captured == nil
	case ir.StringAdd, ir.StringConcat, ir.HasProperty, ir.DeleteProperty:
		// A call of Go (GoAdd, GoConcat, GoHas, GoDelete), where the VM
		// answers it.
		return !b.goCalls()
	case ir.StringPart:
		// Checked natively; where the check failed before, Go's as a concat
		// of one part.
		return b.fb != nil && b.fb.Generic(pc) && !b.goCalls()
	case ir.FieldDefine:
		// One whose definition is not planned Go makes from native code
		// (whole), which goes on after it: no exit, whose block would have
		// no state.
		_, ok := b.define(pc)
		return !ok && !b.wholeAt(pc)
	case ir.Rethrow:
		return b.generic(pc)
	case ir.ArgumentRead:
		// The call's arguments, an inlined callee's (InlineSite's
		// ReadsArguments), but where a read of them failed before; the
		// function's own, a call of Go (GoArgument), where the VM answers
		// it.
		if b.cur == b.root {
			return !b.goCalls() || b.f.Keeps >= b.f.keepLimit()
		}
		return !b.cur.site.ReadsArguments && !(b.cur.site.Window && b.cur.site.Arguments) || b.generic(pc)
	case ir.IterStep:
		// A call of Go (GoCall, Step), where the VM answers it. Register
		// code's loop step writes its register only where the loop goes
		// on: the root's assigns it in the block after the step (stepOn),
		// which the step alone may reach; an inlined callee's, and one
		// whose next instruction something else jumps to, is Go's.
		if !b.goCalls() {
			return true
		}
		if b.p.Origin != nil && !b.p.Code[pc].When {
			return b.cur != b.root || b.stepJoined(pc)
		}
		return false
	case ir.Closure, ir.CloseUpvalues:
		// A call of Go (GoClosure, GoClose), on the function's own frame --
		// a closure's with a keep cell for it (closureKeep).
		if b.cur != b.root || !b.goCalls() {
			return true
		}
		if b.p.Code[pc].Op == ir.Closure {
			_, ok := b.closureKeep(pc)
			return !ok
		}
	}
	return false
}

// stepDef is a register loop step's result and its register (stepOn).
type stepDef struct {
	slot int
	v    *Value
}

// stepJoined reports whether anything but the loop step at pc goes to the
// instruction after it: a jump, a branch, another step, a handler.
func (b *builder) stepJoined(pc int) bool {
	next := pc + 1
	for q, in := range b.p.Code {
		if q != pc && (in.Op == ir.Jump || in.Op == ir.Branch || in.Op == ir.IterStep) && in.Target == next {
			return true
		}
	}
	for _, h := range b.p.Handlers {
		if int(h.PC) == next {
			return true
		}
	}
	return false
}

// closureKeep is the keep cell the closure the root's code makes at pc is
// kept in, or false for one Go makes at an exit. The cell is taken the
// first time planning asks (host), as a literal's is (literal): taken only
// when the closure was built -- after the calls planned before it had
// taken theirs -- it could be one too many, the closure then left to an
// exit where planning had made none, and made by no one, its local left
// as it was: undefined, which a call of it then called.
func (b *builder) closureKeep(pc int) (int, bool) {
	if k, ok := b.closureKeeps[pc]; ok {
		return k, k >= 0
	}
	k := -1
	if b.f.Keeps < b.f.keepLimit() {
		k = b.f.Keeps
		b.f.Keeps++
	}
	if b.closureKeeps == nil {
		b.closureKeeps = map[int]int{}
	}
	b.closureKeeps[pc] = k
	return k, k >= 0
}

// thisCheckKind is how a check of slot that it is initialized leaves where
// it is not (ir.Instruction's Check): a register code's derived
// constructor's this slot (ir.Program's This), unbound, to Go, which
// throws the ReferenceError, or a guard's exit, the interpreter going on.
func (b *builder) thisCheckKind(slot int) ir.ExitKind {
	if p := b.p; p.Origin != nil && p.This && slot == p.UpvalueBase+p.Upvalues {
		return ir.HostExit
	}
	return ir.GuardExit
}

// literalKeep is a literal's site and the keep cell its object is kept in,
// or -1 for a literal Go makes.
type literalKeep struct {
	site LiteralSite
	keep int
}

// literal is the site of the object literal at pc of the frame being
// translated (LiteralFeedback), if its code may make it natively, and the
// keep cell its object is kept in.
// The cell is taken the first time planning asks (host), and the answer
// kept: planning and building must agree, and a cell taken only when the
// literal was built -- after the calls before it had taken theirs -- could
// be one too many, the literal then left to Go where planning had made no
// exit for it, and the literal not made at all.
func (b *builder) literal(pc int) (LiteralSite, int, bool) {
	fr := b.cur
	if k, ok := fr.literals[pc]; ok {
		return k.site, k.keep, k.keep >= 0
	}
	k := literalKeep{keep: -1}
	if f, ok := b.fb.(LiteralFeedback); ok && b.f.Keeps < b.f.keepLimit() {
		if site, ok := f.Literal(pc); ok && site.Pool != 0 {
			k = literalKeep{site: site, keep: b.f.Keeps}
			b.f.Keeps++
		}
	}
	if fr.literals == nil {
		fr.literals = map[int]literalKeep{}
	}
	fr.literals[pc] = k
	return k.site, k.keep, k.keep >= 0
}

// reloadUpvalues has each of the function's captured bindings be its cell's
// value now: at an entry, and after a native call, whose callee may assign
// one. Between, a binding is the value last read or assigned, as a local
// is; unused, the loads are dead. After a call, the loads carry the state
// there, st, which a guard that unboxes one exits to (unboxPhis), as an
// entry's header is for its loads.
//
// A local an inner closure captures (ir.Program's Captured) is so too: the
// closure reads and assigns it where the frame has it, which is its cell.
func (b *builder) reloadUpvalues(blk *Block) { b.reloadCells(blk, b.root.p.Captured) }

// reloadCells is reloadUpvalues of the captured bindings and of locals,
// those of the root's that locals marks.
func (b *builder) reloadCells(blk *Block, locals []bool) {
	p := b.root.p
	if b.stale == nil {
		b.stale = make([]*Value, p.UpvalueBase+p.Upvalues)
	}
	mark := func(slot int) {
		if b.stale[slot] == nil {
			b.stale[slot] = b.f.alloc(Value{Op: OpUpvalueStale, Type: Tagged, Index: slot})
		}
		b.write(slot, blk, b.stale[slot])
	}
	for i := range p.Upvalues {
		mark(p.UpvalueBase + i)
	}
	for i, c := range locals {
		if c {
			mark(i)
		}
	}
}

// reloadAfterCall is reloadUpvalues after a call, which may assign a
// captured binding, or a local a closure assigns -- not one no closure
// assigns (ir.Program's Assigned), which the call leaves as it is. The
// heaps typed code reads are stale after it too, the call able to
// detach or move their buffers (heapOf).
func (b *builder) reloadAfterCall(blk *Block) {
	b.staleHeaps(blk)
	if p := b.root.p; p.Upvalues != 0 || p.Assigned != nil {
		b.reloadCells(blk, p.Assigned)
	}
}

// hasCells reports whether the function has captured bindings, or locals
// closures capture: what a call may assign (reloadUpvalues) -- or is
// typed code, whose heaps a call may change (heapOf).
func (b *builder) hasCells() bool {
	return b.root.p.Upvalues > 0 || b.root.p.Captured != nil || b.root.p.Kinds != nil
}

// cellOf is the cell of slot, a captured binding's or a captured local's,
// in blk: where its value is now.
func (b *builder) cellOf(blk *Block, slot int) *Value {
	p := b.root.p
	if p.Cells && slot < p.UpvalueBase {
		// A captured local in a cell of its own (ir.Program's Cells).
		cell := b.f.newValue(blk, OpUpvalueCell, Source)
		cell.Index, cell.Aux = cellIndex(p, slot), 1
		return cell
	}
	if slot >= p.UpvalueBase {
		cell := b.f.newValue(blk, OpUpvalueCell, Source)
		cell.Index = slot - p.UpvalueBase
		return cell
	}
	cell := b.f.newValue(blk, OpLocalCell, Source)
	cell.Index = slot
	return cell
}

// loadUpvalue is slot, a captured binding or a captured local, loaded from
// its cell at the end of blk, with st the state a guard that unboxes it
// exits to (unboxPhis), if any: a read's, which the binding's variable met
// marked stale.
// reloadStale is slot's value as blk leaves it where its value there is
// stale, the marker: a captured binding's or a heap's, loaded from its
// cell at blk's end.
func (b *builder) reloadStale(blk *Block, slot int, stale *Value) *Value {
	if slot < 0 {
		v := b.heapLoad(blk, slot)
		b.found(slot, blk, v)
		return v
	}
	v := b.loadUpvalue(blk, stale.Index, nil)
	b.write(slot, blk, v)
	return v
}

func (b *builder) loadUpvalue(blk *Block, slot int, st *FrameState) *Value {
	cell := b.cellOf(blk, slot)
	v := b.f.newValue(blk, OpLoadCell, Tagged, cell)
	v.Shadow, v.State = cell, st
	return v
}

// rootState is the state of the root frame's instruction being built,
// made the first time it is asked for.
func (b *builder) rootState() *FrameState {
	if b.rootSt == nil {
		cur := b.cur
		b.cur = b.root
		b.rootSt = b.state(b.rootBlk, b.rootPC)
		b.cur = cur
	}
	return b.rootSt
}

// storeCaptured assigns v to captured local slot in its cell, where an
// inner closure reads it, as an assignment to a captured binding does
// (ir.UpvalueWrite): exiting, if it must, to the state of the root frame's
// instruction that assigns it, which Go makes again -- the state its
// guards share, not one of its own: a state names every live slot, and a
// function of many captured locals made one at each assignment.
func (b *builder) storeCaptured(blk *Block, slot int, v *Value) {
	b.storeCapturedAt(blk, slot, v, b.rootState())
}

// storeCapturedAt is storeCaptured exiting, if it must, to st.
func (b *builder) storeCapturedAt(blk *Block, slot int, v *Value, st *FrameState) {
	w := b.f.newValue(blk, OpPropWrite, None, b.cellOf(blk, slot), v)
	w.Aux, w.State = int(ir.HostExit), st
	st.addUse()
	w.Key, w.Upvalue = localKey(slot), true
	if b.root.p.Cells {
		// A cell of its own, as a captured binding's is.
		w.Key = upvalueKey
	}
}

// cellIndex is captured local slot's index among the cells of a program
// whose captured locals are in cells of their own (ir.Program's Cells):
// how many captured locals come before it, as the VM's GoCells makes them.
func cellIndex(p *ir.Program, slot int) int {
	n := 0
	for i := range slot {
		if p.IsCaptured(i) {
			n++
		}
	}
	return n
}

// stringCell is the cell of the string constant at pc (ConstantFeedback).
func (b *builder) stringCell(pc int) (uintptr, bool) {
	f, ok := b.fb.(ConstantFeedback)
	if !ok {
		return 0, false
	}
	cell, ok := f.StringCell(pc)
	return cell, ok && cell != 0
}

// define is what the literal's field at pc adds (LiteralFeedback).
func (b *builder) define(pc int) (*PropertyAdd, bool) {
	f, ok := b.fb.(LiteralFeedback)
	if !ok || b.cur != b.root {
		return nil, false
	}
	add, ok := f.Define(pc)
	return add, ok && add != nil && add.Define
}

// referenceReads is p with the element reads whose speculation failed
// (Feedback's Generic) carried by their cells, as the reads of elements
// used as more than numbers are. p itself is not changed.
func referenceReads(p *ir.Program, fb Feedback) *ir.Program {
	if fb == nil {
		return p
	}
	var q *ir.Program
	for pc, in := range p.Code {
		if in.Op != ir.ArrayRead && in.Op != ir.ArrayUpdate || in.Reference || !reachable(p, pc) || !fb.Generic(pc) {
			continue
		}
		if q == nil {
			c := *p
			c.Code = append([]ir.Instruction(nil), p.Code...)
			q = &c
		}
		q.Code[pc].Reference = true
	}
	if q == nil {
		return p
	}
	return q
}

// goesOn reports whether the instruction at pc is a call Go makes from
// native code, and nothing else (GoCall): native code goes on after it as
// after any other value, in the same block, with no entry after it.
func (b *builder) goesOn(pc int) bool {
	calls := b.callsAt(pc)
	return len(calls) == 1 && calls[0].Go == GoCall
}

// AppendFeedback is Feedback that lets numbers written at arrays' lengths
// be appended natively (Func.ArrayProtos): the realm's prototypes, with
// their shapes as the code is to find them, or false where none may be --
// a prototype with an index in its table, which a shape does not tell of.
type AppendFeedback interface {
	ArrayProtos() ([2]Holder, bool)
}

// FrequencyFeedback is Feedback that knows how often each of a function's
// calls is made for each invocation of it, as the existing tiers counted
// them: V8's call frequency, a call site's count of calls over its
// function's invocations, which V8's feedback vector keeps from its lower
// tiers. Inlining takes the function's own calls made most often first,
// and none made less often than minInliningFrequency (plan,
// inlineSiteAt). A call inside a callee inlined is inlined however seldom
// it is made, as before the counts: V8 inlines the callee and leaves such
// a call a call, where an inlined frame here makes none (InlineCalls) --
// not inlining the callee for it would make a call of the callee, made
// at least that often, of a cold one inlined.
type FrequencyFeedback interface {
	// Frequency is how often the call at pc is made for each invocation
	// of the frame's function, if the feedback knows.
	Frequency(pc int) (float64, bool)
}

// minInliningFrequency is V8's min_inlining_frequency as V8 runs: a call
// made less often than this for each invocation of the function being
// compiled is not inlined, whatever its callee's size, as Turbofan's
// JSInliningHeuristic asks the frequency before it forces a small
// function in -- Turbofan being the optimizing compiler Node runs; the
// exemption of a small callee is Turbolev's (CanInlineCall). The flag's
// default is 0.15, but V8 lowers it to 0.05 wherever Maglev is on, as it
// is by default (flag-definitions.h's
// DEFINE_WEAK_VALUE_IMPLICATION(maglev, min_inlining_frequency, 0.05)):
// Node prints --min-inlining-frequency=0.05 among its flags' values, and
// its optimizing compiler inlines all twelve of AWFY CD's recurse's
// calls of Vector2D's plus and minus, each made in an eighth of
// recurse's runs.
const minInliningFrequency = 0.05

// MinInliningFrequency is minInliningFrequency, for the VM, which decides
// a call made less often a native call, not one to inline (its
// regCallCold).
const MinInliningFrequency = minInliningFrequency

// siteFrequency is how often the root's call at pc, the frame being
// translated being the root's, is made for each invocation of it, if known
// (FrequencyFeedback).
func (b *builder) siteFrequency(pc int) (float64, bool) {
	ff, ok := b.fb.(FrequencyFeedback)
	if b.cur != b.root || !ok {
		return 0, false
	}
	return ff.Frequency(pc)
}

// HasCacheFeedback is Feedback that has native code look the in
// operator's answers up in the VM's cache of them (Func.HasCache): its
// address, made if there is none yet, or 0 for none.
type HasCacheFeedback interface {
	HasCache() uintptr
}

// KeyAtomsFeedback is Feedback that has native code hand out a for...in's
// keys of an array's indices and read an element by a key string through
// the VM's tables of them (Func's KeyAtoms and IntStrings): their
// addresses, the strings' made if there is none yet, or 0 for none.
type KeyAtomsFeedback interface {
	KeyAtoms() (keyAtoms, intStrings uintptr)
}

// KeepFeedback is Feedback that bounds the keep cells a build takes
// (Func.KeepLimit): abi.MaxKeeps when the backend could not fit the cells
// a build with none took into the context's (mir's ErrKeeps).
type KeepFeedback interface {
	KeepLimit() int
}

// functionCalls reports whether calls are all of functions' code, through
// their links (CallSite's Entry), as opposed to built-ins native code makes
// itself or calls of Go.
func functionCalls(calls []*CallSite) bool {
	for _, t := range calls {
		if t.Entry == 0 || t.Go != 0 {
			return false
		}
	}
	return len(calls) != 0
}

// ResumeFeedback is Feedback that knows where Go has gone on after a call
// native code makes or inlines, by the PC after it, the interpreter's: the
// code is built with an entry there (Func's Unresumed).
type ResumeFeedback interface {
	Resumed(pc int) bool
}

// resumed reports whether Go has gone on at pc, the PC after a call, in the
// function's code (ResumeFeedback).
func (b *builder) resumed(pc int) bool {
	f, ok := b.fb.(ResumeFeedback)
	return ok && f.Resumed(int(b.p.Maps[pc].PC))
}

// generic reports an arithmetic operation or a comparison at pc whose
// speculation failed (Feedback's Generic): what are not numbers go to Go,
// which resumes after it, as they do for == and %.
func (b *builder) generic(pc int) bool {
	switch in := b.p.Code[pc]; {
	case in.Op == ir.Binary, in.Op == ir.Branch && in.Operator != ir.Truth, in.Op == ir.Rethrow, in.Op == ir.ArgumentRead,
		in.Op == ir.Unary && in.Operator != ir.Not, in.Op == ir.Update:
		// Or a register instruction's component after a commit (ir's
		// Generic), which may not leave -- where what came before it in
		// its origin committed as it was built (atomicInstruction): after
		// a property's read built to call nothing, add_prop's addition
		// speculates as an add does, and its exit runs the instruction
		// again from its start, the read with it.
		return in.Generic && b.atomic || b.fb != nil && b.fb.Generic(pc) || b.operandsGeneric(pc)
	}
	return false
}

// frame is a program the builder translates: the function's own, or a
// callee inlined at one of its calls.
type frame struct {
	p  *ir.Program
	fb Feedback
	// readCalls are the frame's property reads of register code built to
	// call -- an accessor, or Go for what a megamorphic site's own table
	// and the megamorphic cache do not have -- which commit
	// (commitsBuilt).
	readCalls []int32
	// base is a callee's first slot among the builder's, past the
	// function's own and earlier callees'; blockAt its blocks, by its PC.
	base    int
	blockAt []*Block
	site    InlineSite
	// call is the state at the call, where every exit inside the callee
	// goes: Go makes the call, or the interpreter does, from the start. The
	// call's result goes to slot result, and code goes on at cont, the
	// block after the call. start sets the callee's slots: its arguments,
	// its receiver and undefined (init, by its slot), when the call has
	// been checked.
	call      *FrameState
	result    int
	cont      *Block
	start     *Block
	init      []*Value
	callBlock *Block // the caller's block that ends at the call
	// parent is the frame of the call, the function's own or a callee's;
	// depth how many inlined frames this is inside, itself included; and
	// inlined the callees inlined in it, by the PC of their call.
	parent  *frame
	depth   int
	inlined map[int]*frame
	// receiver is an inlined construction's object, its result, and keep
	// the cell it is kept in, taken when the inlining was decided.
	receiver *Value
	keep     int
	// args are the arguments the call gave it, which a call it forwards
	// them to is given (InlineSite's Forward), and argSlot the first of
	// them among the caller's slots at the call, where an exit's frame
	// finds them (InlineSite's Arguments).
	args    []*Value
	argSlot int
	// nest is how deep it is inlined, as maxInlineDepth counts: depth, but
	// for forwarding constructors (InlineSite's Restart).
	nest int
	// hosts are host's answers, by PC: 0 for none yet, 1 for no, 2 for yes.
	hosts []uint8
	// asked are accessor's answers, by PC: 0 for none yet, 1 for no, 2 for
	// one of accessors.
	asked     []uint8
	accessors map[int]*AccessorSite
	// answers are property's and stringProperty's answers, once asked, and
	// answerAt, by PC, 1 plus the index of each's: the plan asks of each
	// property site whether it is left to Go (host) and whether it knows
	// a shape (unfed), and the translation asks again, and each answer is
	// a search of the VM's caches -- a fifth of a build had been the same
	// answers found again.
	answers  []siteAnswer
	answerAt []int
	// literals are the literals the frame's code makes natively, by PC,
	// each with the keep cell planning gave it (literal): an inlined
	// callee's too, from its own code's pools.
	literals map[int]literalKeep
	// paramArgs are a parameter list's arguments, the values its slots had
	// at the frame's start, which its prologue binds them from (ir's
	// ParamsDead), and which the states inside it keep past their operands
	// (ir.Program's PrologueArgs).
	paramArgs []*Value
	// path names the frame by the calls it is inlined at, the PC of each
	// in its caller's code, from the root's on (givenPath), and at is the
	// last of them.
	path string
	at   int
	// pending are, in a first build, the calls of what the frame is given
	// (InlineCallsGiven) it leaves to exits, whose states the build's
	// analysis reads (calledClosures). given says the frame is a closure
	// the root makes, inlined at such a call in a second build, made at
	// givenPC0 (givenSite), and callee the value its call calls there,
	// which the build checks is that closure (unmadeAlone).
	pending  []int
	given    bool
	givenPC0 int
	callee   *Value
}

// givenPath is the path of a frame inlined at pc in the code of the frame
// whose path is parent (frame's path).
func givenPath(parent string, pc int) string { return parent + "." + strconv.Itoa(pc) }

// siteAnswer is a property site, if the feedback has one, and a string
// property's (frame's answers).
type siteAnswer struct {
	site, str PropertySite
	ok, strOK bool
	asked     uint8
}

// answer is pc's entry in the frame's answers, made if need be.
func (b *builder) answer(pc int) *siteAnswer {
	fr := b.cur
	if fr.answerAt == nil {
		// Room for an answer at each property site, made once: an entry
		// is 312 bytes, and a list grown by doubling had left each
		// doubling's room behind in the workspace, half again as much.
		fr.answerAt = b.f.ints(len(b.p.Code))
		n := 0
		for _, in := range b.p.Code {
			if in.Op == ir.PropertyRead || in.Op == ir.PropertyWrite || in.Op == ir.ReferenceRead {
				n++
			}
		}
		fr.answers = b.f.makeAnswers(n)
	}
	if k := fr.answerAt[pc]; k != 0 {
		return &fr.answers[k-1]
	}
	fr.answers = b.f.appendAnswer(fr.answers, siteAnswer{})
	fr.answerAt[pc] = len(fr.answers)
	return &fr.answers[len(fr.answers)-1]
}

// The answers a site's entry holds (siteAnswer's asked).
const (
	askedSite uint8 = 1 << iota
	askedString
)

// inlinedAt is the callee inlined at pc in the frame being translated.
func (b *builder) inlinedAt(pc int) *frame {
	if b.cur == b.root {
		return b.inlined[pc]
	}
	return b.cur.inlined[pc]
}

// enter makes fr the frame being translated.
func (b *builder) enter(fr *frame) {
	b.cur, b.p, b.fb = fr, fr.p, fr.fb
}

// frameAt is the frame blk is in.
func (b *builder) frameAt(blk *Block) *frame {
	if blk.ID < len(b.frameOf) && b.frameOf[blk.ID] != nil {
		return b.frameOf[blk.ID]
	}
	return b.root
}

// inlineAt is the callee to inline at pc in the frame being translated, if
// any: a call the VM has seen call one function (Feedback's Inline), whose
// failures have not made it generic, within inlining's bounds; with the
// calls it makes inlined in it, every one, or it is not.
func (b *builder) inlineAt(pc int, total *int) (*frame, bool) {
	return b.inlineSiteAt(pc, total, nil)
}

// inlineSiteAt is inlineAt with the site the feedback gives at pc, if
// known already (site), which planning asks for first to order the root's
// calls by what inlining them saves (plan).
func (b *builder) inlineSiteAt(pc int, total *int, known *inlineAsked) (*frame, bool) {
	accessor, isAccessor := b.accessor(pc)
	// A closure the root makes, called where a callee calls what it is
	// given (givenSite), whatever else the call has called.
	given := known != nil && known.given
	if b.fb == nil || b.p.Code[pc].Op != ir.Host && !isAccessor || b.cur.nest >= maxInlineDepth || !isAccessor && !given && b.fb.Generic(pc) {
		return nil, false
	}
	// Made too seldom to pay for its code, as V8's heuristic finds a call
	// below min_inlining_frequency, whatever the callee's size.
	if freq, counted := b.siteFrequency(pc); counted && freq < minInliningFrequency {
		return nil, false
	}
	var site InlineSite
	ok := false
	switch {
	case isAccessor:
		// A getter or a setter, called with the receiver and, for a setter,
		// the value (AccessorSite).
		if accessor.Inline != nil {
			site, ok = *accessor.Inline, true
		}
	case known != nil:
		site, ok = known.site, known.ok
	default:
		site, ok = b.fb.Inline(pc)
	}
	var calls, givenCalls []int
	if ok {
		calls, givenCalls, ok = b.inlineCalls(site, isAccessor)
	}
	if !ok || !inlineFits(*total, inlineSize(site.Program)) || !b.typedInline(site) ||
		site.Argc < 0 || site.Params < 0 || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) {
		return nil, false
	}
	if site.Window && isAccessor {
		// A register callee that is the accessor a register read or write
		// calls (AccessorSite's Inline): its window the function, from
		// its cell, the receiver and a setter's value (accessorInline);
		// a getter's result the read's register.
		if !site.Method || site.Argc != b2i(accessor.Setter) || site.Construct || site.Restart || site.Forward || site.Arguments ||
			site.ReadsArguments || site.LexThis || site.Captured != nil || site.ThisSlot >= 0 || site.Params < 2 || site.FunctionCell == 0 {
			return nil, false
		}
		result := b.cur.base + b.p.Code[pc].Dest
		if accessor.Setter {
			result = -1
		}
		return b.inlineFrame(site, calls, givenCalls, total, result, pc)
	}
	if in := &b.p.Code[pc]; in.Window || site.Window {
		// Register code's call of a register callee (InlineSite's Window):
		// its operands the call's window's registers, of its shape -- a
		// plain call, a method's or a construction -- its result the
		// call's register.
		if !in.Window || !site.Window || isAccessor || site.Restart || site.ThisSlot >= 0 || site.Params < 2 ||
			site.LexThis && (site.Construct || site.Method || site.Forward || site.LexThisSlot < 0) {
			// A closure the root makes (calledSite), what it captured the
			// root's own slots (Captured), is called plainly, an arrow's
			// this the root's (LexThis). One whose result is to a register
			// a closure captures has its return store it in its cell too
			// (storeCapturedAt).
			return nil, false
		}
		if site.Forward {
			// f.apply(x, arguments) -- apply_args, its window f, apply and
			// x -- in a callee inlined with its call's arguments
			// (Arguments): f called with x and those (inlineWindowCall).
			if in.Extra != 2 || site.Construct || site.Method || b.cur == b.root || !b.cur.site.Arguments || site.Argc != b.cur.site.Argc {
				return nil, false
			}
		} else if site.Argc != in.Extra || site.Method != in.When {
			return nil, false
		}
		if site.Construct && (site.Pool == 0 || site.Method || b.f.Keeps >= b.f.keepLimit() || !returnsWindowReceiver(site.Program)) {
			// A construction's receiver, from its pool, is kept in a cell
			// as a call's result is; and the callee returns nothing or it,
			// so that the receiver is the result.
			return nil, false
		}
		return b.inlineFrame(site, calls, givenCalls, total, b.cur.base+in.Dest, pc)
	}
	depth, after := b.p.Maps[pc].Depth, b.p.Maps[pc+1].Depth
	callee := site.Argc + 1
	if site.Method {
		callee++
	}
	if isAccessor && (site.Construct || site.Restart || site.Forward || !site.Method || site.Argc != b2i(accessor.Setter)) {
		// Its operands are the receiver and the value, the instruction's
		// own, no function; a getter's result is the instruction's.
		return nil, false
	}
	if site.Forward {
		// f, apply and the receiver; the arguments are the frame's own.
		callee = 3
		if b.cur == b.root || !b.cur.site.Restart && !b.cur.site.Arguments || site.Argc != b.cur.site.Argc {
			return nil, false
		}
	}
	if site.LexThis && (isAccessor || site.Construct || site.Forward || site.Coerce || site.ThisSlot < 0) {
		return nil, false
	}
	if !isAccessor && (depth < callee || after != depth-callee+1 || site.ThisSlot >= 0 && !site.Method && !site.Construct && !site.Forward && !site.LexThis && site.ThisCell == 0) {
		return nil, false
	}
	if site.Construct {
		// Its receiver is kept in a cell, as a call's result is; and it
		// returns nothing, so that the receiver is the result.
		if site.Pool == 0 || site.Method || b.f.Keeps >= b.f.keepLimit() {
			return nil, false
		}
		for pc, in := range site.Program.Code {
			if reachable(site.Program, pc) && in.Op == ir.Return && in.Left != ir.Literal(ir.Value{Kind: ir.Undefined}) && !returnsThis(site.Program, pc, site.ThisSlot) {
				return nil, false
			}
		}
	}
	result := b.cur.base + b.p.Locals + after - 1
	if isAccessor {
		// A getter's result is the read's; a setter's is dropped.
		result = b.cur.base + b.p.Code[pc].Dest
		if accessor.Setter {
			result = -1
		}
	}
	return b.inlineFrame(site, calls, givenCalls, total, result, pc)
}

// inlineCalls is what a callee to inline at a site leaves to Go
// (InlineCalls), and, where the build inlines calls of what callees are
// given -- a first one leaving them to exits for its analysis (pending),
// a second one inlining what its analysis found (given) -- those calls
// apart (InlineCallsGiven): not in an accessor, a construction or a
// frame Go makes again or of its arguments, whose callees are given
// nothing a closure could be.
func (b *builder) inlineCalls(site InlineSite, accessor bool) (calls, given []int, ok bool) {
	if !b.provisional && b.given == nil || accessor || site.Construct || site.Restart || site.Forward || site.Arguments || site.Captured != nil {
		calls, ok = b.inlineCallsOf(site.Program)
		return calls, nil, ok
	}
	cf, isCF := site.Feedback.(CalledClosureFeedback)
	if !isCF {
		calls, ok = b.inlineCallsOf(site.Program)
		return calls, nil, ok
	}
	return InlineCallsGiven(site.Program, func(pc int) (int, bool) {
		argc, method, ok := cf.CallShape(pc)
		return argc, ok && !method
	})
}

// inlineAnswer is InlineCalls' answer for p.
type inlineAnswer struct {
	p     *ir.Program
	calls []int
	ok    bool
}

// inlineCallsOf is InlineCalls(p), asked once a build for each program,
// which never changes while it builds: a callee is asked at each of its
// calls, and each call twice (plan, inlineSiteAt), and each answer checked
// the whole program (ir.Program's Validate) -- a sixth of the time a
// register build took to plan its inlining. The callers only read the
// calls.
func (b *builder) inlineCallsOf(p *ir.Program) ([]int, bool) {
	for i := range b.inlineAsked {
		if a := &b.inlineAsked[i]; a.p == p {
			return a.calls, a.ok
		}
	}
	calls, ok := InlineCalls(p)
	b.inlineAsked = append(b.inlineAsked, inlineAnswer{p: p, calls: calls, ok: ok})
	return calls, ok
}

// cellsBack has the cells a native call's frame takes at its entries go
// back to their pool when it returns natively (Func's CellsBack), where
// nothing past the call can hold them: every closure the function makes
// is a loop's callback it inlines and does not make (planCallback's
// made), whose frame Go makes, the closure with it, only at an exit,
// after which the frame is Go's and returns through Go; and no inlined
// callee makes a closure (InlineCalls).
func (b *builder) cellsBack() {
	p := b.root.p
	if !p.Cells || len(b.made) == 0 {
		return
	}
	for pc, in := range p.Code {
		if in.Op == ir.Closure && reachable(p, pc) && b.made[pc] == 0 {
			return
		}
	}
	fb, ok := b.fb.(CellsFeedback)
	if !ok {
		return
	}
	if pool, cells, ok := fb.CellsPool(); ok {
		b.f.CellsBack, b.f.CellsPool, b.f.CellsCount = true, pool, len(cells)
	}
}

// looped reports whether a backward branch of p goes to pc: a loop's
// header.
func looped(p *ir.Program, pc int) bool {
	for q, in := range p.Code {
		if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target == pc && pc <= q {
			return true
		}
	}
	return false
}

// calledSite is the callee to inline at pc, a call of the closure the
// function makes at pc0 (calledClosures): its function, as the VM gives it
// (CalledClosureFeedback), called with the call's arguments and no
// receiver, what it captured read and assigned as the function's own
// slots where they are (InlineSite's Captured), as a loop's callback the
// function makes is (planCallback); no check of the callee, which the
// analysis found is that closure.
func (b *builder) calledSite(pc, pc0 int) (InlineSite, bool) {
	if b.cur != b.root {
		return InlineSite{}, false
	}
	return b.givenSite(pc, pc0)
}

// givenSite is calledSite's callee in the frame being translated, the
// root's or a callee's: the closure the root makes at pc0, called at pc
// with no receiver, what it captured the root's slots where they are --
// in a callee, at a call of what the callee is given, where the closure
// is what the root gave it (calledClosures).
func (b *builder) givenSite(pc, pc0 int) (InlineSite, bool) {
	cf, ok := b.root.fb.(CalledClosureFeedback)
	at, ok2 := b.fb.(CalledClosureFeedback)
	if !ok || !ok2 {
		return InlineSite{}, false
	}
	made, s, ok := cf.CalledClosure(pc0)
	if !ok || s.Program == nil {
		return InlineSite{}, false
	}
	argc, method, ok := at.CallShape(pc)
	if !ok || method {
		return InlineSite{}, false
	}
	p := b.root.p
	s.Callee, s.Closure, s.Upvalues, s.Argc, s.Method = 0, made.Closure, 0, argc, false
	s.Captured = make([]int, len(made.Captures))
	for i, c := range made.Captures {
		k := int(c >> 1)
		switch {
		case c&1 != 0 && k < p.UpvalueBase && p.IsCaptured(k):
			s.Captured[i] = k
		case c&1 == 0 && k < p.Upvalues:
			s.Captured[i] = p.UpvalueBase + k
		default:
			return InlineSite{}, false
		}
	}
	if b.madeCalls == nil {
		b.madeCalls = map[int]*MadeCallback{}
	}
	b.madeCalls[pc0] = &made
	return s, true
}

// unmakeCalled leaves unmade the closures all of whose calls planning
// inlined (calledSite): their slots hold their templates, which only frame
// states name, as a loop's callback the function makes (MadeCallback).
func (b *builder) unmakeCalled() {
	for pc0, made := range b.madeCalls {
		all := true
		for pc, at := range b.called {
			if at == pc0 && b.inlined[pc] == nil {
				all = false
			}
		}
		for path, at := range b.given {
			// And every call of it a callee it is given makes.
			if fr := b.frameAtPath(path); at == pc0 && (fr == nil || !fr.given || fr.givenPC0 != pc0) {
				all = false
			}
		}
		if !all || b.made[pc0] != 0 {
			delete(b.madeCalls, pc0)
			continue
		}
		if b.made == nil {
			b.made = map[int]uintptr{}
		}
		b.made[pc0] = made.Template
		b.f.Called++
	}
}

// frameAtPath is the callee inlined at path (frame's path), if planning
// inlined one there, among the root's inlined calls and theirs.
func (b *builder) frameAtPath(path string) *frame {
	fr, at := b.root, b.inlined
	for _, part := range strings.Split(path, ".")[1:] {
		pc, err := strconv.Atoi(part)
		if err != nil || at == nil || at[pc] == nil {
			return nil
		}
		fr = at[pc]
		at = fr.inlined
	}
	if fr == b.root {
		return nil
	}
	return fr
}

// closureFrames are the callees inlined that are closures the root makes,
// called with no check of the callee (frame's given), in the root's
// inlined calls and theirs.
func (b *builder) closureFrames() []*frame {
	var out []*frame
	var walk func(at map[int]*frame)
	walk = func(at map[int]*frame) {
		for _, pc := range sortedKeys(at) {
			fr := at[pc]
			if fr.given {
				out = append(out, fr)
			}
			walk(fr.inlined)
		}
	}
	walk(b.inlined)
	return out
}

// errUnmadeSeen is a second build's refusal of closures it left unmade
// (calledClosures) that its own SSA shows are seen, or not called, where
// the first build's did not show it: the first build had left a callee's
// calls of what it is given to exits (frame's pending), past which it
// built nothing.
var errUnmadeSeen = errors.New("ssa: a closure not made is seen")

// unmadeAlone checks, in a second build, each closure planning left unmade
// that the root makes and only calls (unmakeCalled): its values -- the
// template its slot holds, and phis merging nothing but those -- are used
// by nothing but frame states and its initialization's check, and every
// call inlined of it with no check of its callee (closureFrames) calls
// one of them. The first build found so, but where it left a callee's
// calls of what it is given to exits, it built nothing past them -- a
// parameter assigned after its call, a closure given on to another call
// in the loop's turn -- which this one does.
func (b *builder) unmadeAlone() error {
	frames := b.closureFrames()
	for pc0 := range b.madeCalls {
		tmpl := b.made[pc0]
		var closes []*frame
		for _, fr := range frames {
			if fr.givenPC0 == pc0 {
				closes = append(closes, fr)
			}
		}
		if tmpl == 0 {
			if len(closes) != 0 {
				// Inlined with no check of its callee, and made: the check
				// is only that its value is the closure, which a made one's
				// is not.
				return errUnmadeSeen
			}
			continue
		}
		// The local it is kept in, the instruction after it: an entry's
		// load of it is the closure too, made by the interpreter, as
		// calledClosures has it, and so is its dead zone's marker; and,
		// in register code, a copy of it an entry after an operation Go
		// ran loads (resumeCopies).
		p := b.root.p
		home := calledHome(p, pc0, b.f.FrameLocals)
		set, also := map[*Value]bool{}, map[*Value]bool{}
		var phis []*Value
		for _, blk := range b.f.Blocks {
			for _, v := range blk.Values {
				switch {
				case v.Op == OpLoadCell && len(v.Args) == 1 && v.Args[0].Op == OpConstCell && v.Args[0].Const.Bits == uint64(tmpl):
					set[v] = true
				case v.Op == OpPhi:
					phis = append(phis, v)
				case home >= 0 && (v.Op == OpLoadSlot && blk.PC < 0 && v.Aux == home || v.Op == OpConst && v.Const.Kind == ir.Uninitialized):
					also[v] = true
				}
			}
		}
		for _, v := range phis {
			set[v] = true
		}
		copies := newResumeCopies(b.f, p)
		for changed := true; changed; {
			changed = false
			for _, v := range phis {
				if !set[v] {
					continue
				}
				for _, a := range v.Args {
					if !set[a] && !also[a] && !copies.holds(a, set) {
						delete(set, v)
						changed = true
						break
					}
				}
			}
		}
		for _, blk := range b.f.Blocks {
			if blk.Control != nil && set[blk.Control] {
				return errUnmadeSeen
			}
			for _, v := range blk.Values {
				if set[v] && v.Op == OpPhi || v.Op == OpCheckInit {
					continue
				}
				for _, a := range v.Args {
					if set[a] {
						return errUnmadeSeen
					}
				}
			}
		}
		for _, fr := range closes {
			if fr.callee == nil || !set[fr.callee] {
				return errUnmadeSeen
			}
		}
	}
	return nil
}

// returnsThis reports whether p's return at pc returns its receiver, in
// slot this, as a class's constructor does (push_this, return): the value
// the instruction before it, which nothing jumps past, copies there. A
// construction's result is its receiver then, as when it returns nothing.
func returnsThis(p *ir.Program, pc, this int) bool {
	in := p.Code[pc]
	if this < 0 || pc == 0 || in.Left.Slot < 0 {
		return false
	}
	if c := p.Code[pc-1]; c.Op != ir.Copy || c.Left.Slot != this || c.Dest != in.Left.Slot {
		return false
	}
	for _, x := range p.Code {
		if (x.Op == ir.Jump || x.Op == ir.Branch || x.Op == ir.IterStep) && x.Target == pc {
			return false
		}
	}
	for _, h := range p.Handlers {
		if int(h.PC) == pc {
			return false
		}
	}
	return true
}

// inlineFrame is the frame of site inlined in the frame being translated,
// its result to slot result, if inlining's bounds allow it, with calls,
// those of its operations it leaves to Go (InlineCalls), inlined in it,
// every one, or it is not.
func (b *builder) inlineFrame(site InlineSite, calls, given []int, total *int, result, at int) (*frame, bool) {
	q := referenceReads(site.Program, site.Feedback)
	// Its slots follow the function's and earlier callees': every slot's
	// number stays below abi.MaxRecords, as records name slots by number.
	if b.nslots+q.Locals+q.StackSize > abi.MaxRecords {
		return nil, false
	}
	nslots, before, keeps := b.nslots, *total, b.f.Keeps
	*total += inlineSize(site.Program)
	fr := b.f.newFrame()
	*fr = frame{p: q, fb: site.Feedback, site: site, result: result, base: b.nslots,
		parent: b.cur, depth: b.cur.depth + 1, nest: b.cur.nest + 1, keep: -1, at: at, givenPC0: -1}
	if b.provisional || b.given != nil {
		// Its path, which only a build inlining calls of what callees are
		// given asks for.
		fr.path = givenPath(b.cur.path, at)
	}
	if site.Construct {
		// Its receiver's keep cell, taken now, as a literal's is
		// (literal): the inlining is decided now.
		fr.keep = b.f.Keeps
		b.f.Keeps++
	}
	if site.Restart || site.Arguments {
		// A forwarding constructor is its call of the method it forwards
		// to: it does not count toward how deep calls are inlined.
		fr.nest = b.cur.nest
	}
	b.nslots += q.Locals + q.StackSize
	inlined := len(b.f.Inlined)
	cur := b.cur
	refuse := func() (*frame, bool) {
		b.enter(cur)
		b.nslots, *total, b.f.Keeps = nslots, before, keeps
		b.f.Inlined = b.f.Inlined[:inlined]
		return nil, false
	}
	if len(calls) != 0 || len(given) != 0 {
		b.enter(fr)
		// fixed says a call in one of its loops calls the closure the root
		// gives it, inlined -- or, in a first build, left to an exit to
		// find whether it does.
		fixed := false
		for _, at := range calls {
			if b.nativeHost(at) || b.inlineExit(at) {
				// Made natively, or left to an exit in the callee's frame,
				// seldom taken (InlineExitFeedback).
				continue
			}
			child, ok := b.inlineAt(at, total)
			if !ok {
				return refuse()
			}
			if fr.inlined == nil {
				fr.inlined = map[int]*frame{}
			}
			fr.inlined[at] = child
		}
		for _, at := range given {
			// A call of what it is given: in a first build an exit, whose
			// state says what the callee is (calledClosures) -- not what
			// the call's feedback inlines, a closure checked by its object,
			// which a closure the caller makes anew at each call fails;
			// in a second, the closure the root makes that the first found
			// there, inlined (givenSite); otherwise, as any call, what its
			// feedback inlines. Else the callee is not inlined at all: its
			// loop would leave native code at every turn.
			if b.provisional {
				fr.pending = append(fr.pending, at)
				b.pendingAny = true
				fixed = fixed || loopHolds(fr.p, at)
				continue
			}
			var child *frame
			if pc0, found := b.given[givenPath(fr.path, at)]; found {
				if s, ok := b.givenSite(at, pc0); ok {
					if child, ok = b.inlineSiteAt(at, total, &inlineAsked{site: s, ok: true, given: true}); ok {
						child.given, child.givenPC0 = true, pc0
						fixed = fixed || loopHolds(fr.p, at)
					}
				}
			} else if b.nativeHost(at) || b.inlineExit(at) {
				continue
			} else {
				child, _ = b.inlineAt(at, total)
			}
			if child == nil {
				return refuse()
			}
			if fr.inlined == nil {
				fr.inlined = map[int]*frame{}
			}
			fr.inlined[at] = child
		}
		if !fixed && looping(fr.p) {
			// A loop whose calls the inlining fixes none of: its call is
			// made once for all its turns (InlineCallsGiven).
			return refuse()
		}
		b.enter(cur)
	}
	if !inlineFits(before, *total-before) {
		// Past the function's budget only a small callee is inlined, its
		// own callees counted in, as V8's candidate is small with the
		// bytecode its code inlines (JSInliningHeuristic's
		// candidate_is_small).
		return refuse()
	}
	b.f.Inlined = append(b.f.Inlined, site.Closure)
	b.inlineAccessors(fr, total)
	return fr, true
}

// inlineAsked is what the feedback gave for a call of the root's to
// inline (Feedback's Inline), asked once while planning orders the calls
// (plan): whether it gave a site.
type inlineAsked struct {
	site InlineSite
	ok   bool
	// given says the site is a closure the root makes, called where a
	// callee calls what it is given (givenSite).
	given bool
	// loops is how many of the root's loops hold the PC (loopNesting);
	// end, at a loop's header, is one past its furthest back edge, and
	// ends how many loops end at the PC.
	loops, end, ends int32
	// freq is how often the call is made for each invocation of the
	// function, where known (siteFrequency).
	freq  float64
	known bool
}

// inlineOrder orders two calls of the root's to inline, asked as x and y
// (plan): by their frequencies, the call made more often first, as V8's
// JSInliningHeuristic orders its candidates (frequency); then the call
// more loops hold first; then a construction first.
func inlineOrder(x, y *inlineAsked) int {
	switch fx, fy := x.frequency(), y.frequency(); {
	case fx != fy:
		return cmp.Compare(fy, fx)
	case x.loops != y.loops:
		return int(y.loops - x.loops)
	}
	return b2i(y.ok && y.site.Construct) - b2i(x.ok && x.site.Construct)
}

// frequency is how often k's call is made for each invocation of the
// function, to order the calls to inline by (inlineOrder): the existing
// tiers' count where known (FrequencyFeedback); otherwise as often as the
// order before the counts took it, so that no call the counts do not know
// is judged colder than it was -- once for each invocation outside the
// function's loops, more often than any count inside one. V8 knows almost
// every call's frequency; a call here may be of a body the call path
// answers without a frame, which nothing counts.
func (k *inlineAsked) frequency() float64 {
	switch {
	case k.known:
		return k.freq
	case k.loops > 0:
		return math.Inf(1)
	}
	return 1
}

// loopNesting has asked, by PC, how many of p's loops hold each PC, and
// returns the most: a loop is a header, which a backward branch or jump
// goes to, and the code from it to the furthest such branch.
func loopNesting(p *ir.Program, asked []inlineAsked) int {
	for pc, in := range p.Code {
		if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc && reachable(p, pc) {
			asked[in.Target].end = max(asked[in.Target].end, int32(pc+1))
		}
	}
	open, most := int32(0), int32(0)
	for pc := range asked {
		k := &asked[pc]
		if k.end != 0 {
			open++
			asked[k.end-1].ends++
		}
		k.loops = open
		most = max(most, open)
		open -= k.ends
	}
	return int(most)
}

// planInline makes an inlined callee's blocks, between the block that ends
// at its call and cont, the block after the call: the start block, then a
// block at every target of a branch and after every branch, return and
// exit. A return goes to cont; an exit, wherever it is, goes to the call's
// state (frame.call), where Go makes the call and native code goes on
// after it.
func (b *builder) planInline(fr *frame, call, cont *Block) {
	defer b.enter(b.root)
	b.enter(fr)
	p := fr.p
	fr.cont = cont
	b.frameKinds(fr)
	leaders := b.f.bools(len(p.Code) + 1)
	leaders[0] = true
	for pc, in := range p.Code {
		if !reachable(p, pc) {
			continue
		}
		switch {
		case in.Op == ir.Jump || in.Op == ir.Branch:
			leaders[in.Target], leaders[pc+1] = true, true
		case in.Op == ir.Return || b.host(pc) || b.unfed(pc):
			leaders[pc+1] = true
		}
	}
	newBlock := func(pc, end int) *Block {
		var blk *Block
		if b.translating {
			// Made while the blocks are filled: a loop's callback
			// (inlineCallback), its IDs past every list's.
			blk = b.blockAfter(call)
			blk.PC = pc
		} else {
			blk = b.f.newBlock(pc)
		}
		for len(b.frameOf) <= blk.ID {
			b.frameOf = append(b.frameOf, nil)
			b.endOf = append(b.endOf, 0)
		}
		b.frameOf[blk.ID], b.endOf[blk.ID] = fr, end
		return blk
	}
	fr.start = newBlock(0, -1)
	fr.start.Kind = BlockPlain
	fr.blockAt = make([]*Block, len(p.Code)+1)
	var starts []int
	for pc := range p.Code {
		if leaders[pc] && reachable(p, pc) {
			starts = append(starts, pc)
		}
	}
	for i, pc := range starts {
		end := len(p.Code) - 1
		if i+1 < len(starts) {
			end = starts[i+1] - 1
		}
		for q := pc; q <= end; q++ {
			if op := p.Code[q].Op; op == ir.Jump || op == ir.Branch || op == ir.Return || b.host(q) || b.unfed(q) {
				end = q
				break
			}
		}
		fr.blockAt[pc] = newBlock(pc, end)
	}
	b.edge(call, fr.start)
	b.edge(fr.start, fr.blockAt[0])
	for _, pc := range starts {
		blk := fr.blockAt[pc]
		end := b.endOf[blk.ID]
		switch last := p.Code[end]; {
		case b.unseen(end):
			// An operator whose operands met nothing, left to Go.
			blk.Kind = BlockExit
		case last.Op == ir.Jump:
			blk.Kind = BlockPlain
			b.edge(blk, fr.blockAt[last.Target])
		case last.Op == ir.Branch:
			blk.Kind = BlockIf
			taken, fall := fr.blockAt[last.Target], fr.blockAt[end+1]
			if last.When {
				b.edge(blk, taken)
				b.edge(blk, fall)
			} else {
				b.edge(blk, fall)
				b.edge(blk, taken)
			}
		case last.Op == ir.Return:
			blk.Kind = BlockPlain
			b.edge(blk, cont)
		case fr.inlined[end] != nil:
			// Into the callee inlined in it, whose returns go to the
			// block after it.
			blk.Kind = BlockPlain
			fr.inlined[end].callBlock = blk
		case b.host(end) || b.unfed(end):
			blk.Kind = BlockExit
		default:
			blk.Kind = BlockPlain
			b.edge(blk, fr.blockAt[end+1])
		}
	}
	for _, pc := range sortedKeys(fr.inlined) {
		b.planInline(fr.inlined[pc], fr.inlined[pc].callBlock, fr.blockAt[pc+1])
	}
}

// returnsNatively reports whether fr's code, inlined, returns on a path
// native code takes: from its start to a return through no exit, nor a
// callee inlined in it that does not return so itself. A loop's callback
// inlined in it (inlineCallback) must: the loop's turn goes on after it.
func (b *builder) returnsNatively(fr *frame) bool {
	cur := b.cur
	b.enter(fr)
	defer b.enter(cur)
	p := fr.p
	seen := b.f.bools(len(p.Code))
	var walk func(pc int) bool
	walk = func(pc int) bool {
		for pc < len(p.Code) && !seen[pc] {
			seen[pc] = true
			switch in := p.Code[pc]; {
			case in.Op == ir.Return:
				return true
			case b.unseen(pc):
				return false
			case fr.inlined[pc] != nil:
				if !b.returnsNatively(fr.inlined[pc]) {
					return false
				}
			case in.Op == ir.Jump:
				pc = in.Target
				continue
			case in.Op == ir.Branch:
				if walk(in.Target) {
					return true
				}
			case b.host(pc) || b.unfed(pc):
				return false
			}
			pc++
		}
		return false
	}
	return walk(0)
}

// inlineCallback makes the call of a loop's callback (forEachCall), of its
// operands ops -- this, the callback, its arguments -- in the loop's body,
// the callee fr inlined there, as V8's JSCallReducer inlines a forEach's
// callback into the loop it makes: the callee's frame given its arguments
// and its receiver, every exit inside it going to st, the loop's state at
// the call, past which the exit makes the callee's frame (InlineState) --
// Go finishes the callback, then the loop from its index
// (abi.Context's Loop). It returns the callback's result and the block the
// loop's body goes on in after it.
func (b *builder) inlineCallback(body *Block, fr *frame, ops []*Value, argSlot int, st *FrameState) (*Value, *Block) {
	f, site := b.f, fr.site
	fr.call = st
	undefined := f.newValue(body, OpConst, Tagged)
	undefined.Const = ir.Value{Kind: ir.Undefined}
	fr.init = f.refsOf(fr.p.Locals + fr.p.StackSize)
	for s := range fr.init {
		fr.init[s] = undefined
	}
	fr.args, fr.argSlot = ops[2:], argSlot
	if site.Window {
		// A register callee's frame is its window: the function in its
		// register 0, the receiver in 1, the arguments from 2, as
		// inlineWindowCall lays a call's out.
		fr.init[0], fr.init[1] = ops[1], ops[0]
		for i, a := range fr.args {
			if 2+i < site.Params && 2+i < len(fr.init) {
				fr.init[2+i] = a
			}
		}
	} else {
		for i := 0; i < site.Params && i < len(fr.args); i++ {
			fr.init[i] = fr.args[i]
		}
	}
	if site.ThisSlot >= 0 && site.LexThis {
		// An arrow the root makes: the root's own this, not the loop's.
		fr.init[site.ThisSlot] = b.read(b.root.base+site.LexThisSlot, body)
	} else if site.ThisSlot >= 0 {
		fr.init[site.ThisSlot] = ops[0]
	}
	// The body ends at the call; the rest of it goes on in after, which
	// the callee's returns go to.
	after := b.blockAfter(body)
	b.placeAfter(body, []*Block{after}, after)
	body.Kind, body.Succs, body.Control = BlockPlain, nil, nil
	b.inlineBlocks(fr, body, after)
	return b.read(fr.result, after), after
}

// inlineBlocks makes and fills the blocks of fr, a callee inlined at a
// call made while the blocks are being filled -- a loop's callback
// (inlineCallback) -- between from, the block that ends at the call, which
// is filled, and to, the block after it, which the callee's returns go
// to: planned as any inlined callee's (planInline), then put after from in
// reverse post-order from the callee's start, which is the order blocks
// are filled and kept in, those it does not reach -- an exit's -- dropped
// as translate drops them, and filled so.
func (b *builder) inlineBlocks(fr *frame, from, to *Block) {
	f := b.f
	first := len(f.Blocks)
	b.planInline(fr, from, to)
	made := slices.Clone(f.Blocks[first:])
	f.Blocks = f.Blocks[:first]
	in := make(map[*Block]bool, len(made))
	for _, blk := range made {
		in[blk] = true
	}
	seen := make(map[*Block]bool, len(made))
	var order []*Block
	var visit func(*Block)
	visit = func(blk *Block) {
		if !in[blk] || seen[blk] {
			return
		}
		seen[blk] = true
		for i := len(blk.Succs) - 1; i >= 0; i-- {
			visit(blk.Succs[i])
		}
		order = append(order, blk)
	}
	visit(fr.start)
	slices.Reverse(order)
	for _, blk := range append(made, to) {
		preds := blk.Preds[:0]
		for _, p := range blk.Preds {
			if !in[p] || seen[p] {
				preds = append(preds, p)
			}
		}
		blk.Preds, blk.Backedge = preds, f.bools(len(preds))
		if blk != to {
			b.markBackedges(blk)
		}
	}
	at := slices.Index(f.Blocks, from) + 1
	f.Blocks = slices.Insert(f.Blocks, at, order...)
	// What the root's block being filled had under way, which filling the
	// callee's would take for its own.
	next, here, herePC, region, words := b.next, b.here, b.herePC, b.region, b.words
	b.next = nil
	b.filled[from.ID] = true
	for _, blk := range order {
		b.trySeal(blk)
		mark := len(b.splits)
		b.enter(b.frameAt(blk))
		b.fill(blk)
		b.enter(b.root)
		b.filled[blk.ID] = true
		for _, s := range blk.Succs {
			b.trySeal(s)
		}
		for _, s := range b.splits[mark:] {
			b.filled[s.ID] = true
			for _, t := range s.Succs {
				b.trySeal(t)
			}
		}
	}
	if !b.sealed[to.ID] {
		b.seal(to)
	}
	b.next, b.here, b.herePC, b.region, b.words = next, here, herePC, region, words
}

// inlineCall checks the call at pc calls the function inlined there, and
// gives the callee's start its slots: its arguments, past which undefined,
// its receiver, and undefined in every other.
func (b *builder) inlineCall(blk *Block, pc int, fr *frame, gd *guarder, state func() *FrameState) {
	site := fr.site
	if site.Window {
		b.inlineWindowCall(blk, pc, fr, gd, state)
		return
	}
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	if site.Forward {
		b.forwardCall(blk, pc, sp, fr, gd)
		return
	}
	// Another callee is called by Go, from the call's state, as a call that
	// is not inlined is, and native code goes on after it -- but for a
	// closure the function makes, which the call calls (calledSite), no
	// object until an exit makes it.
	if site.Callee != 0 {
		object := gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-site.Argc-1, blk))
		same := gd.guard(OpSameObject, None, ir.HostExit, object)
		same.Const = ir.Value{Bits: uint64(site.Callee)}
	} else if fr.given {
		// What the call calls, which the build checks is the closure it
		// was found to (unmadeAlone).
		fr.callee = b.read(sp-site.Argc-1, blk)
	}
	if site.Coerce && site.ThisSlot >= 0 {
		// A receiver that is not an object is coerced, which Go does.
		gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-site.Argc-2, blk))
	}
	// Room past the operands for the callee's frame, which an exit inside
	// it writes there, and a context for Go to make it from, and one for
	// each inlined caller it is in.
	room := gd.guard(OpFrameRoom, None, ir.HostExit)
	room.Index = fr.base - b.root.p.Locals + fr.p.Locals + fr.p.StackSize
	room.Const = ir.Value{Bits: uint64(fr.depth)}
	fr.call = state()
	if site.Construct {
		// The receiver, from the pool, as a construction's, kept in a cell:
		// the call's operands from the function on, nothing run.
		args := make([]*Value, site.Argc+1)
		for i := range args {
			args[i] = b.read(sp-site.Argc-1+i, blk)
		}
		call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
		b.f.cache(call).Calls = []*CallSite{{Callee: site.Callee, Argc: site.Argc, ThisSlot: -1, Pool: site.Pool,
			ProtoIndex: site.ProtoIndex, ProtoKey: site.ProtoKey, Receiver: true}}
		call.Index = fr.keep
		cell := b.f.newValue(blk, OpCallCell, Source, call)
		fr.receiver = b.f.newValue(blk, OpKept, Tagged, cell, call)
		fr.receiver.Shadow = cell
	}
	undefined := b.f.newValue(blk, OpConst, Tagged)
	undefined.Const = ir.Value{Kind: ir.Undefined}
	fr.init = b.f.refsOf(fr.p.Locals + fr.p.StackSize)
	for s := range fr.init {
		fr.init[s] = undefined
	}
	fr.args, fr.argSlot = make([]*Value, site.Argc), sp-site.Argc
	for i := range fr.args {
		fr.args[i] = b.read(sp-site.Argc+i, blk)
	}
	for i := 0; i < site.Params && i < site.Argc; i++ {
		fr.init[i] = fr.args[i]
	}
	switch {
	case site.ThisSlot >= 0 && site.Construct:
		fr.init[site.ThisSlot] = fr.receiver
	case site.ThisSlot >= 0 && site.LexThis:
		// An arrow the root makes: the root's own this, not a receiver.
		fr.init[site.ThisSlot] = b.read(b.root.base+site.LexThisSlot, blk)
	case site.ThisSlot >= 0 && site.ThisCell != 0 && !site.Method:
		// A plain call's: the global object or undefined (ThisCell).
		cell := b.f.newValue(blk, OpConstCell, Source)
		cell.Const = ir.Value{Bits: uint64(site.ThisCell)}
		this := b.f.newValue(blk, OpLoadCell, Tagged, cell)
		this.Shadow = cell
		fr.init[site.ThisSlot] = this
	case site.ThisSlot >= 0:
		fr.init[site.ThisSlot] = b.read(sp-site.Argc-2, blk)
	}
}

// returnsWindowReceiver reports whether every return p, a register
// callee's program, may make returns undefined or its receiver, its window's
// register 1: a construction's result is then its receiver.
func returnsWindowReceiver(p *ir.Program) bool {
	for pc, in := range p.Code {
		if reachable(p, pc) && in.Op == ir.Return && in.Left != ir.Literal(ir.Value{Kind: ir.Undefined}) && in.Left != ir.Slot(1) {
			return false
		}
	}
	return true
}

// inlineWindowCall is inlineCall of register code's call at pc of a
// register callee (InlineSite's Window): the function, the call's window's
// register 0, checked to be the callee, as a native call's is; a method
// call's receiver, register 1, checked to be an object where the callee
// would coerce it; room past the frame for the callee's, which an exit
// inside it writes there; a construction's receiver from its pool, kept in
// a cell; and the callee's start given its window -- the function, the
// receiver, the arguments, past which undefined.
func (b *builder) inlineWindowCall(blk *Block, pc int, fr *frame, gd *guarder, state func() *FrameState) {
	site, in := fr.site, b.p.Code[pc]
	a := b.cur.base + in.Left.Slot
	fn := b.read(a, blk)
	if site.Forward {
		b.windowForwardCall(blk, a, fn, fr, gd, state)
		return
	}
	if site.Callee != 0 {
		object := gd.guard(OpObjectOf, Ptr, ir.HostExit, fn)
		same := gd.guard(OpSameObject, None, ir.HostExit, object)
		same.Const = ir.Value{Bits: uint64(site.Callee)}
	} else if fr.given {
		// What the call calls, which the build checks is the closure it
		// was found to (unmadeAlone); a closure the root makes and calls
		// (calledSite) is that closure, which no check needs.
		fr.callee = fn
	}
	var this *Value
	if site.Method {
		this = b.read(a+1, blk)
		if site.Coerce {
			// A receiver that is not an object is coerced, which Go does.
			gd.guard(OpObjectOf, Ptr, ir.HostExit, this)
		}
	}
	room := gd.guard(OpFrameRoom, None, ir.HostExit)
	room.Index = fr.base - b.root.p.Locals + fr.p.Locals + fr.p.StackSize
	room.Const = ir.Value{Bits: uint64(fr.depth)}
	fr.call = state()
	if site.Construct {
		// The receiver, from the pool, as a construction's, kept in a cell:
		// the call's function and arguments its operands, nothing run.
		args := make([]*Value, site.Argc+1)
		args[0] = fn
		for i := range site.Argc {
			args[1+i] = b.read(a+2+i, blk)
		}
		call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
		b.f.cache(call).Calls = []*CallSite{{Callee: site.Callee, Argc: site.Argc, ThisSlot: -1, Pool: site.Pool,
			ProtoIndex: site.ProtoIndex, ProtoKey: site.ProtoKey, Receiver: true}}
		call.Index = fr.keep
		cell := b.f.newValue(blk, OpCallCell, Source, call)
		fr.receiver = b.f.newValue(blk, OpKept, Tagged, cell, call)
		fr.receiver.Shadow = cell
		this = fr.receiver
	}
	undefined := b.f.newValue(blk, OpConst, Tagged)
	undefined.Const = ir.Value{Kind: ir.Undefined}
	fr.init = b.f.refsOf(fr.p.Locals + fr.p.StackSize)
	for s := range fr.init {
		fr.init[s] = undefined
	}
	fr.init[0] = fn
	switch {
	case this != nil:
		fr.init[1] = this
	case site.LexThis:
		// An arrow the root makes: the root's own this, not a receiver.
		fr.init[1] = b.read(b.root.base+site.LexThisSlot, blk)
	case site.ThisCell != 0:
		// A plain call's: the global object or undefined (ThisCell).
		cell := b.f.newValue(blk, OpConstCell, Source)
		cell.Const = ir.Value{Bits: uint64(site.ThisCell)}
		v := b.f.newValue(blk, OpLoadCell, Tagged, cell)
		v.Shadow = cell
		fr.init[1] = v
	}
	fr.args, fr.argSlot = make([]*Value, site.Argc), a+2
	for i := range fr.args {
		fr.args[i] = b.read(a+2+i, blk)
		if 2+i < site.Params && 2+i < len(fr.init) {
			fr.init[2+i] = fr.args[i]
		}
	}
}

// windowForwardCall is inlineWindowCall of apply_args, f.apply(x,
// arguments), in a register callee inlined with its call's arguments
// (InlineSite's Forward and Arguments): f, its window's register 0 at a,
// checked to be the callee, and apply, register 1, the realm's own; x,
// register 2, the callee's
// receiver, checked to be an object where the callee would coerce it;
// and the callee's window its function, x and the arguments the frame
// forwarding them was given, as V8 eliminates the arguments object.
func (b *builder) windowForwardCall(blk *Block, a int, fn *Value, fr *frame, gd *guarder, state func() *FrameState) {
	site := fr.site
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, fn)
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(site.Callee)}
	apply := gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(a+1, blk))
	same = gd.guard(OpSameObject, None, ir.HostExit, apply)
	same.Const = ir.Value{Bits: uint64(site.Apply)}
	this := b.read(a+2, blk)
	if site.Coerce {
		// A receiver that is not an object is coerced, which Go does.
		gd.guard(OpObjectOf, Ptr, ir.HostExit, this)
	}
	room := gd.guard(OpFrameRoom, None, ir.HostExit)
	room.Index = fr.base - b.root.p.Locals + fr.p.Locals + fr.p.StackSize
	room.Const = ir.Value{Bits: uint64(fr.depth)}
	fr.call = state()
	undefined := b.f.newValue(blk, OpConst, Tagged)
	undefined.Const = ir.Value{Kind: ir.Undefined}
	fr.init = b.f.refsOf(fr.p.Locals + fr.p.StackSize)
	for s := range fr.init {
		fr.init[s] = undefined
	}
	fr.init[0], fr.init[1] = fn, this
	fr.args, fr.argSlot = b.cur.args, b.cur.argSlot
	for i, v := range fr.args {
		if 2+i < site.Params && 2+i < len(fr.init) {
			fr.init[2+i] = v
		}
	}
}

// maxArgumentSelect is the most arguments of an inlined call that
// argumentSelect picks among by a chain of compares of the key: past it a
// read of a key no constant leaves native code, as one past the count
// does.
const maxArgumentSelect = 8

// argumentSelect makes arguments[key] of an inlined call (InlineSite's
// ReadsArguments), in, a key not a constant: its argument of that index,
// the key checked to be a number, an integer below the call's count of
// them, as V8's code reads the elements of an inlined frame's arguments
// object, which escape analysis replaced by the call's values -- anything
// else leaves native code, as a constant past the count does, Go reading
// it in the callee's frame. The argument is
// picked by compares of the key with each index but the last's, each
// assigning its argument on a block of its own, the last's the value
// otherwise: EarleyBoyer's sc_list reads its arguments so, `var a =
// arguments; ... a[i]`, which had left native code in each call inlined.
// What aliases a parameter is never read so: a sloppy callee's arguments
// object of parameters is not inlined as one that reads it.
func (b *builder) argumentSelect(blk *Block, in ir.Instruction, key *Value, gd *guarder) {
	f := b.f
	args := b.cur.args
	n := len(args)
	constF := func(x float64) *Value {
		c := f.newValue(blk, OpConstF64, Float64)
		c.Const = ir.Float(x)
		return c
	}
	k := gd.guard(OpUnboxF64, Float64, ir.GuardExit, key)
	check := func(c *Value) { gd.guard(OpCheckTrue, None, ir.GuardExit, f.newValue(blk, OpBoxBool, Tagged, c)) }
	whole := f.newValue(blk, OpCmpF64, Bool, k, f.newValue(blk, OpTruncF64, Float64, k))
	whole.Aux = int(ir.Eq)
	check(whole)
	lo := f.newValue(blk, OpCmpF64, Bool, k, constF(0))
	lo.Aux = int(ir.Ge)
	check(lo)
	hi := f.newValue(blk, OpCmpF64, Bool, k, constF(float64(n)))
	hi.Aux = int(ir.Lt)
	check(hi)
	b.assign(in.Dest, blk, args[n-1])
	for j := range n - 1 {
		is := f.newValue(blk, OpCmpF64, Bool, k, constF(float64(j)))
		is.Aux = int(ir.Eq)
		yes, cont := b.splitOn(blk, is)
		b.assign(in.Dest, yes, args[j])
		blk = cont
	}
}

// constIndex is v as an array index, if it is a constant one: a number that
// is a non-negative integer, as a key that names an element.
func constIndex(v *Value) (int, bool) {
	var x float64
	switch {
	case v.Op == OpConstF64:
		x = math.Float64frombits(v.Const.Bits)
	case v.Op == OpConst && v.Const.Kind == ir.Number:
		x = math.Float64frombits(v.Const.Bits)
	case v.Op == OpBoxF64 && len(v.Args) == 1 && v.Args[0].Op == OpConstF64:
		x = math.Float64frombits(v.Args[0].Const.Bits)
	default:
		return 0, false
	}
	if x < 0 || x >= 1<<31 || x != math.Trunc(x) {
		return 0, false
	}
	return int(x), true
}

// GoFeedback is Feedback whose VM answers calls of Go from native code
// (CallSite's Go): a builder given one makes them.
type GoFeedback interface {
	GoCalls() bool
}

// WalkedCloseFeedback is Feedback whose VM says where native code finds
// the validity cell (abi's chain cells) that says, while it is valid, that
// closing an array walked in place runs nothing -- nothing up the array
// iterator's chain has a return method: the address of a word holding the
// cell's, or nil, the cell's first byte its valid flag. OpIterClosed
// closes such an array where it is valid, as V8's load of a property
// nothing has is kept by its chain's validity cell, and leaves any other
// for Go.
type WalkedCloseFeedback interface {
	WalkedCloseCell() uintptr
}

// goCalls reports whether the code may call Go (GoFeedback).
func (b *builder) goCalls() bool {
	f, ok := b.fb.(GoFeedback)
	return ok && f.GoCalls()
}

// goAdd makes in, an addition, by a call of Go from native code (GoAdd),
// as V8's code calls its StringAdd builtin: what Go refuses leaves for Go to
// make after an exit.
func (b *builder) goAdd(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) {
	release, old := b.bigRelease(blk, b.herePC, in)
	args := []*Value{operand(in.Left), operand(in.Right)}
	if old == nil && b.emptyAdd(blk, in, args) {
		return
	}
	if old != nil {
		args = append(args, old)
	}
	call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
	if b.f.Keeps < b.f.keepLimit() {
		fb := b.bigFeedback()
		big := fb != nil && b.cur == b.root && fb.BigInt(b.herePC)
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoAdd, ThisSlot: -1, Release: release, Big: big, Owned: in.Owned}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
}

// emptyAdd makes in, "" + x or x + "", an addition Go makes (GoAdd) one
// operand of which, args', is the empty string's constant, and reports
// whether it did: x itself where it is a string, told by its word, as
// V8's StringAdd of the empty string is the other operand, and Go's
// addition on a branch of its own where it is anything else -- a number
// it converts, a value it refuses -- native code going on after either.
// "" + x is how scripts convert to a string, and had been a call of Go
// for every string: an HTML escaper's of its input, the string it
// mostly is, 11% of its time.
func (b *builder) emptyAdd(blk *Block, in ir.Instruction, args []*Value) bool {
	var x *Value
	switch {
	case emptyStringConst(args[0]):
		x = args[1]
	case emptyStringConst(args[1]):
		x = args[0]
	default:
		return false
	}
	f := b.f
	if f.Keeps >= f.keepLimit() {
		return false
	}
	st := b.state(blk, b.herePC)
	is := f.newValue(blk, OpTypeIs, Bool, x)
	is.Index, is.Aux, is.State = int(ir.TypeString), int(ir.HostExit), st
	st.addUse()
	yes, other, _ := b.split(blk, is, true)
	b.assign(in.Dest, yes, x)
	call := f.newValue(other, OpCall, Tagged, args...)
	f.cache(call).Calls = []*CallSite{{Go: GoAdd, ThisSlot: -1}}
	call.Aux, call.State, call.Index = int(ir.HostExit), st, f.Keeps
	f.Keeps++
	st.addUse()
	cell := f.newValue(other, OpCallCell, Source, call)
	r := f.newValue(other, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, other, r)
	return true
}

// emptyStringConst reports whether v is the empty string's constant: a
// load of a string constant's cell whose string is known to be empty
// (ConstString).
func emptyStringConst(v *Value) bool {
	if v == nil || v.Op != OpLoadCell || len(v.Args) != 1 {
		return false
	}
	s, ok := v.Args[0].ConstString()
	return ok && s == ""
}

// goCompare is in, a relational comparison whose guard has failed, by a
// call of Go from native code (GoCompare): its result, a boolean.
func (b *builder) goCompare(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) *Value {
	call := gd.guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
	if b.f.Keeps < b.f.keepLimit() {
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoCompare, Argc: int(in.Operator), ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	return r
}

// goArithAt reports whether in, the operator at pc of the frame being
// built -- the root's, or a callee's inlined, whose own feedback the
// builder asks -- met BigInts (BigIntFeedback) and is generic, or has no
// native form for BigInts held in their values (bigNative): Go makes it
// from native code (GoArith), which gives back no block of an inlined
// callee's (bigRelease). In a pure body, whose sites the interpreter never
// marks, an operand native code has unboxed already, a BigInt typed
// array's element, is a BigInt whatever the site met, as for bigNative:
// map's (x) => x * 2n over a BigInt64Array, inlined in map's loop, had
// left native code at its first product past the int64 range at every
// call once its speculation failed, as V8's code calls its BigInt
// multiply once its feedback is no BigInt64's. A generic addition is
// GoAdd's, a comparison GoCompare's.
func (b *builder) goArithAt(pc int, in ir.Instruction, operand func(ir.Operand) *Value) bool {
	switch op := in.Operator; op {
	case ir.Sub, ir.Mul:
		if !b.generic(pc) {
			return false
		}
	case ir.Div, ir.Mod, ir.Pow, ir.BitAnd, ir.BitOr, ir.BitXor, ir.Shl, ir.Shr:
	default:
		return false
	}
	fb, ok := b.fb.(BigIntFeedback)
	if !ok || !b.goCalls() {
		return false
	}
	return fb.BigInt(pc) || b.pureBody() && in.Left.Slot >= 0 && in.Right.Slot >= 0 &&
		(boxedBig(operand(in.Left)) != nil || boxedBig(operand(in.Right)) != nil)
}

// goArith makes in, at a site goArithAt names, by a call of Go from native
// code (GoArith), as goCompare makes a comparison.
func (b *builder) goArith(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) *Value {
	release, old := b.bigRelease(blk, b.herePC, in)
	args := []*Value{operand(in.Left), operand(in.Right)}
	if old != nil {
		args = append(args, old)
	}
	call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
	if b.f.Keeps < b.f.keepLimit() {
		big := in.Operator == ir.Add || in.Operator == ir.Sub || in.Operator == ir.Mul || in.Operator == ir.Div || in.Operator == ir.Mod
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoArith, Argc: int(in.Operator), ThisSlot: -1, Release: release, Big: big}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	return r
}

// goHas makes in, the in operator, by a call of Go from native code
// (GoHas), as goIndex makes an element's read: its result, a boolean.
func (b *builder) goHas(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) {
	call := gd.guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
	if b.f.Keeps < b.f.keepLimit() {
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoHas, ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
}

// capturesLocals reports whether a closure's Captures name a captured local
// of its maker's, a cell of the code's own (index<<1|1).
func capturesLocals(caps []int32) bool {
	for _, x := range caps {
		if x&1 != 0 {
			return true
		}
	}
	return false
}

// goDelete makes in, the delete operator, by a call of Go from native code
// (GoDelete, or GoDeleteStrict in strict code), as goHas makes in: its
// result, a boolean.
func (b *builder) goDelete(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) {
	call := gd.guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
	if b.f.Keeps < b.f.keepLimit() {
		op := GoDelete
		if in.Strict {
			op = GoDeleteStrict
		}
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: op, ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
}

// relational reports whether op is <, <=, > or >=.
func relational(op ir.Operator) bool {
	return op == ir.Lt || op == ir.Le || op == ir.Gt || op == ir.Ge
}

// goIndex makes in, an element read as it is, by a call of Go from native
// code (GoIndex), as goAdd makes an addition.
func (b *builder) goIndex(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) {
	call := gd.guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
	if b.f.Keeps < b.f.keepLimit() {
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoIndex, ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
}

// MapStepFeedback is Feedback that knows the realm's Map and Set
// iterators' next functions, whose cursors a loop's step walks natively
// (CallSite's MapNexts).
type MapStepFeedback interface {
	MapNexts() [2]uintptr
}

// GenStep is a generator's function a loop's step has met (CallSite's
// Gens): the address of its bytecode, which the step checks the
// generator's closure runs, and of the environment and realm the closure
// must have, the caller's; and the addresses of the cell its code's
// generator entry is in, 0 while it has none, and of the count of its
// native calls, its link's, which the VM keeps alive.
type GenStep struct {
	Fn, Env, Realm uintptr
	Entry, Count   uintptr
}

// GenStepFeedback is Feedback that knows the generators a loop's step at
// pc has met (CallSite's Gens), and the realm's %GeneratorPrototype%.next,
// whose cursors native code resumes by a native call.
type GenStepFeedback interface {
	GenSteps(pc int) []GenStep
	GenNext() uintptr
}

// genSteps are the generators the step at pc resumes natively
// (GenStepFeedback), and the realm's generators' next; none for none.
func (b *builder) genSteps(pc int) ([]GenStep, uintptr) {
	f, ok := b.fb.(GenStepFeedback)
	if !ok || b.cur != b.root {
		return nil, 0
	}
	gens := f.GenSteps(pc)
	if len(gens) == 0 {
		return nil, 0
	}
	return gens, f.GenNext()
}

// mapNexts are the realm's Map and Set iterators' next functions
// (MapStepFeedback), or none.
func (b *builder) mapNexts() [2]uintptr {
	if f, ok := b.fb.(MapStepFeedback); ok {
		return f.MapNexts()
	}
	return [2]uintptr{}
}

// goKeyedWrite makes in, an element's write, by a call of Go from native
// code (GoKeyedWrite), as V8's code calls its generic keyed store: Go
// stores what storing runs nothing for, and refuses the rest, which leaves
// for Go to store after an exit, native code going on after it. Each such
// write had left native code: microbench's prop_create, obj[i] = i of an
// object's own index properties, at every store.
func (b *builder) goKeyedWrite(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) {
	call := gd.guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right), operand(in.Third))
	if b.f.Keeps < b.f.keepLimit() {
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoKeyedWrite, ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	}
}

// iterStep makes in, a loop's step, by a call of Go from native code
// (GoCall, Step) -- an exit and an entry at every turn had cost a small
// loop's body four times over -- and ends blk with the branch on its answer: the uninitialized value,
// which no step yields, for a cursor done, any other its next value. What
// Go refuses leaves for Go to make after an exit.
func (b *builder) iterStep(blk *Block, pc int, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) {
	// Where the loop goes on, read before a guard splits blk, which gives
	// its successors to the block it goes on in.
	var on *Block
	if b.p.Origin != nil && !in.When && len(blk.Succs) == 2 {
		on = blk.Succs[1]
	}
	if b.userStep(blk, pc, in, on, operand) {
		return
	}
	call := gd.guard(OpCall, Tagged, ir.HostExit, operand(in.Left))
	if b.f.Keeps < b.f.keepLimit() && in.Rest {
		// A rest element's: no step natively, which takes one value.
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoCall, Rest: true, ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	} else if b.f.Keeps < b.f.keepLimit() {
		gens, next := b.genSteps(pc)
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoCall, Step: true, Fill: in.When, ThisSlot: -1, MapNexts: b.mapNexts(),
			Gens: gens, GenNext: next}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	if on != nil {
		// Register code's: its register written where the loop goes on
		// alone, the block after the step (Succs[1]), which the step
		// alone reaches (hostOf); done, it keeps what it held.
		if b.stepOn == nil {
			b.stepOn = map[*Block]stepDef{}
		}
		b.stepOn[on] = stepDef{in.Dest, r}
	} else {
		b.assign(in.Dest, blk, r)
	}
	// The step runs the iterator's next, which may assign a captured
	// binding, as a native call's callee may.
	if b.hasCells() {
		b.reloadAfterCall(blk)
	}
	if in.When {
		// A destructuring's: its value, undefined once done.
		return
	}
	done := b.f.newValue(blk, OpConst, Tagged)
	done.Const = ir.Value{Kind: ir.Uninitialized}
	// Never left: the constant is no string (OpEqTagged).
	is := gd.guard(OpEqTagged, Bool, ir.HostExit, r, done)
	is.Index = 1
	blk.Control = is
	is.Uses++
}

// stepCalls are the native calls of register code's loop's step at pc,
// in, of its iterator's next (StepCallFeedback), and the site of its
// answer's call of Go (CallSite's StepResult), or none.
func (b *builder) stepCalls(pc int, in ir.Instruction) (calls []*CallSite, result *CallSite) {
	if b.fb == nil || b.p.Origin == nil || in.When || in.Rest || b.cur != b.root || !b.goCalls() || b.f.Keeps+3 > b.f.keepLimit() {
		return nil, nil
	}
	sites := b.fb.NativeCalls(pc)
	for i := range sites {
		switch site := &sites[i]; {
		case site.Go == GoCall && site.StepResult:
			result = site
		case site.Go == 0 && site.Method && site.Argc == 0 && site.Window != 0 && site.Pool == 0 && !site.Super && !site.Forward && site.Via == 0 && len(calls) == 0:
			calls = append(calls, site)
		}
	}
	if len(calls) == 0 || result == nil {
		return nil, nil
	}
	return calls, result
}

// userStep makes in, register code's loop's step at pc, whose cursor steps
// an iterator of a next its feedback knows (stepCalls) -- an iterator
// written in script, such as a class's [Symbol.iterator] returns -- by a
// native call of the next, as any native call of a function is made,
// through its link, its code's where it has some, Go's otherwise; and its
// answer by the iterator protocol, natively for a result of the shape the
// feedback knows (CallSite's StepResult), by Go otherwise. A cursor of
// anything else -- an array, a Map, a generator, an iterator of another
// next, one done -- Go steps, on a branch of its own (split), as iterStep
// has it: OpCursorCell's cell is none there. Each step of an iterator
// written in script had been a call of Go, which called the next in the
// interpreter. It reports whether it made the step; on, where the loop
// goes on, is given the step's value (stepOn), and blk's end the branch
// on it as iterStep's.
func (b *builder) userStep(blk *Block, pc int, in ir.Instruction, on *Block, operand func(ir.Operand) *Value) bool {
	calls, result := b.stepCalls(pc, in)
	if calls == nil || on == nil {
		return false
	}
	f := b.f
	st := b.state(blk, pc)
	cursor := operand(in.Left)
	iterCell := f.newValue(blk, OpCursorCell, Source, cursor)
	iterCell.Const = ir.Value{Bits: uint64(calls[0].Callee)}
	none := f.newValue(blk, OpCellNone, Bool, iterCell)
	gen, nat, cont := b.split(blk, none, true)
	keep := func() int {
		k := f.Keeps
		f.Keeps++
		return k
	}
	made := func(at *Block, args []*Value, sites []*CallSite) (*Value, *Value) {
		call := f.newValue(at, OpCall, Tagged, args...)
		f.cache(call).Calls = sites
		call.Aux, call.State, call.Index = int(ir.HostExit), st, keep()
		st.addUse()
		cell := f.newValue(at, OpCallCell, Source, call)
		r := f.newValue(at, OpKept, Tagged, cell, call)
		r.Shadow = cell
		return call, r
	}
	// Go's step, as iterStep makes it.
	gens, next := b.genSteps(pc)
	_, r0 := made(gen, []*Value{cursor}, []*CallSite{{Go: GoCall, Step: true, ThisSlot: -1, MapNexts: b.mapNexts(), Gens: gens, GenNext: next}})
	// The next called natively, on the iterator, of no argument: its
	// window made of the two, its result its answer, whose protocol the
	// call of Go after it makes but where native code does.
	iter := f.newValue(nat, OpLoadCell, Tagged, iterCell)
	iter.Shadow = iterCell
	nextCell := f.newValue(nat, OpCursorCell, Source, cursor)
	nextCell.Index = 1
	fn := f.newValue(nat, OpLoadCell, Tagged, nextCell)
	fn.Shadow = nextCell
	call, res := made(nat, []*Value{iter, fn}, calls)
	// It consumes none of the frame's registers: its window is no one's.
	call.IC.Consumed = 0
	_, r1 := made(nat, []*Value{cursor, res}, []*CallSite{result})
	if b.hasCells() {
		// The next, or a getter of its result's, may assign a captured
		// binding, as any callee may.
		b.reloadAfterCall(nat)
		b.reloadAfterCall(gen)
	}
	// Where both meet, the step's value: the loop's end's test of it, and
	// its register where the loop goes on (stepOn), as iterStep's.
	r := f.newValue(cont, OpPhi, Tagged, r0, r1)
	if b.stepOn == nil {
		b.stepOn = map[*Block]stepDef{}
	}
	b.stepOn[on] = stepDef{in.Dest, r}
	done := f.newValue(cont, OpConst, Tagged)
	done.Const = ir.Value{Kind: ir.Uninitialized}
	// Never left: the constant is no string (OpEqTagged).
	is := f.newValue(cont, OpEqTagged, Bool, r, done)
	is.Aux, is.State, is.Index = int(ir.HostExit), st, 1
	st.addUse()
	cont.Control = is
	is.Uses++
	return true
}

// goFrame makes in, a closure or the end of a scope that closes what
// closures captured, by a call of Go from native code on the function's
// own frame (GoClosure, GoClose), as the interpreter makes them: the
// captured locals are where the frame has them, as native code assigns
// them there (storeCaptured). What Go refuses -- a frame it has none of,
// as a native callee's -- leaves for Go to make after an exit.
func (b *builder) goFrame(blk *Block, pc int, in ir.Instruction, gd *guarder) {
	call := gd.guard(OpCall, Tagged, ir.HostExit)
	site := &CallSite{Go: GoClosure, Argc: int(in.Key), ThisSlot: -1}
	if in.Op == ir.CloseUpvalues {
		site.Go, site.Closes = GoClose, in.Extra
	}
	if fb, ok := b.fb.(ClosureFeedback); ok && in.Op == ir.Closure {
		// Natively, from its pool: what it captures of the code's locals
		// is in cells of their own, none open on the frame -- or it
		// captures none, only its maker's own captured bindings and an
		// arrow's this, as AWFY Havlak's findSet's arrow does, which Go had
		// made at every call, 414,000 a run.
		if cs, ok := fb.Closure(pc); ok && cs.Pool != 0 && (b.p.Cells || !capturesLocals(cs.Captures)) {
			site.Pool, site.Captures, site.LexThis, site.LexThisReg = cs.Pool, cs.Captures, cs.LexThis, cs.LexThisReg
		}
	}
	if fb, ok := b.fb.(CellsFeedback); ok && in.Op == ir.CloseUpvalues && b.p.Cells {
		// Natively, from the function's pool of cells, as Go made each
		// one: AWFY Havlak's findLoops, whose loop's body makes an arrow
		// capturing a const of its iteration, 565,000 a run.
		if pool, cells, ok := fb.CellsPool(); ok {
			site.Pool, site.Captures = pool, cells
		}
	}
	b.f.cache(call).Calls, call.Index = []*CallSite{site}, b.f.Keeps
	switch {
	case in.Op == ir.Closure:
		// The cell planning took for it (closureKeep).
		call.Index, _ = b.closureKeep(pc)
	case b.f.Keeps >= b.f.keepLimit():
		// A close writes no keep cell (the VM's GoClose): any one does,
		// the first, when they are all taken.
		call.Index = 0
	default:
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	if in.Op == ir.Closure {
		b.assign(in.Dest, blk, r)
	}
}

// goConcat makes in, a concat, by a call of Go from native code (GoConcat),
// its parts the call's operands: what Go refuses leaves for Go to make
// after an exit.
func (b *builder) goConcat(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) {
	// A call's operands at a time (abi.MaxGoArgs), each call's string the
	// next one's first part, as the compiler joins a chain past its 32: a
	// concat of more had left native code at every turn -- a template's,
	// or a chain of a few calls and the strings between them. A part Go
	// refuses leaves for Go, which joins them all again from the operands,
	// none of them changed, the parts joined so far having run nothing.
	var r *Value
	for at := 0; at < in.Extra; {
		var parts []*Value
		if r != nil {
			parts = append(parts, r)
		}
		for ; at < in.Extra && len(parts) < abi.MaxGoArgs; at++ {
			parts = append(parts, operand(ir.Slot(in.Dest+at)))
		}
		call := gd.guard(OpCall, Tagged, ir.HostExit, parts...)
		if b.f.Keeps < b.f.keepLimit() {
			b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoConcat, Argc: len(parts), ThisSlot: -1}}, b.f.Keeps
			b.f.Keeps++
		}
		cell := b.f.newValue(blk, OpCallCell, Source, call)
		r = b.f.newValue(blk, OpKept, Tagged, cell, call)
		r.Shadow = cell
	}
	b.assign(in.Dest, blk, r)
}

// nativeHost reports whether native code makes the host operation at pc
// itself: an intrinsic call or instanceof the VM knows.
func (b *builder) nativeHost(pc int) bool {
	if in := b.p.Code[pc]; (in.Frame || in.Whole && in.Extra <= 4) && b.cur == b.root && b.goCalls() {
		// Go's from native code (GoHost).
		return true
	}
	if b.awaitAt(pc) {
		// An await native code goes on past in place (await).
		return true
	}
	if _, ok := b.intrinsic(pc); ok {
		return true
	}
	if _, ok := b.instanceOf(pc); ok {
		return true
	}
	if _, ok := b.fromCharCode(pc); ok {
		return true
	}
	return b.charCode(pc)
}

// awaitAt reports whether the instruction at pc is an async function's
// await (ir.Instruction's Await) that native code goes on past in place
// where the interpreter would (await): in the function's own frame, where
// Go may be called from native code.
func (b *builder) awaitAt(pc int) bool {
	in := &b.p.Code[pc]
	return in.Op == ir.Host && in.Await && b.cur == b.root && b.goCalls() && in.Left.Slot >= 0 && in.Dest >= 0 &&
		pc+1 < len(b.p.Code) && reachable(b.p, pc+1)
}

// await makes in, an async function's await at pc (awaitAt), as the
// interpreter's await (the VM's regSuspend) makes it: where the frame is
// one its await's job resumed and no job is queued (OpAwaitReady), the
// job the await would queue would be the queue's next, which would resume
// the frame with what the value settled to at once, so the frame goes on
// in place with that -- the value itself where it is not an object, and,
// where it is, what Go finds it settled to (GoAwait), a fulfilled promise
// of the realm's constructor's value -- native code going on after it on
// either branch (split). Anything else leaves at the await's state, where
// Go suspends the frame, as the interpreter would. The interpreter's
// interrupt check there is its budget's; native code's loops poll at
// their back edges. Left to Go, each in-place await was a host exit that
// Go made in place: async_await's loop, an await at each turn, was judged
// slow for its exits and ran in the interpreter.
func (b *builder) await(blk *Block, pc int, in ir.Instruction) {
	f := b.f
	x := b.read(in.Left.Slot, blk)
	st := b.state(blk, pc)
	obj := f.newValue(blk, OpAwaitReady, Bool, x)
	obj.Aux, obj.State = int(ir.HostExit), st
	st.addUse()
	yes, no, _ := b.split(blk, obj, true)
	b.assign(in.Dest, no, x)
	call := f.newValue(yes, OpCall, Tagged, x)
	if f.Keeps < f.keepLimit() {
		f.cache(call).Calls, call.Index = []*CallSite{{Go: GoAwait, ThisSlot: -1}}, f.Keeps
		f.Keeps++
	}
	call.Aux, call.State = int(ir.HostExit), st
	st.addUse()
	kept := f.newValue(yes, OpCallCell, Source, call)
	r := f.newValue(yes, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.assign(in.Dest, yes, r)
}

// fromCharCode is the address of String.fromCharCode if the method call of
// one argument at pc calls it (FromCharCodeFeedback), which fromCharCodeCall
// makes.
func (b *builder) fromCharCode(pc int) (uintptr, bool) {
	if b.fb == nil || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) {
		return 0, false
	}
	if _, ok := b.callSlotsOf(pc, 1, true); !ok {
		return 0, false
	}
	f, ok := b.fb.(FromCharCodeFeedback)
	if !ok {
		return 0, false
	}
	return f.FromCharCode(pc)
}

// fromCharCodeCall makes the call of String.fromCharCode at pc, at address
// fn: checked to call it, its argument an integer below 256, the result is
// the runtime's string of that code unit (OpCodeChar), in the call's result
// slot, with nothing made; anything else leaves for Go. In the function's
// own code, where the call has a keep cell for its result, a number that is
// another code -- a character past Latin-1, as a font's glyphs are -- Go
// makes from native code, as V8's code makes a string of any code unit
// (StringFromSingleCharCode): a call of Go of the leaf (GoLeaf) on a branch
// of its own (split), native code going on after either. Leaving there,
// PDF.js's Font.charToGlyph, whose characters are past 255, left native
// code at each call, the interpreter finishing the rest of each.
func (b *builder) fromCharCodeCall(blk *Block, pc int, fn uintptr, gd *guarder) {
	s, _ := b.callSlotsOf(pc, 1, true)
	callee := b.read(s.fn, blk)
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, callee)
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	x := b.read(s.arg, blk)
	code := gd.guard(OpUnboxF64, Float64, ir.HostExit, x)
	keep, kept := b.callKept(pc)
	if b.cur != b.root || !kept || !b.goCalls() {
		cell := gd.guard(OpCodeChar, Source, ir.HostExit, code)
		v := b.f.newValue(blk, OpLoadCell, Tagged, cell)
		v.Shadow = cell
		b.assign(s.dest, blk, v)
		return
	}
	f := b.f
	recv := b.read(s.recv, blk)
	cell := f.newValue(blk, OpCodeCharOr, Source, code)
	none := f.newValue(blk, OpCellNone, Bool, cell)
	st := b.state(blk, pc)
	miss, hit, _ := b.split(blk, none, true)
	v := f.newValue(hit, OpLoadCell, Tagged, cell)
	v.Shadow = cell
	b.assign(s.dest, hit, v)
	call := f.newValue(miss, OpCall, Tagged, callee, recv, x)
	f.cache(call).Calls = []*CallSite{{Go: GoLeaf, Argc: 1, Method: true, ThisSlot: -1, Leaf: fn}}
	call.Aux, call.State, call.Index = int(ir.HostExit), st, keep
	st.addUse()
	kc := f.newValue(miss, OpCallCell, Source, call)
	r := f.newValue(miss, OpKept, Tagged, kc, call)
	r.Shadow = kc
	b.assign(s.dest, miss, r)
	if b.hasCells() {
		b.reloadAfterCall(miss)
	}
}

// typeofAt is the address of the table of the runtime's strings typeof
// answers (TypeofFeedback) if pc, in the function's own code, is a typeof
// Go makes otherwise (a GoCall site of TypeOf), which typeofCall makes.
func (b *builder) typeofAt(pc int) (uintptr, bool) {
	if b.fb == nil || b.cur != b.root || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) || !b.goCalls() {
		return 0, false
	}
	if in := &b.p.Code[pc]; in.Window {
		// Register code's, its operand and its result its registers
		// (typeofCall).
		if in.Left.Slot < 0 || in.Dest < 0 {
			return 0, false
		}
	} else if b.p.Maps[pc].Depth < 1 || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth {
		return 0, false
	}
	if calls := b.callsAt(pc); len(calls) != 1 || calls[0].Go != GoCall || !calls[0].TypeOf {
		return 0, false
	}
	f, ok := b.fb.(TypeofFeedback)
	if !ok {
		return 0, false
	}
	cells, ok := f.TypeofCells()
	return cells, ok && cells != 0
}

// typeofCall makes the typeof at pc, its operand and its result its
// registers in register code's, the top of the stack in a slot program's:
// the runtime's string of the type, loaded from its cell in the table at
// cells (OpTypeofCell), with nothing made or called, as V8's code loads a
// type's name from its roots; what native code cannot tell, a proxy's, a
// call of Go (the site's GoCall) makes, on a branch of its own (split),
// native code going on after either.
func (b *builder) typeofCall(blk *Block, pc int, cells uintptr) {
	f := b.f
	src := b.p.Locals + b.p.Maps[pc].Depth - 1
	slot := src
	if in := &b.p.Code[pc]; in.Window {
		src, slot = in.Left.Slot, in.Dest
	}
	x := b.read(src, blk)
	cell := f.newValue(blk, OpTypeofCell, Source, x)
	cell.Const = ir.Value{Bits: uint64(cells)}
	none := f.newValue(blk, OpCellNone, Bool, cell)
	st := b.state(blk, pc)
	miss, hit, _ := b.split(blk, none, true)
	v := f.newValue(hit, OpLoadCell, Tagged, cell)
	v.Shadow = cell
	b.write(slot, hit, v)
	call := f.newValue(miss, OpCall, Tagged, x)
	f.cache(call).Calls = b.callsAt(pc)
	call.Aux, call.State, call.Index = int(ir.HostExit), st, b.callKeepAt(pc)
	st.addUse()
	kept := f.newValue(miss, OpCallCell, Source, call)
	r := f.newValue(miss, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.write(slot, miss, r)
}

// windowTypeof is typeofCall for register code's typeof at pc, in (its
// Window): its operand the window's register, its result in.Dest's.
func (b *builder) windowTypeof(blk *Block, pc int, in ir.Instruction, cells uintptr) {
	f := b.f
	x := b.read(in.Left.Slot, blk)
	cell := f.newValue(blk, OpTypeofCell, Source, x)
	cell.Const = ir.Value{Bits: uint64(cells)}
	none := f.newValue(blk, OpCellNone, Bool, cell)
	st := b.state(blk, pc)
	miss, hit, _ := b.split(blk, none, true)
	v := f.newValue(hit, OpLoadCell, Tagged, cell)
	v.Shadow = cell
	b.assign(in.Dest, hit, v)
	call := f.newValue(miss, OpCall, Tagged, x)
	f.cache(call).Calls = b.callsAt(pc)
	call.Aux, call.State, call.Index = int(ir.HostExit), st, b.callKeepAt(pc)
	st.addUse()
	kept := f.newValue(miss, OpCallCell, Source, call)
	r := f.newValue(miss, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.assign(in.Dest, miss, r)
}

// substring is the address of String.prototype.substring, slice, substr
// or charAt if the method call at pc, of two arguments -- one, charAt's --
// in the function's own code, calls it (SubstringFeedback) and Go makes it
// otherwise (a GoCall site), which substringCall makes, and which it is.
func (b *builder) substring(pc int) (fn uintptr, kind int, ok bool) {
	if b.fb == nil || b.cur != b.root || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) || b.fb.Generic(pc) {
		// A call whose checks have failed is Go's (Feedback's Generic): one
		// known by its method's name, the call of another's.
		return 0, 0, false
	}
	calls := b.callsAt(pc)
	if len(calls) != 1 || calls[0].Go != GoCall || !calls[0].Method || calls[0].Argc != 1 && calls[0].Argc != 2 {
		return 0, 0, false
	}
	argc := calls[0].Argc
	if _, ok := b.callSlotsOf(pc, argc, true); !ok {
		return 0, 0, false
	}
	f, ok := b.fb.(SubstringFeedback)
	if !ok {
		return 0, 0, false
	}
	fn, kind, ok = f.Substring(pc)
	if !ok || kind == SubstrAt != (argc == 1) {
		return 0, 0, false
	}
	return fn, kind, true
}

// substringCall makes the call of String.prototype.substring, slice,
// substr or charAt (kind, OpSubstringChar's Aux) at pc, at address fn,
// checked to call it, its arguments numbers: one code unit of a flat
// string, below 256, is the runtime's string of it (OpSubstringChar), with
// nothing made, as V8 makes a one-character string from its table of
// them; any other, a call of Go (the site's GoCall) makes, on a branch of
// its own (split), native code going on after it.
func (b *builder) substringCall(blk *Block, pc int, fn uintptr, kind int, gd *guarder) {
	f := b.f
	argc := 2
	if kind == SubstrAt {
		argc = 1
	}
	s, _ := b.callSlotsOf(pc, argc, true)
	// The receiver, the function and the arguments, in the order of a call
	// of Go's operands (GoCall).
	operands := []*Value{b.read(s.recv, blk), b.read(s.fn, blk)}
	for k := range argc {
		operands = append(operands, b.read(s.arg+k, blk))
	}
	recv, callee := operands[0], operands[1]
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, callee)
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	args := []*Value{recv}
	for _, x := range operands[2:] {
		args = append(args, gd.guard(OpUnboxF64, Float64, ir.HostExit, x))
	}
	cell := f.newValue(blk, OpSubstringChar, Source, args...)
	cell.Aux = kind
	none := f.newValue(blk, OpCellNone, Bool, cell)
	st := b.state(blk, pc)
	miss, hit, _ := b.split(blk, none, true)
	v := f.newValue(hit, OpLoadCell, Tagged, cell)
	v.Shadow = cell
	b.assign(s.dest, hit, v)
	var call *Value
	if b.goCalls() {
		// The function checked is the realm's, a leaf: the call is one of
		// Go of the leaf (GoLeaf), as leafCall's is, its operands the
		// function, the receiver and the arguments, made without a frame
		// -- what its check refuses leaves for Go to make after an exit --
		// rather than the call Go would make with one.
		args := append([]*Value{callee, recv}, operands[2:]...)
		call = f.newValue(miss, OpCall, Tagged, args...)
		f.cache(call).Calls = []*CallSite{{Go: GoLeaf, Argc: argc, Method: true, ThisSlot: -1, Leaf: fn}}
	} else {
		call = f.newValue(miss, OpCall, Tagged, operands...)
		f.cache(call).Calls = b.callsAt(pc)
	}
	call.Aux, call.State, call.Index = int(ir.HostExit), st, b.callKeepAt(pc)
	st.addUse()
	kept := f.newValue(miss, OpCallCell, Source, call)
	r := f.newValue(miss, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.assign(s.dest, miss, r)
	// A receiver Go converts may run script, which may assign a captured
	// binding, as a callee may.
	if b.hasCells() {
		b.reloadAfterCall(miss)
	}
}

// stringSelfCall makes the operation Go makes at pc (CallSite's Instr) of
// the one operand at slot, whose result is the operand itself where that is
// a string (StringSelf): told by its word, natively, as V8's ToPropertyKey
// takes a string as it is, and by the call, on a branch of its own
// (split), where it is anything else, native code going on after either.
func (b *builder) stringSelfCall(blk *Block, pc int, calls []*CallSite, slot int) {
	f := b.f
	x := b.read(slot, blk)
	st := b.state(blk, pc)
	is := f.newValue(blk, OpTypeIs, Bool, x)
	is.Index, is.Aux, is.State = int(ir.TypeString), int(ir.HostExit), st
	st.addUse()
	_, other, _ := b.split(blk, is, true)
	call := f.newValue(other, OpCall, Tagged, x)
	f.cache(call).Calls = calls
	call.Aux, call.State, call.Index = int(ir.HostExit), st, b.callKeepAt(pc)
	st.addUse()
	kept := f.newValue(other, OpCallCell, Source, call)
	r := f.newValue(other, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.write(slot, other, r)
	// A key's conversion may run script, which may assign a captured
	// binding, as a callee may.
	if b.hasCells() {
		b.reloadAfterCall(other)
	}
}

// charCode reports whether the method call of one argument at pc calls
// String.prototype.charCodeAt (CharCodeFeedback), which charCodeCall
// makes.
func (b *builder) charCode(pc int) bool {
	if b.fb == nil || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) {
		return false
	}
	if _, ok := b.callSlotsOf(pc, 1, true); !ok {
		return false
	}
	f, ok := b.fb.(CharCodeFeedback)
	return ok && f.CharCode(pc)
}

// charCodeCall makes the call of String.prototype.charCodeAt at pc: its
// receiver a flat string, its argument an integer below its length, and
// its function the intrinsic (OpStringCode), the code unit there is its
// result, in the call's result slot; anything else leaves for Go.
func (b *builder) charCodeCall(blk *Block, pc int, gd *guarder, boxF func(*Value) *Value) {
	s, _ := b.callSlotsOf(pc, 1, true)
	key := gd.guard(OpUnboxF64, Float64, ir.HostExit, b.read(s.arg, blk))
	code := gd.guard(OpStringCode, Float64, ir.HostExit, b.read(s.fn, blk), b.read(s.recv, blk), key)
	b.assign(s.dest, blk, boxF(code))
}

// instanceOf is the instanceof at pc in the frame being translated, if the
// VM knows its constructor (InstanceOfFeedback): its operands the value and
// the constructor, its result in the value's slot.
func (b *builder) instanceOf(pc int) (InstanceOfSite, bool) {
	if b.fb == nil || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) {
		return InstanceOfSite{}, false
	}
	if in := &b.p.Code[pc]; in.Window {
		// Register code's, its operands its registers (instanceOfSlots).
		if in.Extra != 1 || in.Right.Slot < 0 || b.fb.Generic(pc) {
			return InstanceOfSite{}, false
		}
	} else if b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-1 || b.p.Maps[pc].Depth < 2 {
		return InstanceOfSite{}, false
	}
	f, ok := b.fb.(InstanceOfFeedback)
	if !ok {
		return InstanceOfSite{}, false
	}
	k, ok := f.InstanceOf(pc)
	if !ok || k.HasInstance.Shape == 0 || k.Prototype.Shape == 0 {
		return InstanceOfSite{}, false
	}
	return k, true
}

// instanceOfOp makes the instanceof at pc (InstanceOfSite): checked to have
// its constructor, whose Symbol.hasInstance is still the realm's, read
// through its shape and Function.prototype's, and whose prototype property,
// read through its shape, is an object, the answer is found along the
// value's prototypes -- any check failing, Go makes it.
func (b *builder) instanceOfOp(blk *Block, pc int, k InstanceOfSite, gd *guarder, boxB func(*Value) *Value) {
	f := b.f
	// Its operands, the value and the constructor, and its result:
	// register code's in its registers, a slot program's on the operand
	// stack.
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	value, ctorSlot, dest := sp-2, sp-1, b.cur.base+b.p.Locals+b.p.Maps[pc+1].Depth-1
	if in := &b.p.Code[pc]; in.Window {
		value, ctorSlot, dest = b.cur.base+in.Left.Slot, b.cur.base+in.Right.Slot, b.cur.base+in.Dest
	}
	ctor := gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(ctorSlot, blk))
	same := gd.guard(OpSameObject, None, ir.HostExit, ctor)
	same.Const = ir.Value{Bits: uint64(k.Ctor)}
	read := func(site PropertySite) *Value {
		cell := gd.guard(OpPropCell, Source, ir.HostExit, ctor)
		cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
		if site.Holders[0].Object != 0 {
			b.f.cache(cell).Holders = b.f.holderChain(HolderChain{Holders: site.Holders, More: site.More})
		}
		v := f.newValue(blk, OpLoadCell, Tagged, cell)
		v.Shadow = cell
		return v
	}
	has := gd.guard(OpObjectOf, Ptr, ir.HostExit, read(k.HasInstance))
	fn := gd.guard(OpSameObject, None, ir.HostExit, has)
	fn.Const = ir.Value{Bits: uint64(k.HasInstanceFn)}
	proto := gd.guard(OpObjectOf, Ptr, ir.HostExit, read(k.Prototype))
	yes := gd.guard(OpInstanceOf, Bool, ir.HostExit, b.read(value, blk), proto)
	b.assign(dest, blk, boxB(yes))
}

// intrinsic is the intrinsic the call at pc in the frame being translated
// makes, if any (IntrinsicFeedback): a method call whose operands are the
// receiver, the function and the arguments, one, or two for Math.pow.
func (b *builder) intrinsic(pc int) (Intrinsic, bool) {
	if b.fb == nil || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) {
		return Intrinsic{}, false
	}
	f, ok := b.fb.(IntrinsicFeedback)
	if !ok {
		return Intrinsic{}, false
	}
	k, ok := f.Intrinsic(pc)
	if !ok {
		return Intrinsic{}, false
	}
	switch k.Op {
	case OpSqrtF64, OpAbsF64, OpPowF64, OpMaxF64, OpMinF64, OpFloorF64, OpCeilF64, OpTruncF64, OpRoundF64, OpSignF64, OpFroundF64, OpMulI32, OpClz32:
	case OpMathCall:
		if k.Go != GoMath && k.Go != GoAtan2 && k.Go != GoHypot || !b.goCalls() {
			return Intrinsic{}, false
		}
	case OpRandomF64:
		if k.Random == 0 || !b.goCalls() {
			return Intrinsic{}, false
		}
	case OpZextW, OpSextW, OpBigOfF64:
		// A BigInt's, boxed in a keep cell, in the root (bigint.go).
		if b.cur != b.root || k.Fn < 0 || k.Fn > 64 || k.Plain != (k.Op == OpBigOfF64) || b.f.Keeps >= b.f.keepLimit() {
			return Intrinsic{}, false
		}
	default:
		return Intrinsic{}, false
	}
	n := k.args()
	if n < 0 || n == 0 && k.Op != OpRandomF64 || n > maxIntrinsicArgs {
		return Intrinsic{}, false
	}
	if _, ok := b.callSlotsOf(pc, n, !k.Plain); !ok {
		return Intrinsic{}, false
	}
	return k, true
}

// args is how many arguments the intrinsic takes.
func (k Intrinsic) args() int {
	switch {
	case k.Op == OpRandomF64:
		return 0
	case k.Args != 0:
		return k.Args
	case k.Op == OpPowF64 || k.Op == OpMaxF64 || k.Op == OpMinF64 || k.Op == OpMathCall && k.Go != GoMath, k.Op == OpZextW || k.Op == OpSextW, k.Op == OpMulI32:
		return 2
	}
	return 1
}

// intrinsicCall makes the intrinsic call at pc (Intrinsic): checked to call
// the function, its argument a number -- either not, Go makes the call --
// its result is computed, in the call's result slot.
func (b *builder) intrinsicCall(blk *Block, pc int, k Intrinsic, gd *guarder, boxF func(*Value) *Value) {
	switch k.Op {
	case OpZextW, OpSextW:
		b.asNCall(blk, pc, k, gd)
		return
	case OpBigOfF64:
		b.bigOfCall(blk, pc, k, gd)
		return
	}
	n := k.args()
	s, _ := b.callSlotsOf(pc, n, !k.Plain)
	object := gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(s.fn, blk))
	same := gd.guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(k.Callee)}
	var xs [maxIntrinsicArgs]*Value
	for i := range n {
		xs[i] = gd.guard(OpUnboxF64, Float64, ir.HostExit, b.read(s.arg+i, blk))
	}
	var r *Value
	switch k.Op {
	case OpPowF64:
		r = gd.guard(OpPowF64, Float64, ir.HostExit, xs[0], xs[1])
	case OpMulI32:
		// Math.imul: each number's ToInt32, and their product's low half,
		// signed.
		x, y := b.f.newValue(blk, OpToInt32, Int32, xs[0]), b.f.newValue(blk, OpToInt32, Int32, xs[1])
		r = b.f.newValue(blk, OpI32ToF64, Float64, b.f.newValue(blk, OpMulI32, Int32, x, y))
	case OpClz32:
		// Math.clz32: the number's ToUint32, whose bits OpToInt32 gives,
		// and its leading zeros, a number from 0 to 32.
		x := b.f.newValue(blk, OpToInt32, Int32, xs[0])
		r = b.f.newValue(blk, OpI32ToF64, Float64, b.f.newValue(blk, OpClz32, Int32, x))
	case OpMaxF64, OpMinF64:
		// Of each in turn, which NaN and the zeros' order allow.
		r = xs[0]
		for _, y := range xs[1:n] {
			r = b.f.newValue(blk, k.Op, Float64, r, y)
		}
	case OpMathCall:
		r = b.f.newValue(blk, OpMathCall, Float64, xs[:n]...)
		r.Index, r.Aux = int(k.Go), k.Fn
	case OpRandomF64:
		r = b.f.newValue(blk, OpRandomF64, Float64)
		r.Const = ir.Value{Bits: uint64(k.Random)}
	default:
		r = b.f.newValue(blk, k.Op, Float64, xs[0])
	}
	b.assign(s.dest, blk, boxF(r))
}

// forwardCall checks a forwarded call (InlineSite's Forward) calls the
// function inlined there through the realm's apply, and gives the callee's
// start its slots: the arguments the frame it is in was given, past which
// undefined, its receiver, and undefined in every other. The call's state,
// where the frames of an exit inside the callee go on, is the frame's at
// the call, not its call's: the frame is made there.
func (b *builder) forwardCall(blk *Block, pc, sp int, fr *frame, gd *guarder) {
	site := fr.site
	target := gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-3, blk))
	same := gd.guard(OpSameObject, None, ir.HostExit, target)
	same.Const = ir.Value{Bits: uint64(site.Callee)}
	apply := gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-2, blk))
	same = gd.guard(OpSameObject, None, ir.HostExit, apply)
	same.Const = ir.Value{Bits: uint64(site.Apply)}
	if site.Coerce && site.ThisSlot >= 0 {
		// A receiver that is not an object is coerced, which Go does.
		gd.guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-1, blk))
	}
	room := gd.guard(OpFrameRoom, None, ir.HostExit)
	room.Index = fr.base - b.root.p.Locals + fr.p.Locals + fr.p.StackSize
	room.Const = ir.Value{Bits: uint64(fr.depth)}
	fr.call = b.frameState(blk, pc)
	undefined := b.f.newValue(blk, OpConst, Tagged)
	undefined.Const = ir.Value{Kind: ir.Undefined}
	fr.init = b.f.refsOf(fr.p.Locals + fr.p.StackSize)
	for s := range fr.init {
		fr.init[s] = undefined
	}
	fr.args, fr.argSlot = b.cur.args, b.cur.argSlot
	for i := 0; i < site.Params && i < len(fr.args); i++ {
		fr.init[i] = fr.args[i]
	}
	if site.ThisSlot >= 0 {
		fr.init[site.ThisSlot] = b.read(sp-1, blk)
	}
}

// shifted is a callee's instruction with its slots made the builder's,
// base past its own. Extra is a slot but where it counts operands -- a
// concat's parts, an array literal's elements, a call's arguments -- which
// it does not move: shifted too, an inlined callee's concat, test262's
// decimalToPercentHexString's, read its parts that far past them, the
// caller's slots or none.
func shifted(in ir.Instruction, base int) ir.Instruction {
	for _, o := range []*ir.Operand{&in.Left, &in.Right, &in.Third} {
		if o.Slot >= 0 {
			o.Slot += base
		}
	}
	in.Dest += base
	switch in.Op {
	case ir.StringConcat, ir.ArrayLiteral, ir.Host, ir.Call:
	default:
		if in.Extra >= 0 {
			in.Extra += base
		}
	}
	if in.Check {
		in.CheckSlot += base
	}
	return in
}

// cell is the op that finds the site's binding.
func (s GlobalSite) cell() Op {
	switch {
	case s.Holder != 0:
		return OpBindingCell
	case s.Lexical:
		return OpLexCell
	}
	return OpGlobalCell
}

// globalCell is the guard that finds the site's binding, exiting as kind
// says where it is not there.
func (b *builder) globalCell(site GlobalSite, gd *guarder, kind ir.ExitKind) *Value {
	cell := gd.guard(site.cell(), Source, kind)
	cell.Index, cell.Key = int(site.Index), site.Key
	if site.Holder != 0 {
		cell.Const, cell.Global = ir.Value{Bits: uint64(site.Holder)}, site.Shadowable
	} else if site.Epoch != 0 && cell.Op == OpGlobalCell {
		cell.Const, cell.Epoch = ir.Value{Bits: uint64(site.Scope)}, site.Epoch
	}
	return cell
}

// global is the feedback for a global read at pc, if any.
func (b *builder) global(pc int) (GlobalSite, bool) {
	if op := b.p.Code[pc].Op; b.fb == nil || op != ir.BindingRead && op != ir.BindingWrite && op != ir.BindingCheck {
		return GlobalSite{}, false
	}
	return b.fb.Global(pc)
}

func reachable(p *ir.Program, pc int) bool {
	return pc >= 0 && pc < len(p.Code) && p.Maps[pc].Depth >= 0
}

// plan finds entries and blocks, and checks every operation is translatable.
func (b *builder) plan() error {
	p := b.p
	// By PC, one past the end included.
	entries, leaders := b.f.bools(len(p.Code)+2), b.f.bools(len(p.Code)+2)
	entries[0], leaders[0] = true, true
	for _, pc := range p.Entries {
		entries[pc], leaders[pc] = true, true
	}
	total := 0
	// The calls to inline, the most frequent first, and of those a
	// construction first: what inlining's budget (maxInlineTotal) allows
	// goes to what saves the most, as V8's goes to the sites its feedback
	// finds called most often for each call of the function
	// (JSInliningHeuristic's candidates, by frequency); a call whose
	// frequency is not known is taken as made once outside the function's
	// loops, and more often than any inside one (inlineOrder). A
	// construction not inlined is made by a call of Go wherever its
	// constructor has no code native callers may call -- one forwarding its
	// arguments, Prototype's Class.create's -- where any other call is a
	// native call: inlined in the order of their PCs, the
	// calls before a function's constructions had taken the bounds once
	// callees reading globals and making constructions of their own could
	// be inlined, and RayTrace's constructions of its colors and
	// intersections were made by Go. By PC, from the workspace's scratch,
	// taken while planning: an InlineSite a PC, made afresh at each
	// compile, had been a fifth of what Typescript's compiles allocated.
	sc := b.f.scr()
	asked := idTable(sc.asked, len(p.Code))
	order := sc.order[:0]
	sc.asked, sc.order = nil, nil
	defer func() { clear(asked); sc.asked, sc.order = asked[:0], order[:0] }()
	loopNesting(p, asked)
	for pc := range p.Code {
		if !reachable(p, pc) || b.inlined[pc] != nil {
			continue
		}
		k := &asked[pc]
		_, isAccessor := b.accessor(pc)
		if pc0, ok := b.called[pc]; ok {
			k.site, k.ok = b.calledSite(pc, pc0)
		} else if b.fb != nil && p.Code[pc].Op == ir.Host && !isAccessor && !b.fb.Generic(pc) {
			k.site, k.ok = b.fb.Inline(pc)
		}
		if !k.ok && !isAccessor {
			// Nothing to inline (inlineSiteAt).
			continue
		}
		k.freq, k.known = b.siteFrequency(pc)
		order = append(order, int32(pc))
	}
	slices.SortStableFunc(order, func(x, y int32) int { return inlineOrder(&asked[x], &asked[y]) })
	for _, at := range order {
		pc := int(at)
		if b.inlined[pc] != nil {
			continue
		}
		k := &asked[pc]
		inlined := len(b.f.Inlined)
		fr, ok := b.inlineSiteAt(pc, &total, k)
		if pc0, called := b.called[pc]; ok && called {
			fr.given, fr.givenPC0 = true, pc0
		}
		if ok && !b.host(pc) {
			b.f.Inlined = b.f.Inlined[:inlined]
		}
		if ok && b.host(pc) {
			// Not a call native code makes itself (nativeHost) -- an
			// intrinsic its feedback names, though another site's
			// feedback has it inline the function the call met.
			if b.inlined == nil {
				b.inlined = map[int]*frame{}
			}
			b.inlined[pc] = fr
		}
	}
	b.unmakeCalled()
	// Go goes on natively after what it ran where it is sure to run it:
	// an instruction left to Go whole; a call, which leaves while the
	// collector marks. An element's write, which leaves only where Go
	// refuses it -- an append is a call of Go that goes on -- has an entry
	// after it only once Go has gone on there, as a call of a function
	// has (below). Anywhere else an exit
	// is a guard's, which seldom fails, and the interpreter runs on from
	// there, as from V8's deoptimizations: an entry is a block whose loads
	// merge with what native code computed, in phis, and in shadows, at
	// every merge after it, and entries had been most of what a compile
	// made -- a guard's after nearly every instruction. Nor is code
	// compiled with an entry where Go went on in the interpreter after a
	// guard's exit: as V8's code is entered only at a function's start and
	// by on-stack replacement at a loop, the interpreter goes on to the
	// next loop header, and native code from there. Entries Go had asked
	// for so, each a join where what the loop had proved held no longer,
	// had Gameboy's instruction dispatch run natively 40% longer with 46.
	resumes := func(pc int) bool {
		switch op := p.Code[pc].Op; {
		case b.host(pc), b.keyedRead(pc), op == ir.ObjectLiteral, op == ir.ArrayLiteral, op == ir.StringAdd, op == ir.StringConcat, op == ir.StringPart, op == ir.ArrayWrite, op == ir.IterStep,
			op == ir.IterClose && !p.Code[pc].When, op == ir.HasProperty, op == ir.DeleteProperty, op == ir.ArgumentRead && b.cur == b.root,
			op == ir.Closure, op == ir.CloseUpvalues:
			return true
		}
		return false
	}
	for pc, in := range p.Code {
		if !reachable(p, pc) {
			continue
		}
		// The calls an instruction left to Go makes natively, decided once,
		// here, which the entries, the blocks and the translation take, and
		// the keep cell each one's result is kept in, taken now, as a
		// literal's is: planning and building must agree.
		if _, ok := b.fromCharCode(pc); ok && b.cur == b.root && b.inlined[pc] == nil && b.f.Keeps < b.f.keepLimit() && b.goCalls() {
			// String.fromCharCode's, which native code makes itself but for
			// a code past Latin-1, a call of Go's (fromCharCodeCall): its
			// result's keep cell.
			b.callTables()
			b.callKeeps[pc] = b.f.Keeps + 1
			b.f.Keeps++
		}
		if b.host(pc) && b.inlined[pc] == nil && b.f.Keeps < b.f.keepLimit() {
			if calls := b.nativeCalls(pc); len(calls) != 0 {
				b.callTables()
				b.calls[pc], b.callKeeps[pc] = calls, b.f.Keeps+1
				b.f.Keeps++
				if site, ok := b.forEachAt(pc); ok && b.nslots+loopSlots(site.Kind) <= abi.MaxRecords && b.f.Keeps+loopKeeps(site.Kind) <= b.f.keepLimit() {
					// A forEach made a loop of: its slots past every
					// other's, and its callback's result's keep cell --
					// and its array's, map's or filter's.
					if b.forEaches == nil {
						b.forEaches = map[int]forEachPlan{}
					}
					fp := forEachPlan{site: site, base: b.nslots, keep: b.f.Keeps}
					nslots, keeps, loops, loopsEnd := b.nslots, b.f.Keeps, b.f.Loops, b.f.LoopsEnd
					b.nslots += loopSlots(site.Kind)
					b.f.Keeps += loopKeeps(site.Kind)
					b.f.Loops, b.f.LoopsEnd = true, b.nslots
					planned := b.planCallback(pc, &fp, &total)
					if site.Kind&abi.LoopKindMask == abi.LoopSort && !planned {
						// A sort is made natively with its comparator
						// inlined, or not at all: the call is Go's.
						b.nslots, b.f.Keeps, b.f.Loops, b.f.LoopsEnd = nslots, keeps, loops, loopsEnd
					} else {
						if planned && fp.made {
							// The closure is not made: nothing exits there,
							// for Go to go on after.
							entries[fp.site.Made.PC+1] = false
						}
						b.forEaches[pc] = fp
					}
				}
			}
		}
		switch in.Op {
		case ir.ArrayRead, ir.ArrayUpdate:
			if in.Reference && resumes(pc) {
				// Go reads what native code does not, and resumes after it.
				entries[pc+1] = true
			}
		case ir.ArrayLength:
			if b.fb != nil && b.fb.Generic(pc) && resumes(pc) {
				// A length native code did not read is Go's once its guard
				// has failed, which resumes after it.
				entries[pc+1] = true
			}
		case ir.Unary, ir.Update:
			if b.generic(pc) && resumes(pc) {
				// An operand not a number once its guard has failed: Go
				// converts it, as for a binary operator's, and resumes after.
				entries[pc+1] = true
			}
		case ir.Raw:
			// Typed code's: a conversion of what is no number, a float's
			// remainder and a power Go makes at a host exit, which resumes
			// after it (raw.go).
			switch in.Raw {
			case ir.RawConvI, ir.RawConvF, ir.RawFMod, ir.RawFMath:
				if resumes(pc) {
					entries[pc+1] = true
				}
			}
		case ir.Nop, ir.Copy, ir.CopyPair, ir.StoreLoad, ir.Swap, ir.Insert2, ir.Insert3,
			ir.Return, ir.ArrayKey, ir.StringFreeze:
		case ir.UpvalueRead:
			// The function's own captured binding is read natively, its
			// check deoptimizing; an inlined callee's leaves at its call.
		case ir.IterClose:
			// Go goes on after a closing it made; a for...in's is never
			// Go's.
			if !in.When {
				entries[pc+1] = true
			}
		case ir.Coercible:
			// Go only throws at it.
		case ir.ParamsDead, ir.ParamInit, ir.ParamDefault:
			// A prologue's, entered only at the function's start.
		case ir.IterStep:
			// Go goes on after a step it made, past it or at the loop's
			// end (ir.Program's Entries), where native code did not; a
			// destructuring's step only past it.
			if !in.When {
				leaders[in.Target] = true
			}
			// Register code's step made natively has none after it: the
			// block after it, which assigns the step's register (stepOn),
			// is reached from the step alone. Where Go makes the step
			// after all, the interpreter goes on to the loop's header.
			if resumes(pc) && !(b.p.Origin != nil && !in.When && !b.host(pc)) {
				entries[pc+1] = true
			}
		case ir.Rethrow:
			// A normal completion goes on natively, any other deoptimizing;
			// once one has (Generic), Go runs each, and native code goes on
			// after a normal one.
			if b.host(pc) && resumes(pc) {
				entries[pc+1] = true
			}
		case ir.StringPart:
			// Checked natively, a failure deoptimizing; where one failed
			// before (Generic), Go converts the part, as a concat of one
			// part, and native code resumes after it.
			if b.fb != nil && b.fb.Generic(pc) && resumes(pc) {
				entries[pc+1] = true
			}
		case ir.TypeTest:
			// An object it cannot tell exits at the typeof, which Go makes;
			// the interpreter goes on, the boolean not where the typeof's
			// string was (compile's lowering of RJTypeof): no entry after it.
		case ir.ArrayWrite:
			// What native code does not store, Go does after an exit, which
			// seldom comes: an append is a call of Go that goes on (GoElemWrite,
			// GoElemStore). So, as after a call of a function (below), there
			// is no entry until Go has gone on there (ResumeFeedback), the
			// code compiled again with it then (Func's Unresumed). An entry
			// after each write had cut a loop of element updates, tab[k] += j
			// ten times a turn, into ten blocks joined by phis of every value
			// it carried, each reading the array's checks anew.
			if b.resumed(pc + 1) {
				entries[pc+1] = true
			} else {
				b.f.Unresumed = append(b.f.Unresumed, int(p.Maps[pc+1].PC))
			}
		case ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead, ir.BindingRead,
			ir.StringMethod, ir.StringCode, ir.BindingWrite, ir.BindingCheck, ir.Resolved, ir.ObjectLiteral, ir.FieldDefine,
			ir.ArrayLiteral, ir.StringConst, ir.BigIntConst, ir.UpvalueWrite, ir.StringAdd, ir.StringConcat, ir.Closure, ir.CloseUpvalues, ir.HasProperty,
			ir.DeleteProperty, ir.ArgumentRead:
			// What native code does not do exits to Go, which resumes after
			// it. A fixed global's check only deoptimizes: the binding can
			// never move, so it never fails, and what follows sees its
			// constant rather than a merge with a slot loaded at an entry.
			// A field Go defines from native code goes on natively.
			if site, ok := b.global(pc); (!ok || !site.Fixed) && resumes(pc) && !(in.Op == ir.FieldDefine && b.goesOn(pc)) {
				entries[pc+1] = true
			}
		case ir.Binary:
			if (in.Operator == ir.Eq || in.Operator == ir.Ne || in.Operator == ir.Mod || b.generic(pc)) && resumes(pc) {
				// A comparison or remainder of non-numbers exits to Go, which
				// resumes after it.
				entries[pc+1] = true
			}
		case ir.Jump:
			leaders[in.Target] = true
			if in.Target <= pc {
				entries[in.Target] = true
			}
		case ir.Branch:
			leaders[in.Target] = true
			if in.Target <= pc {
				entries[in.Target] = true
			}
		case ir.Host, ir.Call:
			switch {
			case b.goesOn(pc):
				// A call Go makes goes on natively: Go leaves it only for
				// a throw, the runtime stopping, or the code to be compiled
				// again -- then the interpreter runs on to the next loop
				// header, as from V8's deoptimizations.
			case !resumes(pc):
			case b.inlined[pc] == nil && functionCalls(b.callsAt(pc)) && !b.resumed(pc+1):
				// A call of a function native code makes returns to it: Go
				// goes on after it only where the call left -- its callee
				// not one the code checks for -- which seldom happens, and
				// at few of a program's calls. Unlike it, a built-in's that
				// native code makes itself, such as push, leaves whenever
				// its fast path does not apply; and an inlined callee is the
				// code itself, whose exits -- an operation it leaves to Go
				// -- leave the code there as the code's own do, where they
				// keep their entries: those calls keep theirs, which made
				// lazily had also cost the loops around them what entries
				// give, values reloaded and unboxed where they come in
				// (AWFY's Queens: placeQueen's column held boxed through the
				// loop, its spills doubled). An entry after each call of a
				// function, its live slots loaded and merged in phis at
				// every join after it, had been a sixth of Typescript's
				// compiles, for a few dozen of its thousands of calls that
				// Go ever went on after. As V8's code is entered only at a
				// function's start and at its loops, there is none until Go
				// has gone on there (ResumeFeedback): the code is compiled
				// again with the entry then (Func's Unresumed).
				b.f.Unresumed = append(b.f.Unresumed, int(p.Maps[pc+1].PC))
			default:
				entries[pc+1] = true
			}
		default:
			return fmt.Errorf("%w: %d at pc %d", ErrUnsupported, in.Op, pc)
		}
		switch {
		case in.Op == ir.Jump || in.Op == ir.Branch || in.Op == ir.Return || in.Op == ir.IterStep && !in.When || b.host(pc) && !b.goesOn(pc) || b.unfed(pc):
			leaders[pc+1] = true
		}
	}
	for pc0 := range b.madeCalls {
		// The closure is not made: nothing exits there, for Go to go on
		// after.
		if !looped(p, pc0+1) {
			entries[pc0+1] = false
		}
	}
	// A parameter list's prologue binds its parameters from what their
	// slots had at the function's start (ParamsDead): native code is never
	// entered inside it, where the slots are in the dead zone. Where it
	// leaves, the interpreter goes on with the call's arguments, and native
	// code is entered again at a loop after the prologue.
	dead, prologue := -1, 0
	for pc, in := range p.Code {
		switch {
		case !reachable(p, pc):
		case in.Op == ir.ParamsDead:
			if dead >= 0 || b.cur != b.root {
				return fmt.Errorf("%w: a second parameter prologue at pc %d", ErrUnsupported, pc)
			}
			dead, prologue = int(in.Key), pc+1
		case in.Op == ir.ParamInit || in.Op == ir.ParamDefault:
			if int(in.Key) >= dead {
				return fmt.Errorf("%w: a parameter bound outside its prologue at pc %d", ErrUnsupported, pc)
			}
			prologue = pc + 1
		}
	}
	for pc := 1; pc < prologue; pc++ {
		if slices.Contains(p.Entries, pc) {
			return fmt.Errorf("%w: a loop in a parameter list's prologue at pc %d", ErrUnsupported, pc)
		}
		entries[pc] = false
	}
	for pc, entry := range entries {
		if entry && reachable(p, pc) {
			b.entryPCs = append(b.entryPCs, pc)
			leaders[pc] = true
		}
	}

	b.blockAt = b.f.blockList(len(p.Code) + 2)
	starts := b.f.ints(len(p.Code) + 2)[:0]
	for pc, leader := range leaders {
		if leader && reachable(b.p, pc) {
			starts = append(starts, pc)
		}
	}
	for _, pc := range starts {
		b.blockAt[pc] = b.f.newBlock(pc)
	}
	b.endOf = b.f.ints(blockRoom * len(b.f.Blocks))[:len(b.f.Blocks)]
	for i, pc := range starts {
		end := len(p.Code) - 1
		if i+1 < len(starts) {
			end = starts[i+1] - 1
		}
		for q := pc; q <= end; q++ {
			if in := p.Code[q]; in.Op == ir.Jump || in.Op == ir.Branch || in.Op == ir.Return || in.Op == ir.IterStep && !in.When || b.host(q) && !b.goesOn(q) || b.unfed(q) {
				end = q
				break
			}
		}
		blk := b.blockAt[pc]
		b.endOf[blk.ID] = end
		last := p.Code[end]
		if b.unseen(end) {
			// An operator whose operands met nothing: Go makes it at a
			// host exit, a branch's too, the interpreter going on.
			blk.Kind = BlockExit
			continue
		}
		switch last.Op {
		case ir.Jump:
			blk.Kind = BlockPlain
			b.edge(blk, b.blockAt[last.Target])
		case ir.Branch:
			blk.Kind = BlockIf
			taken, fall := b.blockAt[last.Target], b.blockAt[end+1]
			if last.When {
				b.edge(blk, taken)
				b.edge(blk, fall)
			} else {
				b.edge(blk, fall)
				b.edge(blk, taken)
			}
		case ir.Return:
			blk.Kind = BlockReturn
		case ir.IterStep:
			if b.host(end) {
				blk.Kind = BlockExit
				break
			}
			if last.When {
				// A destructuring's step, which goes on.
				blk.Kind = BlockPlain
				b.edge(blk, b.blockAt[end+1])
				break
			}
			// Done, to the loop's end; else on, with the value.
			blk.Kind = BlockIf
			b.edge(blk, b.blockAt[last.Target])
			b.edge(blk, b.blockAt[end+1])
		case ir.Host, ir.Call, ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead, ir.BindingRead, ir.BindingWrite, ir.BindingCheck,
			ir.ObjectLiteral, ir.FieldDefine, ir.ArrayLiteral, ir.StringConst, ir.BigIntConst, ir.UpvalueRead, ir.UpvalueWrite, ir.StringAdd, ir.StringConcat, ir.Rethrow,
			ir.Closure, ir.CloseUpvalues, ir.HasProperty, ir.DeleteProperty, ir.ArgumentRead:
			if fr := b.inlined[end]; fr != nil {
				// Into the callee, whose returns go to the block after it.
				blk.Kind = BlockPlain
				fr.callBlock = blk
				break
			}
			if b.unfed(end) {
				// A property operation that deoptimizes at every receiver.
				blk.Kind = BlockExit
				break
			}
			if !b.host(end) {
				blk.Kind = BlockPlain
				b.edge(blk, b.blockAt[end+1])
				break
			}
			if len(b.callsAt(end)) != 0 {
				blk.Kind = BlockPlain
				b.edge(blk, b.blockAt[end+1])
				break
			}
			blk.Kind = BlockExit
		default:
			blk.Kind = BlockPlain
			b.edge(blk, b.blockAt[end+1])
		}
	}
	frames := b.f.scr()
	b.frameOf = idTable(frames.frameOf, blockRoom*len(b.f.Blocks))[:len(b.f.Blocks)]
	frames.frameOf = b.frameOf
	for _, pc := range sortedKeys(b.inlined) {
		b.planInline(b.inlined[pc], b.inlined[pc].callBlock, b.blockAt[pc+1])
	}
	// Entry blocks load the live slots and go to the block for their PC.
	// One is there only for Go to go on at after an instruction it ran
	// (Entry's Resume) unless a loop's back edge goes there, the lowering
	// asks for it -- a handler, a generator's first resumption -- or it is
	// the start; and none is in a generator whose yields a loop's step
	// resumes natively, at the entries after them (Func's GenResumes). An
	// async function's resumption after an await where no entry is runs in
	// the interpreter (the VM's jitRegEnter).
	looped := b.f.bools(len(p.Code) + 1)
	suspends := false
	for pc, in := range p.Code {
		if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc {
			looped[in.Target] = true
		}
		suspends = suspends || in.Suspend
	}
	for _, pc := range b.entryPCs {
		e := b.f.newBlock(-1)
		e.Kind = BlockPlain
		e.Generic = b.fb != nil && b.fb.EntryGeneric(int(p.Maps[pc].PC))
		b.edge(e, b.blockAt[pc])
		resume := pc != 0 && !looped[pc] && !suspends && !slices.Contains(p.Entries, pc)
		osr := pc != 0 && looped[pc] && !suspends && !slices.Contains(p.Entries, pc) && !b.resumed(pc)
		b.f.Entries = append(b.f.Entries, Entry{PC: pc, Depth: p.Maps[pc].Depth, Block: e, Resume: resume, OSR: osr})
	}
	// A loop header is reached by an edge from a block at or after it, in
	// one frame: a callee's blocks have their own PCs, and its loops their
	// own headers (InlineCallsGiven).
	for _, blk := range b.f.Blocks {
		blk.Backedge = b.f.bools(len(blk.Preds))
		b.markBackedges(blk)
	}
	b.f.InlinedCode = total
	return nil
}

// markBackedges marks the edges into blk from blocks of its frame at or
// after it -- but a callee's start, which goes to its first block -- as a
// loop's back edges, and blk its header.
func (b *builder) markBackedges(blk *Block) {
	fr := b.frameAt(blk)
	for i, pred := range blk.Preds {
		if pred.PC >= 0 && blk.PC >= 0 && pred.PC >= blk.PC && b.frameAt(pred) == fr && (fr == b.root || pred != fr.start) {
			blk.Backedge[i] = true
			blk.LoopHeader = true
		}
	}
}

func (b *builder) edge(from, to *Block) {
	if to == nil {
		panic("ssa: edge to an unreachable PC")
	}
	from.Succs = b.f.appendBlock(from.Succs, to)
	to.Preds = b.f.appendBlock(to.Preds, from)
}

// translate fills every block, sealing each once its predecessors are filled
// (Braun et al., "Simple and Efficient Construction of Static Single
// Assignment Form").
func (b *builder) translate() {
	n := len(b.f.Blocks)
	b.nextBlock = n
	sc0 := b.f.scr()
	b.defs.reset(sc0.defKeys, sc0.defVals, sc0.defSpareKeys, sc0.defSpareVals, 4*n)
	b.marks, b.phiArgs, b.dirtyLists, b.mergeDiff = sc0.marks, sc0.phiArgs[:0], sc0.dirtyLists[:0], sc0.mergeDiff[:0]
	b.translating = true
	defer func() {
		sc0.defKeys, sc0.defVals = b.defs.keys, b.defs.vals
		sc0.defSpareKeys, sc0.defSpareVals = b.defs.spareKeys, b.defs.spareVals
		clear(b.dirtyLists[:cap(b.dirtyLists)])
		sc0.marks, sc0.phiArgs, sc0.dirtyLists, sc0.mergeDiff = b.marks, b.phiArgs[:0], b.dirtyLists[:0], b.mergeDiff[:0]
	}()
	b.last = b.f.lastStates(blockRoom * n)[:n]
	b.sealed, b.filled = b.f.bools(blockRoom * n)[:n], b.f.bools(blockRoom * n)[:n]
	sc := b.f.scr()
	b.incomplete = idTable(sc.incomplete, blockRoom*n)[:n]
	sc.incomplete = b.incomplete

	// Reverse post-order from the entries, so that a block's forward
	// predecessors are filled before it.
	order := b.f.blockList(n)[:0]
	seen := b.f.bools(n)
	var visit func(*Block)
	visit = func(blk *Block) {
		if seen[blk.ID] {
			return
		}
		seen[blk.ID] = true
		for i := len(blk.Succs) - 1; i >= 0; i-- {
			visit(blk.Succs[i])
		}
		order = append(order, blk)
	}
	for i := len(b.f.Entries) - 1; i >= 0; i-- {
		visit(b.f.Entries[i].Block)
	}
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
	}
	// Blocks no entry reaches are dropped; the others are kept in this
	// order, which every later pass walks (Func.Blocks).
	b.f.Blocks = append(b.f.Blocks[:0], order...)
	for _, blk := range b.f.Blocks {
		preds := blk.Preds[:0]
		backedge := blk.Backedge[:0]
		for i, pred := range blk.Preds {
			if seen[pred.ID] {
				preds = append(preds, pred)
				backedge = append(backedge, blk.Backedge[i])
			}
		}
		blk.Preds, blk.Backedge = preds, backedge
	}

	for _, blk := range order {
		b.trySeal(blk)
		b.enter(b.frameAt(blk))
		b.fill(blk)
		b.enter(b.root)
		b.filled[blk.ID] = true
		for _, s := range blk.Succs {
			b.trySeal(s)
		}
		for _, s := range b.splits {
			b.filled[s.ID] = true
			for _, t := range s.Succs {
				b.trySeal(t)
			}
		}
		b.splits = b.splits[:0]
	}
	for _, blk := range order {
		if !b.sealed[blk.ID] {
			b.seal(blk)
		}
	}
	for i, blk := range b.f.Blocks {
		blk.ID = i
	}
}

func (b *builder) trySeal(blk *Block) {
	if b.sealed[blk.ID] {
		return
	}
	for _, p := range blk.Preds {
		if !b.filled[p.ID] {
			return
		}
	}
	b.seal(blk)
}

// slotPhi is a phi a block made for a slot before it was sealed.
type slotPhi struct {
	slot int
	phi  *Value
}

func (b *builder) seal(blk *Block) {
	// Slot by slot: completing a phi can make others, and their numbers
	// must not vary from one build to the next.
	pending := b.incomplete[blk.ID]
	slices.SortFunc(pending, func(x, y slotPhi) int { return x.slot - y.slot })
	for _, p := range pending {
		b.addPhiOperands(p.slot, p.phi)
	}
	b.incomplete[blk.ID] = nil
	b.sealed[blk.ID] = true
}

// assign is an instruction's write of a slot, which Func.Written records;
// write is also how reads record the phis they make.
func (b *builder) assign(slot int, blk *Block, v *Value) {
	if slot < len(b.f.written) {
		b.f.written[slot] = true
	}
	if b.cur == b.root && b.p.IsCaptured(slot) {
		b.storeCaptured(blk, slot, v)
	}
	b.write(slot, blk, v)
}

func (b *builder) write(slot int, blk *Block, v *Value) {
	b.found(slot, blk, v)
	// For the block's next state to read again (stateSlots).
	ls := &b.last[blk.ID]
	ls.dirty = b.f.appendInt32(ls.dirty, int32(slot))
}

// found is write without the listing: readVar's record of what it found,
// the slot's value there as it was -- a slot a state names, the state's
// read found already, and one it does not the next state's liveness
// names apart (stateSlots).
func (b *builder) found(slot int, blk *Block, v *Value) {
	b.defs.put(blk.ID, slot, v)
}

// read is slot's value in blk: a captured binding marked stale there
// (reloadUpvalues) is loaded from its cell, where the instruction being
// built reads it, and exits, if a guard unboxes it, to that instruction's
// state -- not to the one after the call that made it stale, which the
// instructions between would be done again from. Loaded as each call left
// it, every binding at every call, a function of hundreds of bindings and
// a thousand calls -- RegExp's run blocks -- made hundreds of thousands
// of loads, most dead, and its compile a hundred megabytes.
func (b *builder) read(slot int, blk *Block) *Value {
	v := b.readVar(slot, blk)
	if v.Op != OpUpvalueStale {
		return v
	}
	var st *FrameState
	if blk == b.here && (b.fb == nil || !b.fb.Generic(b.herePC)) {
		st = b.state(blk, b.herePC)
	}
	v = b.loadUpvalue(blk, v.Index, st)
	b.write(slot, blk, v)
	return v
}

// readVar is slot's value in blk, as SSA construction finds it (Braun et
// al.): a captured binding's stale marker as it is.
func (b *builder) readVar(slot int, blk *Block) *Value {
	if v := b.defs.get(blk.ID, slot); v != nil {
		if v.Block == &markBlock {
			// Reached again while its predecessors are read: a loop's,
			// whose phi this is, its arguments those reads find.
			v = b.newPhiID(blk, v.ID)
			b.found(slot, blk, v)
		}
		return v
	}
	if ls := &b.last[blk.ID]; ls.s != nil && slot >= ls.fr.base && slot < ls.fr.base+ls.n {
		// The value the block's last state has, which nothing wrote since
		// (a write is a definition): a state that shares another's slots
		// does not read those that cannot have changed (stateSlots), and
		// its block need not look for them where they were defined.
		if v := ls.s.Slot(slot); v != nil {
			b.found(slot, blk, v)
			return v
		}
	}
	var v *Value
	switch {
	case !b.sealed[blk.ID]:
		v = b.newPhi(blk)
		b.incomplete[blk.ID] = append(b.incomplete[blk.ID], slotPhi{slot, v})
	case len(blk.Preds) == 1:
		v = b.readVar(slot, blk.Preds[0])
	case len(blk.Preds) == 0 && slot < 0:
		// A heap's base or length at an entry, which loads it where it is
		// read (heapRead).
		v = b.heapEntryStale(slot)
	case len(blk.Preds) == 0 && blk.PC < 0 && slot < blk.Header.Len():
		// A local an entry did not load, as the interpreter's liveness has
		// it dead there (fill): the frame's, as an entry loads it.
		v = b.entryLoad(blk, slot)
	case len(blk.Preds) == 0:
		// A slot read where nothing defined it: an operand slot past an
		// entry's depth. The slot IR never reads one; this keeps the form
		// total.
		v = b.constIn(blk, ir.Value{Kind: ir.Undefined})
	default:
		v = b.mergeVar(slot, blk)
	}
	if slot < 0 && v.Op == OpPhi {
		v.Type = Ptr
	}
	b.found(slot, blk, v)
	return v
}

// markBlock is the block of mergeVar's marks, which no value is of.
var markBlock Block

// mergeVar is slot's value in blk, sealed, of several predecessors: the
// value they all have, or a phi of theirs (Braun et al.'s marker
// algorithm). The block is marked while they are read, and a read that
// reaches it again, round a loop, makes its phi then; else the phi is
// made only where they differ. A phi made for every slot a block's first
// state names, at every block of several predecessors, had made a
// function of n values live across m branches' ends take n·m phis, nearly
// all of them one value's, which Optimize took away.
func (b *builder) mergeVar(slot int, blk *Block) *Value {
	// The mark holds the ID the phi is given, if it is made: the one it
	// had when it was made before the reads.
	if b.markDepth == len(b.marks) {
		b.marks = append(b.marks, &Value{})
	}
	mark := b.marks[b.markDepth]
	mark.Block = &markBlock
	b.markDepth++
	defer func() { b.markDepth-- }()
	mark.ID = b.f.nextID
	b.f.nextID++
	b.found(slot, blk, mark)
	at := len(b.phiArgs)
	for _, p := range blk.Preds {
		a := b.readVar(slot, p)
		if a.Op == OpUpvalueStale && slot >= 0 {
			// The binding as the predecessor leaves it, from its cell.
			a = b.reloadStale(p, slot, a)
		}
		b.phiArgs = append(b.phiArgs, a)
	}
	args := b.phiArgs[at:]
	phi := b.defs.get(blk.ID, slot)
	if slot < 0 {
		// A heap's base or length: the one value its predecessors all
		// have, or else loaded here (heapMerge), never a phi.
		v := b.heapMerge(blk, slot, phi, mark, args)
		b.phiArgs = b.phiArgs[:at]
		return v
	}
	if phi == mark {
		phi = nil
		same := true
		for _, a := range args[1:] {
			same = same && a == args[0]
		}
		if same {
			v := args[0]
			b.phiArgs = b.phiArgs[:at]
			return v
		}
		phi = b.newPhiID(blk, mark.ID)
		if slot < 0 {
			phi.Type = Ptr
		}
	}
	phi.Args = b.f.refsOf(len(args))[:0]
	for _, a := range args {
		a.Uses++
		phi.Args = append(phi.Args, a)
	}
	b.phiArgs = b.phiArgs[:at]
	return phi
}

func (b *builder) newPhi(blk *Block) *Value {
	v := b.f.alloc(Value{Op: OpPhi, Type: Tagged, Block: blk})
	prepend(b.f, blk, v)
	return v
}

// newPhiID is newPhi numbered id, an ID taken before (mergeVar).
func (b *builder) newPhiID(blk *Block, id int) *Value {
	v := b.newPhi(blk)
	b.f.nextID--
	v.ID = id
	return v
}

// prepend puts v first in blk's values.
func prepend(f *Func, blk *Block, v *Value) {
	blk.Values = f.appendValue(blk.Values, nil)
	copy(blk.Values[1:], blk.Values)
	blk.Values[0] = v
}

func (b *builder) addPhiOperands(slot int, phi *Value) {
	preds := phi.Block.Preds
	if slot < 0 {
		// A loop header's heap base or length: the one value its
		// predecessors have but the phi, or loaded here (heapMerge).
		at := len(b.phiArgs)
		for _, p := range preds {
			b.phiArgs = append(b.phiArgs, b.readVar(slot, p))
		}
		b.heapMerge(phi.Block, slot, phi, nil, b.phiArgs[at:])
		b.phiArgs = b.phiArgs[:at]
		return
	}
	if phi.Args == nil {
		phi.Args = b.f.refsOf(len(preds))[:0]
	}
	for _, p := range preds {
		a := b.readVar(slot, p)
		if a.Op == OpUpvalueStale {
			// The binding as the predecessor leaves it, from its cell.
			a = b.reloadStale(p, slot, a)
		}
		a.Uses++
		phi.Args = append(phi.Args, a)
	}
}

// constIn makes a tagged constant at the start of a block.
func (b *builder) constIn(blk *Block, c ir.Value) *Value {
	v := b.f.alloc(Value{Op: OpConst, Type: Tagged, Const: c, Block: blk})
	prepend(b.f, blk, v)
	return v
}

// equality is an Eq or Ne as a bool, of any two values: with null or
// undefined (nullish), or as OpEqTagged compares them. It returns nil for
// any other operation.
func (b *builder) equality(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) *Value {
	if in.Operator != ir.Eq && in.Operator != ir.Ne {
		return nil
	}
	if c := b.nullish(blk, in, operand, gd); c != nil {
		return c
	}
	c := gd.guard(OpEqTagged, Bool, ir.HostExit, operand(in.Left), operand(in.Right))
	if in.Strict {
		c.Index = 1
	}
	if in.Operator == ir.Ne {
		c = b.f.newValue(blk, OpNot, Bool, c)
	}
	return c
}

// nullish is an Eq or Ne with null or undefined, as a bool, whatever the
// other operand is: compared natively, rather than taken for a number.
// It returns nil for any other comparison.
func (b *builder) nullish(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, gd *guarder) *Value {
	if in.Operator != ir.Eq && in.Operator != ir.Ne {
		return nil
	}
	isNullish := func(v *Value) bool {
		return v.Op == OpConst && (v.Const.Kind == ir.Null || v.Const.Kind == ir.Undefined)
	}
	x, y := operand(in.Left), operand(in.Right)
	if isNullish(x) {
		x, y = y, x
	}
	if !isNullish(y) {
		return nil
	}
	var c *Value
	if in.Strict {
		c = b.f.newValue(blk, OpStrictNullish, Bool, x)
		if y.Const.Kind == ir.Undefined {
			c.Aux = 1
		}
	} else {
		c = gd.guard(OpLooseNullish, Bool, ir.HostExit, x)
	}
	if in.Operator == ir.Ne {
		c = b.f.newValue(blk, OpNot, Bool, c)
	}
	return c
}

// stateFrame is what a state at a PC of a frame's code names: of the n
// slots from base, those live there (ir.Program's LiveAt) and, for the
// function's own frame (own), not a captured binding's nor a captured
// local's, which are in their cells; and forced, past base, live or not,
// or -1.
type stateFrame struct {
	p       *ir.Program
	base, n int
	own     bool
	forced  int
}

// excluded reports whether no state of the frame names slot i past its
// base.
func (sf *stateFrame) excluded(i int) bool {
	p := sf.p
	if p.Origin != nil && i >= p.Regs {
		// Register code's slots past its registers -- its captured
		// bindings', in their cells, and its expansions' scratch, live
		// only inside one register instruction -- which the register
		// frame has no place for (ir.Program's Regs).
		return true
	}
	return sf.own && (i >= p.UpvalueBase && i < p.UpvalueBase+p.Upvalues || p.IsCaptured(i))
}

// liveEnd is how many of n slots the liveness says which of: those past
// it are live wherever they are.
func (sf *stateFrame) liveEnd(n int) int {
	if sf.p.Live == nil {
		return 0
	}
	return min(n, sf.p.Live.N)
}

// names reports whether a state at pc, of n slots past the base, names
// slot i past it.
func (sf *stateFrame) names(pc, n, i int) bool {
	if i == sf.forced {
		return true
	}
	if i >= n || sf.excluded(i) {
		return false
	}
	if i < sf.liveEnd(n) {
		return sf.p.Live.Has(pc, i)
	}
	return true
}

// slotsAt lists in l, in order, the slots past sf's base that a state at
// pc, of n slots past it, names. Found from the liveness's set there,
// slot by slot: a state's slots are some three times as many as it
// names.
func (sf *stateFrame) slotsAt(l []int32, pc, n int) []int32 {
	liveEnd := sf.liveEnd(n)
	if liveEnd > 0 {
		for i := range sf.p.Live.Slots(pc) {
			if i >= liveEnd {
				break
			}
			if !sf.excluded(i) {
				l = append(l, int32(i))
			}
		}
	}
	for i := liveEnd; i < n; i++ {
		if !sf.excluded(i) {
			l = append(l, int32(i))
		}
	}
	if sf.forced >= 0 {
		if at, found := slices.BinarySearch(l, int32(sf.forced)); !found {
			l = slices.Insert(l, at, int32(sf.forced))
		}
	}
	return l
}

// slotsChanged lists in l, in order, the slots past sf's base whose
// value in a state at pc1, of n1 slots past it, may not be what one at
// pc0, of n0, has: those written since (dirty, by the builder's slot),
// and those one names and the other may not -- the liveness's sets told
// apart, where their trees differ, and the slots between the two's ends.
func (sf *stateFrame) slotsChanged(l []int32, pc0, n0, pc1, n1 int, dirty ...[]int32) []int32 {
	top := max(n0, n1)
	for _, d := range dirty {
		for _, x := range d {
			if i := int(x) - sf.base; i >= 0 && i < top {
				l = append(l, int32(i))
			}
		}
	}
	le0, le1 := sf.liveEnd(n0), sf.liveEnd(n1)
	if lo := min(le0, le1); lo > 0 {
		for i := range sf.p.Live.Changed(pc0, pc1) {
			if i >= lo {
				break
			}
			l = append(l, int32(i))
		}
	}
	for i := min(le0, le1); i < max(le0, le1); i++ {
		l = append(l, int32(i))
	}
	for i := min(n0, n1); i < top; i++ {
		l = append(l, int32(i))
	}
	slices.Sort(l)
	return slices.Compact(l)
}

// lastState is the last state frameState made in a block: of frame fr,
// at pc, of n slots past the frame's base; and dirty, the slots written
// in the block since, or since it began.
type lastState struct {
	s     *FrameState
	fr    *frame
	pc, n int
	dirty []int32
}

// stateSlots gives s, the state at pc in blk of the frame being
// translated, the values of the slots sf names, and below sf's base
// call's: each read, in order, as SSA construction finds it (read). A
// state is made from the last its block made, or, the block's first,
// from the last of its only predecessor: it shares their nodes
// (slots.go), and reads again only the slots written since or named by
// one and not the other. A slot both name that nothing wrote since has
// the value it had, which a read finds again and makes nothing for --
// the last state's read left it in the block, or in the predecessor,
// where the block's read finds it -- so that what the reads make, and in
// which order, is what reading every slot made. A state made while
// another is (read's loads of captured bindings) reads every slot.
func (b *builder) stateSlots(s *FrameState, blk *Block, pc int, sf *stateFrame, call *FrameState) {
	f := b.f
	s.n = sf.base + sf.n
	f.openEdit()
	nested := b.stating
	b.stating = true
	var from *lastState
	dirty := b.dirtyLists[:0]
	if !nested {
		ls := &b.last[blk.ID]
		switch {
		case ls.s != nil:
			if ls.fr == b.cur {
				from, dirty = ls, append(dirty, ls.dirty)
			}
		case b.sealed[blk.ID] && len(blk.Preds) == 1:
			if from, dirty = b.lastOf(blk.Preds[0], dirty); from != nil {
				dirty = append(dirty, ls.dirty)
			}
		case b.sealed[blk.ID]:
			if from, dirty = b.mergeFrom(blk, dirty); from != nil {
				dirty = append(dirty, ls.dirty)
			}
		}
		if from != nil && (from.s.Args != 0 || from.s.Virtual != 0) {
			from = nil
		}
	}
	b.dirtyLists = dirty[:0]
	// The list is the state's own while it reads, as a state made inside
	// lists its own.
	l := b.named[:0]
	b.named = nil
	var pc0, n0 int
	if from != nil {
		s.shareSlots(from.s)
		pc0, n0 = from.pc, from.n
		l = sf.slotsChanged(l, pc0, n0, pc, sf.n, dirty...)
	} else {
		if call != nil {
			s.shareSlots(call)
		}
		l = sf.slotsAt(l, pc, sf.n)
	}
	if !nested {
		// What is written from here on is the next state's to read.
		b.last[blk.ID].dirty = b.last[blk.ID].dirty[:0]
	}
	for _, ix := range l {
		i := int(ix)
		if from != nil && !sf.names(pc, sf.n, i) {
			f.setSlot(s, sf.base+i, nil)
			continue
		}
		v := b.read(sf.base+i, blk)
		if sf.own && b.words != nil && b.words[v] != nil {
			// An operator made on low bits alone (wraps) has no value an
			// exit could write: its guards exit to the state before it.
			panic("ssa: a BigInt's low bits in a frame state")
		}
		f.setSlot(s, sf.base+i, v)
	}
	b.named = l[:0]
	b.stating = nested
	if !nested {
		ls := &b.last[blk.ID]
		ls.s, ls.fr, ls.pc, ls.n = s, b.cur, pc, sf.n
	}
}

// mergeFrom is the state a sealed block of several predecessors makes its
// first from, and the lists of the slots to read again, after dirty: its
// first predecessor's last state (lastOf), if each predecessor has one of
// the frame being translated; and those read again are the slots written
// in any since its last state and those whose values the last states do
// not agree on, found where their trees differ. A slot named alike by
// every one, which none wrote since, has that value in the block, which a
// read would find again without making anything (mergeVar): a function of
// n values live across m blocks of several predecessors had read all n at
// each, n·m reads and as many definitions.
func (b *builder) mergeFrom(blk *Block, dirty [][]int32) (*lastState, [][]int32) {
	first, dirty := b.lastOf(blk.Preds[0], dirty)
	if first == nil {
		return nil, dirty
	}
	diff := b.mergeDiff[:0]
	for _, p := range blk.Preds[1:] {
		var pl *lastState
		if pl, dirty = b.lastOf(p, dirty); pl == nil {
			return nil, dirty
		}
		if pl.s != first.s {
			diffSlots(first.s, pl.s, func(i int) { diff = append(diff, int32(i)) })
		}
	}
	b.mergeDiff = diff
	return first, append(dirty, diff)
}

// lastOf is the last state of the frame being translated that holds at
// blk's end, blk being filled or sealed, and the lists of the slots written
// since, after dirty: blk's own last state, or, for a block that made none
// and has one predecessor, sealed, that predecessor's (a block of a jump
// alone); nil where there is none, or it is a prologue's or a home's.
func (b *builder) lastOf(blk *Block, dirty [][]int32) (*lastState, [][]int32) {
	for {
		ls := &b.last[blk.ID]
		if ls.s != nil {
			if ls.fr != b.cur || ls.s.Args != 0 || ls.s.Virtual != 0 {
				return nil, dirty
			}
			return ls, append(dirty, ls.dirty)
		}
		if !b.sealed[blk.ID] || len(blk.Preds) != 1 {
			return nil, dirty
		}
		dirty = append(dirty, ls.dirty)
		blk = blk.Preds[0]
	}
}

// state captures the frame at a PC: every live slot's current value. Inside
// an inlined callee it is the caller's at the call and the callee's
// (InlineState).
func (b *builder) state(blk *Block, pc int) *FrameState {
	if b.atomic && b.atomicErr == nil {
		b.atomicErr = fmt.Errorf("ssa: register pc %d: an exit after the instruction committed", b.p.Origin[pc])
	}
	if fr := b.cur; fr != b.root && fr.site.Restart {
		// Go makes the call over again (InlineSite's Restart): the
		// caller's state at it, a copy, its slots shared.
		call := fr.call
		s := b.f.newState(FrameState{PC: call.PC, Depth: call.Depth, Site: call.Site, Inline: call.Inline, Args: call.Args})
		s.shareSlots(call)
		s.n = call.n
		return s
	}
	return b.frameState(blk, pc)
}

// frameState is the state at pc in the frame being translated, its
// callers' at their calls below it.
func (b *builder) frameState(blk *Block, pc int) *FrameState {
	depth := b.p.Maps[pc].Depth
	if fr := b.cur; fr != b.root {
		call := fr.call
		s := b.f.newState(FrameState{PC: call.PC, Depth: call.Depth, Site: call.Site})
		// The callee's locals live there, as the caller's below.
		sf := stateFrame{p: b.p, base: fr.base, n: b.p.Locals + depth, forced: -1}
		if fr.site.Window {
			// A register callee's receiver, its window's register 1, live
			// or not, as an inlined frame's state names it (InlineState's
			// ThisSlot): Go makes a construction's
			// result of it where the callee returns no object, and the
			// frame it makes of the level has it as its this, which a
			// stack trace names the frame by -- left out where the callee
			// did not read it, the frame had whatever the window held
			// ("Number.leaf" for "Leaf.leaf").
			sf.forced = 1
		}
		b.stateSlots(s, blk, pc, &sf, call)
		s.Inline = b.f.inlineState(InlineState{Parent: call.Inline, Closure: fr.site.Closure, Callee: fr.site.Callee, Base: fr.base, Locals: b.p.Locals, ThisSlot: fr.site.ThisSlot,
			PC: b.p.Maps[pc].PC, Depth: depth, Site: pc})
		if fr.site.Arguments || fr.site.ReadsArguments {
			s.Inline.Args = uint64(len(fr.args)+1)<<32 | uint64(fr.base-fr.argSlot)
		}
		b.prologueArgs(s, fr, pc)
		return s
	}
	s := b.f.newState(FrameState{PC: b.p.Maps[pc].PC, Depth: depth, Site: pc})
	// Not a local the interpreter writes before it reads from here: not
	// written, nor kept alive for the exit, as V8 leaves dead registers out
	// of a frame state. Nor a captured binding, which is in its cell.
	sf := stateFrame{p: b.p, n: b.p.Locals + depth, own: true, forced: -1}
	b.stateSlots(s, blk, pc, &sf, nil)
	b.prologueArgs(s, b.root, pc)
	return s
}

// prologueArgs adds to s, the state at pc in fr's code, the arguments of
// fr's call past its operands, where pc is inside its parameter list's
// prologue after ParamsDead (ir.Program's PrologueArgs): the interpreter,
// going on there, binds the parameters still in the dead zone from them,
// which a frame Go makes of a native or an inlined callee's has nowhere
// else.
func (b *builder) prologueArgs(s *FrameState, fr *frame, pc int) {
	n, inSlots, ok := fr.p.PrologueAt(pc)
	if !ok || inSlots || len(fr.paramArgs) != n {
		return
	}
	at := s.n
	s.n, s.Args = at+n, n
	for i, v := range fr.paramArgs {
		b.f.setSlot(s, at+i, v)
	}
}

func (b *builder) fill(blk *Block) {
	f := b.f
	if fr := b.cur; fr != b.root && blk == fr.start {
		for s, v := range fr.init {
			b.write(fr.base+s, blk, v)
		}
		return
	}
	if blk.PC < 0 {
		e, _ := f.entryForBlock(blk)
		h := f.newState(FrameState{PC: b.p.Maps[e.PC].PC, Depth: e.Depth, Site: -1})
		h.n = b.p.Locals + e.Depth
		blk.Header = h
		// A local the interpreter writes before it reads from here is not
		// loaded, as frame states leave it out: an entry's state names a
		// slot's own load, which an exit leaves as it is, and the load had
		// been dead but for that. Read anyway, it is loaded then (read).
		// Nor is a captured binding's slot, as frame states leave it out:
		// the binding is in its cell, read there where it is read
		// (reloadUpvalues), and the VM's frame has no slot for it. Loaded
		// at every entry, a function of many bindings and many calls made
		// a load of each at each, that the entry's state alone kept:
		// RegExp's runBlock11, 34,767 of them.
		sf := stateFrame{p: b.p, n: h.n, own: true, forced: -1}
		l := sf.slotsAt(b.named[:0], e.PC, h.n)
		f.openEdit()
		for _, ix := range l {
			i := int(ix)
			v := b.entryLoad(blk, i)
			b.write(i, blk, v)
			f.setSlot(h, i, v)
		}
		b.named = l[:0]
		if b.cur == b.root {
			// The state the block's next is made from (stateSlots).
			b.last[blk.ID] = lastState{s: h, fr: b.root, pc: e.PC, n: h.n, dirty: b.last[blk.ID].dirty[:0]}
		}
		if b.p.Cells && b.cur == b.root {
			// Its captured locals' cells, made at each entry (GoCells), as
			// V8's code allocates a function's context: a call of Go that
			// exits, refused, to the entry's state.
			call := f.newValue(blk, OpCall, Tagged)
			call.Aux, call.State = int(ir.HostExit), blk.Header
			site := &CallSite{Go: GoCells, ThisSlot: -1}
			if fb, ok := b.fb.(CellsFeedback); ok {
				if pool, cells, ok := fb.CellsPool(); ok {
					site.Pool, site.Captures = pool, cells
				}
			}
			f.cache(call).Calls, call.Index = []*CallSite{site}, 0
		}
		// A captured binding is what its cell holds, which native code
		// assigns (ir.UpvalueWrite), with its pointer word there.
		b.reloadUpvalues(blk)
		return
	}
	if blk.LoopHeader {
		blk.Header = b.state(blk, blk.PC)
		blk.Header.Site = -1
	}
	if d, ok := b.stepOn[blk]; ok {
		// The register a register loop's step writes where the loop
		// goes on (iterStep). A captured one is stored to its cell,
		// exiting to the root frame's state here (storeCaptured): the
		// block's own, at its first instruction. The state the last
		// instruction translated left was another block's -- the code
		// past the loop, which is filled before the loop's body -- and
		// named values that do not dominate the body.
		delete(b.stepOn, blk)
		if len(blk.Preds) != 1 {
			panic("ssa: a register loop step's block is reached from elsewhere")
		}
		b.rootSt, b.rootBlk, b.rootPC = nil, blk, blk.PC
		b.assign(d.slot, blk, d.v)
	}
	end := b.endOf[blk.ID]
	if b.cur == b.root && blk.PC == 0 && b.p.Origin != nil && !looped(b.p, 0) {
		// Register code's start, where a call enters it -- a native
		// caller's above all, which no interpreter's call counts -- polls
		// as a back edge does (OpPoll): a recursion that runs natively, no
		// loop in it, is checked for cancellation, its deadline and the
		// memory limit as the interpreter's calls are.
		v := b.f.newValue(blk, OpPoll, None)
		v.Aux, v.State = int(ir.HostExit), b.state(blk, 0)
	}
	// BigInt operators made on their low bits end in their block (wraps).
	b.region, b.words = nil, nil
	for pc := blk.PC; pc <= end; pc++ {
		b.atomicInstruction(blk, pc)
		if b.next != nil {
			// The instruction branched: the rest of the block goes on where
			// both ways meet (splitOn).
			blk, b.next = b.next, nil
		}
	}
	b.region, b.words = nil, nil
}

// splitOn ends blk, the root frame's block being filled, at the
// instruction being built, with a branch on cond, a boolean: true, to
// other, a block for the instruction to fill, which goes to cont; false,
// to cont, which the rest of blk's instructions go on in, and which takes
// blk's end and its successors. The false edge is the one native code
// takes without a jump of its own (mir's branch). Both are sealed: their
// predecessors are all there are. The instructions after go on in cont
// (fill), and the blocks are filled with blk's (translate). So an
// instruction makes a diamond of its own: a megamorphic site's write, its
// native store and, where that finds no property to store to, a call of
// Go, which only the branch that makes it has.
func (b *builder) splitOn(blk *Block, cond *Value) (other, cont *Block) {
	other, _, cont = b.split(blk, cond, false)
	return other, cont
}

// split is splitOn, or, both, splitOn with a block of its own for the
// false edge too, no, which goes to cont: an instruction whose two ways
// each make a value, which cont's phi takes -- a megamorphic site's read,
// the value loaded from its cell where it finds one and Go's otherwise.
func (b *builder) split(blk *Block, cond *Value, both bool) (yes, no, cont *Block) {
	yes = b.blockAfter(blk)
	var room [3]*Block
	made := append(room[:0], yes)
	if both {
		no = b.blockAfter(blk)
		made = append(made, no)
	}
	cont = b.blockAfter(blk)
	made = append(made, cont)
	b.placeAfter(blk, made, cont)
	blk.Kind, blk.Control, blk.State, blk.ExitKind = BlockIf, cond, nil, 0
	cond.Uses++
	f := b.f
	yes.Kind, yes.Succs, yes.Preds, yes.Backedge = BlockPlain, f.blocksOf(cont), f.blocksOf(blk), f.bools(1)
	if both {
		no.Kind, no.Succs, no.Preds, no.Backedge = BlockPlain, f.blocksOf(cont), f.blocksOf(blk), f.bools(1)
		blk.Succs = f.blocksOf(yes, no)
		cont.Preds, cont.Backedge = f.blocksOf(yes, no), f.bools(2)
	} else {
		blk.Succs = f.blocksOf(yes, cont)
		cont.Preds, cont.Backedge = f.blocksOf(blk, yes), f.bools(2)
	}
	for _, x := range made {
		b.seal(x)
	}
	b.splits = append(b.splits, made...)
	b.next = cont
	return yes, no, cont
}

// blockRoom is how many times the blocks of a program's plan its block
// tables have room for: split and loop add them as the translation goes,
// and each table copied at each would be most of what they cost.
const blockRoom = 2

// blockAfter is a new block of blk's PC and frame, for split and loop.
func (b *builder) blockAfter(blk *Block) *Block {
	nb := b.f.newBlock(blk.PC)
	nb.ID = b.nextBlock
	b.nextBlock++
	// The block's ID past every list's, which each grows to.
	for len(b.last) <= nb.ID {
		b.last = append(b.last, lastState{})
	}
	for len(b.sealed) <= nb.ID {
		b.sealed = append(b.sealed, false)
	}
	for len(b.filled) <= nb.ID {
		b.filled = append(b.filled, false)
	}
	for len(b.incomplete) <= nb.ID {
		b.incomplete = append(b.incomplete, nil)
	}
	for len(b.endOf) <= nb.ID {
		b.endOf = append(b.endOf, 0)
	}
	for len(b.frameOf) <= nb.ID {
		b.frameOf = append(b.frameOf, nil)
	}
	if blk.ID < len(b.frameOf) {
		b.frameOf[nb.ID] = b.frameOf[blk.ID]
	}
	return nb
}

// placeAfter puts made, blocks blockAfter made, right after blk, in the
// order the blocks are filled and kept in -- a block after its dominator,
// the last after the others -- and has cont, the last, end as blk did:
// the rest of blk's instructions are cont's (b.next).
func (b *builder) placeAfter(blk *Block, made []*Block, cont *Block) {
	f := b.f
	// The new blocks are the list's last: they move to just after blk.
	n := len(f.Blocks) - len(made)
	at := 0
	for i, x := range f.Blocks[:n] {
		if x == blk {
			at = i + 1
			break
		}
	}
	copy(f.Blocks[at+len(made):], f.Blocks[at:n])
	copy(f.Blocks[at:], made)
	cont.Kind, cont.Control, cont.Succs, cont.State, cont.ExitKind = blk.Kind, blk.Control, blk.Succs, blk.State, blk.ExitKind
	cont.Yield, cont.YieldSlot, blk.Yield = blk.Yield, blk.YieldSlot, false
	for _, s := range cont.Succs {
		for i, p := range s.Preds {
			if p == blk {
				s.Preds[i] = cont
			}
		}
	}
	b.endOf[cont.ID] = b.endOf[blk.ID]
}

// loop ends blk, the root frame's block being filled, at a loop of blocks
// of its own, as an instruction that loops -- a forEach made natively --
// makes it: blk goes to head, which goes to body or, the loop done, to
// cont; body goes back to head, its back edge, where native code polls.
// cont ends as blk did, and the rest of blk's instructions are cont's.
// The caller fills head, gives it its Control and Header, fills body, and
// then seals head (body and cont are sealed: their one predecessor is
// known).
func (b *builder) loop(blk *Block) (head, body, cont *Block) {
	head, body, cont = b.blockAfter(blk), b.blockAfter(blk), b.blockAfter(blk)
	made := []*Block{head, body, cont}
	b.placeAfter(blk, made, cont)
	blk.Kind, blk.Control, blk.State, blk.ExitKind = BlockPlain, nil, nil, 0
	blk.Succs = []*Block{head}
	head.Kind, head.Succs = BlockIf, []*Block{body, cont}
	head.Preds, head.Backedge, head.LoopHeader = []*Block{blk, body}, []bool{false, true}, true
	body.Kind, body.Succs, body.Preds, body.Backedge = BlockPlain, []*Block{head}, []*Block{head}, []bool{false}
	cont.Preds, cont.Backedge = []*Block{head}, []bool{false}
	b.seal(body)
	b.seal(cont)
	b.splits = append(b.splits, made...)
	b.next = cont
	return head, body, cont
}

// loopTest ends body, a loop's (loop), at test, a value in it: true goes
// to yes, a block of its own, false to step, which ends as body did, going
// back to the loop's head; yes goes to step too or, exits, to cont, the
// loop's end -- a some's callback that found what it was asked to. step is
// placed after body, then yes; the caller fills them.
func (b *builder) loopTest(body, cont *Block, test *Value, exits bool) (yes, step *Block) {
	step, yes = b.blockAfter(body), b.blockAfter(body)
	made := []*Block{step, yes}
	b.placeAfter(body, made, step)
	body.Kind, body.Control, body.State, body.ExitKind = BlockIf, test, nil, 0
	test.Uses++
	body.Succs = []*Block{yes, step}
	yes.Kind, yes.Preds, yes.Backedge = BlockPlain, []*Block{body}, []bool{false}
	if exits {
		yes.Succs = []*Block{cont}
		cont.Preds, cont.Backedge = append(cont.Preds, yes), append(cont.Backedge, false)
		step.Preds, step.Backedge = []*Block{body}, []bool{false}
	} else {
		yes.Succs = []*Block{step}
		step.Preds, step.Backedge = []*Block{body, yes}, []bool{false, false}
	}
	b.seal(yes)
	b.seal(step)
	b.splits = append(b.splits, made...)
	return yes, step
}

func (f *Func) entryForBlock(blk *Block) (Entry, bool) {
	for _, e := range f.Entries {
		if e.Block == blk {
			return e, true
		}
	}
	return Entry{}, false
}

// atomicInstruction is instruction, keeping the atomicity rule of register
// code's program (ir.Program's Origin): an exit at one of a register
// instruction's IR instructions runs the whole register instruction again,
// so none may be made once one of them has committed. ir.Validate proves
// the IR keeps it -- those after a commit are Generic -- but how an
// instruction is built decides what it commits and whether it exits
// (commitsBuilt), and a build that makes an exit after a commit is
// refused (atomicErr). The instructions of an origin are reached only one
// from the one before it, so what came before is theirs alone.
func (b *builder) atomicInstruction(blk *Block, pc int) {
	p := b.p
	if p.Origin == nil {
		b.instruction(blk, pc)
		return
	}
	b.atomic = false
	for q := pc - 1; q >= 0 && p.Origin[q] == p.Origin[pc]; q-- {
		b.atomic = b.atomic || b.commitsBuilt(q)
	}
	b.instruction(blk, pc)
	b.atomic = false
}

// originGoesOn reports whether the instruction at pc is followed, within
// its register instruction's (ir.Program's Origin), by one that may leave
// -- add_prop's read, which its addition and its write follow. Such a read
// is not built to call Go at a megamorphic site's miss, which would commit
// it (commitsBuilt), the addition after it then generic, Go's, which may
// leave, and the program refused for an exit after a commit (atomicErr):
// it leaves for Go at the miss instead, before anything committed, as a
// site Go's calls may not serve does. Copies, which never leave, are no
// such instruction: get_method_w's read, which the method's and the
// receiver's copies into the call's window follow, calls Go at its miss
// as a get_prop's does.
func (b *builder) originGoesOn(pc int) bool {
	o := b.p.Origin
	if o == nil {
		return false
	}
	for q := pc + 1; q < len(o) && o[q] == o[pc]; q++ {
		if b.p.Code[q].Op != ir.Copy {
			return true
		}
	}
	return false
}

// readCalled notes that the read at pc of the frame being translated, of
// register code, is built to call (frame's readCalls).
func (b *builder) readCalled(pc int) {
	if b.p.Origin != nil {
		b.cur.readCalls = append(b.cur.readCalls, int32(pc))
	}
}

// commitsBuilt reports whether register code's instruction pc commits as
// the builder makes it (ir.Program's CommitsAt): a property's read only
// where it was built to call -- an accessor the feedback gives, or Go for
// a megamorphic site's miss (readCalls), which may run a getter; an
// arithmetic operation, a comparison or a string's addition never, Generic
// or not, as Go makes what it does not of them only where that runs
// nothing (GoAdd, GoCompare, GoNumber), leaving the rest to an exit.
func (b *builder) commitsBuilt(pc int) bool {
	in := &b.p.Code[pc]
	switch in.Op {
	case ir.PropertyRead, ir.ReferenceRead:
		if slices.Contains(b.cur.readCalls, int32(pc)) {
			return true
		}
	case ir.Binary, ir.Unary, ir.Update, ir.Branch, ir.StringConcat, ir.StringPart:
	case ir.StringAdd:
		if in.Owned {
			return true
		}
	default:
		return b.p.CommitsAt(pc)
	}
	a, c := in.Writes()
	return a >= 0 && a < b.p.Regs || c >= 0 && c < b.p.Regs
}

// A guarder makes an instruction's guards: values that exit as their
// kinds say, in the instruction's frame state (state), in the block the
// instruction's code is at. A method of a type, where it had been a
// closure the instruction gave its helpers, so that a guard's arguments
// stay on the stack: a variadic call through a func value puts them on
// the heap, some ten thousand lists of one or two values a run of
// Typescript's compiles. Its state is its own for the same reason: the
// instruction's closure, kept in it, put the instruction's block and
// state on the heap.
type guarder struct {
	b *builder
	// blk is the instruction's block, which the instruction sets again
	// as its code goes on in another; pc is the instruction's, and root
	// whether it is the root frame's.
	blk  *Block
	pc   int
	root bool
	// st is an inlined callee's instruction's state, made once asked for.
	st *FrameState
}

// state is the instruction's frame state: the root frame's, the builder's,
// which an assignment to a captured local shares (storeCaptured); an
// inlined callee's, this call's.
func (g *guarder) state() *FrameState {
	b := g.b
	if g.root && b.region != nil {
		// Inside BigInt operators made on their low bits (wraps): the
		// state before the first of them, which nothing since has changed
		// but those operators, which Go makes over again.
		return b.region
	}
	if g.root {
		return b.rootState()
	}
	if g.st == nil {
		g.st = b.state(g.blk, g.pc)
	}
	return g.st
}

// guard is a value of op, of type t, of args, that exits as kind -- or to
// Go, inside an inlined callee, where Go does the operation in the
// callee's frame and native code goes on where it can.
func (g *guarder) guard(op Op, t Type, kind ir.ExitKind, args ...*Value) *Value {
	b := g.b
	if b.cur != b.root {
		kind = ir.HostExit
	}
	v := b.f.newValue(g.blk, op, t, args...)
	v.Aux = int(kind)
	v.State = g.state()
	v.State.addUse()
	return v
}

func (b *builder) instruction(blk *Block, pc int) {
	f, in := b.f, b.p.Code[pc]
	if b.cur != b.root {
		in = shifted(in, b.cur.base)
	}
	// The root frame's instruction's state is the builder's, which an
	// assignment to a captured local shares (storeCaptured); an inlined
	// callee's, this call's.
	root := b.cur == b.root
	if root {
		b.rootSt, b.rootBlk, b.rootPC = nil, blk, pc
	}
	gd := &guarder{b: b, blk: blk, pc: pc, root: root}
	state := gd.state
	b.here, b.herePC = blk, pc
	operand := func(o ir.Operand) *Value {
		if o.Slot < 0 {
			v := f.newValue(blk, OpConst, Tagged)
			v.Const = o.Literal
			return v
		}
		return b.read(o.Slot, blk)
	}
	number := func(o ir.Operand, kind ir.ExitKind) *Value {
		if o.Slot < 0 && o.Literal.Kind == ir.Number {
			v := f.newValue(blk, OpConstF64, Float64)
			v.Const = o.Literal
			return v
		}
		return gd.guard(OpUnboxF64, Float64, kind, operand(o))
	}
	// numberOf is number, but for an operand that a comparison or a Not
	// wrote (ir.Instruction's Bools), a boolean: its number.
	numberOf := func(o ir.Operand, kind ir.ExitKind, boolean bool) *Value {
		if boolean {
			if v := operand(o); v.Op == OpBoxBool {
				return f.newValue(blk, OpBoolToF64, Float64, v.Args[0])
			}
		}
		return number(o, kind)
	}
	// numeric is numberOf for an operator not equality's, generic once its
	// guard failed: an oddball converted natively (OpOddballF64), anything
	// else Go's at a host exit.
	numeric := func(o ir.Operand, generic, boolean bool) *Value {
		if !generic {
			return numberOf(o, ir.GuardExit, boolean)
		}
		if v := operand(o); boolean && v.Op == OpBoxBool {
			return f.newValue(blk, OpBoolToF64, Float64, v.Args[0])
		}
		if o.Slot < 0 && o.Literal.Kind == ir.Number {
			return number(o, ir.HostExit)
		}
		v := gd.guard(OpOddballF64, Float64, ir.HostExit, operand(o))
		if b.goCalls() {
			// A string's number by Go (GoNumber): an arithmetic operator's
			// operand, as a relational one is compared by Go here.
			v.Index = 1
		}
		return v
	}
	boxF := func(x *Value) *Value { return f.newValue(blk, OpBoxF64, Tagged, x) }
	boxB := func(x *Value) *Value { return f.newValue(blk, OpBoxBool, Tagged, x) }
	one := func() *Value {
		v := f.newValue(blk, OpConstF64, Float64)
		v.Const = ir.Float(1)
		return v
	}

	if in.Check {
		gd.guard(OpCheckInit, None, b.thisCheckKind(in.CheckSlot), b.read(in.CheckSlot, blk))
	}
	if in.Raw != ir.RawNone {
		// Typed code's (raw.go).
		b.rawInstruction(blk, pc, in, operand, number, gd)
		return
	}
	if b.unseen(pc) {
		// A site whose operands met nothing, as V8's code deoptimizes
		// softly for insufficient feedback: Go makes the instruction at a
		// host exit, which invalidates nothing, and the lattice learns
		// what it met there (OperandFeedback).
		blk.ExitKind = ir.HostExit
		blk.State = state()
		blk.State.addUse()
		return
	}
	switch in.Op {
	case ir.Nop, ir.Jump:
	case ir.Copy:
		b.assign(in.Dest, blk, operand(in.Left))
	case ir.CopyPair:
		l, r := operand(in.Left), operand(in.Right)
		b.assign(in.Dest, blk, l)
		b.assign(in.Extra, blk, r)
	case ir.StoreLoad:
		b.assign(in.Dest, blk, operand(in.Left))
		b.assign(in.Extra, blk, operand(in.Right))
	case ir.Swap:
		d, e := b.read(in.Dest, blk), b.read(in.Extra, blk)
		b.assign(in.Dest, blk, e)
		b.assign(in.Extra, blk, d)
	case ir.Insert3:
		n := in.Dest
		x, y, z := b.read(n, blk), b.read(n+1, blk), b.read(n+2, blk)
		b.assign(n, blk, z)
		b.assign(n+1, blk, x)
		b.assign(n+2, blk, y)
		b.assign(n+3, blk, z)
	case ir.Insert2:
		n := in.Dest
		x, y := b.read(n, blk), b.read(n+1, blk)
		b.assign(n, blk, y)
		b.assign(n+1, blk, x)
		b.assign(n+2, blk, y)
	case ir.Binary:
		if c := b.equality(blk, in, operand, gd); c != nil {
			b.assign(in.Dest, blk, boxB(c))
			break
		}
		if b.wraps(pc) {
			b.wrapOp(blk, pc, in, gd)
			break
		}
		if v := b.bigNative(blk, pc, in, operand, gd); v != nil {
			if v.Type == Bool {
				v = boxB(v)
			}
			b.assign(in.Dest, blk, v)
			break
		}
		if b.goArithAt(pc, in, operand) {
			b.assign(in.Dest, blk, b.goArith(blk, in, operand, gd))
			break
		}
		if in.Operator == ir.Add && b.generic(pc) && b.goCalls() {
			// An addition whose operands were not numbers: Go makes it, as
			// a string's (ir.StringAdd).
			b.goAdd(blk, in, operand, gd)
			break
		}
		if relational(in.Operator) && b.generic(pc) && b.goCalls() {
			b.assign(in.Dest, blk, b.goCompare(blk, in, operand, gd))
			break
		}
		kind := ir.GuardExit
		if in.Operator == ir.Eq || in.Operator == ir.Ne || in.Operator == ir.Mod || b.generic(pc) {
			kind = ir.HostExit
		}
		var x, y *Value
		if in.Operator != ir.Eq && in.Operator != ir.Ne && (b.generic(pc) || in.Operator == ir.Mod && b.goCalls()) {
			// A remainder's operands too, which leave for Go, not
			// deoptimize, from the first: one that is no number, a
			// string's, had left at every remainder, learning nothing.
			x, y = numeric(in.Left, true, in.Bools&1 != 0), numeric(in.Right, true, in.Bools&2 != 0)
		} else {
			x, y = numberOf(in.Left, kind, in.Bools&1 != 0), numberOf(in.Right, kind, in.Bools&2 != 0)
		}
		if in.Operator == ir.Mod {
			b.assign(in.Dest, blk, boxF(gd.guard(OpModF64, Float64, ir.HostExit, x, y)))
			break
		}
		if in.Operator == ir.Pow {
			// As Math.pow's, where exact, and Go's at an exit otherwise.
			b.assign(in.Dest, blk, boxF(gd.guard(OpPowF64, Float64, ir.HostExit, x, y)))
			break
		}
		b.assign(in.Dest, blk, b.binary(blk, in.Operator, x, y, boxF, boxB))
	case ir.Unary:
		if in.Operator == ir.Not {
			t := gd.guard(OpTruth, Bool, ir.GuardExit, operand(in.Left))
			b.assign(in.Dest, blk, boxB(f.newValue(blk, OpNot, Bool, t)))
			break
		}
		if v := b.bigUnary(blk, pc, in, operand, gd); v != nil {
			b.assign(in.Dest, blk, v)
			break
		}
		// An operand not a number, once its guard has failed, is converted
		// natively, an oddball, or by Go at a host exit: guarded again, the
		// code had failed there at every compile -- s[i] | 0 of a string's
		// character, o.x | 0 of an undefined property.
		x := numeric(in.Left, b.generic(pc), in.Bools&1 != 0)
		switch in.Operator {
		case ir.Neg:
			x = f.newValue(blk, OpNegF64, Float64, x)
		case ir.Int32:
			x = f.newValue(blk, OpI32ToF64, Float64, f.newValue(blk, OpToInt32, Int32, x))
		case ir.BitNot:
			x = f.newValue(blk, OpI32ToF64, Float64, f.newValue(blk, OpNotI32, Int32, f.newValue(blk, OpToInt32, Int32, x)))
		}
		b.assign(in.Dest, blk, boxF(x))
	case ir.Update:
		old := operand(in.Left)
		generic := b.generic(pc)
		x := numeric(in.Left, generic, false)
		if generic {
			// A postfix's value is the old one's number: an oddball's too.
			old = boxF(x)
		}
		op := OpAddF64
		if in.Operator == ir.Sub {
			op = OpSubF64
		}
		v := boxF(f.newValue(blk, op, Float64, x, one()))
		b.assign(in.Dest, blk, v)
		if in.Extra >= 0 {
			if in.Postfix {
				v = old
			}
			b.assign(in.Extra, blk, v)
		}
	case ir.Branch:
		var c *Value
		if in.Operator == ir.Truth {
			c = gd.guard(OpTruth, Bool, ir.GuardExit, operand(in.Left))
		} else if in.Operator == ir.Nullish {
			// ?? and ?.: null or undefined, no conversion.
			c = f.newValue(blk, OpStrictNullish, Bool, operand(in.Left))
			c.Aux = 2
		} else if n := b.equality(blk, in, operand, gd); n != nil {
			c = n
		} else if v := b.relationalBig(blk, pc, in, operand, gd); v != nil {
			c = v
		} else if relational(in.Operator) && b.generic(pc) && b.goCalls() {
			// Go's comparison's boolean, which native code branches on.
			c = gd.guard(OpTruth, Bool, ir.GuardExit, b.goCompare(blk, in, operand, gd))
		} else {
			kind := ir.GuardExit
			if in.Operator == ir.Eq || in.Operator == ir.Ne || b.generic(pc) {
				kind = ir.HostExit
			}
			var x, y *Value
			if in.Operator != ir.Eq && in.Operator != ir.Ne && b.generic(pc) {
				x, y = numeric(in.Left, true, false), numeric(in.Right, true, false)
			} else {
				x, y = number(in.Left, kind), number(in.Right, kind)
			}
			c = f.newValue(blk, OpCmpF64, Bool, x, y)
			c.Aux = int(in.Operator)
		}
		blk.Control = c
		c.Uses++
	case ir.Return:
		v := operand(in.Left)
		gd.guard(OpCheckInit, None, ir.GuardExit, v)
		if b.cur != b.root {
			// The call's result, and on after it: a construction's, its
			// receiver.
			if b.cur.receiver != nil {
				v = b.cur.receiver
			}
			if b.cur.result >= 0 {
				b.assign(b.cur.result, blk, v)
				if b.cur.parent == b.root && b.root.p.IsCaptured(b.cur.result) {
					// Register code's call whose result is a register a
					// closure captures, which its cell holds: stored there
					// too, as a call's own result is (assign), its exit the
					// call's state -- no cell's write leaves native code.
					b.storeCapturedAt(blk, b.cur.result, v, b.cur.call)
				}
			}
			break
		}
		if b.p.Captured != nil && !b.p.Cells {
			// What closures captured of its locals closed before it
			// returns, as the interpreter's frame's are as it is popped:
			// a native callee's frame is gone once it returns (GoClose,
			// which native code makes only while any is open). Always:
			// with the keep cells all taken, a callee that had made a
			// closure before returned with it open on its dead frame.
			b.goFrame(blk, pc, ir.Instruction{Op: ir.CloseUpvalues}, gd)
		}
		blk.Control = v
		v.Uses++
	case ir.ArrayRead, ir.ArrayUpdate:
		if in.Op == ir.ArrayRead && in.Key != 0 && b.p.Kinds != nil {
			// Typed code's function table's element, at the index raw slot
			// Key-1 holds (compile's LowerTyped): what a call of it finds its
			// function by (tableRead).
			defer b.tableRead(blk, in)
		}
		// The element's key, and an update's updated key: the key's own
		// value before or after (Postfix).
		keyOf := func(kind ir.ExitKind) (key, updated *Value) {
			if in.Op != ir.ArrayUpdate {
				return number(in.Right, kind), nil
			}
			old := gd.guard(OpUnboxF64, Float64, kind, operand(in.Right))
			op := OpAddF64
			if in.Operator == ir.Sub {
				op = OpSubF64
			}
			updated = f.newValue(blk, op, Float64, old, one())
			if in.Postfix {
				return old, updated
			}
			return updated, updated
		}
		if in.Reference && in.Op == ir.ArrayRead && b.stringReceiver(pc) {
			// A string's element, a string of one ASCII character, as the
			// runtime has it; Go reads any other's.
			key, _ := keyOf(ir.HostExit)
			cell := gd.guard(OpStringChar, Source, ir.HostExit, operand(in.Left), key)
			v := f.newValue(blk, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			b.assign(in.Dest, blk, v)
			break
		}
		if t, ok := b.typedBigRead(pc); ok && in.Reference && in.Op == ir.ArrayRead {
			// A BigInt64Array's or a BigUint64Array's element, a BigInt
			// held in its value; Go reads any other's.
			array := gd.guard(OpIndexedOf, Ptr, ir.HostExit, operand(in.Left))
			key, _ := keyOf(ir.HostExit)
			b.assign(in.Dest, blk, b.bigBoxed(blk, gd.guard(OpBigElemRead, t, ir.HostExit, array, key)))
			break
		}
		if in.Reference && in.Op == ir.ArrayRead && b.typedReceiver(pc, operand(in.Left)) {
			// A typed array's number; Go reads any other's.
			array := gd.guard(OpIndexedOf, Ptr, ir.HostExit, operand(in.Left))
			key, _ := keyOf(ir.HostExit)
			elem := gd.guard(OpElemRead, Float64, ir.HostExit, array, key)
			b.typedSite(pc, elem)
			b.assign(in.Dest, blk, boxF(elem))
			break
		}
		if in.Reference && in.Op == ir.ArrayRead && b.keyedRead(pc) {
			// Any receiver's, by Go from native code; what Go does not read
			// leaves, and native code goes on after it.
			b.goIndex(blk, in, operand, gd)
			break
		}
		if in.Reference {
			// Whatever the element holds, by its cell; Go reads the rest,
			// and makes an update's of its key. An update's key, a[i++],
			// is written once the read has succeeded, before the element.
			array := gd.guard(OpArrayOf, Ptr, ir.HostExit, operand(in.Left))
			key, updated := keyOf(ir.HostExit)
			cell := gd.guard(OpElemCell, Source, ir.HostExit, array, key)
			v := f.newValue(blk, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			if updated != nil {
				b.assign(in.Extra, blk, boxF(updated))
			}
			b.assign(in.Dest, blk, v)
			break
		}
		// Every check exits to the state before the instruction, so their
		// order is free; the key's update is written only once the read
		// has succeeded, before the element, which wins if both name one
		// slot.
		array := gd.guard(OpIndexedOf, Ptr, ir.GuardExit, operand(in.Left))
		key, updated := keyOf(ir.GuardExit)
		elem := gd.guard(OpElemRead, Float64, ir.GuardExit, array, key)
		b.typedSite(pc, elem)
		if updated != nil {
			b.assign(in.Extra, blk, boxF(updated))
		}
		b.assign(in.Dest, blk, boxF(elem))
	case ir.ArrayLength:
		// An array's, a typed array's or a string's; another receiver's, or
		// a typed array native code does not find in its buffer, leaves.
		// Once that has failed it is Go's, which goes on after it: there
		// is no more generic form, and the guard had failed at every
		// invocation, the rest of each run by the interpreter -- PDF.js's
		// inflater, its tables' lengths.
		kind := ir.GuardExit
		if b.fb != nil && b.fb.Generic(pc) {
			kind = ir.HostExit
			if b.p.Origin != nil && b.whole(blk, pc, in, gd) {
				// Register code's: get_length run whole on the register
				// frame (CallSite's Whole), where a slot program's GoHost
				// below runs it on the frame's operands -- an arguments
				// object's, EarleyBoyer's sc_list's, had left native code
				// at every call.
				break
			}
			if b.cur == b.root && b.goCalls() && b.p.Origin == nil {
				// Go reads it from native code, on the frame (GoHost), as
				// the interpreter does: an arguments object's, EarleyBoyer's
				// sc_list's, had left native code at every call.
				call := gd.guard(OpCall, Tagged, ir.HostExit, operand(in.Left))
				b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoHost, Argc: 1, ThisSlot: -1}}, b.f.Keeps
				b.f.Keeps++
				b.f.HostsLeft = append(b.f.HostsLeft, pc)
				cell := f.newValue(blk, OpCallCell, Source, call)
				r := f.newValue(blk, OpKept, Tagged, cell, call)
				r.Shadow = cell
				b.assign(in.Dest, blk, r)
				if b.hasCells() {
					b.reloadAfterCall(blk)
				}
				break
			}
		}
		b.assign(in.Dest, blk, boxF(gd.guard(OpLength, Float64, kind, operand(in.Left))))
	case ir.ArrayKey:
		gd.guard(OpArrayOf, Ptr, ir.GuardExit, operand(in.Left))
		gd.guard(OpElemKey, None, ir.GuardExit, number(in.Right, ir.GuardExit))
	case ir.ArrayWrite:
		if site, ok := b.keyedStore(pc); ok && !site.Define {
			blk = b.keyedStores(blk, site, operand(in.Left), operand(in.Right), operand(in.Third), b.fb.Generic(pc), state)
			gd.blk = blk
		}
		if b.keyedWrite(pc) {
			// Any receiver's, by Go from native code; what Go does not
			// store leaves, and native code goes on after it.
			b.goKeyedWrite(blk, in, operand, gd)
			break
		}
		// Any value, by its words; a number's, once it is known to be one,
		// as a number (simplify). Go stores what native code does not.
		array := gd.guard(OpIndexedOf, Ptr, ir.HostExit, operand(in.Left))
		key := number(in.Right, ir.HostExit)
		if o := in.Third; o.Slot < 0 && o.Literal.Kind == ir.Number {
			b.typedSite(pc, gd.guard(OpElemWrite, None, ir.HostExit, array, key, number(o, ir.HostExit)))
			break
		}
		if unsigned, ok := b.typedBigWrite(pc); ok {
			// A site that has met BigInt64Arrays alone, or BigUint64Arrays
			// alone: the value's low 64 bits, a BigInt held in its value
			// (OpBigLow), stored as its type's word, as V8's keyed store
			// stores a BigInt64Array's element as a machine word; anything
			// else Go stores after the exit.
			w := gd.guard(OpBigElemWrite, None, ir.HostExit, array, key, gd.guard(OpBigLow, Uint64, ir.HostExit, operand(in.Third)))
			if unsigned {
				w.Index = 1
			}
			break
		}
		b.typedSite(pc, gd.guard(OpElemStore, None, ir.HostExit, array, key, operand(in.Third)))
	case ir.StringMethod:
		cell := gd.guard(OpStringMethod, Source, ir.HostExit, operand(in.Left))
		v := f.newValue(blk, OpLoadCell, Tagged, cell)
		v.Shadow = cell
		b.assign(in.Dest, blk, v)
	case ir.StringCode:
		code := gd.guard(OpStringCode, Float64, ir.HostExit, operand(in.Left), operand(in.Right), number(in.Third, ir.HostExit))
		b.assign(in.Dest, blk, boxF(code))
	case ir.BindingRead:
		site, ok := b.global(pc)
		if !ok {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		kind := ir.HostExit
		if site.Fixed {
			kind = ir.GuardExit
		}
		cell := b.globalCell(site, gd, kind)
		if site.Fixed {
			k := f.newValue(blk, OpConst, Tagged)
			k.Const = site.Constant
			b.assign(in.Dest, blk, k)
			break
		}
		v := f.newValue(blk, OpLoadCell, Tagged, cell)
		v.Shadow = cell
		if site.Typed {
			if b.typedArrays == nil {
				b.typedArrays = map[*Value]uint8{}
			}
			b.typedArrays[v] = site.TypedKind
			if site.TypedKind == 0 {
				b.typedArrays[v] = typedAny
			}
		}
		b.assign(in.Dest, blk, v)
	case ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead:
		if a, ok := b.accessor(pc); ok {
			b.readCalled(pc)
			b.accessorOp(blk, pc, in, a, operand, state)
			break
		}
		if s, ok := b.stringProperty(pc); ok {
			// A string's method, read where String.prototype has it, as
			// V8 reads one off the String map's prototype; or another
			// primitive's -- a BigInt's, a number's, a boolean's -- where
			// the prototype the realm fixed for it, the site's holder, has
			// it, as V8 reads a number's off the HeapNumber map's.
			//
			// A site that has met objects too, or primitives of several
			// kinds: each primitive's kind is a case beside the others',
			// told by the receiver's word (OpTypeIs, which exits for none
			// of these kinds), as a V8 IC keeps the HeapNumber map or the
			// String map among the objects' maps. Each kind's read is made
			// on a branch of its own, the objects' on the last, as the
			// site's object cases have it; all go on where they meet. A
			// read of the last kind with no objects beside it checks the
			// kind itself, any other receiver leaving for Go. Each
			// receiver of a case the site did not compile had left native
			// code, which taught the code nothing.
			site, fed := b.property(pc)
			objects := fed && !in.Private && !unfedSite(site)
			var cont *Block
			for i := range len(s.PrimitiveCases) + 1 {
				k := s
				if i > 0 {
					k = s.PrimitiveCases[i-1]
				}
				var no *Block
				if i < len(s.PrimitiveCases) || objects {
					is := f.newValue(blk, OpTypeIs, Bool, operand(in.Left))
					is.Index, is.Aux, is.State = primitiveType(k.Primitive), int(ir.HostExit), state()
					is.State.addUse()
					var yes, c *Block
					yes, no, c = b.split(blk, is, true)
					if cont == nil {
						cont = c
					}
					blk = yes
					gd.blk = blk
				}
				cell := gd.guard(OpStringProp, Source, ir.HostExit, operand(in.Left))
				cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(k.Shape)}, int(k.Index), k.Key
				if k.Primitive != PrimitiveString || k.Holders[0].Object != 0 {
					c := f.cache(cell)
					c.Holders, c.Primitive = b.f.holderChain(HolderChain{Holders: k.Holders, More: k.More}), k.Primitive
				}
				v := f.newValue(blk, OpLoadCell, Tagged, cell)
				v.Shadow = cell
				b.assign(in.Dest, blk, v)
				if no != nil {
					blk = no
					gd.blk = blk
				}
			}
			if cont != nil {
				// The rest goes on where the ways meet, past any the
				// objects' read makes of its own.
				defer func() { b.next = cont }()
			}
			if !objects {
				break
			}
		}
		site, ok := b.property(pc)
		if !ok {
			if in.Private && b.whole(blk, pc, in, gd) {
				// Register code's private member Go reads or writes from
				// native code, which goes on after it: a site whose cache
				// holds no own field of its key.
				break
			}
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		if !in.Private && unfedSite(site) {
			// A site that knows no shape deoptimizes, as V8's code does for
			// insufficient feedback: the interpreter makes the operation,
			// its cache learning the shape (unfed).
			blk.ExitKind = ir.GuardExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		// A receiver none of the site's cases has deoptimizes, as V8's map
		// checks do, but at a megamorphic site, which leaves for Go only
		// what neither its receiver's own table nor the megamorphic cache
		// has (PropertySite's Megamorphic); and a private member's, whose
		// shapes Go learns at its exits (the VM's jitRegPrivateSeen).
		kind := ir.GuardExit
		if site.Megamorphic || in.Private {
			kind = ir.HostExit
		}
		if in.Private {
			// The key this evaluation of the class minted, the one the
			// site's shapes have.
			k := gd.guard(OpSameSymbol, None, ir.HostExit, operand(in.Third))
			k.Const = ir.Value{Bits: uint64(site.Private)}
		}
		object := gd.guard(OpObjectOf, Ptr, kind, operand(in.Left))
		// A read the receiver's prototypes answered checks them too; a
		// write, which makes a property of the receiver's own, never meets
		// one.
		var holders *HolderChain
		if site.Holders[0].Object != 0 && site.Shape != 0 && in.Op != ir.PropertyWrite {
			holders = b.f.holderChain(HolderChain{Holders: site.Holders, More: site.More})
		} else if site.Holders[0].Object != 0 {
			site.Shape = 0
		}
		var cases []PropertyCase
		if site.Shape != 0 || in.Op == ir.PropertyWrite {
			cases = site.Cases
		}
		if in.Op == ir.PropertyWrite && site.Shape == 0 && len(cases) != 0 {
			// A write whose cache holds no shape native code is compiled
			// for -- one it gave up, a dictionary's, an accessor's -- and
			// which met objects of other shapes it wrote to (the feedback's
			// cases): compiled for the first of them, the rest its cases, as
			// a read past its cache's chain is. The code compares the
			// receiver's shape with the site's first: a site of shape 0 had
			// matched an object with no properties, whose shape is none and
			// whose table is too, and stored through the table, at address
			// 8 -- the FormatJS Intl polyfill's top level, every time.
			site.Shape, site.Index = cases[0].Shape, cases[0].Index
			cases = cases[1:]
		}
		if in.Private && in.When {
			// #x in o: true where the read finds the member -- an object of
			// a shape the site has met -- and Go's answer elsewhere.
			cell := gd.guard(OpPropCell, Source, kind, object)
			cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
			f.setProperty(cell, holders, cases, nil, nil)
			k := f.newValue(blk, OpConst, Tagged)
			k.Const = ir.Bool(true)
			b.assign(in.Dest, blk, k)
			break
		}
		if (in.Op == ir.ReferenceRead || in.Op == ir.PropertyRead) && site.Megamorphic && !in.Private && b.goCalls() && f.Keeps < f.keepLimit() &&
			!b.originGoesOn(pc) {
			// A megamorphic site's read: what neither the receiver's own
			// table nor the megamorphic cache has -- a shape the site never
			// met, a property found where the code does not look -- Go
			// reads, on a branch of its own (split): the property's cell
			// is none there (Try), and a call of Go (GoCall) reads it as
			// the interpreter does, native code going on after it, the
			// value the one loaded from the cell where there is one, as
			// V8's megamorphic loads call the runtime on a miss rather
			// than leave. A number's read is a value's so, which its uses
			// unbox.
			// Of a primitive too, where the site has met one: it has no
			// cell, and Go reads its property (ObjectOfTry).
			// Inside an inlined callee too, where Go reads it only as it
			// needs no frame (abi.GoCallState's Inline, the VM's
			// jitPlainRead) -- a data property an ordinary object or its
			// prototypes have, or none -- and leaves otherwise: a getter's
			// call needs the callee's frame, and a callee whose megamorphic
			// read has met one is not inlined (the VM's jitMegaGetter).
			// Each such miss had left native code, the interpreter finishing
			// the invocation.
			if site.Primitives {
				object.Index = ObjectOfTry
			}
			b.readCalled(pc)
			cell := gd.guard(OpPropCell, Source, kind, object)
			cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
			f.setProperty(cell, holders, cases, nil, nil)
			f.cache(cell).Mega, f.cache(cell).Try = true, true
			if site.Scan && site.Shape == 0 && cases == nil && holders == nil {
				f.cache(cell).Scan = true
			}
			st, recv := cell.State, operand(in.Left)
			none := f.newValue(blk, OpCellNone, Bool, cell)
			miss, hit, _ := b.split(blk, none, true)
			v := f.newValue(hit, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			b.assign(in.Dest, hit, v)
			call := f.newValue(miss, OpCall, Tagged, recv)
			call.Aux, call.State = int(ir.HostExit), st
			st.addUse()
			f.cache(call).Calls, call.Index = []*CallSite{{Go: GoCall, Accessor: AccessorGet, Key: site.Key, ThisSlot: -1}}, f.Keeps
			f.Keeps++
			kept := f.newValue(miss, OpCallCell, Source, call)
			r := f.newValue(miss, OpKept, Tagged, kept, call)
			r.Shadow = kept
			b.assign(in.Dest, miss, r)
			// A getter Go calls may assign a captured binding, as a callee
			// may (ir.Host's calls).
			if b.hasCells() {
				b.reloadAfterCall(miss)
			}
			break
		}
		if in.Op == ir.ReferenceRead || in.Op == ir.PropertyRead && (b.fb != nil && b.fb.Generic(pc) || site.Scan && site.Megamorphic && site.Shape == 0) {
			// A reference's read; or a number's that met a value that was
			// not one, read as a value now, whose uses that want a number
			// unbox it (Feedback's Generic); or one of no shape that looks
			// in the megamorphic cache, an object pattern's in an inlined
			// callee, whose values are anything.
			cell := gd.guard(OpPropCell, Source, kind, object)
			cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
			f.setProperty(cell, holders, cases, nil, nil)
			if site.Megamorphic {
				f.cache(cell).Mega = true
			}
			if site.Scan && site.Shape == 0 && cases == nil && holders == nil {
				f.cache(cell).Scan = true
			}
			v := f.newValue(blk, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			b.assign(in.Dest, blk, v)
			break
		}
		if in.Op == ir.PropertyRead {
			v := gd.guard(OpPropRead, Float64, kind, object)
			v.Const, v.Index, v.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
			f.setProperty(v, holders, cases, nil, nil)
			if site.Megamorphic {
				f.cache(v).Mega = true
			}
			if site.Scan && site.Shape == 0 && cases == nil && holders == nil {
				f.cache(v).Scan = true
			}
			b.assign(in.Dest, blk, boxF(v))
			break
		}
		v := gd.guard(OpPropWrite, None, kind, object, operand(in.Right))
		v.Const, v.Index, v.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
		// A property the write adds, as V8's stores do along a map's
		// transition; or, to an object that has it, stores: of any of the
		// shapes the site met.
		f.setProperty(v, nil, cases, site.Add, site.Adds)
		if site.Megamorphic {
			f.cache(v).Mega = true
			if in.Op == ir.PropertyWrite && !in.Private && (site.Add == nil || !site.Add.Define) && b.goCalls() && f.Keeps < f.keepLimit() {
				// What neither the receiver's own table nor the megamorphic
				// cache has -- a property the write adds, an object of a
				// shape it never met -- Go writes, on a branch of its own
				// (splitOn): the write says whether it left the store to Go,
				// a boolean, and where it did, a call of Go (GoCall) writes
				// it as the interpreter does, native code going on after it,
				// as V8's megamorphic stores call the runtime on a miss
				// rather than leave. The analyses see the call on the branch
				// that makes it, and none where the store is native. Each
				// miss had left native code: RayTrace's testIntersection, at
				// best.hitCount, best one of the intersections' many shapes,
				// at a call in eleven, the interpreter finishing it. Inside
				// an inlined callee too, where Go makes it only as it needs
				// no frame (abi.GoCallState's Inline) -- an addition to an
				// ordinary object, a data property's store, an array's
				// length -- and leaves otherwise, as the write had at each:
				// Typescript's AST nodes, of a class each, constructed
				// through ASTSpan's constructor, inlined in theirs, whose
				// this.minChar = -1 met a new shape at nearly every one; a
				// length's write, rd.actuals.length = 0, which no site's
				// cache holds.
				// A receiver that is no object -- a number, a string, a
				// boolean, which the site met (PropertySite's Primitives)
				// -- is written so too, not left for: its object is none
				// (ObjectOfTry), the write a miss, and Go writes the
				// property of the primitive on the call's branch, as the
				// interpreter does -- a setter on its prototype called with
				// it as this, nothing in sloppy code otherwise, a TypeError
				// in strict code, which leaves at the call. Each such
				// receiver had left native code at the write, the
				// interpreter running the rest of the invocation: a loop
				// whose receiver was a number on every other turn ran 3.6
				// times as long as in the interpreter.
				if site.Primitives {
					object.Index = ObjectOfTry
				}
				st, recv := v.State, operand(in.Left)
				v.Type = Bool
				miss, _ := b.splitOn(blk, v)
				call := f.newValue(miss, OpCall, Tagged, recv, v.Args[1])
				call.Aux, call.State = int(ir.HostExit), st
				st.addUse()
				f.cache(call).Calls, call.Index = []*CallSite{{Go: GoCall, Accessor: AccessorSet, Key: site.Key, Argc: 1, ThisSlot: -1}}, f.Keeps
				f.Keeps++
				// A setter Go calls may assign a captured binding, as a
				// callee may (ir.Host's calls).
				if b.hasCells() {
					b.reloadAfterCall(miss)
				}
			}
		}
	case ir.BindingWrite:
		// The binding's cell, as a read finds it, written as a property
		// store writes one: the stores' checks and keeps take it for a
		// store of the name (aliases), which it is.
		site, ok := b.global(pc)
		if !ok || site.Fixed {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		cell := b.globalCell(site, gd, ir.HostExit)
		w := gd.guard(OpPropWrite, None, ir.HostExit, cell, operand(in.Left))
		w.Key, w.Global = site.Key, true
	case ir.BindingCheck:
		// The name resolves: its binding's cell is there.
		site, ok := b.global(pc)
		if !ok {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.globalCell(site, gd, ir.HostExit)
		k := f.newValue(blk, OpConst, Tagged)
		k.Const = ir.Bool(true)
		b.assign(in.Dest, blk, k)
	case ir.IterStep:
		if b.host(pc) {
			if b.whole(blk, pc, in, gd) {
				break
			}
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.iterStep(blk, pc, in, operand, gd)
	case ir.StringFreeze:
		// A string only: what is known to be anything else has nothing to
		// freeze. It is a store, so that removeDead keeps it, and exits to
		// nothing, so that plan need make no entry after it.
		if x := operand(in.Left); x.Type == Tagged {
			b.f.newValue(blk, OpFreeze, None, x)
		}
	case ir.IterClose:
		if in.When {
			// A for...in's, left early: marked done (OpForInDone).
			f.newValue(blk, OpForInDone, None, operand(in.Left))
			break
		}
		// A cursor closed as a destructuring or a loop ends: nothing to do
		// for one done, or for an array walked in place while nothing up
		// its iterator's chain has a return method (WalkedCloseFeedback);
		// Go closes any other -- by a call of Go native code goes on after
		// (GoIterClose, Index 1) where that runs no script, a generator's
		// a destructuring left suspended, after an exit otherwise.
		c := gd.guard(OpIterClosed, None, ir.HostExit, operand(in.Left))
		if w, ok := b.fb.(WalkedCloseFeedback); ok {
			c.Const.Bits = uint64(w.WalkedCloseCell())
		}
		if b.goCalls() {
			c.Index = 1
		}
	case ir.Closure, ir.CloseUpvalues:
		if tmpl := b.made[pc]; tmpl != 0 && in.Op == ir.Closure && b.cur == b.root {
			// A loop's callback inlined in it, which nothing else sees
			// (madeAt): not made. Its slot holds its template, a cell's,
			// which only frame states name -- Go makes the closure from it
			// where an exit's frame has it, as V8's deoptimizer
			// materializes an object escape analysis removed.
			cell := f.newValue(blk, OpConstCell, Source)
			cell.Const = ir.Value{Bits: uint64(tmpl)}
			v := f.newValue(blk, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			b.assign(in.Dest, blk, v)
			break
		}
		if b.host(pc) {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goFrame(blk, pc, in, gd)
	case ir.Rethrow:
		if b.host(pc) {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		// The completion record is a normal completion's, which is
		// dropped; for any other the interpreter reproduces it, from the
		// state before the rethrow -- but a return's where the lowering
		// says it may return natively (When), as the interpreter's rethrow
		// returns: `try { return x } finally { ... }` had deoptimized at
		// each return.
		normal := f.newValue(blk, OpConstF64, Float64)
		normal.Const = ir.Float(ir.RethrowNormal)
		kind := number(in.Left, ir.GuardExit)
		is := f.newValue(blk, OpCmpF64, Bool, kind, normal)
		is.Aux = int(ir.Eq)
		if !in.When || b.cur != b.root {
			gd.guard(OpCheckTrue, None, ir.GuardExit, boxB(is))
			break
		}
		ret, cont := b.blockAfter(blk), b.blockAfter(blk)
		b.placeAfter(blk, []*Block{ret, cont}, cont)
		blk.Kind, blk.Control, blk.State, blk.ExitKind = BlockIf, is, nil, 0
		is.Uses++
		blk.Succs = []*Block{cont, ret}
		cont.Preds, cont.Backedge = []*Block{blk}, []bool{false}
		ret.Kind, ret.Preds, ret.Backedge = BlockReturn, []*Block{blk}, []bool{false}
		b.seal(ret)
		b.seal(cont)
		b.splits = append(b.splits, ret, cont)
		b.next = cont
		blk = ret
		gd.blk = blk
		two := f.newValue(blk, OpConstF64, Float64)
		two.Const = ir.Float(ir.RethrowReturn)
		returns := f.newValue(blk, OpCmpF64, Bool, kind, two)
		returns.Aux = int(ir.Eq)
		gd.guard(OpCheckTrue, None, ir.GuardExit, boxB(returns))
		v := operand(ir.Slot(in.Left.Slot - 1))
		gd.guard(OpCheckInit, None, ir.GuardExit, v)
		if b.p.Captured != nil && !b.p.Cells {
			// Closed before it returns, as a return closes them.
			b.goFrame(blk, pc, ir.Instruction{Op: ir.CloseUpvalues}, gd)
		}
		blk.Control = v
		v.Uses++
	case ir.ParamsDead:
		// The arguments, what the slots have at the function's start --
		// undefined where one is missing, as the VM's call fills them --
		// and the slots in the dead zone.
		// An inlined callee's are its own slots, past its caller's.
		fr := b.cur
		fr.paramArgs = make([]*Value, in.Key)
		for i := range fr.paramArgs {
			fr.paramArgs[i] = operand(ir.Slot(fr.base + i))
		}
		for i := range fr.paramArgs {
			u := f.newValue(blk, OpConst, Tagged)
			u.Const = ir.Value{Kind: ir.Uninitialized}
			b.assign(fr.base+i, blk, u)
		}
	case ir.ParamInit:
		b.assign(in.Dest, blk, b.cur.paramArgs[in.Key])
	case ir.ParamDefault:
		// Undefined, passed or missing, is what makes a default run.
		undef := f.newValue(blk, OpStrictNullish, Bool, b.cur.paramArgs[in.Key])
		undef.Aux = 1
		b.assign(in.Dest, blk, boxB(undef))
	case ir.Coercible:
		// Neither null nor undefined, as ?? asks; else Go throws, from an
		// exit at the check.
		nullish := f.newValue(blk, OpStrictNullish, Bool, operand(in.Left))
		nullish.Aux = 2
		gd.guard(OpCheckTrue, None, ir.HostExit, boxB(f.newValue(blk, OpNot, Bool, nullish)))
	case ir.Resolved:
		// True where native code checked; Go's answer, after an exit,
		// checked here, and Go throws for an unresolved name.
		gd.guard(OpCheckTrue, None, ir.HostExit, operand(in.Left))
		b.assign(in.Dest, blk, operand(in.Right))
	case ir.ObjectLiteral, ir.ArrayLiteral:
		// The pool's next object, kept in a cell as a construction's
		// receiver is: nothing runs. An array's elements are the operands
		// (CallSite's Argc of them), written into it.
		site, keep, ok := b.literal(pc)
		if !ok {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		var elems []*Value
		if in.Op == ir.ArrayLiteral && !site.Shared {
			elems = make([]*Value, in.Extra)
			for i := range elems {
				elems[i] = operand(ir.Slot(in.Dest + i))
			}
		}
		call := gd.guard(OpCall, Tagged, ir.HostExit, elems...)
		b.f.cache(call).Calls = []*CallSite{{Pool: site.Pool, Alloc: true, Literal: true, Argc: len(elems), ThisSlot: -1,
			Shared: in.Op == ir.ArrayLiteral && site.Shared}}
		call.Index = keep
		cell := f.newValue(blk, OpCallCell, Source, call)
		r := f.newValue(blk, OpKept, Tagged, cell, call)
		r.Shadow = cell
		b.assign(in.Dest, blk, r)
	case ir.StringAdd:
		if !b.goCalls() {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goAdd(blk, in, operand, gd)
	case ir.HasProperty:
		if !b.goCalls() {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goHas(blk, in, operand, gd)
	case ir.ArgumentRead:
		if b.host(pc) {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		if b.cur == b.root {
			// The function's own arguments, which Go reads from the frame
			// it entered the code with (GoArgument): arguments[k], or with
			// When arguments.length.
			args := []*Value{operand(in.Left), operand(in.Third)}
			argc := 2
			if in.When {
				args, argc = args[1:], 1
			} else {
				// The parameters a mapped object aliases, as they are, for
				// a key that is one's alias (ir.Program's ArgumentParams),
				// from the frame's first parameter's slot (ParamBase):
				// register code's registers 0 and 1 are its function and
				// its this.
				for i := range b.p.ArgumentParams {
					args = append(args, b.read(b.p.ParamBase+i, blk))
				}
				argc += b.p.ArgumentParams
			}
			call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
			b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoArgument, Argc: argc, ThisSlot: -1}}, b.f.Keeps
			b.f.Keeps++
			cell := f.newValue(blk, OpCallCell, Source, call)
			r := f.newValue(blk, OpKept, Tagged, cell, call)
			r.Shadow = cell
			b.assign(in.Dest, blk, r)
			break
		}
		// arguments[k] of an inlined call (InlineSite's ReadsArguments), a
		// constant k below the arguments' count, is its argument k, as V8
		// eliminates the arguments object; any other key deoptimizes, and
		// the interpreter, Go's from the next compile on (generic), makes
		// it, its frame given the call's arguments.
		if in.When {
			// arguments.length (register code's args_len), the call's
			// count.
			n := f.newValue(blk, OpConst, Tagged)
			n.Const = ir.Float(float64(len(b.cur.args)))
			b.assign(in.Dest, blk, n)
			break
		}
		if k, ok := constIndex(operand(in.Left)); ok && k < len(b.cur.args) {
			b.assign(in.Dest, blk, b.cur.args[k])
			break
		}
		if n := len(b.cur.args); n > 0 && n <= maxArgumentSelect {
			b.argumentSelect(blk, in, operand(in.Left), gd)
			break
		}
		no := f.newValue(blk, OpConst, Tagged)
		no.Const = ir.Bool(false)
		gd.guard(OpCheckTrue, None, ir.GuardExit, no)
		b.assign(in.Dest, blk, operand(in.Left))
	case ir.DeleteProperty:
		if !b.goCalls() {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goDelete(blk, in, operand, gd)
	case ir.StringPart:
		// A part's conversion the concat that reads it makes (compile's
		// regStringParts): checked to be a primitive that converts calling
		// nothing, as it is; where the check failed before, converted by
		// Go there, as to_string is.
		if b.fb != nil && b.fb.Generic(pc) {
			if !b.goCalls() {
				blk.ExitKind = ir.HostExit
				blk.State = state()
				blk.State.addUse()
				break
			}
			b.goConcat(blk, ir.Instruction{Op: ir.StringConcat, Dest: in.Dest, Extra: 1}, func(ir.Operand) *Value { return operand(in.Left) }, gd)
			break
		}
		v := operand(in.Left)
		gd.guard(OpPrimitive, None, ir.GuardExit, v)
		b.assign(in.Dest, blk, v)
	case ir.StringConcat:
		if !b.goCalls() {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goConcat(blk, in, operand, gd)
	case ir.UpvalueRead, ir.UpvalueWrite:
		if b.cur != b.root && b.cur.site.Captured != nil && int(in.Key) < len(b.cur.site.Captured) {
			// A callback the root never made (InlineSite's Captured): its
			// captured binding the root's own slot, read where the root
			// has it and assigned as the root assigns it -- in its cell,
			// where an exit's frame and what Go runs find it -- exiting
			// to the callee's state.
			slot := b.cur.site.Captured[in.Key]
			if in.Op == ir.UpvalueRead || in.Check {
				v := b.read(slot, blk)
				if in.Check {
					gd.guard(OpCheckInit, None, ir.GuardExit, v)
				}
				if in.Op == ir.UpvalueRead {
					b.assign(in.Dest, blk, v)
					break
				}
			}
			x := operand(in.Left)
			w := gd.guard(OpPropWrite, None, ir.HostExit, b.cellOf(blk, slot), x)
			w.Key, w.Upvalue = localKey(slot), true
			if b.root.p.Cells || slot >= b.root.p.UpvalueBase {
				w.Key = upvalueKey
			}
			b.write(slot, blk, x)
			break
		}
		if b.cur != b.root && b.cur.site.Upvalues != 0 {
			// An inlined callee's captured binding, read from its cell
			// anew each time (OpUpvalueCell's Aux 2): a call may assign it.
			cell := f.newValue(blk, OpUpvalueCell, Source)
			cell.Index, cell.Aux, cell.Const = int(in.Key), 2, ir.Value{Bits: uint64(b.cur.site.Upvalues)}
			if in.Op == ir.UpvalueRead || in.Check {
				v := f.newValue(blk, OpLoadCell, Tagged, cell)
				v.Shadow = cell
				if in.Check {
					gd.guard(OpCheckInit, None, ir.GuardExit, v)
				}
				if in.Op == ir.UpvalueRead {
					b.typedUpvalue(pc, v)
					b.assign(in.Dest, blk, v)
					break
				}
			}
			// Assigned in its cell, as the function's own are: the cell may
			// be one the function's code reads -- a binding both captured, a
			// local of its own the callee captured -- whose value it has
			// read before, which it reads again (reloadUpvalues).
			w := gd.guard(OpPropWrite, None, ir.HostExit, cell, operand(in.Left))
			w.Key, w.Upvalue = upvalueKey, true
			b.reloadUpvalues(blk)
			break
		}
		if b.cur != b.root {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		slot := b.p.UpvalueBase + int(in.Key)
		if in.Op == ir.UpvalueRead {
			v := b.read(slot, blk)
			if in.Check {
				gd.guard(OpCheckInit, None, ir.GuardExit, v)
			}
			b.typedUpvalue(pc, v)
			b.assign(in.Dest, blk, v)
			break
		}
		if in.Check {
			gd.guard(OpCheckInit, None, ir.GuardExit, b.read(slot, blk))
		}
		cell := f.newValue(blk, OpUpvalueCell, Source)
		cell.Index = int(in.Key)
		x := operand(in.Left)
		w := gd.guard(OpPropWrite, None, ir.HostExit, cell, x)
		w.Key, w.Upvalue = upvalueKey, true
		b.write(slot, blk, x)
	case ir.StringConst:
		cell, ok := b.stringCell(pc)
		if !ok {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		c := f.newValue(blk, OpConstCell, Source)
		c.Const = ir.Value{Bits: uint64(cell)}
		if sf, ok := b.fb.(StringContentFeedback); ok {
			if s, ok := sf.StringContent(pc); ok {
				f.cache(c).Str = &s
			}
		}
		v := f.newValue(blk, OpLoadCell, Tagged, c)
		v.Shadow = c
		b.assign(in.Dest, blk, v)
	case ir.BigIntConst:
		var cell uintptr
		var bits uint64
		kind, ok := BigConstHeap, false
		if fb, is := b.fb.(BigIntConstFeedback); is {
			cell, bits, kind, ok = fb.BigIntCell(pc)
		}
		if !ok || cell == 0 {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		c := f.newValue(blk, OpConstCell, Source)
		c.Const = ir.Value{Bits: uint64(cell)}
		v := f.newValue(blk, OpLoadCell, Tagged, c)
		v.Shadow = c
		if kind == BigConstInt64 || kind == BigConstUint64 {
			// Its bits, which an unboxing takes (OpConstI64).
			v.Aux, v.Const = kind, ir.Value{Bits: bits}
		}
		b.assign(in.Dest, blk, v)
	case ir.FieldDefine:
		add, ok := b.define(pc)
		if calls := b.callsAt(pc); !ok && len(calls) == 1 && calls[0].Instr != 0 && b.cur == b.root {
			// A field Go defines, called from native code (CallSite's
			// Instr): one whose literal's layout Define does not plan --
			// past a spread, a computed key or __proto__ -- as V8's code
			// calls its DefineKeyedOwnIC's runtime. Each had left native
			// code.
			call := gd.guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
			b.f.cache(call).Calls, call.Index = calls, b.callKeepAt(pc)
			cell := f.newValue(blk, OpCallCell, Source, call)
			r := f.newValue(blk, OpKept, Tagged, cell, call)
			r.Shadow = cell
			b.write(in.Left.Slot, blk, r)
			if b.hasCells() {
				b.reloadAfterCall(blk)
			}
			break
		}
		if !ok {
			if b.whole(blk, pc, in, gd) {
				break
			}
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		object := gd.guard(OpObjectOf, Ptr, ir.HostExit, operand(in.Left))
		w := gd.guard(OpPropWrite, None, ir.HostExit, object, operand(in.Right))
		w.Key, b.f.cache(w).Add = add.Key, add
	case ir.TypeTest:
		v := gd.guard(OpTypeIs, Bool, ir.HostExit, operand(in.Left))
		v.Index = int(in.Key)
		if !in.When {
			v.Const = ir.Value{Bits: 1}
		}
		b.assign(in.Dest, blk, boxB(v))
	case ir.Host, ir.Call:
		if in.Op == ir.Host && b.awaitAt(pc) {
			b.await(blk, pc, in)
			break
		}
		if in.Op == ir.Host && in.Frame && b.cur == b.root && b.goCalls() {
			// Go makes it from native code, on the frame it entered the code
			// with (GoHost), and native code goes on after it.
			sp := b.p.Locals + b.p.Maps[pc].Depth
			args := make([]*Value, in.Extra)
			for i := range args {
				args[i] = b.read(sp-in.Extra+i, blk)
			}
			call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
			b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoHost, Argc: in.Extra, ThisSlot: -1}}, b.f.Keeps
			b.f.Keeps++
			b.f.HostsLeft = append(b.f.HostsLeft, pc)
			if in.Dest >= 0 {
				cell := f.newValue(blk, OpCallCell, Source, call)
				r := f.newValue(blk, OpKept, Tagged, cell, call)
				r.Shadow = cell
				b.assign(in.Dest, blk, r)
			}
			// A getter or a setter Go calls may assign a captured binding.
			if b.hasCells() {
				b.reloadAfterCall(blk)
			}
			break
		}
		if in.Op == ir.Host && in.Whole && b.wholeAt(pc) && b.goCalls() {
			// A computed key's define the site's keys tell, ahead of Go's.
			if site, ok := b.keyedStore(pc); ok && site.Define {
				if reads, n, _, _ := in.WholeOperands(b.p.Regs); n == 3 {
					blk = b.keyedStores(blk, site, b.read(reads[0], blk), b.read(reads[1], blk), b.read(reads[2], blk), b.fb.Generic(pc), state)
					gd.blk = blk
				}
			}
		}
		if in.Op == ir.Host && in.Whole && b.whole(blk, pc, in, gd) {
			break
		}
		if in.Tail && in.Window {
			// Register code's call in tail position: shallow in the stack,
			// the ordinary call, its result the instruction's register.
			// Deeper, where the interpreter gives the frame to the callee
			// in place (the VM's regTailTo), a call of the closure the frame
			// runs is a jump to the code's start (OpTailSelf): the window's function, receiver and arguments
			// its registers 0, 1 and on -- the receiver a method call's,
			// an arrow's the this it captured, its register 1, undefined
			// otherwise -- undefined in the rest. Any other call there
			// leaves for the interpreter to make (OpTailRoom's), as does
			// one in an inlined frame, or in code whose frame holds what
			// it may not give up: registers a closure captures, the
			// arguments it keeps.
			if p := b.root.p; b.cur == b.root && p.TailOwn && p.Captured == nil && !p.FrameArgs {
				a := in.Left.Slot
				args := make([]*Value, 0, in.Extra+3)
				fn := b.read(a, blk)
				args = append(args, fn, fn)
				switch {
				case p.ArrowThis:
					args = append(args, b.read(1, blk))
				case in.When:
					args = append(args, b.read(a+1, blk))
				default:
					this := f.newValue(blk, OpConst, Tagged)
					this.Const = ir.Value{Kind: ir.Undefined}
					args = append(args, this)
				}
				for i := range in.Extra {
					args = append(args, b.read(a+2+i, blk))
				}
				v := gd.guard(OpTailSelf, None, ir.HostExit, args...)
				v.Index, v.Const = p.ParamBase+p.Params, ir.Value{Bits: uint64(p.Regs)}
				v.Key = TailWindow
				if p.Origin != nil && !looped(p, 0) {
					// The start polls (OpPoll), which the jump goes to.
					v.Key |= TailPolled
				}
				if calls := b.callsAt(pc); len(calls) != 0 && calls[0].Go == 0 {
					// In a native callee's frame, a call of another of
					// the functions the call has is a jump to its code,
					// its window in place of this one, as the stack
					// form's (mir's windowTailJump).
					b.f.cache(v).Calls = calls
				}
			} else {
				room := gd.guard(OpTailRoom, None, ir.HostExit)
				room.Index = b.cur.depth
			}
		} else if in.Tail {
			// Deep in the stack the frame is given up, where it holds no
			// captured local: native code makes a call of the function
			// itself a jump to its start, and, in a native callee's frame,
			// one of a function the call is seen to call a jump to that
			// function's code, its frame in place of this one (OpTailSelf);
			// Go makes any other.
			if p := b.root.p; b.cur == b.root && p.Captured == nil && !p.ArrowThis && !p.ClassConstructor && !p.FrameArgs {
				sp := p.Locals + p.Maps[pc].Depth
				args := make([]*Value, in.Extra+1, in.Extra+2)
				for i := range args {
					args[i] = b.read(sp-in.Extra-1+i, blk)
				}
				if in.When || p.This {
					// A method's receiver, below the function, or none.
					this := f.newValue(blk, OpConst, Tagged)
					this.Const = ir.Value{Kind: ir.Undefined}
					if in.When {
						this = b.read(sp-in.Extra-2, blk)
					}
					args = append(args, this)
				}
				v := gd.guard(OpTailSelf, None, ir.HostExit, args...)
				v.Index, v.Const = p.Params, ir.Value{Bits: uint64(p.UpvalueBase)}
				if in.When || p.This {
					v.Key |= 1
				}
				if p.This {
					v.Key |= 2
				}
				if calls := b.callsAt(pc); len(calls) != 0 && calls[0].Go == 0 {
					b.f.cache(v).Calls = calls
				}
			} else {
				// An inlined callee's frame is not a level of its own: the
				// frames it is inlined in count, as the interpreter's
				// frames would (OpTailRoom's Index).
				room := gd.guard(OpTailRoom, None, ir.HostExit)
				room.Index = b.cur.depth
			}
		}
		if k, ok := b.intrinsic(pc); ok && in.Op == ir.Host {
			b.intrinsicCall(blk, pc, k, gd, boxF)
			break
		}
		if in.Op == ir.Host && b.charCode(pc) {
			b.charCodeCall(blk, pc, gd, boxF)
			break
		}
		if fn, ok := b.fromCharCode(pc); ok && in.Op == ir.Host {
			b.fromCharCodeCall(blk, pc, fn, gd)
			break
		}
		if fp, ok := b.forEaches[pc]; ok && in.Op == ir.Host && b.cur == b.root {
			if fp.site.Kind&abi.LoopKindMask == abi.LoopSort {
				b.sortCall(blk, pc, fp)
				break
			}
			b.forEachCall(blk, pc, fp)
			break
		}
		if fn, ok := b.parseIntAt(pc); ok && in.Op == ir.Host {
			b.parseIntCall(blk, pc, fn, gd, boxF)
			break
		}
		if fn, kind, ok := b.indexOfAt(pc); ok && in.Op == ir.Host {
			b.indexOfCall(blk, pc, fn, kind, gd)
			break
		}
		if fn, ok := b.leafAt(pc); ok && in.Op == ir.Host {
			b.leafCall(blk, pc, fn, gd)
			break
		}
		if fn, ok := b.bigStringAt(pc); ok && in.Op == ir.Host {
			b.bigStringCall(blk, pc, fn, gd)
			break
		}
		if fn, kind, ok := b.substring(pc); ok && in.Op == ir.Host {
			b.substringCall(blk, pc, fn, kind, gd)
			break
		}
		if k, ok := b.instanceOf(pc); ok && in.Op == ir.Host {
			b.instanceOfOp(blk, pc, k, gd, boxB)
			break
		}
		if cells, ok := b.typeofAt(pc); ok && in.Op == ir.Host {
			b.typeofCall(blk, pc, cells)
			break
		}
		if fr := b.inlinedAt(pc); fr != nil {
			b.inlineCall(blk, pc, fr, gd, state)
			break
		}
		if calls := b.callsAt(pc); calls != nil && b.cur == b.root && in.Window {
			b.windowCall(blk, pc, in, calls, gd)
			break
		}
		if calls := b.callsAt(pc); calls != nil && b.cur == b.root {
			// The call's operands, from the receiver or the function on; its
			// result is the slot below them, its pointer word kept.
			n := calls[0].Argc + 1
			if calls[0].Method {
				n++
			}
			if calls[0].Super {
				// The this slot, then the arguments.
				n = calls[0].Argc + 1
			}
			if calls[0].Instr != 0 {
				// An operation's operands (CallSite's Instr).
				n = calls[0].Argc
			}
			sp := b.p.Locals + b.p.Maps[pc].Depth
			if calls[0].StringSelf && n == 1 && len(calls) == 1 {
				b.stringSelfCall(blk, pc, calls, sp-1)
				break
			}
			args := make([]*Value, n)
			for i := range args {
				if i > 0 || !calls[0].Super {
					args[i] = b.read(sp-n+i, blk)
				}
			}
			if calls[0].Super {
				args[0] = b.read(b.p.Locals-1, blk)
			}
			if calls[0].Forward {
				// f.apply(x, arguments) of the realm's apply, which the
				// call makes (CallSite's Forward); any other apply is the
				// interpreter's, which makes the arguments object.
				apply := gd.guard(OpObjectOf, Ptr, ir.HostExit, args[1])
				same := gd.guard(OpSameObject, None, ir.HostExit, apply)
				same.Const = ir.Value{Bits: uint64(calls[0].Apply)}
			}
			call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
			b.f.cache(call).Calls, call.Index = calls, b.callKeepAt(pc)
			if calls[0].Go == GoCall {
				// Go makes it, which counts it as an exit's would.
				b.f.HostsLeft = append(b.f.HostsLeft, pc)
			}
			cell := f.newValue(blk, OpCallCell, Source, call)
			r := f.newValue(blk, OpKept, Tagged, cell, call)
			r.Shadow = cell
			b.write(b.p.Locals+b.p.Maps[pc+1].Depth-1, blk, r)
			if calls[0].Super && b.p.This {
				// The this it bound, which the frame's this slot, the
				// program's last local, holds from here (jitcompile's
				// push_this checks it is bound).
				b.write(b.p.Locals-1, blk, r)
			}
			// The callee may have assigned a captured binding. A guard
			// after the call exits to the next instruction, as the site
			// there fails: never again, once it has (Generic).
			if b.hasCells() {
				b.reloadAfterCall(blk)
			}
			break
		}
		if b.cur == b.root {
			b.f.HostsLeft = append(b.f.HostsLeft, pc)
		}
		blk.ExitKind = ir.HostExit
		blk.State = state()
		blk.State.addUse()
		if b.provisional && b.cur != b.root && slices.Contains(b.cur.pending, pc) {
			// A call of what the callee is given, which the analysis reads
			// the callee of here (calledClosures).
			b.givenStates = append(b.givenStates, givenState{fr: b.cur, pc: pc, state: blk.State})
		}
		// A generator's yield, which code a loop's step resumed natively
		// makes itself (Block's Yield): its value in its operand's
		// register, its handlers the code's by register pc, which a
		// resumption finds again without anything saved.
		if in.Suspend && b.cur == b.root {
			blk.Yield, blk.YieldSlot = true, in.Left.Slot
		}
	}
}

// whole makes register code's instruction at pc, in, which the builder
// leaves to Go, a call of Go that runs it whole on the register frame
// (CallSite's Whole), at the root, where it is its origin's only
// instruction and its operands are its registers (ir.Instruction's
// WholeOperands): its operands the registers it reads, its result the one
// it writes, and native code goes on after it. What Go refuses, or what
// does not go on to the next instruction -- a throw, code found invalid --
// leaves at the call, Go finishing it there. It reports whether it made
// the call.
func (b *builder) whole(blk *Block, pc int, in ir.Instruction, gd *guarder) bool {
	if !b.wholeAt(pc) {
		return false
	}
	p := b.p
	reads, n, dest, _ := in.WholeOperands(p.Regs)
	f := b.f
	site := &CallSite{Go: GoCall, Whole: true, Argc: n, ThisSlot: -1}
	if sites := b.fb.NativeCalls(pc); len(sites) == 1 && sites[0].Whole && sites[0].Argc == n {
		// What native code makes of it itself first, as of the stack
		// form's operation's site (CallSite's ArrayIter, Append, Copies).
		site = &sites[0]
	}
	if site.StringSelf && n == 1 && dest == reads[0] {
		// A computed key's conversion of its own register (StringSelf): a
		// string is its own key, told natively; Go converts any other.
		b.wholeStringSelf(blk, pc, site, dest)
		return true
	}
	args := make([]*Value, n)
	for i := range args {
		args[i] = b.read(reads[i], blk)
	}
	call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
	f.cache(call).Calls, call.Index = []*CallSite{site}, f.Keeps
	f.Keeps++
	f.HostsLeft = append(f.HostsLeft, pc)
	if dest >= 0 {
		cell := f.newValue(blk, OpCallCell, Source, call)
		r := f.newValue(blk, OpKept, Tagged, cell, call)
		r.Shadow = cell
		b.assign(dest, blk, r)
	}
	// What it runs may assign a captured binding.
	if b.hasCells() {
		b.reloadAfterCall(blk)
	}
	return true
}

// wholeStringSelf is whole's call of Go of site, the operation at pc of
// the one register reg it writes too, whose result is the register itself
// where that holds a string (CallSite's StringSelf): told by its word,
// natively, and by the call, on a branch of its own, for anything else,
// native code going on after either.
func (b *builder) wholeStringSelf(blk *Block, pc int, site *CallSite, reg int) {
	f := b.f
	x := b.read(reg, blk)
	st := b.state(blk, pc)
	is := f.newValue(blk, OpTypeIs, Bool, x)
	is.Index, is.Aux, is.State = int(ir.TypeString), int(ir.HostExit), st
	st.addUse()
	_, other, _ := b.split(blk, is, true)
	call := f.newValue(other, OpCall, Tagged, x)
	f.cache(call).Calls, call.Index = []*CallSite{site}, f.Keeps
	f.Keeps++
	call.Aux, call.State = int(ir.HostExit), st
	st.addUse()
	f.HostsLeft = append(f.HostsLeft, pc)
	kept := f.newValue(other, OpCallCell, Source, call)
	r := f.newValue(other, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.assign(reg, other, r)
	// A key's conversion may run script, which may assign a captured
	// binding, as a callee may.
	if b.hasCells() {
		b.reloadAfterCall(other)
	}
}

// wholeAt reports whether whole makes the instruction at pc a call of Go
// that runs it whole: at the root, its origin's only instruction, its
// operands its register instruction's own.
func (b *builder) wholeAt(pc int) bool {
	p := b.p
	if b.cur != b.root || !b.goCalls() || p.Origin == nil || pc+1 >= len(p.Code) || p.Origin[pc+1] == p.Origin[pc] ||
		pc > 0 && p.Origin[pc-1] == p.Origin[pc] {
		return false
	}
	_, _, _, ok := p.Code[pc].WholeOperands(p.Regs)
	return ok
}

// windowCall makes register code's call at pc, in (ir.Instruction's
// Window), by calls, as a slot program's is made: its operands read from
// the window's registers in a slot program's order -- a method's receiver,
// the function, the arguments; a plain call's and a construction's
// function and arguments, register 1 of the window being no operand of
// theirs; a typeof's operand; an instanceof's object and constructor --
// and its result, kept in its cell, written to its register.
func (b *builder) windowCall(blk *Block, pc int, in ir.Instruction, calls []*CallSite, gd *guarder) {
	if calls[0].Super {
		b.superCall(blk, pc, in, calls, gd)
		return
	}
	if c := calls[0]; len(calls) == 1 && c.Go == GoCall && !c.New && !c.InstanceOf && !c.TypeOf && c.Argc <= abi.MaxGoArgs-2 && b.goCalls() && !b.fb.Generic(pc) {
		// A leaf's call (LeafFeedback).
		if lf, ok := b.fb.(LeafFeedback); ok {
			if fn, ok := lf.Leaf(pc); ok {
				b.windowLeaf(blk, pc, in, c, fn, gd)
				return
			}
		}
		if vf, ok := b.fb.(ViaLeafFeedback); ok && c.Method && c.Argc >= 1 {
			// G.call(x, ...) of a leaf G (ViaLeafFeedback).
			if call, fn, ok := vf.ViaLeaf(pc); ok {
				b.windowViaLeaf(blk, pc, in, c, call, fn, gd)
				return
			}
		}
	}
	f, a := b.f, in.Left.Slot
	// The call's arguments, which its value copies (newValue).
	args := f.refsOf(in.Extra + 2)[:0]
	if calls[0].Forward {
		// f.apply(x, arguments) (CallSite's Forward): f, apply and x, its
		// window's first three registers; the realm's apply checked, any
		// other the interpreter's.
		args = append(args, b.read(a, blk), b.read(a+1, blk), b.read(a+2, blk))
		apply := gd.guard(OpObjectOf, Ptr, ir.HostExit, args[1])
		same := gd.guard(OpSameObject, None, ir.HostExit, apply)
		same.Const = ir.Value{Bits: uint64(calls[0].Apply)}
		call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
		f.cache(call).Calls, call.Index = calls, b.callKeepAt(pc)
		cell := f.newValue(blk, OpCallCell, Source, call)
		r := f.newValue(blk, OpKept, Tagged, cell, call)
		r.Shadow = cell
		b.assign(in.Dest, blk, r)
		if b.hasCells() {
			b.reloadAfterCall(blk)
		}
		return
	}
	switch {
	case calls[0].InstanceOf:
		args = append(args, b.read(a, blk), b.read(in.Right.Slot, blk))
	case calls[0].TypeOf:
		if tf, ok := b.fb.(TypeofFeedback); ok && b.goCalls() {
			if cells, ok := tf.TypeofCells(); ok && cells != 0 {
				// Its name loaded from the runtime's table, Go's for what
				// native code cannot tell: a call of Go at each had been
				// register code's.
				b.windowTypeof(blk, pc, in, cells)
				return
			}
		}
		args = append(args, b.read(a, blk))
	}
	if calls[0].Method {
		args = append(args, b.read(a+1, blk))
	}
	if !calls[0].InstanceOf && !calls[0].TypeOf {
		args = append(args, b.read(a, blk))
		for i := range in.Extra {
			args = append(args, b.read(a+2+i, blk))
		}
	}
	if calls[0].Table != 0 {
		calls = b.tableCall(blk, calls, b.read(a, blk))
	}
	call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
	f.cache(call).Calls, call.Index = calls, b.callKeepAt(pc)
	if !calls[0].InstanceOf && !calls[0].TypeOf {
		call.IC.Consumed = b.windowConsumed(pc, in)
	}
	if calls[0].Go == GoCall {
		// Go makes it, which counts it as an exit's would.
		f.HostsLeft = append(f.HostsLeft, pc)
	}
	cell := f.newValue(blk, OpCallCell, Source, call)
	r := f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
	// The callee may have assigned a captured binding.
	if b.hasCells() {
		b.reloadAfterCall(blk)
	}
}

// windowConsumed is the slots of the window of register code's call at
// pc, in, the root's, that the call consumes (InlineCache's Consumed):
// its function's, its receiver's and its arguments' registers that no
// instruction reads past the call's register instruction before writing
// them -- live neither at the next one's start nor at the handler in
// force -- and the result's, which the call writes. The rest, a register
// read again past the call, its state describes for Go to write.
func (b *builder) windowConsumed(pc int, in ir.Instruction) uint64 {
	p := b.p
	if p.Origin == nil || p.Live == nil {
		return 0
	}
	next := pc + 1
	for next < len(p.Code) && p.Origin[next] == p.Origin[pc] {
		next++
	}
	handler := -1
	if p.HandlerAt != nil && p.HandlerAt[pc] != 0 {
		handler = int(p.Handlers[p.HandlerAt[pc]-1].PC)
	}
	var m uint64
	for k := range min(2+in.Extra, 64) {
		slot := in.Left.Slot + k
		if slot >= p.Regs {
			continue
		}
		dead := slot == in.Dest || next < len(p.Code) && !p.LiveAt(next, slot)
		if dead && handler >= 0 && handler < len(p.Code) && p.LiveAt(handler, slot) {
			dead = false
		}
		if dead {
			m |= 1 << k
		}
	}
	return m
}

// superCall makes register code's super(...) at pc, in, of a derived
// constructor (CallSite's Super): its
// operands the this slot (ir.Program's This), the binding, which the call
// checks is unbound, and the arguments, from the window's first register,
// Left; the this it binds, its result, kept in its cell, is R[Dest]'s,
// register 1's -- as the interpreter's super_call writes them -- and the
// this slot's from here, which the call stores in the context's This.
func (b *builder) superCall(blk *Block, pc int, in ir.Instruction, calls []*CallSite, gd *guarder) {
	f, p := b.f, b.p
	this := p.UpvalueBase + p.Upvalues
	args := make([]*Value, 0, in.Extra+1)
	args = append(args, b.read(this, blk))
	for i := range in.Extra {
		args = append(args, b.read(in.Left.Slot+i, blk))
	}
	call := gd.guard(OpCall, Tagged, ir.HostExit, args...)
	f.cache(call).Calls, call.Index = calls, b.callKeepAt(pc)
	if calls[0].Go == GoCall {
		// Go makes it, which counts it as an exit's would.
		f.HostsLeft = append(f.HostsLeft, pc)
	}
	cell := f.newValue(blk, OpCallCell, Source, call)
	r := f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
	b.assign(1, blk, r)
	b.write(this, blk, r)
	if b.hasCells() {
		b.reloadAfterCall(blk)
	}
}

// callTables makes the tables of the calls by PC, once one has a site.
func (b *builder) callTables() {
	if b.calls == nil {
		n := len(b.p.Code)
		b.calls, b.callKeeps = b.f.callLists(n), b.f.ints(n)
	}
}

// callsAt is calls at pc, which is the function's, or an inlined callee's,
// past the table's end: none.
func (b *builder) callsAt(pc int) []*CallSite {
	if pc < len(b.calls) {
		return b.calls[pc]
	}
	return nil
}

// callKept is the keep cell of the call at pc, and whether it has one.
func (b *builder) callKept(pc int) (int, bool) {
	if pc < len(b.callKeeps) && b.callKeeps[pc] != 0 {
		return b.callKeeps[pc] - 1, true
	}
	return 0, false
}

// callKeepAt is the keep cell of the call at pc, which has one.
func (b *builder) callKeepAt(pc int) int {
	keep, _ := b.callKept(pc)
	return keep
}

// nativeCalls are the functions the call at pc may call natively, if any
// (CallSite): those the VM has seen it call, if speculation has not given
// up on it and there is an entry after it to go on at; or the call of Go
// that makes it (GoCall), which speculates nothing.
func (b *builder) nativeCalls(pc int) []*CallSite {
	if b.fb == nil || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) {
		return nil
	}
	if a, ok := b.accessor(pc); ok {
		return b.accessorCalls(a)
	}
	generic := b.fb.Generic(pc)
	var calls []*CallSite
	// The feedback's sites are read-only; copy only a site whose coercion
	// the call can omit.
	sites := b.fb.NativeCalls(pc)
	for i := range sites {
		site := &sites[i]
		if site.Go == GoCall && (!b.goCalls() || len(calls) != 0) || site.Go != GoCall && generic {
			continue
		}
		depth, after := b.p.Maps[pc].Depth, b.p.Maps[pc+1].Depth
		operands := site.Argc + 1
		if site.Method {
			operands++
		}
		if site.Super || site.Instr != 0 {
			// Its arguments alone; an operation's operands.
			operands = site.Argc
		}
		if site.Forward && (site.Argc != 2 || site.Method) {
			continue
		}
		// A callee that reads its receiver gets the method call's, or a
		// construction's, from its pool, or a super()'s, the instance --
		// or, an arrow, the this it
		// captured (ArrowThis), which the call reads from its function
		// object: left out, an arrow reading its this, learned at a miss,
		// missed at every call -- AWFY CD's Vector.forEach, 45,000 exits a
		// run.
		if in := &b.p.Code[pc]; in.Window {
			// Register code's call: its operands are its window's
			// registers, not the operand stack's top (windowCall), the
			// site's shape the instruction's.
			super := site.Super && b.p.This && b.cur == b.root
			if site.Argc != in.Extra || site.Method != in.When || site.Super != super || site.Instr != 0 || site.Accessor != AccessorNone || site.Step || site.ForOf || site.ForIn ||
				site.TypeOf && (in.Extra != 0 || site.Go != GoCall) || site.InstanceOf && (in.Extra != 1 || site.Go != GoCall) || site.Literal || site.Via != 0 && (!in.When || in.Extra < 1) ||
				site.Go == 0 && !site.Push && !site.Pop && !site.Alloc && (site.Window != in.Left.Slot+1 && !super || site.ThisSlot >= 0 || site.Function ||
					site.Pool != 0 && site.Method) {
				// A native call's callee's window is made from the
				// instruction's (CallSite's Window); a super()'s, of
				// a derived constructor's own code, from its this slot and
				// its arguments (superCall). A push or a pop native code
				// makes itself has no frame, nor a built-in's construction
				// from a pool (Alloc).
				continue
			}
		} else if site.Argc < 0 || depth < operands || after != depth-operands+1 {
			continue
		}
		if site.ThisSlot >= 0 && !site.Method && site.Pool == 0 && !site.ArrowThis && !site.Super && !site.PlainThis && !site.Forward {
			continue
		}
		if site.ThisSlot < 0 && site.Window == 0 && site.Coerce {
			// A receiver it never reads needs no coercing: register code's
			// callee's is in its window, as the feedback says.
			copy := *site
			site = &copy
			site.Coerce = false
		}
		calls = b.f.appendSite(calls, site)
	}
	return calls
}

func (s *FrameState) addUse() {}

// binary is a slot IR binary operator on two unboxed numbers.
func (b *builder) binary(blk *Block, op ir.Operator, x, y *Value, boxF, boxB func(*Value) *Value) *Value {
	f := b.f
	switch op {
	case ir.Add:
		return boxF(f.newValue(blk, OpAddF64, Float64, x, y))
	case ir.Sub:
		return boxF(f.newValue(blk, OpSubF64, Float64, x, y))
	case ir.Mul:
		return boxF(f.newValue(blk, OpMulF64, Float64, x, y))
	case ir.Div:
		return boxF(f.newValue(blk, OpDivF64, Float64, x, y))
	case ir.Lt, ir.Le, ir.Gt, ir.Ge, ir.Eq, ir.Ne:
		c := f.newValue(blk, OpCmpF64, Bool, x, y)
		c.Aux = int(op)
		return boxB(c)
	}
	l := f.newValue(blk, OpToInt32, Int32, x)
	r := f.newValue(blk, OpToInt32, Int32, y)
	var v *Value
	switch op {
	case ir.BitAnd:
		v = f.newValue(blk, OpAndI32, Int32, l, r)
	case ir.BitOr:
		v = f.newValue(blk, OpOrI32, Int32, l, r)
	case ir.BitXor:
		v = f.newValue(blk, OpXorI32, Int32, l, r)
	case ir.Shl:
		v = f.newValue(blk, OpShlI32, Int32, l, r)
	case ir.Shr:
		v = f.newValue(blk, OpSarI32, Int32, l, r)
	case ir.UShr:
		return boxF(f.newValue(blk, OpU32ToF64, Float64, f.newValue(blk, OpShrU32, Int32, l, r)))
	default:
		panic(fmt.Sprintf("ssa: binary operator %d", op))
	}
	return boxF(f.newValue(blk, OpI32ToF64, Float64, v))
}

// sortedKeys is a map's keys in order, so that blocks are made the same
// every time.
func sortedKeys(m map[int]*frame) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
