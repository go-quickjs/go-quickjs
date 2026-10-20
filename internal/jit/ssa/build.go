package ssa

import (
	"errors"
	"fmt"
	"math"
	"slices"

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
	NativeCalls(pc int) []CallSite
}

// Intrinsic is a call of a built-in the VM knows, which native code makes
// itself, as V8 reduces Math.sqrt(x) to a square root in its code: the
// function object, which the call checks it calls (the VM keeps it
// alive), and what it computes of its argument, a number, or of its two
// for Math.pow (OpPowF64) and Math.atan2 and Math.hypot, or of any number
// from one for Math.max and Math.min -- another leaves the call to Go,
// which makes it.
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
	// (OpBigOfF64). BigInt.asUintN(n, x) and asIntN's are OpZextW's and
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
type ForEachSite struct {
	Fn       uintptr
	Kind     int
	Proto    uintptr
	Callback CallSite
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
	if kind&abi.LoopKindMask == abi.LoopReduce {
		return loopLocals + 6
	}
	return loopLocals + 5
}

// loopKeeps is how many keep cells a loop of kind takes: its callback's
// result's, and map's and filter's array's.
func loopKeeps(kind int) int {
	if makesArray(kind) {
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
}

// forEachAt is the forEach, or another loop, the method call at pc of the
// root, one Go makes (a GoCall site), makes on an array, if its feedback
// has one (ForEachFeedback): one argument, the callback, or two -- its
// this, reduce's initial value -- too. One that makes an array needs Go
// to make it (GoNewArray).
func (b *builder) forEachAt(pc int) (ForEachSite, bool) {
	calls := b.calls[pc]
	if b.fb == nil || b.cur != b.root || b.p.Code[pc].Op != ir.Host || len(calls) != 1 || calls[0].Go != GoCall || !calls[0].Method ||
		calls[0].Argc != 1 && calls[0].Argc != 2 || b.fb.Generic(pc) || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) {
		return ForEachSite{}, false
	}
	f, ok := b.fb.(ForEachFeedback)
	if !ok {
		return ForEachSite{}, false
	}
	site, ok := f.ForEach(pc)
	typed := site.Kind&abi.LoopTyped != 0
	if base := site.Kind & abi.LoopKindMask; !ok || base <= 0 || base >= abi.LoopKinds || makesArray(base) && (typed || !b.goCalls()) || typed && site.Proto == 0 {
		return ForEachSite{}, false
	}
	return site, true
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
	argc := b.calls[pc][0].Argc
	sp := b.p.Locals + b.p.Maps[pc].Depth
	dest := b.p.Locals + b.p.Maps[pc+1].Depth - 1
	recv, fn, cb := b.read(sp-argc-2, blk), b.read(sp-argc-1, blk), b.read(sp-argc, blk)
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
	// The callback a function, which each method checks before it reads
	// an element: Go's call throws its TypeError otherwise.
	callable := at(blk, call, OpTypeIs, Bool, cb)
	callable.Index = int(ir.TypeFunction)
	at(blk, call, OpCheckTrue, None, f.newValue(blk, OpBoxBool, Tagged, callable))
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
	boxF := func(blk *Block, x *Value) *Value { return f.newValue(blk, OpBoxF64, Tagged, x) }
	// The element at k: a typed array's a number, an array's from its cell
	// -- where it has one, but find's and findIndex's -- whatever it is.
	finds := kind == abi.LoopFind || kind == abi.LoopFindIndex
	element := func(blk *Block, st *FrameState, k *Value, present bool) *Value {
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
	// leaving the call to Go.
	out := undefined(blk)
	if makesArray(kind) {
		mk := at(blk, call, OpCall, Tagged, recv, boxF(blk, n))
		site := &CallSite{Go: GoNewArray, ThisSlot: -1}
		if kind == abi.LoopMap {
			site.Argc = 1
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
		third = b.read(sp-1, blk)
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
		b.write(dest, blk, whole)
	}
	loop := f.newValue(blk, OpLoopMark, None)
	loop.Const = ir.Value{Bits: uint64(mark)<<32 | uint64(base-b.root.p.Locals+1)}
	// The state inside the loop: the call's, and the loop's slots, depth of
	// its operands.
	state := func(blk *Block, depth int) *FrameState {
		s := f.newState(FrameState{PC: call.PC, Depth: call.Depth, Site: call.Site, Slots: f.refsOf(base + loopLocals + depth)})
		for i, v := range call.Slots {
			if v != nil {
				s.Slots[i] = v
				v.Uses++
			}
		}
		for i := base; i < base+loopLocals+depth; i++ {
			s.Slots[i] = b.read(i, blk)
			s.Slots[i].Uses++
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
	site := fp.site.Callback
	c := at(body, state(body, len(ops)), OpCall, Tagged, ops...)
	f.cache(c).Calls, c.Index = []*CallSite{&site}, fp.keep
	// The callback may have assigned a captured binding, as a callee may.
	if b.hasCells() {
		b.reloadAfterCall(body)
	}
	step := body
	if kind != abi.LoopForEach {
		// Its result, kept where the call left it: the loop's, which an
		// exit before the next index gives Go to finish the index with.
		kept := f.newValue(body, OpCallCell, Source, c)
		v := f.newValue(body, OpKept, Tagged, kept, c)
		v.Shadow = kept
		b.write(base+loopResult, body, v)
		switch kind {
		case abi.LoopReduce:
			b.write(base+loopThis, body, v)
		case abi.LoopMap:
			// Appended at the index, which is the array's length: the
			// elements before it all appended.
			st := state(body, 0)
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
				b.write(dest, yes, constant(yes, ir.Bool(true)))
			case abi.LoopEvery:
				b.write(dest, yes, constant(yes, ir.Bool(false)))
			case abi.LoopFind:
				b.write(dest, yes, b.read(base+loopElem, yes))
			case abi.LoopFindIndex:
				b.write(dest, yes, boxF(yes, k))
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
	done := f.newValue(cont, OpLoopMark, None)
	done.Const = ir.Value{}
	if reduce {
		b.write(dest, cont, b.read(base+loopThis, cont))
	}
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
	calls := b.calls[pc]
	if len(calls) != 1 || calls[0].Go != GoCall || !calls[0].Method || calls[0].Argc != 1 && calls[0].Argc != 2 {
		return 0, 0, false
	}
	argc := calls[0].Argc
	if b.p.Maps[pc].Depth < argc+2 || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-argc-1 {
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
func (b *builder) indexOfCall(blk *Block, pc int, fn uintptr, kind int, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	f := b.f
	argc := b.calls[pc][0].Argc
	sp := b.p.Locals + b.p.Maps[pc].Depth
	base := sp - argc - 2
	recv, callee := b.read(base, blk), b.read(base+1, blk)
	object := guard(OpObjectOf, Ptr, ir.HostExit, callee)
	same := guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	args := []*Value{recv}
	for k := range argc {
		args = append(args, b.read(base+2+k, blk))
	}
	site := &CallSite{Go: GoIndexOf, Argc: kind << 1, ThisSlot: -1}
	if argc == 2 {
		site.Argc |= IndexOfFrom
	}
	call := guard(OpCall, Tagged, ir.HostExit, args...)
	f.cache(call).Calls, call.Index = []*CallSite{site}, b.callKeeps[pc]
	kept := f.newValue(blk, OpCallCell, Source, call)
	r := f.newValue(blk, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.write(base, blk, r)
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
	calls := b.calls[pc]
	if len(calls) != 1 || calls[0].Go != GoCall || !calls[0].Method || calls[0].Argc > 1 {
		return 0, false
	}
	argc := calls[0].Argc
	if b.p.Maps[pc].Depth < argc+2 || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-argc-1 {
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
func (b *builder) bigStringCall(blk *Block, pc int, fn uintptr, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	f := b.f
	argc := b.calls[pc][0].Argc
	sp := b.p.Locals + b.p.Maps[pc].Depth
	base := sp - argc - 2
	recv, callee := b.read(base, blk), b.read(base+1, blk)
	object := guard(OpObjectOf, Ptr, ir.HostExit, callee)
	same := guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	args := []*Value{recv}
	for k := range argc {
		args = append(args, b.read(base+2+k, blk))
	}
	call := guard(OpCall, Tagged, ir.HostExit, args...)
	f.cache(call).Calls, call.Index = []*CallSite{{Go: GoBigString, Argc: argc, ThisSlot: -1}}, b.callKeeps[pc]
	kept := f.newValue(blk, OpCallCell, Source, call)
	r := f.newValue(blk, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.write(base, blk, r)
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
	calls := b.calls[pc]
	if len(calls) != 1 || calls[0].Go != GoCall || calls[0].Method || calls[0].New || calls[0].Argc != 1 ||
		b.p.Maps[pc].Depth < 2 || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-1 {
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
func (b *builder) parseIntCall(blk *Block, pc int, fn uintptr, guard func(Op, Type, ir.ExitKind, ...*Value) *Value, boxF func(*Value) *Value) {
	f := b.f
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	object := guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-2, blk))
	same := guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	x := guard(OpUnboxF64, Float64, ir.HostExit, b.read(sp-1, blk))
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
	guard(OpCheckTrue, None, ir.HostExit, f.newValue(blk, OpBoxBool, Tagged, cmp(ir.Lt, a, constant(1e21))))
	some := f.newValue(blk, OpMaxF64, Float64, number(cmp(ir.Ge, a, constant(1))),
		f.newValue(blk, OpMaxF64, Float64, number(cmp(ir.Eq, x, constant(0))), number(cmp(ir.Ge, x, constant(1e-6)))))
	guard(OpCheckTrue, None, ir.HostExit, f.newValue(blk, OpBoxBool, Tagged, cmp(ir.Gt, some, constant(0))))
	// The integer part, a zero's +0: the only number left whose integer
	// part is -0 is -0 itself.
	r := f.newValue(blk, OpAddF64, Float64, f.newValue(blk, OpTruncF64, Float64, x), constant(0))
	b.assign(b.cur.base+b.p.Locals+b.p.Maps[pc+1].Depth-1, blk, boxF(r))
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
}

// StringFeedback is Feedback that knows the property reads whose receivers
// have been strings (StringProperty): a method's, which String.prototype
// has as its own data property, by the shape it has (PropertySite's Shape)
// and the property's index in its table.
type StringFeedback interface {
	StringProperty(pc int) (PropertySite, bool)
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
	return b.typedArrays[recv] || b.typedRead(pc)
}

// typedRead reports whether the element read at pc has met typed arrays
// (ElementFeedback).
func (b *builder) typedRead(pc int) bool {
	f, ok := b.fb.(ElementFeedback)
	return ok && f.TypedRead(pc)
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
// have been strings, if any.
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
// pool's prototype; its result, if not an object, is the receiver. One with
// Alloc is a built-in's with nothing to run, `new Array()`: its result is
// the pool's object, whose prototype the realm fixed. One with Receiver is a
// construction's receiver alone, taken from the pool once its function's
// prototype is checked as for a construction, the constructor then inlined
// (InlineSite's Construct). One with Via calls
// Function.prototype.call, at Via, of the function at Callee, its receiver:
// that function is called, with the first argument as its receiver and
// the others as its arguments. One with Push calls Array.prototype.push,
// the realm's, at Callee, with one argument: on a dense array of its own,
// extensible, its length writable, with room, whose prototypes are Protos
// with their shapes and no elements -- none with an indexed property a
// setter could be asked about -- the argument is its new last element,
// and the result its length. One with Pop calls Array.prototype.pop, the
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
	New, Step                    bool
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
	// StartOnly says the callee's code makes its locals past its
	// parameters undefined as it starts (Func's StartOnly): the call leaves
	// them to it.
	StartOnly bool
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
	// (ir.ArrayLiteral), whose Argc operands are its elements.
	Literal bool
	// Fill, with Step, marks a destructuring's step (ir.IterStep's When),
	// whose result is undefined once the iterator is done, not the
	// uninitialized value a loop's step branches on.
	Fill bool
	// MapNexts, with Step, are the realm's Map and Set iterators' next
	// functions (MapStepFeedback): a cursor over an iterator whose next is
	// one of them native code steps itself, as V8 inlines a Map
	// iterator's next. Zero for none.
	MapNexts [2]uintptr
	// ForOf marks a for...of's or a destructuring's cursor made of the
	// iterable, its one operand.
	ForOf bool
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
	// Super marks a call of Go (GoCall) that is a derived constructor's
	// super(...) of its Argc operands, the arguments -- no function among
	// them -- as V8's code calls the parent constructor: Go makes it on
	// the code's own frame, binding its this, which is the result and
	// which the this slot takes. Its first operand is the this slot, the
	// uninitialized value unless an earlier super() bound it.
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
}

// ClosureSite is a closure's site that native code makes the closure at
// (ClosureFeedback): the address of the abi.ObjectPool its closures come
// from, which the VM keeps alive, and CallSite's Captures and LexThis.
type ClosureSite struct {
	Pool     uintptr
	Captures []int32
	LexThis  bool
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
// operand is the value, both its words. One into an array a loop made
// (StoreFresh, the operation's index 1) Go appends at the array's length
// as CreateDataProperty defines it, whatever the prototypes hold.
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
// GoConcat is concat (ir.StringConcat): Go converts its Argc operands to
// strings and joins them, as GoAdd adds; what could run code or throw it
// leaves to an exit, after which Go makes it.
//
// GoIndex is an element read as it is (ir.Instruction's Reference) of
// any receiver and key, the first operand and the second, as V8's code
// calls its generic keyed load: Go reads an object's property by a string
// key, or its element by an integer, where that runs nothing -- a data
// property, found by the runtime's shared property cache, or a dense
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
// room for that many elements if Argc is 1, map's, where the array's
// species is Array's -- no constructor of its own, Array.prototype's and
// Array[Symbol.species] as they were made -- as V8's code checks its
// ArraySpeciesProtector. It refuses any other, which leaves the call for Go
// to make after an exit.
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
)

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
		if !c.Alloc && !c.Receiver && !c.Push && (c.Go == 0 || c.Go == GoCall || c.Go == GoDelete || c.Go == GoDeleteStrict) {
			return false
		}
	}
	return len(v.Calls()) != 0
}

// InlineSite is a call the VM has seen call one function, whose program,
// lowered for inlining, Inlinable accepts: that program and its own
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
}

// Inlining's bounds: a callee's instructions, all callees' in a function,
// the calls a function inlines, and how deep: a callee's calls are inlined
// in it, as V8 inlines them, to this many levels.
const (
	maxInline      = 48
	maxInlineTotal = 192
	maxInlines     = 8
	maxInlineDepth = 3
)

// Inlinable reports whether a callee may be inlined: a small program that
// reads and writes properties and elements, computes, branches forward
// and returns, which native code can do all of, with no operation it
// leaves to Go, which would have Go finish the call every time. An exit
// inside it makes its frame (InlineState), so that what it did before is
// not done again.
func Inlinable(p *ir.Program) bool {
	calls, ok := InlineCalls(p)
	return ok && len(calls) == 0
}

// InlineCalls reports whether a callee may be inlined if the operations it
// leaves to Go, which it returns by PC, are calls inlined in it too: it is
// Inlinable but for those.
func InlineCalls(p *ir.Program) ([]int, bool) {
	if p == nil || p.Validate() != nil || len(p.Code) > maxInline || len(p.Globals) != 0 || p.Captured != nil || p.DerivedConstructor {
		// A callee whose locals closures capture: an inlined frame's
		// slots have no cells, and Go makes no closure on one. A derived
		// constructor's super() is Go's, on a frame of its own.
		return nil, false
	}
	var calls []int
	for pc, in := range p.Code {
		if !reachable(p, pc) {
			continue
		}
		switch in.Op {
		case ir.Call, ir.Closure, ir.CloseUpvalues:
			return nil, false
		case ir.Host:
			calls = append(calls, pc)
		case ir.Jump, ir.Branch:
			if in.Target <= pc {
				return nil, false
			}
		}
	}
	return calls, true
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
// more.
type GlobalSite struct {
	Key        uint32
	Index      int32
	Lexical    bool
	Fixed      bool
	Constant   ir.Value
	Holder     uintptr
	Shadowable bool
	Typed      bool
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
	// Scan, for a read whose cache is empty though it may have run -- the
	// VM's interpreter reads a small table without filling it, which only
	// its tree tier does -- has a receiver's own small table searched for
	// the key, plain data, and deoptimizes where it does not have it,
	// rather than at every receiver.
	Scan bool
}

// unfed reports whether the property operation at pc has a site that
// knows nothing to make it natively with -- no shape, no case, no
// addition, its cache not megamorphic -- and so always deoptimizes, as
// V8's code deoptimizes for insufficient feedback: its block ends there,
// with no entry after it.
func (b *builder) unfed(pc int) bool {
	switch b.p.Code[pc].Op {
	case ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead:
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

// Holder is a prototype a property read was answered by, or passed through,
// at its address, which the VM keeps alive, with the shape it had: 0 for
// none.
type Holder struct{ Object, Shape uintptr }

// BuildWith is Build with what the VM knows of the sites.
func BuildWith(p *ir.Program, fb Feedback) (*Func, error) { return build(nil, p, fb) }

func build(w *Workspace, p *ir.Program, fb Feedback) (*Func, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	p = referenceReads(p, fb)
	f := &Func{Locals: p.Locals, StackSize: p.StackSize, FrameLocals: p.Locals, ThisSlot: -1, ws: w}
	if kf, ok := fb.(KeepFeedback); ok {
		f.KeepLimit = kf.KeepLimit()
	}
	if af, ok := fb.(AppendFeedback); ok {
		f.ArrayProtos, _ = af.ArrayProtos()
	}
	f.written = f.bools(p.Locals + p.StackSize)
	f.assigned = p.Assigned
	b := &builder{p: p, fb: fb, f: f, nslots: p.Locals + p.StackSize}
	b.root = &frame{p: p, fb: fb}
	b.cur = b.root
	if err := b.plan(); err != nil {
		return nil, err
	}
	b.translate()
	bigLeafSites(f)
	// The shadows and the stores' checks are Optimize's to make, which
	// would remake them, or the first to run or compile f's (Finish).
	return b.f, nil
}

type builder struct {
	// p and fb are the program the block being translated is in, and its
	// feedback: the function's own (root), or an inlined callee's.
	p      *ir.Program
	fb     Feedback
	f      *Func
	nslots int

	// root is the function's own frame, cur the one translated now;
	// inlined are the callees, by the PC of their call, inlines how many
	// calls are inlined, at every level; frameOf is each block's frame, by
	// block ID, nil for the function's own.
	root, cur *frame
	inlined   map[int]*frame
	inlines   int
	// calls are the calls made natively (OpCall), by PC: their blocks go
	// on to the next instruction's.
	calls map[int][]*CallSite
	// callKeeps are, by PC, the keep cells of the calls' results (calls).
	callKeeps map[int]int
	// forEaches are the forEach calls the root makes a loop of, by PC
	// (forEachAt), decided in planning.
	forEaches map[int]forEachPlan
	// typedArrays are the global reads whose bindings held typed arrays
	// (GlobalSite's Typed), which element reads of them read as such.
	typedArrays map[*Value]bool
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

	entryPCs []int
	blockAt  []*Block // by the PC a block starts at
	// endOf is the last PC of each block, by block ID.
	endOf []int

	// By block ID, which is unique while the builder runs: each block's
	// definitions by slot, whether it is sealed and filled, and the phis
	// it made before it was sealed.
	defs       [][]*Value
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
		return !ok
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
		// an inlined callee's read where its closure's cells are known
		// (InlineSite's Upvalues); the rest Go's.
		return b.cur != b.root && (b.p.Code[pc].Op != ir.UpvalueRead || b.cur.site.Upvalues == 0)
	case ir.StringAdd, ir.StringConcat, ir.HasProperty, ir.DeleteProperty:
		// A call of Go (GoAdd, GoConcat, GoHas, GoDelete), where the VM
		// answers it.
		return !b.goCalls()
	case ir.FieldDefine:
		_, ok := b.define(pc)
		return !ok
	case ir.Rethrow:
		return b.generic(pc)
	case ir.ArgumentRead:
		// The call's arguments, an inlined callee's (InlineSite's
		// ReadsArguments), but where a read of them failed before.
		return b.cur == b.root || !b.cur.site.ReadsArguments || b.generic(pc)
	case ir.IterStep:
		// A call of Go (GoCall, Step), where the VM answers it.
		return !b.goCalls()
	case ir.Closure, ir.CloseUpvalues:
		// A call of Go (GoClosure, GoClose), on the function's own frame.
		return b.cur != b.root || !b.goCalls()
	}
	return false
}

// derivedReturnExits reports whether in, a return of the root's, is a
// derived constructor's of anything but undefined (ir.Program's
// DerivedConstructor), which Go makes: an object is the construction's
// result, anything else a TypeError.
func (b *builder) derivedReturnExits(in ir.Instruction) bool {
	return in.Op == ir.Return && b.p.DerivedConstructor && (in.Left.Slot >= 0 || in.Left.Literal.Kind != ir.Undefined)
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
// assigns (ir.Program's Assigned), which the call leaves as it is.
func (b *builder) reloadAfterCall(blk *Block) {
	if p := b.root.p; p.Upvalues != 0 || p.Assigned != nil {
		b.reloadCells(blk, p.Assigned)
	}
}

// hasCells reports whether the function has captured bindings, or locals
// closures capture: what a call may assign (reloadUpvalues).
func (b *builder) hasCells() bool { return b.root.p.Upvalues > 0 || b.root.p.Captured != nil }

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
	st := b.rootState()
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
	calls := b.calls[pc]
	return len(calls) == 1 && calls[0].Go == GoCall
}

// AppendFeedback is Feedback that lets numbers written at arrays' lengths
// be appended natively (Func.ArrayProtos): the realm's prototypes, with
// their shapes as the code is to find them, or false where none may be --
// a prototype with an index in its table, which a shape does not tell of.
type AppendFeedback interface {
	ArrayProtos() ([2]Holder, bool)
}

// KeepFeedback is Feedback that bounds the keep cells a build takes
// (Func.KeepLimit): abi.MaxKeeps when the backend could not fit the cells
// a build with none took into the context's (mir's ErrKeeps).
type KeepFeedback interface {
	KeepLimit() int
}

// generic reports an arithmetic operation or a comparison at pc whose
// speculation failed (Feedback's Generic): what are not numbers go to Go,
// which resumes after it, as they do for == and %.
func (b *builder) generic(pc int) bool {
	switch in := b.p.Code[pc]; {
	case in.Op == ir.Binary, in.Op == ir.Branch && in.Operator != ir.Truth, in.Op == ir.Rethrow, in.Op == ir.ArgumentRead,
		in.Op == ir.Unary && in.Operator != ir.Not, in.Op == ir.Update:
		return b.fb != nil && b.fb.Generic(pc)
	}
	return false
}

// frame is a program the builder translates: the function's own, or a
// callee inlined at one of its calls.
type frame struct {
	p  *ir.Program
	fb Feedback
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
}

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
		fr.answerAt = b.f.ints(len(b.p.Code))
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
	accessor, isAccessor := b.accessor(pc)
	if b.fb == nil || b.p.Code[pc].Op != ir.Host && !isAccessor || b.inlines >= maxInlines || b.cur.nest >= maxInlineDepth || !isAccessor && b.fb.Generic(pc) {
		return nil, false
	}
	var site InlineSite
	ok := false
	if isAccessor {
		// A getter or a setter, called with the receiver and, for a setter,
		// the value (AccessorSite).
		if accessor.Inline != nil {
			site, ok = *accessor.Inline, true
		}
	} else {
		site, ok = b.fb.Inline(pc)
	}
	var calls []int
	if ok {
		calls, ok = InlineCalls(site.Program)
	}
	if !ok || *total+len(site.Program.Code) > maxInlineTotal ||
		site.Argc < 0 || site.Params < 0 || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) {
		return nil, false
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
	if !isAccessor && (depth < callee || after != depth-callee+1 || site.ThisSlot >= 0 && !site.Method && !site.Construct && !site.Forward) {
		return nil, false
	}
	if site.Construct {
		// Its receiver is kept in a cell, as a call's result is; and it
		// returns nothing, so that the receiver is the result.
		if site.Pool == 0 || site.Method || b.f.Keeps >= b.f.keepLimit() {
			return nil, false
		}
		for pc, in := range site.Program.Code {
			if reachable(site.Program, pc) && in.Op == ir.Return && in.Left != ir.Literal(ir.Value{Kind: ir.Undefined}) {
				return nil, false
			}
		}
	}
	q := referenceReads(site.Program, site.Feedback)
	// Its slots follow the function's and earlier callees': every slot's
	// number stays below abi.MaxRecords, as records name slots by number.
	if b.nslots+q.Locals+q.StackSize > abi.MaxRecords {
		return nil, false
	}
	nslots, before, inlines, keeps := b.nslots, *total, b.inlines, b.f.Keeps
	*total += len(site.Program.Code)
	fr := &frame{p: q, fb: site.Feedback, site: site, result: b.cur.base + b.p.Locals + after - 1, base: b.nslots,
		parent: b.cur, depth: b.cur.depth + 1, nest: b.cur.nest + 1, keep: -1}
	if isAccessor {
		// A getter's result is the read's; a setter's is dropped.
		fr.result = b.cur.base + b.p.Code[pc].Dest
		if accessor.Setter {
			fr.result = -1
		}
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
	if !site.Restart && !site.Arguments {
		// A forwarding constructor is its calls of the methods it forwards
		// to, which count: not a callee more toward maxInlines.
		b.inlines++
	}
	if len(calls) != 0 {
		cur := b.cur
		b.enter(fr)
		for _, at := range calls {
			if b.nativeHost(at) {
				continue
			}
			child, ok := b.inlineAt(at, total)
			if !ok {
				b.enter(cur)
				b.nslots, *total, b.inlines, b.f.Keeps = nslots, before, inlines, keeps
				return nil, false
			}
			if fr.inlined == nil {
				fr.inlined = map[int]*frame{}
			}
			fr.inlined[at] = child
		}
		b.enter(cur)
	}
	b.inlineAccessors(fr, total)
	return fr, true
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
		blk := b.f.newBlock(pc)
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

// inlineCall checks the call at pc calls the function inlined there, and
// gives the callee's start its slots: its arguments, past which undefined,
// its receiver, and undefined in every other.
func (b *builder) inlineCall(blk *Block, pc int, fr *frame, guard func(Op, Type, ir.ExitKind, ...*Value) *Value, state func() *FrameState) {
	site := fr.site
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	if site.Forward {
		b.forwardCall(blk, pc, sp, fr, guard)
		return
	}
	// Another callee is called by Go, from the call's state, as a call that
	// is not inlined is, and native code goes on after it.
	object := guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-site.Argc-1, blk))
	same := guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(site.Callee)}
	if site.Coerce && site.ThisSlot >= 0 {
		// A receiver that is not an object is coerced, which Go does.
		guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-site.Argc-2, blk))
	}
	// Room past the operands for the callee's frame, which an exit inside
	// it writes there, and a context for Go to make it from, and one for
	// each inlined caller it is in.
	room := guard(OpFrameRoom, None, ir.HostExit)
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
		call := guard(OpCall, Tagged, ir.HostExit, args...)
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
	if site.ThisSlot >= 0 && site.Construct {
		fr.init[site.ThisSlot] = fr.receiver
	} else if site.ThisSlot >= 0 {
		fr.init[site.ThisSlot] = b.read(sp-site.Argc-2, blk)
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
func (b *builder) goAdd(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	release, old := b.bigRelease(blk, b.herePC, in)
	args := []*Value{operand(in.Left), operand(in.Right)}
	if old != nil {
		args = append(args, old)
	}
	call := guard(OpCall, Tagged, ir.HostExit, args...)
	if b.f.Keeps < b.f.keepLimit() {
		fb := b.bigFeedback()
		big := fb != nil && b.cur == b.root && fb.BigInt(b.herePC)
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoAdd, ThisSlot: -1, Release: release, Big: big}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
}

// goCompare is in, a relational comparison whose guard has failed, by a
// call of Go from native code (GoCompare): its result, a boolean.
func (b *builder) goCompare(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) *Value {
	call := guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
	if b.f.Keeps < b.f.keepLimit() {
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoCompare, Argc: int(in.Operator), ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	return r
}

// goArithAt reports whether the root's operator op at pc met BigInts
// (BigIntFeedback) and is generic, or has no native form for BigInts held
// in their values (bigNative): Go makes it from native code (GoArith). A
// generic addition is GoAdd's, a comparison GoCompare's.
func (b *builder) goArithAt(pc int, op ir.Operator) bool {
	switch op {
	case ir.Sub, ir.Mul:
		if !b.generic(pc) {
			return false
		}
	case ir.Div, ir.Mod, ir.Pow, ir.BitAnd, ir.BitOr, ir.BitXor, ir.Shl, ir.Shr:
	default:
		return false
	}
	fb, ok := b.fb.(BigIntFeedback)
	return ok && b.cur == b.root && fb.BigInt(pc) && b.goCalls()
}

// goArith makes in, at a site goArithAt names, by a call of Go from native
// code (GoArith), as goCompare makes a comparison.
func (b *builder) goArith(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) *Value {
	release, old := b.bigRelease(blk, b.herePC, in)
	args := []*Value{operand(in.Left), operand(in.Right)}
	if old != nil {
		args = append(args, old)
	}
	call := guard(OpCall, Tagged, ir.HostExit, args...)
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
func (b *builder) goHas(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	call := guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
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
func (b *builder) goDelete(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	call := guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
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
func (b *builder) goIndex(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	call := guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
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
func (b *builder) goKeyedWrite(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	call := guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right), operand(in.Third))
	if b.f.Keeps < b.f.keepLimit() {
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoKeyedWrite, ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	}
}

// iterStep makes in, a loop's step, by a call of Go from native code
// (GoCall, Step) -- as the tree tier's step calls iterNext, where an exit
// and an entry at every turn had cost a small loop's body four times over
// -- and ends blk with the branch on its answer: the uninitialized value,
// which no step yields, for a cursor done, any other its next value. What
// Go refuses leaves for Go to make after an exit.
func (b *builder) iterStep(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	call := guard(OpCall, Tagged, ir.HostExit, operand(in.Left))
	if b.f.Keeps < b.f.keepLimit() {
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoCall, Step: true, Fill: in.When, ThisSlot: -1, MapNexts: b.mapNexts()}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
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
	is := guard(OpEqTagged, Bool, ir.HostExit, r, done)
	is.Index = 1
	blk.Control = is
	is.Uses++
}

// goFrame makes in, a closure or the end of a scope that closes what
// closures captured, by a call of Go from native code on the function's
// own frame (GoClosure, GoClose), as the interpreter makes them: the
// captured locals are where the frame has them, as native code assigns
// them there (storeCaptured). What Go refuses -- a frame it has none of,
// as a native callee's -- leaves for Go to make after an exit.
func (b *builder) goFrame(blk *Block, pc int, in ir.Instruction, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	call := guard(OpCall, Tagged, ir.HostExit)
	site := &CallSite{Go: GoClosure, Argc: int(in.Key), ThisSlot: -1}
	if in.Op == ir.CloseUpvalues {
		site.Go = GoClose
	}
	if fb, ok := b.fb.(ClosureFeedback); ok && in.Op == ir.Closure {
		// Natively, from its pool: what it captures of the code's locals
		// is in cells of their own, none open on the frame -- or it
		// captures none, only its maker's own captured bindings and an
		// arrow's this, as AWFY Havlak's findSet's arrow does, which Go had
		// made at every call, 414,000 a run.
		if cs, ok := fb.Closure(pc); ok && cs.Pool != 0 && (b.p.Cells || !capturesLocals(cs.Captures)) {
			site.Pool, site.Captures, site.LexThis = cs.Pool, cs.Captures, cs.LexThis
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
	if in.Op == ir.CloseUpvalues && b.f.Keeps >= b.f.keepLimit() {
		// A close writes no keep cell (the VM's GoClose): any one does,
		// the first, when they are all taken.
		call.Index = 0
	} else {
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
func (b *builder) goConcat(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	parts := make([]*Value, in.Extra)
	for i := range parts {
		parts[i] = operand(ir.Slot(in.Dest + i))
	}
	call := guard(OpCall, Tagged, ir.HostExit, parts...)
	if b.f.Keeps < b.f.keepLimit() {
		b.f.cache(call).Calls, call.Index = []*CallSite{{Go: GoConcat, Argc: len(parts), ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
}

// nativeHost reports whether native code makes the host operation at pc
// itself: an intrinsic call or instanceof the VM knows.
func (b *builder) nativeHost(pc int) bool {
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

// fromCharCode is the address of String.fromCharCode if the method call of
// one argument at pc calls it (FromCharCodeFeedback), which fromCharCodeCall
// makes.
func (b *builder) fromCharCode(pc int) (uintptr, bool) {
	if b.fb == nil || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) ||
		b.p.Maps[pc].Depth < 3 || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-2 {
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
// slot, with nothing made; anything else leaves for Go.
func (b *builder) fromCharCodeCall(blk *Block, pc int, fn uintptr, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	object := guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-2, blk))
	same := guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	code := guard(OpUnboxF64, Float64, ir.HostExit, b.read(sp-1, blk))
	cell := guard(OpCodeChar, Source, ir.HostExit, code)
	v := b.f.newValue(blk, OpLoadCell, Tagged, cell)
	v.Shadow = cell
	b.assign(sp-3, blk, v)
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
	calls := b.calls[pc]
	if len(calls) != 1 || calls[0].Go != GoCall || !calls[0].Method || calls[0].Argc != 1 && calls[0].Argc != 2 {
		return 0, 0, false
	}
	argc := calls[0].Argc
	if b.p.Maps[pc].Depth < argc+2 || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-argc-1 {
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
func (b *builder) substringCall(blk *Block, pc int, fn uintptr, kind int, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	f := b.f
	argc := 2
	if kind == SubstrAt {
		argc = 1
	}
	sp := b.p.Locals + b.p.Maps[pc].Depth
	base := sp - argc - 2
	operands := make([]*Value, argc+2)
	for k := range operands {
		operands[k] = b.read(base+k, blk)
	}
	recv, callee := operands[0], operands[1]
	object := guard(OpObjectOf, Ptr, ir.HostExit, callee)
	same := guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(fn)}
	args := []*Value{recv}
	for _, x := range operands[2:] {
		args = append(args, guard(OpUnboxF64, Float64, ir.HostExit, x))
	}
	cell := f.newValue(blk, OpSubstringChar, Source, args...)
	cell.Aux = kind
	none := f.newValue(blk, OpCellNone, Bool, cell)
	st := b.state(blk, pc)
	miss, hit, _ := b.split(blk, none, true)
	v := f.newValue(hit, OpLoadCell, Tagged, cell)
	v.Shadow = cell
	b.write(base, hit, v)
	call := f.newValue(miss, OpCall, Tagged, operands...)
	call.Aux, call.State = int(ir.HostExit), st
	st.addUse()
	f.cache(call).Calls, call.Index = b.calls[pc], b.callKeeps[pc]
	kept := f.newValue(miss, OpCallCell, Source, call)
	r := f.newValue(miss, OpKept, Tagged, kept, call)
	r.Shadow = kept
	b.write(base, miss, r)
	// A receiver Go converts may run script, which may assign a captured
	// binding, as a callee may.
	if b.hasCells() {
		b.reloadAfterCall(miss)
	}
}

// charCode reports whether the method call of one argument at pc calls
// String.prototype.charCodeAt (CharCodeFeedback), which charCodeCall
// makes.
func (b *builder) charCode(pc int) bool {
	if b.fb == nil || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) ||
		b.p.Maps[pc].Depth < 3 || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-2 {
		return false
	}
	f, ok := b.fb.(CharCodeFeedback)
	return ok && f.CharCode(pc)
}

// charCodeCall makes the call of String.prototype.charCodeAt at pc: its
// receiver a flat string, its argument an integer below its length, and
// its function the intrinsic (OpStringCode), the code unit there is its
// result, in the call's result slot; anything else leaves for Go.
func (b *builder) charCodeCall(blk *Block, pc int, guard func(Op, Type, ir.ExitKind, ...*Value) *Value, boxF func(*Value) *Value) {
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	key := guard(OpUnboxF64, Float64, ir.HostExit, b.read(sp-1, blk))
	code := guard(OpStringCode, Float64, ir.HostExit, b.read(sp-2, blk), b.read(sp-3, blk), key)
	b.assign(sp-3, blk, boxF(code))
}

// instanceOf is the instanceof at pc in the frame being translated, if the
// VM knows its constructor (InstanceOfFeedback): its operands the value and
// the constructor, its result in the value's slot.
func (b *builder) instanceOf(pc int) (InstanceOfSite, bool) {
	if b.fb == nil || b.p.Code[pc].Op != ir.Host || pc+1 >= len(b.p.Code) || !reachable(b.p, pc+1) ||
		b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-1 || b.p.Maps[pc].Depth < 2 {
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
func (b *builder) instanceOfOp(blk *Block, pc int, k InstanceOfSite, guard func(Op, Type, ir.ExitKind, ...*Value) *Value, boxB func(*Value) *Value) {
	f := b.f
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	ctor := guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-1, blk))
	same := guard(OpSameObject, None, ir.HostExit, ctor)
	same.Const = ir.Value{Bits: uint64(k.Ctor)}
	read := func(site PropertySite) *Value {
		cell := guard(OpPropCell, Source, ir.HostExit, ctor)
		cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
		if site.Holders[0].Object != 0 {
			b.f.cache(cell).Holders = &HolderChain{Holders: site.Holders, More: site.More}
		}
		v := f.newValue(blk, OpLoadCell, Tagged, cell)
		v.Shadow = cell
		return v
	}
	has := guard(OpObjectOf, Ptr, ir.HostExit, read(k.HasInstance))
	fn := guard(OpSameObject, None, ir.HostExit, has)
	fn.Const = ir.Value{Bits: uint64(k.HasInstanceFn)}
	proto := guard(OpObjectOf, Ptr, ir.HostExit, read(k.Prototype))
	yes := guard(OpInstanceOf, Bool, ir.HostExit, b.read(sp-2, blk), proto)
	b.assign(b.cur.base+b.p.Locals+b.p.Maps[pc+1].Depth-1, blk, boxB(yes))
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
	case OpSqrtF64, OpAbsF64, OpPowF64, OpMaxF64, OpMinF64, OpFloorF64, OpCeilF64, OpTruncF64, OpRoundF64, OpSignF64, OpFroundF64:
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
	recv := 1
	if k.Plain {
		recv = 0
	}
	if n := k.args(); n < 0 || n == 0 && k.Op != OpRandomF64 || n > maxIntrinsicArgs || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-n-recv || b.p.Maps[pc].Depth < n+1+recv {
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
	case k.Op == OpPowF64 || k.Op == OpMaxF64 || k.Op == OpMinF64 || k.Op == OpMathCall && k.Go != GoMath, k.Op == OpZextW || k.Op == OpSextW:
		return 2
	}
	return 1
}

// intrinsicCall makes the intrinsic call at pc (Intrinsic): checked to call
// the function, its argument a number -- either not, Go makes the call --
// its result is computed, in the call's result slot.
func (b *builder) intrinsicCall(blk *Block, pc int, k Intrinsic, guard func(Op, Type, ir.ExitKind, ...*Value) *Value, boxF func(*Value) *Value) {
	switch k.Op {
	case OpZextW, OpSextW:
		b.asNCall(blk, pc, k, guard)
		return
	case OpBigOfF64:
		b.bigOfCall(blk, pc, k, guard)
		return
	}
	sp := b.cur.base + b.p.Locals + b.p.Maps[pc].Depth
	n := k.args()
	object := guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-n-1, blk))
	same := guard(OpSameObject, None, ir.HostExit, object)
	same.Const = ir.Value{Bits: uint64(k.Callee)}
	var xs [maxIntrinsicArgs]*Value
	for i := range n {
		xs[i] = guard(OpUnboxF64, Float64, ir.HostExit, b.read(sp-n+i, blk))
	}
	var r *Value
	switch k.Op {
	case OpPowF64:
		r = guard(OpPowF64, Float64, ir.HostExit, xs[0], xs[1])
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
	b.assign(b.cur.base+b.p.Locals+b.p.Maps[pc+1].Depth-1, blk, boxF(r))
}

// forwardCall checks a forwarded call (InlineSite's Forward) calls the
// function inlined there through the realm's apply, and gives the callee's
// start its slots: the arguments the frame it is in was given, past which
// undefined, its receiver, and undefined in every other. The call's state,
// where the frames of an exit inside the callee go on, is the frame's at
// the call, not its call's: the frame is made there.
func (b *builder) forwardCall(blk *Block, pc, sp int, fr *frame, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) {
	site := fr.site
	target := guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-3, blk))
	same := guard(OpSameObject, None, ir.HostExit, target)
	same.Const = ir.Value{Bits: uint64(site.Callee)}
	apply := guard(OpObjectOf, Ptr, ir.HostExit, b.read(sp-2, blk))
	same = guard(OpSameObject, None, ir.HostExit, apply)
	same.Const = ir.Value{Bits: uint64(site.Apply)}
	room := guard(OpFrameRoom, None, ir.HostExit)
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
func (b *builder) globalCell(site GlobalSite, guard func(Op, Type, ir.ExitKind, ...*Value) *Value, kind ir.ExitKind) *Value {
	cell := guard(site.cell(), Source, kind)
	cell.Index, cell.Key = int(site.Index), site.Key
	if site.Holder != 0 {
		cell.Const, cell.Global = ir.Value{Bits: uint64(site.Holder)}, site.Shadowable
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
	for pc := range p.Code {
		if !reachable(p, pc) {
			continue
		}
		if fr, ok := b.inlineAt(pc, &total); ok && b.host(pc) {
			// Not a call native code makes itself (nativeHost) -- an
			// intrinsic its feedback names, though another site's
			// feedback has it inline the function the call met.
			if b.inlined == nil {
				b.inlined = map[int]*frame{}
			}
			b.inlined[pc] = fr
		}
	}
	// Go goes on natively after what it ran where it is sure to run it:
	// an instruction left to Go whole; a call, which leaves while the
	// collector marks; and an element's write, which leaves past the
	// elements, as an array that grows is written. Anywhere else an exit
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
		case b.host(pc), b.keyedRead(pc), op == ir.ObjectLiteral, op == ir.ArrayLiteral, op == ir.StringAdd, op == ir.StringConcat, op == ir.ArrayWrite, op == ir.IterStep,
			op == ir.IterClose, op == ir.HasProperty, op == ir.DeleteProperty,
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
		if b.host(pc) && b.inlined[pc] == nil && b.f.Keeps < b.f.keepLimit() {
			if calls := b.nativeCalls(pc); len(calls) != 0 {
				if b.calls == nil {
					b.calls, b.callKeeps = map[int][]*CallSite{}, map[int]int{}
				}
				b.calls[pc], b.callKeeps[pc] = calls, b.f.Keeps
				b.f.Keeps++
				if site, ok := b.forEachAt(pc); ok && b.nslots+loopSlots(site.Kind) <= abi.MaxRecords && b.f.Keeps+loopKeeps(site.Kind) <= b.f.keepLimit() {
					// A forEach made a loop of: its slots past every
					// other's, and its callback's result's keep cell --
					// and its array's, map's or filter's.
					if b.forEaches == nil {
						b.forEaches = map[int]forEachPlan{}
					}
					b.forEaches[pc] = forEachPlan{site: site, base: b.nslots, keep: b.f.Keeps}
					b.nslots += loopSlots(site.Kind)
					b.f.Keeps += loopKeeps(site.Kind)
					b.f.Loops, b.f.LoopsEnd = true, b.nslots
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
		case ir.Nop, ir.Copy, ir.CopyPair, ir.StoreLoad, ir.Swap, ir.Insert2, ir.Insert3,
			ir.Return, ir.ArrayKey:
		case ir.UpvalueRead:
			// The function's own captured binding is read natively, its
			// check deoptimizing; an inlined callee's leaves at its call.
		case ir.IterClose:
			// Go goes on after a closing it made.
			entries[pc+1] = true
		case ir.IterStep:
			// Go goes on after a step it made, past it or at the loop's
			// end (ir.Program's Entries), where native code did not; a
			// destructuring's step only past it.
			if !in.When {
				leaders[in.Target] = true
			}
			if resumes(pc) {
				entries[pc+1] = true
			}
		case ir.Rethrow:
			// A normal completion goes on natively, any other deoptimizing;
			// once one has (Generic), Go runs each, and native code goes on
			// after a normal one.
			if b.host(pc) && resumes(pc) {
				entries[pc+1] = true
			}
		case ir.TypeTest:
			// An object it cannot tell exits at the typeof, which Go makes;
			// the interpreter goes on, the boolean not where the typeof's
			// string was (compile's typeTests): no entry after it.
		case ir.ArrayWrite, ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead, ir.BindingRead,
			ir.StringMethod, ir.StringCode, ir.BindingWrite, ir.BindingCheck, ir.Resolved, ir.ObjectLiteral, ir.FieldDefine,
			ir.ArrayLiteral, ir.StringConst, ir.BigIntConst, ir.UpvalueWrite, ir.StringAdd, ir.StringConcat, ir.Closure, ir.CloseUpvalues, ir.HasProperty,
			ir.DeleteProperty, ir.ArgumentRead:
			// What native code does not do exits to Go, which resumes after
			// it. A fixed global's check only deoptimizes: the binding can
			// never move, so it never fails, and what follows sees its
			// constant rather than a merge with a slot loaded at an entry.
			if site, ok := b.global(pc); (!ok || !site.Fixed) && resumes(pc) {
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
			if b.goesOn(pc) {
				// A call Go makes goes on natively: Go leaves it only for
				// a throw, the runtime stopping, or the code to be compiled
				// again -- then the interpreter runs on to the next loop
				// header, as from V8's deoptimizations.
			} else if resumes(pc) {
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
	b.endOf = make([]int, len(b.f.Blocks))
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
			if b.derivedReturnExits(last) {
				blk.Kind = BlockExit
			}
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
			ir.Closure, ir.CloseUpvalues, ir.HasProperty, ir.DeleteProperty:
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
			if len(b.calls[end]) != 0 {
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
	b.frameOf = make([]*frame, len(b.f.Blocks))
	for _, pc := range sortedKeys(b.inlined) {
		b.planInline(b.inlined[pc], b.inlined[pc].callBlock, b.blockAt[pc+1])
	}
	// Entry blocks load the live slots and go to the block for their PC.
	for _, pc := range b.entryPCs {
		e := b.f.newBlock(-1)
		e.Kind = BlockPlain
		e.Generic = b.fb != nil && b.fb.EntryGeneric(int(p.Maps[pc].PC))
		b.edge(e, b.blockAt[pc])
		b.f.Entries = append(b.f.Entries, Entry{PC: pc, Depth: p.Maps[pc].Depth, Block: e})
	}
	// A loop header is reached by an edge from a block at or after it, in
	// one frame: a callee's blocks have their own PCs, and no loops.
	for _, blk := range b.f.Blocks {
		blk.Backedge = b.f.bools(len(blk.Preds))
		for i, pred := range blk.Preds {
			if pred.PC >= 0 && blk.PC >= 0 && pred.PC >= blk.PC && b.frameAt(pred) == b.root && b.frameAt(blk) == b.root {
				blk.Backedge[i] = true
				blk.LoopHeader = true
			}
		}
	}
	return nil
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
	b.defs = b.f.refLists(n)
	b.sealed, b.filled = b.f.bools(n), b.f.bools(n)
	b.incomplete = make([][]slotPhi, n)

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
	d := b.defs[blk.ID]
	if d == nil {
		d = b.f.refsOf(b.nslots)
		b.defs[blk.ID] = d
	}
	d[slot] = v
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
	if d := b.defs[blk.ID]; d != nil && d[slot] != nil {
		return d[slot]
	}
	var v *Value
	switch {
	case !b.sealed[blk.ID]:
		v = b.newPhi(blk)
		b.incomplete[blk.ID] = append(b.incomplete[blk.ID], slotPhi{slot, v})
	case len(blk.Preds) == 1:
		v = b.readVar(slot, blk.Preds[0])
	case len(blk.Preds) == 0 && blk.PC < 0 && slot < len(blk.Header.Slots):
		// A local an entry did not load, as the interpreter's liveness has
		// it dead there (fill): the frame's, as an entry loads it.
		v = b.f.newValue(blk, OpLoadSlot, Tagged)
		v.Aux = slot
	case len(blk.Preds) == 0:
		// A slot read where nothing defined it: an operand slot past an
		// entry's depth. The slot IR never reads one; this keeps the form
		// total.
		v = b.constIn(blk, ir.Value{Kind: ir.Undefined})
	default:
		v = b.newPhi(blk)
		b.write(slot, blk, v)
		b.addPhiOperands(slot, v)
	}
	b.write(slot, blk, v)
	return v
}

func (b *builder) newPhi(blk *Block) *Value {
	v := b.f.alloc(Value{Op: OpPhi, Type: Tagged, Block: blk})
	prepend(b.f, blk, v)
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
	if phi.Args == nil {
		phi.Args = b.f.refsOf(len(preds))[:0]
	}
	for _, p := range preds {
		a := b.readVar(slot, p)
		if a.Op == OpUpvalueStale {
			// The binding as the predecessor leaves it, from its cell.
			a = b.loadUpvalue(p, a.Index, nil)
			b.write(slot, p, a)
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
func (b *builder) equality(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) *Value {
	if in.Operator != ir.Eq && in.Operator != ir.Ne {
		return nil
	}
	if c := b.nullish(blk, in, operand, guard); c != nil {
		return c
	}
	c := guard(OpEqTagged, Bool, ir.HostExit, operand(in.Left), operand(in.Right))
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
func (b *builder) nullish(blk *Block, in ir.Instruction, operand func(ir.Operand) *Value, guard func(Op, Type, ir.ExitKind, ...*Value) *Value) *Value {
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
		c = guard(OpLooseNullish, Bool, ir.HostExit, x)
	}
	if in.Operator == ir.Ne {
		c = b.f.newValue(blk, OpNot, Bool, c)
	}
	return c
}

// state captures the frame at a PC: every live slot's current value. Inside
// an inlined callee it is the caller's at the call and the callee's
// (InlineState).
func (b *builder) state(blk *Block, pc int) *FrameState {
	if fr := b.cur; fr != b.root && fr.site.Restart {
		// Go makes the call over again (InlineSite's Restart): the
		// caller's state at it, a copy, as states are not shared.
		call := fr.call
		s := b.f.newState(FrameState{PC: call.PC, Depth: call.Depth, Site: call.Site, Slots: b.f.refsOf(len(call.Slots)), Inline: call.Inline})
		for i, v := range call.Slots {
			if v != nil {
				s.Slots[i] = v
				v.Uses++
			}
		}
		return s
	}
	return b.frameState(blk, pc)
}

// frameState is the state at pc in the frame being translated, its
// callers' at their calls below it.
func (b *builder) frameState(blk *Block, pc int) *FrameState {
	if fr := b.cur; fr != b.root {
		call, depth := fr.call, b.p.Maps[pc].Depth
		s := b.f.newState(FrameState{PC: call.PC, Depth: call.Depth, Site: call.Site, Slots: b.f.refsOf(fr.base + b.p.Locals + depth)})
		for i, v := range call.Slots {
			if v != nil {
				s.Slots[i] = v
				v.Uses++
			}
		}
		for i := fr.base; i < len(s.Slots); i++ {
			if !b.p.LiveAt(pc, i-fr.base) {
				// The callee's local, dead there, as the caller's below.
				continue
			}
			s.Slots[i] = b.read(i, blk)
			s.Slots[i].Uses++
		}
		s.Inline = &InlineState{Parent: call.Inline, Closure: fr.site.Closure, Callee: fr.site.Callee, Base: fr.base, Locals: b.p.Locals, ThisSlot: fr.site.ThisSlot,
			PC: b.p.Maps[pc].PC, Depth: depth, Site: pc}
		if fr.site.Arguments || fr.site.ReadsArguments {
			s.Inline.Args = uint64(len(fr.args)+1)<<32 | uint64(fr.base-fr.argSlot)
		}
		return s
	}
	depth := b.p.Maps[pc].Depth
	s := b.f.newState(FrameState{PC: b.p.Maps[pc].PC, Depth: depth, Slots: b.f.refsOf(b.p.Locals + depth), Site: pc})
	for i := range s.Slots {
		if !b.p.LiveAt(pc, i) || i >= b.p.UpvalueBase && i < b.p.UpvalueBase+b.p.Upvalues || b.p.IsCaptured(i) {
			// A local the interpreter writes before it reads from here:
			// not written, nor kept alive for the exit, as V8 leaves dead
			// registers out of a frame state. Nor is a captured binding,
			// which is in its cell.
			continue
		}
		s.Slots[i] = b.read(i, blk)
		s.Slots[i].Uses++
		if b.words != nil && b.words[s.Slots[i]] != nil {
			// An operator made on low bits alone (wraps) has no value an
			// exit could write: its guards exit to the state before it.
			panic("ssa: a BigInt's low bits in a frame state")
		}
	}
	return s
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
		blk.Header = f.newState(FrameState{PC: b.p.Maps[e.PC].PC, Depth: e.Depth, Slots: f.refsOf(b.p.Locals + e.Depth), Site: -1})
		for i := range blk.Header.Slots {
			if !b.p.LiveAt(e.PC, i) || i >= b.p.UpvalueBase && i < b.p.UpvalueBase+b.p.Upvalues || b.p.IsCaptured(i) {
				// A local the interpreter writes before it reads from
				// here is not loaded, as frame states leave it out: an
				// entry's state names a slot's own load, which an exit
				// leaves as it is, and the load had been dead but for
				// that. Read anyway, it is loaded then (read). Nor is a
				// captured binding's slot, as frame states leave it out: the
				// binding is in its cell, read there where it is read
				// (reloadUpvalues), and the VM's frame has no slot for it.
				// Loaded at every entry, a function of many bindings and
				// many calls made a load of each at each, that the entry's
				// state alone kept: RegExp's runBlock11, 34,767 of them.
				continue
			}
			v := f.newValue(blk, OpLoadSlot, Tagged)
			v.Aux = i
			b.write(i, blk, v)
			blk.Header.Slots[i] = v
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
	end := b.endOf[blk.ID]
	// BigInt operators made on their low bits end in their block (wraps).
	b.region, b.words = nil, nil
	for pc := blk.PC; pc <= end; pc++ {
		b.instruction(blk, pc)
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
	made := []*Block{yes}
	if both {
		no = b.blockAfter(blk)
		made = append(made, no)
	}
	cont = b.blockAfter(blk)
	made = append(made, cont)
	b.placeAfter(blk, made, cont)
	blk.Kind, blk.Control, blk.State, blk.ExitKind = BlockIf, cond, nil, 0
	cond.Uses++
	yes.Kind, yes.Succs, yes.Preds, yes.Backedge = BlockPlain, []*Block{cont}, []*Block{blk}, []bool{false}
	if both {
		no.Kind, no.Succs, no.Preds, no.Backedge = BlockPlain, []*Block{cont}, []*Block{blk}, []bool{false}
		blk.Succs = []*Block{yes, no}
		cont.Preds, cont.Backedge = []*Block{yes, no}, []bool{false, false}
	} else {
		blk.Succs = []*Block{yes, cont}
		cont.Preds, cont.Backedge = []*Block{blk, yes}, []bool{false, false}
	}
	for _, x := range made {
		b.seal(x)
	}
	b.splits = append(b.splits, made...)
	b.next = cont
	return yes, no, cont
}

// blockAfter is a new block of blk's PC and frame, for split and loop.
func (b *builder) blockAfter(blk *Block) *Block {
	nb := b.f.newBlock(blk.PC)
	nb.ID = b.nextBlock
	b.nextBlock++
	// The block's ID past every list's, which each grows to.
	for len(b.defs) <= nb.ID {
		b.defs = append(b.defs, nil)
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
	f.Blocks = f.Blocks[:len(f.Blocks)-len(made)]
	at := 0
	for i, x := range f.Blocks {
		if x == blk {
			at = i + 1
			break
		}
	}
	rest := append(made, f.Blocks[at:]...)
	f.Blocks = append(f.Blocks[:at], rest...)
	cont.Kind, cont.Control, cont.Succs, cont.State, cont.ExitKind = blk.Kind, blk.Control, blk.Succs, blk.State, blk.ExitKind
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
	var st *FrameState
	state := func() *FrameState {
		if root && b.region != nil {
			// Inside BigInt operators made on their low bits (wraps): the
			// state before the first of them, which nothing since has
			// changed but those operators, which Go makes over again.
			return b.region
		}
		if root {
			return b.rootState()
		}
		if st == nil {
			st = b.state(blk, pc)
		}
		return st
	}
	b.here, b.herePC = blk, pc
	operand := func(o ir.Operand) *Value {
		if o.Slot < 0 {
			v := f.newValue(blk, OpConst, Tagged)
			v.Const = o.Literal
			return v
		}
		return b.read(o.Slot, blk)
	}
	guard := func(op Op, t Type, kind ir.ExitKind, args ...*Value) *Value {
		if b.cur != b.root {
			// Inside an inlined callee Go does the operation, in the
			// callee's frame, and native code goes on where it can.
			kind = ir.HostExit
		}
		v := f.newValue(blk, op, t, args...)
		v.Aux = int(kind)
		v.State = state()
		v.State.addUse()
		return v
	}
	number := func(o ir.Operand, kind ir.ExitKind) *Value {
		if o.Slot < 0 && o.Literal.Kind == ir.Number {
			v := f.newValue(blk, OpConstF64, Float64)
			v.Const = o.Literal
			return v
		}
		return guard(OpUnboxF64, Float64, kind, operand(o))
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
		v := guard(OpOddballF64, Float64, ir.HostExit, operand(o))
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
		guard(OpCheckInit, None, ir.GuardExit, b.read(in.CheckSlot, blk))
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
		if c := b.equality(blk, in, operand, guard); c != nil {
			b.assign(in.Dest, blk, boxB(c))
			break
		}
		if b.wraps(pc) {
			b.wrapOp(blk, pc, in, guard)
			break
		}
		if v := b.bigNative(blk, pc, in, operand, guard); v != nil {
			if v.Type == Bool {
				v = boxB(v)
			}
			b.assign(in.Dest, blk, v)
			break
		}
		if b.goArithAt(pc, in.Operator) {
			b.assign(in.Dest, blk, b.goArith(blk, in, operand, guard))
			break
		}
		if in.Operator == ir.Add && b.generic(pc) && b.goCalls() {
			// An addition whose operands were not numbers: Go makes it, as
			// a string's (ir.StringAdd).
			b.goAdd(blk, in, operand, guard)
			break
		}
		if relational(in.Operator) && b.generic(pc) && b.goCalls() {
			b.assign(in.Dest, blk, b.goCompare(blk, in, operand, guard))
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
			b.assign(in.Dest, blk, boxF(guard(OpModF64, Float64, ir.HostExit, x, y)))
			break
		}
		if in.Operator == ir.Pow {
			// As Math.pow's, where exact, and Go's at an exit otherwise.
			b.assign(in.Dest, blk, boxF(guard(OpPowF64, Float64, ir.HostExit, x, y)))
			break
		}
		b.assign(in.Dest, blk, b.binary(blk, in.Operator, x, y, boxF, boxB))
	case ir.Unary:
		if in.Operator == ir.Not {
			t := guard(OpTruth, Bool, ir.GuardExit, operand(in.Left))
			b.assign(in.Dest, blk, boxB(f.newValue(blk, OpNot, Bool, t)))
			break
		}
		if v := b.bigUnary(blk, pc, in, operand, guard); v != nil {
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
			c = guard(OpTruth, Bool, ir.GuardExit, operand(in.Left))
		} else if in.Operator == ir.Nullish {
			// ?? and ?.: null or undefined, no conversion.
			c = f.newValue(blk, OpStrictNullish, Bool, operand(in.Left))
			c.Aux = 2
		} else if n := b.equality(blk, in, operand, guard); n != nil {
			c = n
		} else if v := b.relationalBig(blk, pc, in, operand, guard); v != nil {
			c = v
		} else if relational(in.Operator) && b.generic(pc) && b.goCalls() {
			// Go's comparison's boolean, which native code branches on.
			c = guard(OpTruth, Bool, ir.GuardExit, b.goCompare(blk, in, operand, guard))
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
		if b.cur == b.root && b.derivedReturnExits(in) {
			// Go's, which throws for anything but an object.
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		if b.cur == b.root && b.p.DerivedConstructor {
			// undefined: a derived constructor's this, as its construction
			// returns it, if super() bound it; Go returns, and throws, if
			// not.
			this := b.read(b.p.Locals-1, blk)
			guard(OpCheckInit, None, ir.HostExit, this)
			blk.Control = this
			this.Uses++
			break
		}
		v := operand(in.Left)
		guard(OpCheckInit, None, ir.GuardExit, v)
		if b.cur != b.root {
			// The call's result, and on after it: a construction's, its
			// receiver.
			if b.cur.receiver != nil {
				v = b.cur.receiver
			}
			if b.cur.result >= 0 {
				b.assign(b.cur.result, blk, v)
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
			b.goFrame(blk, pc, ir.Instruction{Op: ir.CloseUpvalues}, guard)
		}
		blk.Control = v
		v.Uses++
	case ir.ArrayRead, ir.ArrayUpdate:
		// The element's key, and an update's updated key: the key's own
		// value before or after (Postfix).
		keyOf := func(kind ir.ExitKind) (key, updated *Value) {
			if in.Op != ir.ArrayUpdate {
				return number(in.Right, kind), nil
			}
			old := guard(OpUnboxF64, Float64, kind, operand(in.Right))
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
			cell := guard(OpStringChar, Source, ir.HostExit, operand(in.Left), key)
			v := f.newValue(blk, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			b.assign(in.Dest, blk, v)
			break
		}
		if in.Reference && in.Op == ir.ArrayRead && b.typedReceiver(pc, operand(in.Left)) {
			// A typed array's number; Go reads any other's.
			array := guard(OpIndexedOf, Ptr, ir.HostExit, operand(in.Left))
			key, _ := keyOf(ir.HostExit)
			b.assign(in.Dest, blk, boxF(guard(OpElemRead, Float64, ir.HostExit, array, key)))
			break
		}
		if in.Reference && in.Op == ir.ArrayRead && b.keyedRead(pc) {
			// Any receiver's, by Go from native code; what Go does not read
			// leaves, and native code goes on after it.
			b.goIndex(blk, in, operand, guard)
			break
		}
		if in.Reference {
			// Whatever the element holds, by its cell; Go reads the rest,
			// and makes an update's of its key. An update's key, a[i++],
			// is written once the read has succeeded, before the element.
			array := guard(OpArrayOf, Ptr, ir.HostExit, operand(in.Left))
			key, updated := keyOf(ir.HostExit)
			cell := guard(OpElemCell, Source, ir.HostExit, array, key)
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
		array := guard(OpIndexedOf, Ptr, ir.GuardExit, operand(in.Left))
		key, updated := keyOf(ir.GuardExit)
		elem := guard(OpElemRead, Float64, ir.GuardExit, array, key)
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
		}
		b.assign(in.Dest, blk, boxF(guard(OpLength, Float64, kind, operand(in.Left))))
	case ir.ArrayKey:
		guard(OpArrayOf, Ptr, ir.GuardExit, operand(in.Left))
		guard(OpElemKey, None, ir.GuardExit, number(in.Right, ir.GuardExit))
	case ir.ArrayWrite:
		if b.keyedWrite(pc) {
			// Any receiver's, by Go from native code; what Go does not
			// store leaves, and native code goes on after it.
			b.goKeyedWrite(blk, in, operand, guard)
			break
		}
		// Any value, by its words; a number's, once it is known to be one,
		// as a number (simplify). Go stores what native code does not.
		array := guard(OpIndexedOf, Ptr, ir.HostExit, operand(in.Left))
		key := number(in.Right, ir.HostExit)
		if o := in.Third; o.Slot < 0 && o.Literal.Kind == ir.Number {
			guard(OpElemWrite, None, ir.HostExit, array, key, number(o, ir.HostExit))
			break
		}
		guard(OpElemStore, None, ir.HostExit, array, key, operand(in.Third))
	case ir.StringMethod:
		cell := guard(OpStringMethod, Source, ir.HostExit, operand(in.Left))
		v := f.newValue(blk, OpLoadCell, Tagged, cell)
		v.Shadow = cell
		b.assign(in.Dest, blk, v)
	case ir.StringCode:
		code := guard(OpStringCode, Float64, ir.HostExit, operand(in.Left), operand(in.Right), number(in.Third, ir.HostExit))
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
		cell := b.globalCell(site, guard, kind)
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
				b.typedArrays = map[*Value]bool{}
			}
			b.typedArrays[v] = true
		}
		b.assign(in.Dest, blk, v)
	case ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead:
		if a, ok := b.accessor(pc); ok {
			b.accessorOp(blk, pc, in, a, operand, state)
			break
		}
		if s, ok := b.stringProperty(pc); ok {
			// A string's method, read where String.prototype has it, as
			// V8 reads one off the String map's prototype; or a BigInt's,
			// where BigInt.prototype, the site's holder, has it.
			cell := guard(OpStringProp, Source, ir.HostExit, operand(in.Left))
			cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(s.Shape)}, int(s.Index), s.Key
			if s.Holders[0].Object != 0 {
				f.cache(cell).Holders = &HolderChain{Holders: s.Holders}
			}
			v := f.newValue(blk, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			b.assign(in.Dest, blk, v)
			break
		}
		site, ok := b.property(pc)
		if !ok {
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
		// shapes Go learns at its exits (the VM's jitPrivateSeen).
		kind := ir.GuardExit
		if site.Megamorphic || in.Private {
			kind = ir.HostExit
		}
		if in.Private {
			// The key this evaluation of the class minted, the one the
			// site's shapes have.
			k := guard(OpSameSymbol, None, ir.HostExit, operand(in.Third))
			k.Const = ir.Value{Bits: uint64(site.Private)}
		}
		object := guard(OpObjectOf, Ptr, kind, operand(in.Left))
		// A read the receiver's prototypes answered checks them too; a
		// write, which makes a property of the receiver's own, never meets
		// one.
		var holders *HolderChain
		if site.Holders[0].Object != 0 && site.Shape != 0 && in.Op != ir.PropertyWrite {
			holders = &HolderChain{Holders: site.Holders, More: site.More}
		} else if site.Holders[0].Object != 0 {
			site.Shape = 0
		}
		var cases []PropertyCase
		if site.Shape != 0 || in.Op == ir.PropertyWrite {
			cases = site.Cases
		}
		if in.Private && in.When {
			// #x in o: true where the read finds the member -- an object of
			// a shape the site has met -- and Go's answer elsewhere.
			cell := guard(OpPropCell, Source, kind, object)
			cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
			f.setProperty(cell, holders, cases, nil, nil)
			k := f.newValue(blk, OpConst, Tagged)
			k.Const = ir.Bool(true)
			b.assign(in.Dest, blk, k)
			break
		}
		if (in.Op == ir.ReferenceRead || in.Op == ir.PropertyRead) && site.Megamorphic && !in.Private && b.cur == b.root && b.goCalls() && f.Keeps < f.keepLimit() {
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
			cell := guard(OpPropCell, Source, kind, object)
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
		if in.Op == ir.ReferenceRead || in.Op == ir.PropertyRead && b.fb != nil && b.fb.Generic(pc) {
			// A reference's read; or a number's that met a value that was
			// not one, read as a value now, whose uses that want a number
			// unbox it (Feedback's Generic).
			cell := guard(OpPropCell, Source, kind, object)
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
			v := guard(OpPropRead, Float64, kind, object)
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
		v := guard(OpPropWrite, None, kind, object, operand(in.Right))
		v.Const, v.Index, v.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
		// A property the write adds, as V8's stores do along a map's
		// transition; or, to an object that has it, stores: of any of the
		// shapes the site met.
		f.setProperty(v, nil, cases, site.Add, site.Adds)
		if site.Megamorphic {
			f.cache(v).Mega = true
			if in.Op == ir.PropertyWrite && !in.Private && (site.Add == nil || !site.Add.Define) && b.cur == b.root && b.goCalls() && f.Keeps < f.keepLimit() {
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
				// at a call in eleven, the interpreter finishing it.
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
		cell := b.globalCell(site, guard, ir.HostExit)
		w := guard(OpPropWrite, None, ir.HostExit, cell, operand(in.Left))
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
		b.globalCell(site, guard, ir.HostExit)
		k := f.newValue(blk, OpConst, Tagged)
		k.Const = ir.Bool(true)
		b.assign(in.Dest, blk, k)
	case ir.IterStep:
		if b.host(pc) {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.iterStep(blk, in, operand, guard)
	case ir.IterClose:
		// A cursor closed as a destructuring or a loop ends: nothing to do
		// for one done, or for an array walked in place while nothing up
		// its iterator's chain has a return method (WalkedCloseFeedback);
		// Go closes any other.
		c := guard(OpIterClosed, None, ir.HostExit, operand(in.Left))
		if w, ok := b.fb.(WalkedCloseFeedback); ok {
			c.Const.Bits = uint64(w.WalkedCloseCell())
		}
	case ir.Closure, ir.CloseUpvalues:
		if b.host(pc) || b.f.Keeps >= b.f.keepLimit() {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goFrame(blk, pc, in, guard)
	case ir.Rethrow:
		if b.host(pc) {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		// The completion record is a normal completion's, which is
		// dropped; for any other the interpreter reproduces it, from the
		// state before the rethrow.
		normal := f.newValue(blk, OpConstF64, Float64)
		normal.Const = ir.Float(ir.RethrowNormal)
		is := f.newValue(blk, OpCmpF64, Bool, number(in.Left, ir.GuardExit), normal)
		is.Aux = int(ir.Eq)
		guard(OpCheckTrue, None, ir.GuardExit, boxB(is))
	case ir.Resolved:
		// True where native code checked; Go's answer, after an exit,
		// checked here, and Go throws for an unresolved name.
		guard(OpCheckTrue, None, ir.HostExit, operand(in.Left))
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
		if in.Op == ir.ArrayLiteral {
			elems = make([]*Value, in.Extra)
			for i := range elems {
				elems[i] = operand(ir.Slot(in.Dest + i))
			}
		}
		call := guard(OpCall, Tagged, ir.HostExit, elems...)
		b.f.cache(call).Calls = []*CallSite{{Pool: site.Pool, Alloc: true, Literal: true, Argc: len(elems), ThisSlot: -1}}
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
		b.goAdd(blk, in, operand, guard)
	case ir.HasProperty:
		if !b.goCalls() {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goHas(blk, in, operand, guard)
	case ir.ArgumentRead:
		if b.host(pc) {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		// arguments[k] of an inlined call (InlineSite's ReadsArguments), a
		// constant k below the arguments' count, is its argument k, as V8
		// eliminates the arguments object; any other key deoptimizes, and
		// the interpreter, Go's from the next compile on (generic), makes
		// it, its frame given the call's arguments.
		if k, ok := constIndex(operand(in.Left)); ok && k < len(b.cur.args) {
			b.assign(in.Dest, blk, b.cur.args[k])
			break
		}
		no := f.newValue(blk, OpConst, Tagged)
		no.Const = ir.Bool(false)
		guard(OpCheckTrue, None, ir.GuardExit, no)
		b.assign(in.Dest, blk, operand(in.Left))
	case ir.DeleteProperty:
		if !b.goCalls() {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goDelete(blk, in, operand, guard)
	case ir.StringConcat:
		if !b.goCalls() || in.Extra > abi.MaxGoArgs {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goConcat(blk, in, operand, guard)
	case ir.UpvalueRead, ir.UpvalueWrite:
		if b.cur != b.root && in.Op == ir.UpvalueRead && b.cur.site.Upvalues != 0 {
			// An inlined callee's captured binding, read from its cell
			// anew each time (OpUpvalueCell's Aux 2): a call may assign it.
			cell := f.newValue(blk, OpUpvalueCell, Source)
			cell.Index, cell.Aux, cell.Const = int(in.Key), 2, ir.Value{Bits: uint64(b.cur.site.Upvalues)}
			v := f.newValue(blk, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			if in.Check {
				guard(OpCheckInit, None, ir.GuardExit, v)
			}
			b.assign(in.Dest, blk, v)
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
				guard(OpCheckInit, None, ir.GuardExit, v)
			}
			b.assign(in.Dest, blk, v)
			break
		}
		if in.Check {
			guard(OpCheckInit, None, ir.GuardExit, b.read(slot, blk))
		}
		cell := f.newValue(blk, OpUpvalueCell, Source)
		cell.Index = int(in.Key)
		x := operand(in.Left)
		w := guard(OpPropWrite, None, ir.HostExit, cell, x)
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
		if !ok {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		object := guard(OpObjectOf, Ptr, ir.HostExit, operand(in.Left))
		w := guard(OpPropWrite, None, ir.HostExit, object, operand(in.Right))
		w.Key, b.f.cache(w).Add = add.Key, add
	case ir.TypeTest:
		v := guard(OpTypeIs, Bool, ir.HostExit, operand(in.Left))
		v.Index = int(in.Key)
		if !in.When {
			v.Const = ir.Value{Bits: 1}
		}
		b.assign(in.Dest, blk, boxB(v))
	case ir.Host, ir.Call:
		if in.Tail {
			// Deep in the stack the frame is given up, where it holds no
			// captured local: native code makes a call of the function
			// itself a jump to its start, and, in a native callee's frame,
			// one of a function the call is seen to call a jump to that
			// function's code, its frame in place of this one (OpTailSelf);
			// Go makes any other.
			if p := b.root.p; b.cur == b.root && p.Captured == nil && !p.ArrowThis && !p.ClassConstructor {
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
				v := guard(OpTailSelf, None, ir.HostExit, args...)
				v.Index, v.Const = p.Params, ir.Value{Bits: uint64(p.UpvalueBase)}
				if in.When || p.This {
					v.Key |= 1
				}
				if p.This {
					v.Key |= 2
				}
				if calls := b.calls[pc]; len(calls) != 0 && calls[0].Go == 0 {
					b.f.cache(v).Calls = calls
				}
			} else {
				guard(OpTailRoom, None, ir.HostExit)
			}
		}
		if k, ok := b.intrinsic(pc); ok && in.Op == ir.Host {
			b.intrinsicCall(blk, pc, k, guard, boxF)
			break
		}
		if in.Op == ir.Host && b.charCode(pc) {
			b.charCodeCall(blk, pc, guard, boxF)
			break
		}
		if fn, ok := b.fromCharCode(pc); ok && in.Op == ir.Host {
			b.fromCharCodeCall(blk, pc, fn, guard)
			break
		}
		if fp, ok := b.forEaches[pc]; ok && in.Op == ir.Host && b.cur == b.root {
			b.forEachCall(blk, pc, fp)
			break
		}
		if fn, ok := b.parseIntAt(pc); ok && in.Op == ir.Host {
			b.parseIntCall(blk, pc, fn, guard, boxF)
			break
		}
		if fn, kind, ok := b.indexOfAt(pc); ok && in.Op == ir.Host {
			b.indexOfCall(blk, pc, fn, kind, guard)
			break
		}
		if fn, ok := b.bigStringAt(pc); ok && in.Op == ir.Host {
			b.bigStringCall(blk, pc, fn, guard)
			break
		}
		if fn, kind, ok := b.substring(pc); ok && in.Op == ir.Host {
			b.substringCall(blk, pc, fn, kind, guard)
			break
		}
		if k, ok := b.instanceOf(pc); ok && in.Op == ir.Host {
			b.instanceOfOp(blk, pc, k, guard, boxB)
			break
		}
		if fr := b.inlinedAt(pc); fr != nil {
			b.inlineCall(blk, pc, fr, guard, state)
			break
		}
		if calls := b.calls[pc]; calls != nil && b.cur == b.root {
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
			sp := b.p.Locals + b.p.Maps[pc].Depth
			args := make([]*Value, n)
			for i := range args {
				if i > 0 || !calls[0].Super {
					args[i] = b.read(sp-n+i, blk)
				}
			}
			if calls[0].Super {
				args[0] = b.read(b.p.Locals-1, blk)
			}
			call := guard(OpCall, Tagged, ir.HostExit, args...)
			b.f.cache(call).Calls, call.Index = calls, b.callKeeps[pc]
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
		blk.ExitKind = ir.HostExit
		blk.State = state()
		blk.State.addUse()
	}
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
	for _, site := range b.fb.NativeCalls(pc) {
		if site.Go == GoCall && (!b.goCalls() || len(calls) != 0) || site.Go != GoCall && generic {
			continue
		}
		depth, after := b.p.Maps[pc].Depth, b.p.Maps[pc+1].Depth
		operands := site.Argc + 1
		if site.Method {
			operands++
		}
		if site.Super {
			// Its arguments alone.
			operands = site.Argc
		}
		// A callee that reads its receiver gets the method call's, or a
		// construction's, from its pool, or a super()'s, the instance --
		// or, an arrow, the this it
		// captured (ArrowThis), which the call reads from its function
		// object: left out, an arrow reading its this, learned at a miss,
		// missed at every call -- AWFY CD's Vector.forEach, 45,000 exits a
		// run.
		if site.Argc < 0 || depth < operands || after != depth-operands+1 || site.ThisSlot >= 0 && !site.Method && site.Pool == 0 && !site.ArrowThis && !site.Super && !site.PlainThis {
			continue
		}
		if site.ThisSlot < 0 {
			// A receiver it never reads needs no coercing.
			site.Coerce = false
		}
		c := new(CallSite)
		*c = site
		calls = append(calls, c)
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
