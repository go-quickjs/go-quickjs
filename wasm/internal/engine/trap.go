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
func trap(c TrapCode) { panic(&Trap{Code: c}) }
