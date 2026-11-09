package engine

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

// indirectChecksType reports whether call_indirect of type typeIndex through
// table must check the type of the function it finds: whether the table's
// element type admits functions of other types.
func indirectChecksType(m *module.Module, typeIndex, table uint32) bool {
	return !module.IsHeapSubtypeOf(m.Tables[table].Type.Heap, module.HeapIndex(typeIndex), m)
}

// indirectCallee returns the function call_indirect of type typeIndex finds
// at index i of table, trapping if there is none of that type: "table index
// is out of bounds" past the table's end; for a null entry, "function
// signature mismatch" where the call checks its callee's type and "null
// function" where the table's type made the check needless.
func indirectCallee(inst *Instance, typeIndex, table uint32, i uint64, checked bool) *Function {
	t := inst.Tables[table]
	if i >= uint64(len(t.Elems)) {
		trap(TrapTableOutOfBounds)
	}
	e := t.Elems[i]
	if e.IsNull() {
		if checked {
			trap(TrapFuncSigMismatch)
		}
		trap(TrapNullFunc)
	}
	fn := e.Func()
	if checked {
		want := inst.Module.Canonical[typeIndex]
		if fn.Type != want && !module.IsCanonicalSubtype(fn.Type, want) {
			trap(TrapFuncSigMismatch)
		}
	}
	return fn
}
