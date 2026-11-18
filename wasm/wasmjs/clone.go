//go:build quickjs_wasm

package wasmjs

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
)

// Structured cloning.
//
// A Module and a shared Memory can be cloned -- posted to a worker, or given
// to structuredClone -- and nothing else of the namespace can. A Module's
// clone is a Module of the same compiled code, which the runtime it arrives
// in instantiates without compiling it again: its native code, its compile
// settings and the budget its native code is charged to stay the compiling
// runtime's. A shared Memory's clone is a Memory of the same memory, whose
// buffer is a SharedArrayBuffer of the same bytes: a grow made through either
// is seen by both, and a wait on one is woken by a notify on the other.

// errNoNamespace refuses a clone in a runtime the namespace is not installed
// in, which has nothing to make it with.
var errNoNamespace = &vm.DataCloneError{Message: "Unable to deserialize cloned data."}

// moduleToken is a Module, serialized.
type moduleToken struct{ md *moduleData }

// CloneToken serializes a Module: any can be cloned.
func (md *moduleData) CloneToken() (vm.HostCloneToken, bool) { return moduleToken{md}, true }

// Revive makes the Module in r.
func (t moduleToken) Revive(r *vm.Runtime) (vm.Value, error) {
	a := apiOf(r)
	if a == nil {
		return vm.Undefined, errNoNamespace
	}
	return vm.Obj(a.moduleObject(t.md, a.moduleProto)), nil
}

// memoryToken is a shared Memory, serialized.
type memoryToken struct{ mem *engine.Memory }

// CloneToken serializes a Memory: only a shared one can be cloned.
func (d *memoryData) CloneToken() (vm.HostCloneToken, bool) {
	if !d.mem.Shared {
		return nil, false
	}
	// The runtime it is cloned to may grow the memory, on its goroutine.
	d.mem.Unbudget()
	return memoryToken{d.mem}, true
}

// Revive makes a Memory of the memory in r: a new one, even in the runtime
// it was cloned from, as a clone is a new object.
func (t memoryToken) Revive(r *vm.Runtime) (vm.Value, error) {
	a := apiOf(r)
	if a == nil {
		return vm.Undefined, errNoNamespace
	}
	o := vm.HostObject(a.memoryProto, &memoryData{mem: t.mem, buf: vm.Undefined})
	if a.wrappers.get(unsafe.Pointer(t.mem)) == nil {
		a.wrappers.put(unsafe.Pointer(t.mem), o)
	}
	return vm.Obj(o), nil
}
