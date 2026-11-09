package engine

// TrapCode identifies why execution trapped.
type TrapCode uint8

// The traps. Their messages are those a WebAssembly.RuntimeError carries.
const (
	TrapUnreachable TrapCode = iota + 1
	TrapMemOutOfBounds
	TrapUnalignedAccess
	TrapDivByZero
	TrapDivUnrepresentable
	TrapRemByZero
	TrapFloatUnrepresentable
	TrapTableOutOfBounds
	TrapNullFunc
	TrapFuncSigMismatch
	TrapDataSegmentOutOfBounds
	TrapElementSegmentOutOfBounds
	TrapNullDereference
	TrapIllegalCast
	TrapArrayOutOfBounds
	TrapArrayTooLarge
	TrapRethrowNull
	// TrapStackOverflow is the call stack's exhaustion, a RangeError in JS.
	TrapStackOverflow
	// TrapAtomicsWaitNotAllowed is a wait on a memory that is not shared.
	TrapAtomicsWaitNotAllowed
)

var trapMessages = [...]string{
	TrapUnreachable:               "unreachable",
	TrapMemOutOfBounds:            "memory access out of bounds",
	TrapUnalignedAccess:           "operation does not support unaligned accesses",
	TrapDivByZero:                 "divide by zero",
	TrapDivUnrepresentable:        "divide result unrepresentable",
	TrapRemByZero:                 "remainder by zero",
	TrapFloatUnrepresentable:      "float unrepresentable in integer range",
	TrapTableOutOfBounds:          "table index is out of bounds",
	TrapNullFunc:                  "null function",
	TrapFuncSigMismatch:           "function signature mismatch",
	TrapDataSegmentOutOfBounds:    "data segment out of bounds",
	TrapElementSegmentOutOfBounds: "element segment out of bounds",
	TrapNullDereference:           "dereferencing a null pointer",
	TrapIllegalCast:               "illegal cast",
	TrapArrayOutOfBounds:          "array element access out of bounds",
	TrapArrayTooLarge:             "requested new array is too large",
	TrapRethrowNull:               "rethrowing null value",
	TrapStackOverflow:             "Maximum call stack size exceeded",
	TrapAtomicsWaitNotAllowed:     "Atomics.wait cannot be called in this context",
}

// String returns the trap's message.
func (c TrapCode) String() string {
	if int(c) < len(trapMessages) && trapMessages[c] != "" {
		return trapMessages[c]
	}
	return "unknown trap"
}

// Trap is the error a trap makes: a RuntimeError whose message is the
// trap's, or a message of its own for the traps instantiation reports with
// details.
type Trap struct {
	Code TrapCode
	Msg  string
	// Frames is the wasm call stack where the trap happened, innermost
	// first.
	Frames []Frame
}

// Frame is a function on a trap's call stack.
type Frame struct {
	Module *Instance
	Func   uint32
	// Offset is the module offset of the instruction that was executing.
	Offset uint32
}

// Error returns the trap's message.
func (t *Trap) Error() string {
	if t.Msg != "" {
		return t.Msg
	}
	return t.Code.String()
}

// trap panics with a trap of code c; the interpreter's entry recovers it.
// The innermost frame reports the offset the exec's call stack holds for
// it: native code's, which it records before Go traps for it, or a call's.
func trap(c TrapCode) { panic(&Trap{Code: c}) }

// trapAt panics with a trap of code c raised by the instruction at module
// offset at, which the innermost frame reports; at is 0 where there is no
// frame, in a constant expression. It panics with a trapSite, which
// recovered makes the *Trap: a value that a closure inlining trapAt passes
// to the runtime's conversion, so that it keeps nothing live across a call
// and spills nothing on the path that does not trap.
func trapAt(c TrapCode, at uint32) { panic(trapSite{c, at}) }

// trapSite is what trapAt panics with: the trap's code and the offset of
// the instruction that raised it.
type trapSite struct {
	code TrapCode
	at   uint32
}

// trap returns the trap the site raised, its innermost frame at the
// site's instruction among frames.
func (s trapSite) trap(frames []Frame) *Trap {
	if s.at != 0 && len(frames) > 0 {
		frames[0].Offset = s.at
	}
	return &Trap{Code: s.code, Frames: frames}
}
