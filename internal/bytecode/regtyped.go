package bytecode

// Typed register code (docs/register-interpreter.md, "The typed table"):
// a function whose every register has one kind for the whole function --
// a Value, a raw integer word or a raw float64 -- proven by the producer
// that typed it, and run through a second table, whose values of the
// operations typed code never holds are its raw operations. The ordinary
// code (G) stays the function's; the typed code (T) is a twin of it that
// only a call site enters.

// RegKind is a register's kind in typed code (RegCode.Kinds).
type RegKind uint8

const (
	// RegValue is a Value, as every register of ordinary code.
	RegValue RegKind = iota
	// RegInt is a raw int32 in the register's number word: its low 32
	// bits, which an int operation reads and writes, the 32 above them
	// unread.
	RegInt
	// RegFloat is a raw float64 in the register's number word, its NaN of
	// any payload.
	RegFloat
)

// TypedPlainOps are the ordinary operations typed code may hold, which
// keep their values and their handlers in the typed table: padding, Value
// moves into a call's window, its this and constant arguments, an import
// read from the module's bindings, jumps, the call and the returns.
var TypedPlainOps = []ROp{RNop, RMove, RLoadInt, RLoadUndef, RGetUpval, RJmp, RCallW, RRet, RRetUndef}

// TypedProtoOps are the ordinary operations the call path's prototype
// gives typed twins of as well (TypedTwin): the arithmetic, the
// compare-and-branches and the global reads of call_fib_rec, call_even,
// call_odd and cross_add, whose twins run them by plain's handlers
// through the typed table.
var TypedProtoOps = []ROp{RGetGlobal, RAdd, RAddI, RSubI, ROrI, RAddI32, RJNLtI, RJSNeI}

// TypedTableOps are the ordinary operations whose plain handlers the typed
// table holds: TypedPlainOps and TypedProtoOps.
func TypedTableOps() []ROp {
	return append(append([]ROp(nil), TypedPlainOps...), TypedProtoOps...)
}

// SetTyped gives the function whose register code l is the typed code t,
// compiled when first asked for (RegTypedCode). It is set where the
// function is compiled, before the runtimes running it share it.
func (l *RegLater) SetTyped(t *RegLater) { l.typed = t }

// RegTypedCode is f's typed register code, compiled now if it has not
// been, or nil where f has none.
func (f *Function) RegTypedCode() *RegCode {
	l := f.Reg
	if l == nil || l.typed == nil {
		return nil
	}
	return l.typed.code()
}

// TypedPC marks a pc of typed code where a stack trace keeps it apart
// from one of the function's ordinary code (Function.PosAt): a frame
// running the typed code holds its pc, which the trace marks as it
// captures the frame. Code is far shorter than this bit.
const TypedPC = 1 << 31

// RegTypedCompiled is f's typed register code where it has been compiled,
// or nil.
func (f *Function) RegTypedCompiled() *RegCode {
	if l := f.Reg; l != nil && l.typed != nil && l.typed.done.Load() {
		return l.typed.rc
	}
	return nil
}

// HasRegTyped reports whether f has typed register code, compiled or not.
func (f *Function) HasRegTyped() bool {
	l := f.Reg
	return l != nil && l.typed != nil
}

// TypedTwin is rc's typed twin for the call path's prototype: the same
// instructions as typed code, every register a Value, its call sites
// numbered past rc's -- the closure's call caches serve both codes, and a
// typed site caches only what typed code may enter -- or nil where rc
// holds an operation the typed table does not run (TypedTableOps), a
// handler, or more call sites than a C operand numbers twice.
func TypedTwin(rc *RegCode) *RegCode {
	if rc == nil || !rc.Wide || rc.Typed || len(rc.Handlers) > 0 || rc.LocalReg != nil ||
		rc.UsesArguments || rc.ForwardsArguments || rc.KeepsArgs || rc.ThisRef || 2*rc.CallSites > 1<<16 {
		return nil
	}
	var ok [256]bool
	for _, op := range TypedTableOps() {
		ok[op] = true
	}
	t := *rc
	t.Code = make([]RInstr, len(rc.Code))
	for pc, w := range rc.Code {
		if !ok[w.Op()] {
			return nil
		}
		if w.Op() == RCallW {
			w = w&^(0xffff<<48) | RInstr(uint64(w.C()+uint16(rc.CallSites))<<48)
		}
		t.Code[pc] = w
	}
	t.CallSites = 2 * rc.CallSites
	t.Typed = true
	t.Kinds = make([]RegKind, rc.NumRegs)
	return &t
}

// AsmUp is what typed code's link checks of one of its function's
// upvalues (RegCode.Asm): the module binding the code reads it as.
type AsmUp struct {
	Kind AsmUpKind
	// View is a heap view's element type (asmjs.View), Name a Math
	// function's name, Len a function table's length.
	View uint8
	Name string
	Len  int
}

// AsmUpKind is what a module binding an upvalue holds is.
type AsmUpKind uint8

const (
	// AsmUpNone is an upvalue the typed code does not read.
	AsmUpNone AsmUpKind = iota
	// AsmUpView is a view of the heap, which the heap's accesses read
	// through the code entry's heap instead.
	AsmUpView
	// AsmUpMath is a stdlib Math function the code inlines: the realm's
	// own, or the code is not entered.
	AsmUpMath
	// AsmUpConst is a stdlib constant, a number.
	AsmUpConst
	// AsmUpVarI and AsmUpVarF are a module variable, an int's or a
	// double's cell, read and written by TGGetI ... TGSetF.
	AsmUpVarI
	AsmUpVarF
	// AsmUpFFI is an import, called by call_w.
	AsmUpFFI
	// AsmUpFunc is a function of the module, called by call_w.
	AsmUpFunc
	// AsmUpTable is a function table, read by TTabGet.
	AsmUpTable
)
