package decoder

import (
	"sync"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The validator walks an instruction sequence once, keeping a stack of the
// operand types and a stack of the enclosing control constructs. Its typing
// of unreachable code, the order of its checks and the position each error
// is reported at are part of its behaviour: an embedder sees the first error
// of a module, so which one comes first matters as much as whether there is
// one.

// reachability is a control construct's state: reachable, reachable by the
// spec though known never to run (which is validated as reachable code), or
// unreachable, where the operand stack is polymorphic.
type reachability uint8

const (
	reachable reachability = iota
	specOnlyReachable
	unreachable
)

// innerReachability is the reachability of a construct started in code of
// reachability r.
func innerReachability(r reachability) reachability {
	if r == reachable {
		return reachable
	}
	return specOnlyReachable
}

type controlKind uint8

const (
	ctrlIf controlKind = iota
	ctrlIfElse
	ctrlBlock
	ctrlLoop
	ctrlTry
	ctrlTryTable
	ctrlTryCatch
	ctrlTryCatchAll
)

// value is an operand: its type and the position of the instruction that
// pushed it, which errors about it name.
type value struct {
	pc uint32
	t  module.ValueType
}

// merge is the values a branch to a construct, or its end, carries: arity
// values at merges[at:]. reached records whether code jumps or falls there.
type merge struct {
	at, arity uint32
	reached   bool
}

// catchCase is one handler of a try_table; depth counts the try_table
// itself.
type catchCase struct {
	kind  byte
	tag   uint32
	depth uint32
}

// The catch kinds of try_table.
const (
	catchKind       = 0
	catchRefKind    = 1
	catchAllKind    = 2
	catchAllRefKind = 3
)

// control is an enclosing block, loop, if or try.
type control struct {
	kind       controlKind
	reach      reachability
	mightThrow bool
	pc         uint32
	// stackDepth is the operand stack's height at the construct's start,
	// below its parameters; initDepth the height of the stack of initialized
	// locals.
	stackDepth, initDepth uint32
	// prevCatch is the index of the try enclosing this one, or -1.
	prevCatch  int32
	start, end merge
	// mergeBase and catchAt are where this construct's merge values and
	// catch cases start in their arenas.
	mergeBase, catchAt, catchCount uint32
}

func (c *control) isTry() bool {
	return c.kind == ctrlTry || c.kind == ctrlTryCatch || c.kind == ctrlTryCatchAll
}

func (c *control) isIf() bool { return c.kind == ctrlIf || c.kind == ctrlIfElse }

// brMerge is the merge a branch to the construct targets: a loop's start,
// any other construct's end.
func (c *control) brMerge() *merge {
	if c.kind == ctrlLoop {
		return &c.start
	}
	return &c.end
}

// funcValidator validates one function body or constant expression at a
// time. Its stacks are kept from one function to the next.
type funcValidator struct {
	Decoder
	m         *module.Module
	lim       *Limits
	det       detected
	constMode bool

	params, results []module.ValueType
	constResult     [1]module.ValueType

	locals            []module.ValueType
	localDecls        []localDecl
	initialized       []bool
	initStack         []uint32
	hasNonDefaultable bool

	stack   []value
	ctrl    []control
	merges  []value
	catches []catchCase

	currentCatch int32
	// reachableOK is whether the current code is reachable and no error was
	// found.
	reachableOK bool
	endFound    bool

	// maxStack is the most operands after an instruction of code that is
	// not unreachable; refs says a reference was pushed, and tries is the
	// deepest nesting of legacy trys (module.FrameBound).
	maxStack, tries int
	refs            bool
}

type localDecl struct {
	count uint32
	t     module.ValueType
}

func (v *funcValidator) reset(b []byte, pc, end int) {
	v.b, v.pc, v.end, v.err = b, pc, end, nil
	v.stack = v.stack[:0]
	v.ctrl = v.ctrl[:0]
	v.merges = v.merges[:0]
	v.catches = v.catches[:0]
	v.initStack = v.initStack[:0]
	v.currentCatch = -1
	v.reachableOK = true
	v.endFound = false
	v.hasNonDefaultable = false
	v.maxStack, v.tries, v.refs = 0, 0, false
}

// ValidateFunctions validates the bodies of m's defined functions in order,
// under limits l, and reports the first invalid one's error as "Compiling
// function #N failed: ...", with the function's name from the name section
// if it has one.
func ValidateFunctions(m *module.Module, l *Limits) *Error {
	v := &funcValidator{m: m, lim: l}
	for i := m.NumImportedFunctions; i < uint32(len(m.Functions)); i++ {
		v.validateFunction(&m.Functions[i])
		if v.err != nil {
			return functionError(m, i, v.err)
		}
	}
	return nil
}

// functionError names function i in error e.
func functionError(m *module.Module, i uint32, e *Error) *Error {
	name, ok := functionName(m, i)
	if !ok {
		return &Error{Offset: e.Offset, Msg: "Compiling function #" + utoa(i) + " failed: " + e.Msg}
	}
	s := truncated(name)
	for j := 0; j < len(s); j++ {
		if s[j] == 0 {
			s = s[:j]
			break
		}
	}
	return &Error{Offset: e.Offset, Msg: "Compiling function #" + utoa(i) + ":\"" + s + "\" failed: " + e.Msg}
}

func utoa(i uint32) string {
	var buf [10]byte
	n := len(buf)
	for {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
		if i == 0 {
			return string(buf[n:])
		}
	}
}

// maxNameKey is the largest index a name map entry may have.
const maxNameKey = 10_000_000

// functionName returns function index's name from the first function names
// subsection of the first name section: the first entry for the index that
// is valid UTF-8, empty names included.
func functionName(m *module.Module, index uint32) ([]byte, bool) {
	if m.NameSection.Offset == 0 {
		return nil, false
	}
	d := newDecoder(m.Bytes, int(m.NameSection.Offset), int(m.NameSection.End()))
	for d.ok() && d.more() {
		nameType := d.consumeU8("name type")
		if nameType&0x80 != 0 {
			break
		}
		payloadLen := d.consumeU32v("name payload length")
		if !d.checkAvailable(payloadLen) {
			break
		}
		if nameType != 1 {
			d.consumeBytes(payloadLen)
			continue
		}
		count := d.consumeU32v("names count")
		var found []byte
		ok := false
		for i := uint32(0); i < count; i++ {
			idx := d.consumeU32v("index")
			name := consumeString(d, false, "name")
			if d.failed() {
				break
			}
			if idx > maxNameKey {
				continue
			}
			s := m.Bytes[name.Offset:name.End()]
			if !utf8.Valid(s) {
				continue
			}
			if idx == index && !ok {
				found, ok = s, true
			}
		}
		return found, ok
	}
	return nil, false
}

func (v *funcValidator) validateFunction(f *module.Function) {
	m := v.m
	v.constMode = false
	v.reset(m.Bytes, int(f.Code.Offset), int(f.Code.End()))
	sig := m.Types[f.TypeIndex].Func
	v.params, v.results = sig.Params, sig.Results
	localsLen := v.decodeLocals(v.pc)
	if v.failed() {
		return
	}
	v.pc += localsLen
	nonDefaultable := 0
	for _, t := range v.locals[len(v.params):] {
		if !t.IsDefaultable() {
			nonDefaultable++
		}
	}
	v.hasNonDefaultable = nonDefaultable > 0
	if v.hasNonDefaultable {
		if cap(v.initialized) < len(v.locals) {
			v.initialized = make([]bool, len(v.locals))
		}
		v.initialized = v.initialized[:len(v.locals)]
		for i, t := range v.locals {
			v.initialized[i] = i < len(v.params) || t.IsDefaultable()
		}
	}
	v.decodeBody()
	if v.failed() {
		return
	}
	if len(v.ctrl) != 0 {
		if len(v.ctrl) > 1 {
			v.errorAt(int(v.ctrl[len(v.ctrl)-1].pc), "unterminated control structure")
		} else {
			v.errorAt(v.pc, "function body must end with \"end\" opcode")
		}
		return
	}
	f.Frame = module.FrameBound{Stack: uint32(v.maxStack) + 1, Tries: uint32(v.tries), Refs: v.refs}
}

// decodeLocals reads the local declarations at pc into v.locals, after the
// parameters, returning their length.
func (v *funcValidator) decodeLocals(pc int) int {
	numLocals := uint32(len(v.params))
	entries, total := v.readU32v(pc, "local decls count")
	if v.failed() {
		return 0
	}
	if uint32(v.end-v.pc)/2 < entries {
		v.errorAt(pc, "local decls count bigger than remaining function size")
		return 0
	}
	v.localDecls = v.localDecls[:0]
	maxLocals := v.lim.Locals
	for e := uint32(0); e < entries; e++ {
		if v.pc >= v.end {
			v.errorAt(v.end, "expected more local decls but reached end of input")
			return 0
		}
		count, n := v.readU32v(pc+total, "local count")
		if v.failed() {
			return 0
		}
		if count > maxLocals-numLocals {
			v.errorAt(pc+total, "local count too large")
			return 0
		}
		total += n
		t, tl := readValueType(&v.Decoder, pc+total, &v.det, v.lim)
		validateValueType(&v.Decoder, pc+total, v.m, t)
		if v.failed() {
			return 0
		}
		total += tl
		numLocals += count
		v.localDecls = append(v.localDecls, localDecl{count, t})
	}
	v.locals = append(v.locals[:0], v.params...)
	for _, d := range v.localDecls {
		for i := uint32(0); i < d.count; i++ {
			v.locals = append(v.locals, d.t)
		}
	}
	return total
}

// decodeBody validates the instructions from v.pc to v.end in the function's
// outermost block.
func (v *funcValidator) decodeBody() {
	results := v.results
	if v.constMode {
		results = v.constResult[:]
	}
	endAt := uint32(len(v.merges))
	for _, t := range results {
		v.merges = append(v.merges, value{uint32(v.pc), t})
	}
	v.ctrl = append(v.ctrl, control{
		kind: ctrlBlock, reach: reachable, pc: uint32(v.pc), prevCatch: -1,
		start: merge{at: endAt, reached: true},
		end:   merge{at: endAt, arity: uint32(len(results))},
	})
	for v.pc < v.end {
		n := v.decodeInstruction()
		if v.err != nil || n <= 0 {
			return
		}
		v.pc += n
		if h := len(v.stack); h > v.maxStack && len(v.ctrl) > 0 && v.ctrl[len(v.ctrl)-1].reach != unreachable {
			v.maxStack = h
		}
	}
	if v.pc != v.end {
		v.errorAt(v.pc, "Beyond end of code")
	}
}

var constPool = sync.Pool{New: func() any { return new(funcValidator) }}

// validateConstExpr decodes the constant expression at b[start:end] that must
// produce a value of type expected. It returns where decoding stopped (just
// past the expression's final end when it is valid), whether that end was
// found, and the first error. An expression's reading of exnref types does
// not see the module's use of legacy exception handling: each expression
// starts without any features detected.
func validateConstExpr(m *module.Module, _ *detected, b []byte, start, end int, expected module.ValueType, l *Limits) (stop int, endFound bool, err *Error) {
	v := constPool.Get().(*funcValidator)
	v.m, v.lim, v.det = m, l, detected{}
	v.constMode = true
	v.reset(b, start, end)
	v.params, v.results = nil, nil
	v.locals = v.locals[:0]
	v.constResult[0] = expected
	v.decodeBody()
	stop, endFound, err = v.end, v.endFound, v.err
	if err != nil {
		stop = v.pc
	}
	v.m = nil
	constPool.Put(v)
	return stop, endFound, err
}

// --- Operand stack ---

func (v *funcValidator) push(t module.ValueType) {
	v.stack = append(v.stack, value{uint32(v.pc), t})
	if t.IsRef() {
		v.refs = true
	}
}

func (v *funcValidator) pushValue(x value) {
	v.stack = append(v.stack, x)
	if x.t.IsRef() {
		v.refs = true
	}
}

func (v *funcValidator) top() *control { return &v.ctrl[len(v.ctrl)-1] }

// controlAt returns the construct depth levels out from the innermost.
func (v *funcValidator) controlAt(depth uint32) *control {
	return &v.ctrl[uint32(len(v.ctrl))-1-depth]
}

func (v *funcValidator) bottomValue() value { return value{uint32(v.pc), module.Bottom} }

// safeOpcodeNameAt returns the name of the instruction at pc.
func (v *funcValidator) safeOpcodeNameAt(pc int) string {
	if pc >= v.end {
		return "<end>"
	}
	b := v.b[pc]
	if !opcode.IsPrefix(b) {
		return opcode.Opcode(b).Name()
	}
	idx, n := peekU32v(v.b[:v.end], pc+1)
	if n == 0 || idx > 0xfff {
		return opcode.Opcode(0).Name()
	}
	return opcode.Prefixed(b, idx).Name()
}

// peekU32v reads a LEB128 u32 at pos without reporting errors: 0, 0 if
// there is none.
func peekU32v(b []byte, pos int) (uint32, int) {
	var r uint32
	for i := 0; i < 5; i++ {
		if pos+i >= len(b) {
			return 0, 0
		}
		c := b[pos+i]
		r |= uint32(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			if i == 4 && c&0xf0 != 0 {
				return 0, 0
			}
			return r, i + 1
		}
	}
	return 0, 0
}

func (v *funcValidator) notEnoughArguments(needed, actual int) {
	v.errorf(v.pc, "not enough arguments on the stack for %s (need %d, got %d)", v.safeOpcodeNameAt(v.pc), needed, actual)
}

func (v *funcValidator) popTypeError(index int, x value, expected string) {
	v.errorf(int(x.pc), "%s[%d] expected %s, found %s of type %s",
		v.safeOpcodeNameAt(v.pc), index, expected, v.safeOpcodeNameAt(int(x.pc)), x.t.Name())
}

func (v *funcValidator) validateStackValue(index int, x value, expected module.ValueType) {
	if x.t == expected || x.t.Kind == module.KindBottom || expected.Kind == module.KindBottom ||
		module.IsSubtypeOf(x.t, expected, v.m) {
		return
	}
	v.popTypeError(index, x, "type "+expected.Name())
}

// pop pops one operand of any type.
func (v *funcValidator) pop() value {
	c := v.top()
	if uint32(len(v.stack)) <= c.stackDepth {
		if c.reach != unreachable {
			v.notEnoughArguments(1, 0)
		}
		return v.bottomValue()
	}
	x := v.stack[len(v.stack)-1]
	v.stack = v.stack[:len(v.stack)-1]
	return x
}

// ensureStackArguments makes sure count operands are above the current
// construct's base: in unreachable code by inserting bottom values below
// the ones there are, returning how many; in reachable code a shortfall is
// an error.
func (v *funcValidator) ensureStackArguments(count int) int {
	limit := int(v.top().stackDepth)
	if len(v.stack) >= count+limit {
		return 0
	}
	if v.top().reach != unreachable {
		v.notEnoughArguments(count, len(v.stack)-limit)
	}
	current := len(v.stack) - limit
	additional := count - current
	bot := v.bottomValue()
	for i := 0; i < additional; i++ {
		v.stack = append(v.stack, bot)
	}
	if current > 0 {
		copy(v.stack[limit+additional:], v.stack[limit:limit+current])
		for i := 0; i < additional; i++ {
			v.stack[limit+i] = bot
		}
	}
	return additional
}

// popN pops len(ts) operands, checking each against its type, deepest
// first. The values returned stay valid until the next push.
func (v *funcValidator) popN(ts ...module.ValueType) []value {
	n := len(ts)
	v.ensureStackArguments(n)
	base := len(v.stack) - n
	vals := v.stack[base:]
	v.stack = v.stack[:base]
	for i, t := range ts {
		v.validateStackValue(i, vals[i], t)
	}
	return vals
}

// pop1 pops one operand of type t.
func (v *funcValidator) pop1(t module.ValueType) value { return v.popN(t)[0] }

// peek returns the operand depth places below the top.
func (v *funcValidator) peek(depth int) value {
	limit := int(v.top().stackDepth)
	if len(v.stack) <= limit+depth {
		if !(!v.constMode && v.top().reach == unreachable) {
			v.notEnoughArguments(depth+1, len(v.stack)-limit)
		}
		return v.bottomValue()
	}
	return v.stack[len(v.stack)-depth-1]
}

// drop drops count operands, or as many as there are.
func (v *funcValidator) drop(count int) {
	limit := int(v.top().stackDepth)
	if len(v.stack) < limit+count {
		count = len(v.stack) - limit
	}
	v.stack = v.stack[:len(v.stack)-count]
}

func (v *funcValidator) pushTypes(ts []module.ValueType) {
	for _, t := range ts {
		v.push(t)
	}
}

// --- Control ---

// typeCheckMerge checks the operands against merge mg: exactly its arity of
// them if strict, at least as many otherwise. In unreachable code the
// operands there are are checked and, if pushBranch, missing ones are
// inserted with the merge's types. With rewrite, the checked operands take
// the merge's types.
func (v *funcValidator) typeCheckMerge(mg *merge, strict, pushBranch, rewrite bool, what string) bool {
	arity := mg.arity
	c := v.top()
	actual := uint32(len(v.stack)) - c.stackDepth
	if arity == 0 && (!strict || actual == 0) {
		return true
	}
	if arity == 1 && ((strict && actual == 1) || (!strict && actual >= 1)) &&
		v.stack[len(v.stack)-1].t == v.merges[mg.at].t {
		return true
	}
	if v.constMode || c.reach != unreachable {
		if (strict && actual != arity) || (!strict && actual < arity) {
			v.errorf(v.pc, "expected %d elements on the stack for %s, found %d", arity, what, actual)
			return false
		}
		vals := v.stack[uint32(len(v.stack))-arity:]
		for i := uint32(0); i < arity; i++ {
			old := v.merges[mg.at+i].t
			if !module.IsSubtypeOf(vals[i].t, old, v.m) {
				v.errorf(v.pc, "type error in %s[%d] (expected %s, got %s)", what, i, old.Name(), vals[i].t.Name())
				return false
			}
			if rewrite {
				vals[i].t = old
			}
		}
		return true
	}
	if strict && actual > arity {
		v.errorf(v.pc, "expected %d elements on the stack for %s, found %d", arity, what, actual)
		return false
	}
	for i, depth := int(arity)-1, 0; i >= 0; i, depth = i-1, depth+1 {
		x := v.peek(depth)
		v.validateStackValue(i, x, v.merges[mg.at+uint32(i)].t)
	}
	if pushBranch {
		inserted := uint32(v.ensureStackArguments(int(arity)))
		if inserted > 0 {
			base := uint32(len(v.stack)) - arity
			for i := uint32(0); i < min(arity, inserted); i++ {
				if v.stack[base+i].t.Kind == module.KindBottom {
					v.stack[base+i].t = v.merges[mg.at+i].t
				}
			}
		}
	}
	return v.ok()
}

func (v *funcValidator) typeCheckFallThru() bool {
	return v.typeCheckMerge(&v.top().end, true, true, false, "fallthru")
}

func (v *funcValidator) typeCheckBranch(c *control, push, rewrite bool) bool {
	return v.typeCheckMerge(c.brMerge(), false, push, rewrite, "branch")
}

func (v *funcValidator) fallThrough() {
	c := v.top()
	if !v.typeCheckFallThru() {
		return
	}
	if c.reach == reachable {
		c.end.reached = true
	}
}

// doReturn checks the operands against the function's results.
func (v *funcValidator) doReturn(strict bool, what string) bool {
	if !v.typeCheckMerge(&v.ctrl[0].end, strict, false, false, what) {
		return false
	}
	if v.constMode && v.reachableOK {
		v.endFound = true
		v.end = v.pc + 1
	}
	v.endControl()
	return true
}

// endControl makes the rest of the current construct unreachable.
func (v *funcValidator) endControl() {
	c := v.top()
	v.stack = v.stack[:c.stackDepth]
	c.reach = unreachable
	v.reachableOK = false
}

func (v *funcValidator) setSucceedingCodeDynamicallyUnreachable() {
	c := v.top()
	if c.reach == reachable {
		c.reach = specOnlyReachable
		v.reachableOK = false
	}
}

func (v *funcValidator) markMightThrow() {
	if !v.reachableOK || v.currentCatch == -1 {
		return
	}
	v.ctrl[v.currentCatch].mightThrow = true
}

// blockType is a block type immediate: a type index, no result, or one
// result.
type blockType struct {
	length    int
	isIndex   bool
	index     uint32
	sig       *module.FuncType
	hasResult bool
	result    module.ValueType
}

func (bt *blockType) inArity() int {
	if bt.sig != nil {
		return len(bt.sig.Params)
	}
	return 0
}

func (bt *blockType) outArity() int {
	if bt.sig != nil {
		return len(bt.sig.Results)
	}
	if bt.hasResult {
		return 1
	}
	return 0
}

func (bt *blockType) in(i int) module.ValueType { return bt.sig.Params[i] }

func (bt *blockType) out(i int) module.ValueType {
	if bt.sig != nil {
		return bt.sig.Results[i]
	}
	return bt.result
}

func (v *funcValidator) readBlockType(pos int) blockType {
	val, n := v.readI33v(pos, "block type")
	bt := blockType{length: n}
	if val < 0 {
		if val < -64 {
			v.errorf(pos, "invalid block type %d", val)
			return bt
		}
		if byte(val)&0x7f != codeVoid {
			bt.hasResult = true
			bt.result, bt.length = readValueType(&v.Decoder, pos, &v.det, v.lim)
		}
		return bt
	}
	bt.isIndex = true
	bt.index = uint32(val)
	return bt
}

func (v *funcValidator) validateBlockType(pos int, bt *blockType) bool {
	if bt.isIndex {
		if !v.m.HasSignature(bt.index) {
			v.errorf(pos, "block type index %d is not a signature definition", bt.index)
			return false
		}
		bt.sig = v.m.Types[bt.index].Func
		return true
	}
	if bt.hasResult {
		return validateValueType(&v.Decoder, pos, v.m, bt.result)
	}
	return true
}

// pushControl starts a construct of kind with block type bt, whose
// parameters it takes from the stack.
func (v *funcValidator) pushControl(kind controlKind, bt *blockType) *control {
	n := bt.inArity()
	v.ensureStackArguments(n)
	base := len(v.stack) - n
	for i := 0; i < n; i++ {
		v.validateStackValue(i, v.stack[base+i], bt.in(i))
	}
	reach := innerReachability(v.top().reach)
	mergeBase := uint32(len(v.merges))
	pc := uint32(v.pc)
	for i := 0; i < bt.outArity(); i++ {
		v.merges = append(v.merges, value{pc, bt.out(i)})
	}
	for i := 0; i < n; i++ {
		v.stack[base+i].t = bt.in(i)
	}
	startAt := uint32(len(v.merges))
	v.merges = append(v.merges, v.stack[base:]...)
	if kind == ctrlTry {
		n := 1
		for i := range v.ctrl {
			if v.ctrl[i].isTry() {
				n++
			}
		}
		v.tries = max(v.tries, n)
	}
	v.ctrl = append(v.ctrl, control{
		kind: kind, reach: reach, pc: pc,
		stackDepth: uint32(base), initDepth: uint32(len(v.initStack)), prevCatch: -1,
		start:     merge{at: startAt, arity: uint32(n), reached: reach == reachable},
		end:       merge{at: mergeBase, arity: uint32(bt.outArity())},
		mergeBase: mergeBase, catchAt: uint32(len(v.catches)),
	})
	return v.top()
}

func (v *funcValidator) pushMergeValues(c *control, mg *merge) {
	if v.constMode {
		return
	}
	v.stack = append(v.stack[:c.stackDepth], v.merges[mg.at:mg.at+mg.arity]...)
	if !v.refs {
		for _, x := range v.merges[mg.at : mg.at+mg.arity] {
			if x.t.IsRef() {
				v.refs = true
			}
		}
	}
}

func (v *funcValidator) popControl() {
	c := v.top()
	if c.kind != ctrlLoop || c.reach == unreachable {
		v.pushMergeValues(c, &c.end)
	}
	v.rollbackLocalsInitialization(c)
	parentReached := c.reach == reachable || c.end.reached || c.kind == ctrlIf
	v.merges = v.merges[:c.mergeBase]
	v.catches = v.catches[:c.catchAt]
	v.ctrl = v.ctrl[:len(v.ctrl)-1]
	if !parentReached {
		v.setSucceedingCodeDynamicallyUnreachable()
	}
	v.reachableOK = v.ok() && v.top().reach == reachable
}

// --- Locals ---

func (v *funcValidator) isLocalInitialized(i uint32) bool {
	return !v.hasNonDefaultable || v.initialized[i]
}

func (v *funcValidator) setLocalInitialized(i uint32) {
	if !v.hasNonDefaultable || v.initialized[i] {
		return
	}
	v.initialized[i] = true
	v.initStack = append(v.initStack, i)
}

func (v *funcValidator) rollbackLocalsInitialization(c *control) {
	if !v.hasNonDefaultable {
		return
	}
	for uint32(len(v.initStack)) > c.initDepth {
		i := v.initStack[len(v.initStack)-1]
		v.initStack = v.initStack[:len(v.initStack)-1]
		v.initialized[i] = false
	}
}
