package vm

// A throw statement the tree tier builds as an edge to a handler of its own
// frame (throwToHandler) gave the value to unwindToHandler as an error: a
// Thrown, with the frames of the stack captured for a trace, made for the
// handler to take the value out of again at once. throw i in a loop took
// 300 ns a turn, ten times QuickJS's, most of it the trace and then the
// collection of what was allocated for it. Where no debugger is told of
// the throw -- unwindToHandler's first question -- throwValueToHandler
// unwinds to the handler with the value itself, and nothing is made.
//
// It is a variable, assigned a closure in this file's init, laid out after
// every closure of the tree tier's (see AGENTS.md).

var (
	// throwValueToHandler is unwindToHandler for v thrown at sp in f to its
	// innermost handler, which the caller has found to be the one it
	// expects, without a debugger: f's pc is then the handler's, and the
	// stack holds what it takes.
	throwValueToHandler func(r *Runtime, f *frame, sp int, v Value)
)

func init() {
	throwValueToHandler = func(r *Runtime, f *frame, sp int, v Value) {
		h := f.handlers[len(f.handlers)-1]
		f.handlers = f.handlers[:len(f.handlers)-1]
		depth := f.base + h.stackDepth
		r.closeIteratorsIn(depth, sp)
		r.stack[depth] = v
		if h.isFinally {
			r.stack[depth+1] = Float(float64(completionThrow))
		}
		f.pc = h.pc
	}
}
