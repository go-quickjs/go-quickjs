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

// What the objects standing for wasm's hold.

func (s *funcSlot) MemoryFootprint(has func(unsafe.Pointer) bool) int64 {
	size := s.fn.MemoryFootprint(has)
	if s.budget != nil {
		size += s.budget.MemoryFootprint(has)
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
