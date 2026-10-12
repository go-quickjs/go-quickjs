package ssa

import (
	"errors"
	"fmt"
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

// InstanceOfFeedback is Feedback that knows instanceof's constructors.
type InstanceOfFeedback interface {
	InstanceOf(pc int) (InstanceOfSite, bool)
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

// stringProperty is the feedback for a method read at pc whose receivers
// have been strings, if any.
func (b *builder) stringProperty(pc int) (PropertySite, bool) {
	f, ok := b.fb.(StringFeedback)
	if !ok || b.p.Code[pc].Op != ir.ReferenceRead {
		return PropertySite{}, false
	}
	s, ok := f.StringProperty(pc)
	return s, ok && s.Shape != 0
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
	// New marks a construction Go makes (GoCall at a new).
	New                          bool
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
	// Literal marks an object literal's object (ir.ObjectLiteral), the
	// pool's, with Alloc: no function, no operands; or an array literal's
	// (ir.ArrayLiteral), whose Argc operands are its elements.
	Literal bool
	// Go marks a call of Go, not of a function, as V8's code calls a
	// runtime function: what Go does with the call's operands, which
	// native code passes as their words (abi.Context's GoOp).
	Go GoOp
}

// GoOp is what a call of Go does (CallSite's Go).
type GoOp uint8

// GoAdd is +, an operand possibly a string (ir.StringAdd): Go makes it of
// primitives; an object, whose conversion may run anything, or a symbol,
// which throws, it leaves to an exit, after which Go makes it. GoRefill
// fills an object pool that ran out again (abi.ObjectPool's Source), its
// address the first operand's pointer word: a call that takes from one
// makes it, and is made again from its start.
//
// GoStore stores a pointer while the collector marks, which native code,
// with no write barrier, does not: the first operand is the value, the
// second's pointer word the cell's address (mir's property stores).
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
// as V8's code calls its grow stub. The operands are the array's pointer
// word, the key's and the value's number words; Go refuses a store that
// could run a script or throw.
//
// GoElemCell is an element read as a reference that native code does not
// find -- past the array's length, or a hole: Go says whether it is
// undefined with nothing to ask, the array's chain holding no element and
// no index, the first operand its pointer word, the key its number word;
// the read is then from Encoding's UndefinedCell.
//
// GoTake takes a pool's next object while the collector marks, as V8's
// code calls its runtime to allocate: Go clears the pool's cell and keeps
// the object in the call's keep cell, with its write barrier, where native
// code, with none, does not. The pool's address is the first operand's
// pointer word, and the call's checks found an object there.
//
// GoAddAlong adds a property along a transition while the collector marks
// (mir's addAlong), as V8's code calls its write barrier: Go appends the
// entry to the object's table, which has room, and sets its next shape,
// with its write barrier. The first operand's pointer word is the object,
// its number word the entry's key and its flags, past bit 32; the
// second's pointer word the next shape; the third the value.
//
// GoConcat is concat (ir.StringConcat): Go converts its Argc operands to
// strings and joins them, as GoAdd adds; what could run code or throw it
// leaves to an exit, after which Go makes it.
const (
	GoAdd GoOp = 1 + iota
	GoRefill
	GoStore
	GoPow
	GoMath
	GoAtan2
	GoHypot
	GoCall
	GoMod
	GoPoll
	GoElemWrite
	GoElemCell
	GoTake
	GoAddAlong
	GoConcat
)

// RefillsPools reports whether a call takes an object from a pool, which
// a call of Go fills again when it runs out (GoRefill).
func RefillsPools(v *Value) bool {
	for _, c := range v.Calls {
		if c.Pool != 0 {
			return true
		}
	}
	return false
}

// popsElement reports whether a call is Array.prototype.pop's (CallSite's
// Pop): it writes an element's cell, and its own result's.
func popsElement(v *Value) bool {
	for _, c := range v.Calls {
		if !c.Pop {
			return false
		}
	}
	return len(v.Calls) != 0
}

// allocOnly reports whether a call runs nothing that can write the heap
// but past what any value was read from: it makes a pool's object
// (CallSite.Alloc), or adds an element past an array's last (Push).
func allocOnly(v *Value) bool {
	for _, c := range v.Calls {
		// A call of Go makes what it returns, and writes nothing else --
		// but a call it makes (GoCall), which runs anything.
		if !c.Alloc && !c.Receiver && !c.Push && (c.Go == 0 || c.Go == GoCall) {
			return false
		}
	}
	return len(v.Calls) != 0
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
// arguments object.
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
	if p == nil || p.Validate() != nil || len(p.Code) > maxInline || len(p.Globals) != 0 {
		return nil, false
	}
	var calls []int
	for pc, in := range p.Code {
		if !reachable(p, pc) {
			continue
		}
		switch in.Op {
		case ir.Call:
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
type GlobalSite struct {
	Key      uint32
	Index    int32
	Lexical  bool
	Fixed    bool
	Constant ir.Value
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
	// Add, for a write whose cache adds the property, says how; Shape is
	// then 0. Adds are the adds a write met objects of other shapes take,
	// as V8's polymorphic stores keep a transition for each map; a write's
	// Cases are the own properties others have.
	Add  *PropertyAdd
	Adds []*PropertyAdd
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
	f.written = f.bools(p.Locals + p.StackSize)
	b := &builder{p: p, fb: fb, f: f, nslots: p.Locals + p.StackSize}
	b.root = &frame{p: p, fb: fb}
	b.cur = b.root
	if err := b.plan(); err != nil {
		return nil, err
	}
	b.translate()
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
	frameOf   []*frame
	// literals are the literals made natively, by PC, each with the keep
	// cell planning gave it (literal).
	literals map[int]literalKeep
	// stale are the captured bindings' markers (OpUpvalueStale), by index;
	// here is the block of the instruction being built, at herePC, whose
	// state a read there loads a binding from its cell with (read).
	stale  []*Value
	here   *Block
	herePC int

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
}

// property is the feedback for a property operation at pc, if any.
func (b *builder) property(pc int) (PropertySite, bool) {
	if op := b.p.Code[pc].Op; b.fb == nil || op != ir.PropertyRead && op != ir.PropertyWrite && op != ir.ReferenceRead {
		return PropertySite{}, false
	}
	return b.fb.Property(pc)
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
	case ir.UpvalueRead, ir.UpvalueWrite:
		// The function's own captured bindings, which its context has;
		// an inlined callee's, Go's.
		return b.cur != b.root
	case ir.StringAdd, ir.StringConcat:
		// A call of Go (GoAdd, GoConcat), where the VM answers it.
		return !b.goCalls()
	case ir.FieldDefine:
		_, ok := b.define(pc)
		return !ok
	}
	return false
}

// literalKeep is a literal's site and the keep cell its object is kept in,
// or -1 for a literal Go makes.
type literalKeep struct {
	site LiteralSite
	keep int
}

// literal is the site of the object literal at pc (LiteralFeedback), if
// its code may make it natively, and the keep cell its object is kept in.
// The cell is taken the first time planning asks (host), and the answer
// kept: planning and building must agree, and a cell taken only when the
// literal was built -- after the calls before it had taken theirs -- could
// be one too many, the literal then left to Go where planning had made no
// exit for it, and the literal not made at all.
func (b *builder) literal(pc int) (LiteralSite, int, bool) {
	if b.cur != b.root {
		return LiteralSite{}, -1, false
	}
	if k, ok := b.literals[pc]; ok {
		return k.site, k.keep, k.keep >= 0
	}
	k := literalKeep{keep: -1}
	if f, ok := b.fb.(LiteralFeedback); ok && b.f.Keeps < b.f.keepLimit() {
		if site, ok := f.Literal(pc); ok && site.Pool != 0 {
			k = literalKeep{site: site, keep: b.f.Keeps}
			b.f.Keeps++
		}
	}
	if b.literals == nil {
		b.literals = map[int]literalKeep{}
	}
	b.literals[pc] = k
	return k.site, k.keep, k.keep >= 0
}

// reloadUpvalues has each of the function's captured bindings be its cell's
// value now: at an entry, and after a native call, whose callee may assign
// one. Between, a binding is the value last read or assigned, as a local
// is; unused, the loads are dead. After a call, the loads carry the state
// there, st, which a guard that unboxes one exits to (unboxPhis), as an
// entry's header is for its loads.
func (b *builder) reloadUpvalues(blk *Block) {
	p := b.root.p
	if b.stale == nil {
		b.stale = make([]*Value, p.Upvalues)
	}
	for i := range p.Upvalues {
		if b.stale[i] == nil {
			b.stale[i] = b.f.alloc(Value{Op: OpUpvalueStale, Type: Tagged, Index: i})
		}
		b.write(p.UpvalueBase+i, blk, b.stale[i])
	}
}

// loadUpvalue is captured binding i, loaded from its cell at the end of
// blk, with st the state a guard that unboxes it exits to (unboxPhis), if
// any: a read's, which the binding's variable met marked stale.
func (b *builder) loadUpvalue(blk *Block, i int, st *FrameState) *Value {
	cell := b.f.newValue(blk, OpUpvalueCell, Source)
	cell.Index = i
	v := b.f.newValue(blk, OpLoadCell, Tagged, cell)
	v.Shadow, v.State = cell, st
	return v
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
		if in.Op != ir.ArrayRead || in.Reference || !reachable(p, pc) || !fb.Generic(pc) {
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

// KeepFeedback is Feedback that bounds the keep cells a build takes
// (Func.KeepLimit): abi.MaxKeeps when the backend could not fit the cells
// a build with none took into the context's (mir's ErrKeeps).
type KeepFeedback interface {
	KeepLimit() int
}

// EntryFeedback is Feedback that may ask for an entry at a PC after an
// exit Go resumes after -- a guard's, which code is otherwise compiled
// with no entry after (plan): the VM asks once Go has had to go on in the
// interpreter too often, having found none.
type EntryFeedback interface {
	WantsEntry(pc int) bool
}

// wantsEntry reports whether the VM asks for an entry at pc (EntryFeedback).
func (b *builder) wantsEntry(pc int) bool {
	f, ok := b.fb.(EntryFeedback)
	return ok && pc < len(b.p.Code) && f.WantsEntry(pc)
}

// generic reports an arithmetic operation or a comparison at pc whose
// speculation failed (Feedback's Generic): what are not numbers go to Go,
// which resumes after it, as they do for == and %.
func (b *builder) generic(pc int) bool {
	switch in := b.p.Code[pc]; {
	case in.Op == ir.Binary, in.Op == ir.Branch && in.Operator != ir.Truth:
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
	// them to is given (InlineSite's Forward).
	args []*Value
	// nest is how deep it is inlined, as maxInlineDepth counts: depth, but
	// for forwarding constructors (InlineSite's Restart).
	nest int
	// hosts are host's answers, by PC: 0 for none yet, 1 for no, 2 for yes.
	hosts []uint8
}

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
	if b.fb == nil || b.p.Code[pc].Op != ir.Host || b.inlines >= maxInlines || b.cur.nest >= maxInlineDepth || b.fb.Generic(pc) {
		return nil, false
	}
	site, ok := b.fb.Inline(pc)
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
	if site.Forward {
		// f, apply and the receiver; the arguments are the frame's own.
		callee = 3
		if b.cur == b.root || !b.cur.site.Restart || site.Argc != b.cur.site.Argc {
			return nil, false
		}
	}
	if depth < callee || after != depth-callee+1 || site.ThisSlot >= 0 && !site.Method && !site.Construct && !site.Forward {
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
	if site.Construct {
		// Its receiver's keep cell, taken now, as a literal's is
		// (literal): the inlining is decided now.
		fr.keep = b.f.Keeps
		b.f.Keeps++
	}
	if site.Restart {
		// A forwarding constructor is its call of the method it forwards
		// to: it does not count toward how deep calls are inlined.
		fr.nest = b.cur.nest
	}
	b.nslots += q.Locals + q.StackSize
	b.inlines++
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
		case in.Op == ir.Return || b.host(pc):
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
			if op := p.Code[q].Op; op == ir.Jump || op == ir.Branch || op == ir.Return || b.host(q) {
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
		case b.host(end):
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
		call.Calls = []*CallSite{{Callee: site.Callee, Argc: site.Argc, ThisSlot: -1, Pool: site.Pool,
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
	fr.args = make([]*Value, site.Argc)
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

// GoFeedback is Feedback whose VM answers calls of Go from native code
// (CallSite's Go): a builder given one makes them.
type GoFeedback interface {
	GoCalls() bool
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
	call := guard(OpCall, Tagged, ir.HostExit, operand(in.Left), operand(in.Right))
	if b.f.Keeps < b.f.keepLimit() {
		call.Calls, call.Index = []*CallSite{{Go: GoAdd, ThisSlot: -1}}, b.f.Keeps
		b.f.Keeps++
	}
	cell := b.f.newValue(blk, OpCallCell, Source, call)
	r := b.f.newValue(blk, OpKept, Tagged, cell, call)
	r.Shadow = cell
	b.assign(in.Dest, blk, r)
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
		call.Calls, call.Index = []*CallSite{{Go: GoConcat, Argc: len(parts), ThisSlot: -1}}, b.f.Keeps
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
	_, ok := b.instanceOf(pc)
	return ok
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
			cell.Holders = &HolderChain{Holders: site.Holders, More: site.More}
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
	default:
		return Intrinsic{}, false
	}
	if n := k.args(); n < 1 || n > maxIntrinsicArgs || b.p.Maps[pc+1].Depth != b.p.Maps[pc].Depth-n-1 || b.p.Maps[pc].Depth < n+2 {
		return Intrinsic{}, false
	}
	return k, true
}

// args is how many arguments the intrinsic takes.
func (k Intrinsic) args() int {
	switch {
	case k.Args != 0:
		return k.Args
	case k.Op == OpPowF64 || k.Op == OpMaxF64 || k.Op == OpMinF64 || k.Op == OpMathCall && k.Go != GoMath:
		return 2
	}
	return 1
}

// intrinsicCall makes the intrinsic call at pc (Intrinsic): checked to call
// the function, its argument a number -- either not, Go makes the call --
// its result is computed, in the call's result slot.
func (b *builder) intrinsicCall(blk *Block, pc int, k Intrinsic, guard func(Op, Type, ir.ExitKind, ...*Value) *Value, boxF func(*Value) *Value) {
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
	fr.args = b.cur.args
	for i := 0; i < site.Params && i < len(fr.args); i++ {
		fr.init[i] = fr.args[i]
	}
	if site.ThisSlot >= 0 {
		fr.init[site.ThisSlot] = b.read(sp-1, blk)
	}
}

// shifted is a callee's instruction with its slots made the builder's,
// base past its own.
func shifted(in ir.Instruction, base int) ir.Instruction {
	for _, o := range []*ir.Operand{&in.Left, &in.Right, &in.Third} {
		if o.Slot >= 0 {
			o.Slot += base
		}
	}
	in.Dest += base
	if in.Extra >= 0 {
		in.Extra += base
	}
	if in.Check {
		in.CheckSlot += base
	}
	return in
}

// cell is the op that finds the site's binding.
func (s GlobalSite) cell() Op {
	if s.Lexical {
		return OpLexCell
	}
	return OpGlobalCell
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
	total := 0
	for pc := range p.Code {
		if !reachable(p, pc) {
			continue
		}
		if fr, ok := b.inlineAt(pc, &total); ok {
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
	// made -- a guard's after nearly every instruction. Code Go has had to
	// go on without one too often is compiled with them (EntryFeedback).
	resumes := func(pc int) bool {
		switch op := p.Code[pc].Op; {
		case b.host(pc), op == ir.ObjectLiteral, op == ir.ArrayLiteral, op == ir.StringAdd, op == ir.StringConcat, op == ir.ArrayWrite:
			return true
		}
		return b.wantsEntry(pc + 1)
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
			}
		}
		switch in.Op {
		case ir.ArrayRead:
			if in.Reference && resumes(pc) {
				// Go reads what native code does not, and resumes after it.
				entries[pc+1] = true
			}
		case ir.Nop, ir.Copy, ir.CopyPair, ir.StoreLoad, ir.Swap, ir.Insert2, ir.Insert3,
			ir.Unary, ir.Update, ir.Return, ir.ArrayUpdate, ir.ArrayLength, ir.ArrayKey:
		case ir.UpvalueRead:
			// The function's own captured binding is read natively, its
			// check deoptimizing; an inlined callee's leaves at its call.
		case ir.TypeTest:
			// An object it cannot tell exits at the typeof, which Go makes;
			// the interpreter goes on, the boolean not where the typeof's
			// string was (compile's typeTests): no entry after it.
		case ir.ArrayWrite, ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead, ir.BindingRead,
			ir.StringMethod, ir.StringCode, ir.BindingWrite, ir.BindingCheck, ir.Resolved, ir.ObjectLiteral, ir.FieldDefine,
			ir.ArrayLiteral, ir.StringConst, ir.UpvalueWrite, ir.StringAdd, ir.StringConcat:
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
			if in.Operator == ir.Eq || in.Operator == ir.Ne || b.generic(pc) {
				entries[pc+1] = entries[pc+1] || b.wantsEntry(pc+1)
				entries[in.Target] = entries[in.Target] || b.wantsEntry(in.Target)
			}
		case ir.Host, ir.Call:
			if b.goesOn(pc) {
				// A call Go makes goes on natively: Go leaves it only for
				// a throw, the runtime stopping, or the code to be compiled
				// again -- then the interpreter runs on to the next loop
				// header, as from V8's deoptimizations.
				entries[pc+1] = entries[pc+1] || b.wantsEntry(pc+1)
			} else if resumes(pc) {
				entries[pc+1] = true
			}
		default:
			return fmt.Errorf("%w: %d at pc %d", ErrUnsupported, in.Op, pc)
		}
		switch {
		case in.Op == ir.Jump || in.Op == ir.Branch || in.Op == ir.Return || b.host(pc) && !b.goesOn(pc):
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
			if op := p.Code[q].Op; op == ir.Jump || op == ir.Branch || op == ir.Return || b.host(q) && !b.goesOn(q) {
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
		case ir.Host, ir.Call, ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead, ir.BindingRead, ir.BindingWrite, ir.BindingCheck,
			ir.ObjectLiteral, ir.FieldDefine, ir.ArrayLiteral, ir.StringConst, ir.UpvalueRead, ir.UpvalueWrite, ir.StringAdd, ir.StringConcat:
			if fr := b.inlined[end]; fr != nil {
				// Into the callee, whose returns go to the block after it.
				blk.Kind = BlockPlain
				fr.callBlock = blk
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
		return s
	}
	depth := b.p.Maps[pc].Depth
	s := b.f.newState(FrameState{PC: b.p.Maps[pc].PC, Depth: depth, Slots: b.f.refsOf(b.p.Locals + depth), Site: pc})
	for i := range s.Slots {
		if !b.p.LiveAt(pc, i) || i >= b.p.UpvalueBase && i < b.p.UpvalueBase+b.p.Upvalues {
			// A local the interpreter writes before it reads from here:
			// not written, nor kept alive for the exit, as V8 leaves dead
			// registers out of a frame state. Nor is a captured binding,
			// which is in its cell.
			continue
		}
		s.Slots[i] = b.read(i, blk)
		s.Slots[i].Uses++
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
			if !b.p.LiveAt(e.PC, i) || i >= b.p.UpvalueBase && i < b.p.UpvalueBase+b.p.Upvalues {
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
		// A captured binding is what its cell holds, which native code
		// assigns (ir.UpvalueWrite), with its pointer word there.
		b.reloadUpvalues(blk)
		return
	}
	if blk.LoopHeader {
		blk.Header = b.state(blk, blk.PC)
		blk.Header.Site = -1
	}
	for pc := blk.PC; pc <= b.endOf[blk.ID]; pc++ {
		b.instruction(blk, pc)
	}
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
	var st *FrameState
	state := func() *FrameState {
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
		if in.Operator == ir.Add && b.generic(pc) && b.goCalls() {
			// An addition whose operands were not numbers: Go makes it, as
			// a string's (ir.StringAdd).
			b.goAdd(blk, in, operand, guard)
			break
		}
		kind := ir.GuardExit
		if in.Operator == ir.Eq || in.Operator == ir.Ne || in.Operator == ir.Mod || b.generic(pc) {
			kind = ir.HostExit
		}
		x, y := number(in.Left, kind), number(in.Right, kind)
		if in.Operator == ir.Mod {
			b.assign(in.Dest, blk, boxF(guard(OpModF64, Float64, ir.HostExit, x, y)))
			break
		}
		b.assign(in.Dest, blk, b.binary(blk, in.Operator, x, y, boxF, boxB))
	case ir.Unary:
		if in.Operator == ir.Not {
			t := guard(OpTruth, Bool, ir.GuardExit, operand(in.Left))
			b.assign(in.Dest, blk, boxB(f.newValue(blk, OpNot, Bool, t)))
			break
		}
		x := number(in.Left, ir.GuardExit)
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
		x := guard(OpUnboxF64, Float64, ir.GuardExit, old)
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
		} else if n := b.equality(blk, in, operand, guard); n != nil {
			c = n
		} else {
			kind := ir.GuardExit
			if in.Operator == ir.Eq || in.Operator == ir.Ne || b.generic(pc) {
				kind = ir.HostExit
			}
			c = f.newValue(blk, OpCmpF64, Bool, number(in.Left, kind), number(in.Right, kind))
			c.Aux = int(in.Operator)
		}
		blk.Control = c
		c.Uses++
	case ir.Return:
		v := operand(in.Left)
		guard(OpCheckInit, None, ir.GuardExit, v)
		if b.cur != b.root {
			// The call's result, and on after it: a construction's, its
			// receiver.
			if b.cur.receiver != nil {
				v = b.cur.receiver
			}
			b.assign(b.cur.result, blk, v)
			break
		}
		blk.Control = v
		v.Uses++
	case ir.ArrayRead, ir.ArrayUpdate:
		if in.Reference {
			// Whatever the element holds, by its cell; Go reads the rest.
			array := guard(OpArrayOf, Ptr, ir.HostExit, operand(in.Left))
			cell := guard(OpElemCell, Source, ir.HostExit, array, number(in.Right, ir.HostExit))
			v := f.newValue(blk, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			b.assign(in.Dest, blk, v)
			break
		}
		// Every check exits to the state before the instruction, so their
		// order is free; the key's update is written only once the read
		// has succeeded, before the element, which wins if both name one
		// slot.
		array := guard(OpArrayOf, Ptr, ir.GuardExit, operand(in.Left))
		var key, updated *Value
		if in.Op == ir.ArrayUpdate {
			old := guard(OpUnboxF64, Float64, ir.GuardExit, operand(in.Right))
			op := OpAddF64
			if in.Operator == ir.Sub {
				op = OpSubF64
			}
			updated = f.newValue(blk, op, Float64, old, one())
			key = updated
			if in.Postfix {
				key = old
			}
		} else {
			key = number(in.Right, ir.GuardExit)
		}
		elem := guard(OpElemRead, Float64, ir.GuardExit, array, key)
		if updated != nil {
			b.assign(in.Extra, blk, boxF(updated))
		}
		b.assign(in.Dest, blk, boxF(elem))
	case ir.ArrayLength:
		b.assign(in.Dest, blk, boxF(guard(OpLength, Float64, ir.GuardExit, operand(in.Left))))
	case ir.ArrayKey:
		guard(OpArrayOf, Ptr, ir.GuardExit, operand(in.Left))
		guard(OpElemKey, None, ir.GuardExit, number(in.Right, ir.GuardExit))
	case ir.ArrayWrite:
		// Go stores what native code does not: a value that is not a
		// number, or into an element that does not hold one.
		array := guard(OpArrayOf, Ptr, ir.HostExit, operand(in.Left))
		key, value := number(in.Right, ir.HostExit), number(in.Third, ir.HostExit)
		guard(OpElemWrite, None, ir.HostExit, array, key, value)
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
		cell := guard(site.cell(), Source, kind)
		cell.Index, cell.Key = int(site.Index), site.Key
		if site.Fixed {
			k := f.newValue(blk, OpConst, Tagged)
			k.Const = site.Constant
			b.assign(in.Dest, blk, k)
			break
		}
		v := f.newValue(blk, OpLoadCell, Tagged, cell)
		v.Shadow = cell
		b.assign(in.Dest, blk, v)
	case ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead:
		if s, ok := b.stringProperty(pc); ok {
			// A string's method, read where String.prototype has it, as
			// V8 reads one off the String map's prototype.
			cell := guard(OpStringProp, Source, ir.HostExit, operand(in.Left))
			cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(s.Shape)}, int(s.Index), s.Key
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
		object := guard(OpObjectOf, Ptr, ir.HostExit, operand(in.Left))
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
		if in.Op == ir.ReferenceRead || in.Op == ir.PropertyRead && b.fb != nil && b.fb.Generic(pc) {
			// A reference's read; or a number's that met a value that was
			// not one, read as a value now, whose uses that want a number
			// unbox it (Feedback's Generic).
			cell := guard(OpPropCell, Source, ir.HostExit, object)
			cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
			cell.Holders, cell.Cases = holders, cases
			v := f.newValue(blk, OpLoadCell, Tagged, cell)
			v.Shadow = cell
			b.assign(in.Dest, blk, v)
			break
		}
		if in.Op == ir.PropertyRead {
			v := guard(OpPropRead, Float64, ir.HostExit, object)
			v.Const, v.Index, v.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
			v.Holders, v.Cases = holders, cases
			b.assign(in.Dest, blk, boxF(v))
			break
		}
		v := guard(OpPropWrite, None, ir.HostExit, object, operand(in.Right))
		v.Const, v.Index, v.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
		// A property the write adds, as V8's stores do along a map's
		// transition; or, to an object that has it, stores: of any of the
		// shapes the site met.
		v.Add, v.Adds, v.Cases = site.Add, site.Adds, cases
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
		cell := guard(site.cell(), Source, ir.HostExit)
		cell.Index, cell.Key = int(site.Index), site.Key
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
		cell := guard(site.cell(), Source, ir.HostExit)
		cell.Index, cell.Key = int(site.Index), site.Key
		k := f.newValue(blk, OpConst, Tagged)
		k.Const = ir.Bool(true)
		b.assign(in.Dest, blk, k)
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
		call.Calls = []*CallSite{{Pool: site.Pool, Alloc: true, Literal: true, Argc: len(elems), ThisSlot: -1}}
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
	case ir.StringConcat:
		if !b.goCalls() || in.Extra > abi.MaxGoArgs {
			blk.ExitKind = ir.HostExit
			blk.State = state()
			blk.State.addUse()
			break
		}
		b.goConcat(blk, in, operand, guard)
	case ir.UpvalueRead, ir.UpvalueWrite:
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
		w.Key, w.Add = add.Key, add
	case ir.TypeTest:
		v := guard(OpTypeIs, Bool, ir.HostExit, operand(in.Left))
		v.Index = int(in.Key)
		if !in.When {
			v.Const = ir.Value{Bits: 1}
		}
		b.assign(in.Dest, blk, boxB(v))
	case ir.Host, ir.Call:
		if k, ok := b.intrinsic(pc); ok && in.Op == ir.Host {
			b.intrinsicCall(blk, pc, k, guard, boxF)
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
			sp := b.p.Locals + b.p.Maps[pc].Depth
			args := make([]*Value, n)
			for i := range args {
				args[i] = b.read(sp-n+i, blk)
			}
			call := guard(OpCall, Tagged, ir.HostExit, args...)
			call.Calls, call.Index = calls, b.callKeeps[pc]
			cell := f.newValue(blk, OpCallCell, Source, call)
			r := f.newValue(blk, OpKept, Tagged, cell, call)
			r.Shadow = cell
			b.write(b.p.Locals+b.p.Maps[pc+1].Depth-1, blk, r)
			// The callee may have assigned a captured binding. A guard
			// after the call exits to the next instruction, as the site
			// there fails: never again, once it has (Generic).
			if b.p.Upvalues > 0 {
				b.reloadUpvalues(blk)
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
		// A callee that reads its receiver gets the method call's, or a
		// construction's, from its pool.
		if site.Argc < 0 || depth < operands || after != depth-operands+1 || site.ThisSlot >= 0 && !site.Method && site.Pool == 0 {
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
