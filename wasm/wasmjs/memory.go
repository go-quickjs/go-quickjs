package wasmjs

import (
	"math"
	"runtime"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
)

// memoryData is what a WebAssembly.Memory carries: the memory and its
// current buffer.
type memoryData struct {
	mem *engine.Memory
	// buf is the buffer the buffer attribute returns, made when first asked
	// for: a fixed-length ArrayBuffer, detached and made anew when the memory
	// grows, or a resizable one that follows the memory as it grows. A
	// shared memory's is a SharedArrayBuffer of the memory's length when it
	// was made.
	buf       vm.Value
	resizable bool
	bufLen    uint64
}

// addressValue is a size or an index of a memory or a table as script sees
// it: a Number for an i32 address type, a BigInt for i64.
func addressValue(v uint64, is64 bool) vm.Value {
	if is64 {
		return vm.HostBigInt64(int64(v))
	}
	return vm.Float(float64(v))
}

// addressToU64 is AddressValueToU64: [EnforceRange] unsigned long for an
// i32 address type, a BigInt in u64's range for i64. name says what is
// converted: "Argument 0", "Property 'initial'".
func (a *api) addressToU64(t thrower, v vm.Value, name string, is64 bool) (uint64, error) {
	if is64 {
		if err := a.bigIntRefused(v); err != nil {
			return 0, err
		}
		n, lossless, err := vm.HostToBigUint64(a.rt, v)
		if err != nil {
			return 0, err
		}
		if !lossless {
			return 0, t.typeError("%s must be in u64 range", name)
		}
		return n, nil
	}
	f, err := a.toNumber(v)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, t.typeError("%s must be convertible to a valid number", name)
	}
	if f < 0 {
		return 0, t.typeError("%s must be non-negative", name)
	}
	if f > math.MaxUint32 {
		return 0, t.typeError("%s must be in the unsigned long range", name)
	}
	return uint64(f), nil
}

// descriptorAddress reads a memory or table descriptor's address type.
func (a *api) descriptorAddress(t thrower, desc vm.Value) (bool, error) {
	v, err := a.rt.GetProp(desc, a.rt.Intern("address"))
	if err != nil || v.IsUndefined() {
		return false, err
	}
	s, err := a.rt.ToString(v)
	if err != nil {
		return false, err
	}
	switch s.Go() {
	case "i64":
		return true, nil
	case "i32":
		return false, nil
	}
	return false, t.typeError("Unknown address type '%s'; pass 'i32' or 'i64'", s.Go())
}

// descriptorBound reads the optional address value prop of a descriptor,
// bounded to [lower, upper].
func (a *api) descriptorBound(t thrower, desc vm.Value, prop string, is64 bool, lower, upper uint64) (uint64, bool, error) {
	v, err := a.rt.GetProp(desc, a.rt.Intern(prop))
	if err != nil || v.IsUndefined() {
		return 0, false, err
	}
	n, err := a.addressToU64(t, v, "Property '"+prop+"'", is64)
	if err != nil {
		return 0, false, err
	}
	if n < lower {
		return 0, false, t.rangeError("Property '%s': value %d is below the lower bound %x", prop, n, lower)
	}
	if n > upper {
		return 0, false, t.rangeError("Property '%s': value %d is above the upper bound %d", prop, n, upper)
	}
	return n, true, nil
}

// newMemory is new WebAssembly.Memory(descriptor).
func (a *api) newMemory(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Memory()")
	if !rt.Constructing() {
		return vm.Undefined, t.typeError("WebAssembly.Memory must be invoked with 'new'")
	}
	proto, err := a.newTargetProto(rt)
	if err != nil {
		return vm.Undefined, err
	}
	desc := arg(args, 0)
	if !desc.IsObject() {
		return vm.Undefined, t.typeError("Argument 0 must be a memory descriptor")
	}
	is64, err := a.descriptorAddress(t, desc)
	if err != nil {
		return vm.Undefined, err
	}
	limit := a.limits.Memory32Pages
	if is64 {
		limit = a.limits.Memory64Pages
	}
	initial, ok, err := a.descriptorBound(t, desc, "initial", is64, 0, limit)
	if err != nil {
		return vm.Undefined, err
	}
	if !ok {
		return vm.Undefined, t.typeError("Property 'initial' is required")
	}
	maximum, hasMax, err := a.descriptorBound(t, desc, "maximum", is64, initial, limit)
	if err != nil {
		return vm.Undefined, err
	}
	sv, err := rt.GetProp(desc, rt.Intern("shared"))
	if err != nil {
		return vm.Undefined, err
	}
	shared := sv.Truthy()
	if shared && !hasMax {
		return vm.Undefined, t.typeError("If shared is true, maximum property should be defined.")
	}
	mem := engine.NewMemory(initial, maximum, hasMax, is64, shared)
	if mem == nil || !mem.SetBudget(a.budget) {
		return vm.Undefined, t.rangeError("could not allocate memory")
	}
	return vm.Obj(a.memoryObject(mem, proto)), nil
}

// memoryObject returns the WebAssembly.Memory of mem, made the first time
// it is asked for.
func (a *api) memoryObject(mem *engine.Memory, proto *vm.Object) *vm.Object {
	if o := a.wrappers.get(unsafe.Pointer(mem)); o != nil {
		return o
	}
	d := &memoryData{mem: mem, buf: vm.Undefined}
	o := vm.HostObject(proto, d)
	if !mem.Shared {
		// Growing the memory, from script or from wasm, refreshes its
		// buffer. The memory keeps its WebAssembly.Memory, so that the buffer
		// refreshed is the one script has, however long ago it took it.
		mem.OnGrow = func(oldBytes, newBytes uint64) {
			a.refreshBuffer(d)
			runtime.KeepAlive(o)
		}
	}
	a.wrappers.put(unsafe.Pointer(mem), o)
	return o
}

// memoryOf returns what this is the WebAssembly.Memory of, or the error of
// a receiver that is not one.
func (a *api) memoryOf(t thrower, this vm.Value) (*memoryData, error) {
	d, ok := vm.HostData(this).(*memoryData)
	if !ok {
		return nil, t.typeError("Receiver is not a WebAssembly.Memory")
	}
	return d, nil
}

// refreshBuffer refreshes the buffer of a memory that grew: a fixed-length
// buffer is detached, to be made anew when next asked for, and a resizable
// one is given the memory's new bytes.
func (a *api) refreshBuffer(d *memoryData) {
	if d.buf.IsUndefined() {
		return
	}
	if d.resizable {
		vm.HostSetArrayBufferBytes(a.rt, d.buf, d.mem.Bytes())
		return
	}
	vm.HostDetachArrayBuffer(a.rt, d.buf)
	d.buf = vm.Undefined
}

// buffer returns the memory's buffer, making it if there is none.
func (a *api) buffer(d *memoryData) vm.Value {
	mem := d.mem
	if mem.Shared {
		size := mem.Size()
		if d.buf.IsUndefined() || d.bufLen != size {
			// The buffer is of the memory's size now, and made anew when
			// the memory has grown: it spans that much, which the runtime's
			// memory limit counts, not the reservation.
			d.buf = vm.HostSharedArrayBuffer(a.rt, mem.Bytes(), int(size), mem, -1, nil)
			// A shared memory's buffer is frozen.
			vm.HostPreventExtensions(d.buf.Object())
			d.bufLen = size
		}
		return d.buf
	}
	if d.buf.IsUndefined() {
		d.buf = vm.HostArrayBuffer(a.rt, mem.Bytes(), mem, false, 0, nil)
		d.resizable = false
	}
	return d.buf
}

// memoryBuffer is the getter of WebAssembly.Memory.prototype.buffer.
func (a *api) memoryBuffer(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	d, err := a.memoryOf(a.thrower("WebAssembly.Memory.buffer"), this)
	if err != nil {
		return vm.Undefined, err
	}
	return a.buffer(d), nil
}

// memoryGrow is WebAssembly.Memory.prototype.grow(delta).
func (a *api) memoryGrow(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Memory.grow()")
	d, err := a.memoryOf(t, this)
	if err != nil {
		return vm.Undefined, err
	}
	mem := d.mem
	delta, err := a.addressToU64(t, arg(args, 0), "Argument 0", mem.Is64)
	if err != nil {
		return vm.Undefined, err
	}
	old := mem.Pages()
	if mem.HasMax && delta > mem.Max-old {
		return vm.Undefined, t.rangeError("Maximum memory size exceeded")
	}
	ret := mem.Grow(delta)
	if ret < 0 {
		return vm.Undefined, t.rangeError("Unable to grow instance memory")
	}
	if !mem.Shared {
		a.refreshBuffer(d)
	}
	return addressValue(uint64(ret), mem.Is64), nil
}

// memoryToFixedLengthBuffer is
// WebAssembly.Memory.prototype.toFixedLengthBuffer().
func (a *api) memoryToFixedLengthBuffer(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	d, err := a.memoryOf(a.thrower("WebAssembly.Memory.toFixedLengthBuffer()"), this)
	if err != nil {
		return vm.Undefined, err
	}
	if d.mem.Shared || !d.resizable || d.buf.IsUndefined() {
		return a.buffer(d), nil
	}
	old := d.buf
	d.buf, d.resizable = vm.Undefined, false
	fixed := a.buffer(d)
	vm.HostDetachArrayBuffer(a.rt, old)
	return fixed, nil
}

// memoryToResizableBuffer is
// WebAssembly.Memory.prototype.toResizableBuffer().
func (a *api) memoryToResizableBuffer(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Memory.toResizableBuffer()")
	d, err := a.memoryOf(t, this)
	if err != nil {
		return vm.Undefined, err
	}
	mem := d.mem
	if !mem.HasMax {
		return vm.Undefined, t.typeError("Memory must have a maximum")
	}
	if mem.Shared {
		return a.buffer(d), nil
	}
	if d.resizable {
		return d.buf, nil
	}
	old := d.buf
	maxBytes := int64(mem.Max) * engine.PageSize
	d.buf = vm.HostArrayBuffer(a.rt, mem.Bytes(), mem, true, maxBytes, func(n int) error {
		size := mem.Size()
		if n < 0 || uint64(n) < size || (uint64(n)-size)%engine.PageSize != 0 {
			return a.rt.ThrowRangeError("ArrayBuffer.prototype.resize: Invalid length parameter")
		}
		delta := (uint64(n) - size) / engine.PageSize
		if delta > mem.Max-mem.Pages() || mem.Grow(delta) < 0 {
			return a.rt.ThrowRangeError("ArrayBuffer.prototype.resize: Invalid length parameter")
		}
		a.refreshBuffer(d)
		return nil
	})
	d.resizable = true
	if !old.IsUndefined() {
		vm.HostDetachArrayBuffer(a.rt, old)
	}
	return d.buf, nil
}
