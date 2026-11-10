//go:build quickjs_wasm

package wasmjs

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
)

// Memory limits.
//
// A runtime made with a memory limit measures its heap by walking it, and
// what wasm holds is part of that heap: the bridge's budget charges the
// runtime's meter as memories and tables are made and grown and as GC
// objects are allocated, and the walk asks each wasm object it reaches
// through the JS objects standing for them what it holds (vm.HostSized).
// A memory or table grow past the limit fails as one that cannot be had
// does; a GC allocation past it stops the script, as a JavaScript one does.

// jsMeter is the runtime's meter, as the budget charges it.
type jsMeter struct{ r *vm.Runtime }

func (m jsMeter) TryReserve(n int64) bool { return vm.HostReserveMemory(m.r, n) }
func (m jsMeter) Charge(n int64) error    { return vm.HostChargeMemory(m.r, n) }
func (m jsMeter) Exceeded() error         { return vm.HostMemoryExceeded(m.r) }

// The JIT's memory.
//
// A runtime made WithJIT compiles its modules' functions to native code as
// it does its JavaScript, and bounds what that holds by its JIT memory
// limit (WithJITMemoryLimit, or its default) as it bounds its JavaScript's
// code: a budget of that limit, which every module of the runtime's code
// is charged to, past which the engine drops old code and then refuses
// compiles (engine.JITBudget). It is a budget of its own, not the
// JavaScript JIT's: that one's accounting and the code it drops live on the
// runtime's goroutine, while wasm's code is compiled, placed and dropped
// under locks of its own, from any goroutine a call or a compile runs on;
// and each budget makes room by dropping its own JIT's code. The runtime's
// two JITs together hold at most twice its limit.

// wasmJITBudget returns the budget of the runtime's modules' native code,
// making it the first time.
func (a *api) wasmJITBudget() *engine.JITBudget {
	if a.jitBudget == nil {
		a.jitBudget = engine.NewJITBudget(vm.HostJITMemoryLimit(a.rt))
	}
	return a.jitBudget
}

// What the objects standing for wasm's hold.

func (s *funcSlot) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	size := s.fn.MemoryFootprint(has)
	if s.a.budget != nil {
		size += s.a.budget.MemoryFootprint(has)
	}
	return size
}

func (d *instanceData) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	return d.inst.MemoryFootprint(has)
}

func (d *memoryData) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	return d.mem.MemoryFootprint(has)
}

func (d *tableData) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	return d.t.MemoryFootprint(has)
}

func (d *globalData) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	return d.g.MemoryFootprint(has)
}

func (d *gcSlot) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	return engine.RefFootprint(d.ref, has)
}

func (d *exceptionData) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	return engine.RefFootprint(engine.ExnRef(d.e), has)
}

var (
	_ vm.HostSized = (*funcSlot)(nil)
	_ vm.HostSized = (*instanceData)(nil)
	_ vm.HostSized = (*memoryData)(nil)
	_ vm.HostSized = (*tableData)(nil)
	_ vm.HostSized = (*globalData)(nil)
	_ vm.HostSized = (*gcSlot)(nil)
	_ vm.HostSized = (*exceptionData)(nil)
	_ vm.HostSized = (*engine.Memory)(nil)
)
