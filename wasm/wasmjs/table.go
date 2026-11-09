//go:build quickjs_wasm

package wasmjs

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// tableData is what a WebAssembly.Table carries.
type tableData struct {
	t *engine.Table
}

// tableElementTypes are the element types a Table's descriptor may name.
var tableElementTypes = map[string]module.ValueType{
	"anyfunc":   module.FuncRef,
	"externref": module.ExternRef,
	"anyref":    module.AnyRef,
	"eqref":     module.EqRef,
	"structref": module.StructRef,
	"arrayref":  module.ArrayRef,
	"i31ref":    module.I31Ref,
}

// newTable is new WebAssembly.Table(descriptor, value).
func (a *api) newTable(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Table()")
	if !rt.Constructing() {
		return vm.Undefined, t.typeError("WebAssembly.Table must be invoked with 'new'")
	}
	proto, err := a.newTargetProto(rt)
	if err != nil {
		return vm.Undefined, err
	}
	desc := arg(args, 0)
	if !desc.IsObject() {
		return vm.Undefined, t.typeError("Argument 0 must be a table descriptor")
	}
	ev, err := rt.GetProp(desc, rt.Intern("element"))
	if err != nil {
		return vm.Undefined, err
	}
	es, err := rt.ToString(ev)
	if err != nil {
		return vm.Undefined, err
	}
	typ, ok := tableElementTypes[es.Go()]
	if !ok {
		return vm.Undefined, t.typeError("Descriptor property 'element' must be a WebAssembly reference type")
	}
	is64, err := a.descriptorAddress(t, desc)
	if err != nil {
		return vm.Undefined, err
	}
	initial, ok, err := a.descriptorBound(t, desc, "initial", is64, 0, uint64(a.limits.TableInitEntries))
	if err != nil {
		return vm.Undefined, err
	}
	if !ok {
		return vm.Undefined, t.typeError("Property 'initial' is required")
	}
	maximum, hasMax, err := a.descriptorBound(t, desc, "maximum", is64, initial, ^uint64(0))
	if err != nil {
		return vm.Undefined, err
	}
	init := a.hostRef(defaultRef(typ))
	if initial > 0 && len(args) >= 2 && !args[1].IsUndefined() {
		ref, msg := a.toWasmRef(args[1], typ)
		if msg != "" {
			return vm.Undefined, t.typeError("Argument 2 must be undefined or a value of type compatible with the type of the new table: %s.", msg)
		}
		init = ref
	}
	tab := engine.NewTable(typ, nil, initial, maximum, hasMax, is64, init)
	if !tab.SetBudget(a.budget) {
		return vm.Undefined, vm.HostMemoryExceeded(rt)
	}
	return vm.Obj(a.tableObject(tab, proto)), nil
}

// tableObject returns the WebAssembly.Table of tab, made the first time it
// is asked for.
func (a *api) tableObject(tab *engine.Table, proto *vm.Object) *vm.Object {
	if o := a.wrappers.get(unsafe.Pointer(tab)); o != nil {
		return o
	}
	o := vm.HostObject(proto, &tableData{t: tab})
	a.wrappers.put(unsafe.Pointer(tab), o)
	return o
}

// tableOf returns the table this is the WebAssembly.Table of, or the error
// of a receiver that is not one.
func (a *api) tableOf(t thrower, this vm.Value) (*engine.Table, error) {
	d, ok := vm.HostData(this).(*tableData)
	if !ok {
		return nil, t.typeError("Receiver is not a WebAssembly.Table")
	}
	return d.t, nil
}

// tableLength is the getter of WebAssembly.Table.prototype.length.
func (a *api) tableLength(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	tab, err := a.tableOf(a.thrower("WebAssembly.Table.length()"), this)
	if err != nil {
		return vm.Undefined, err
	}
	return addressValue(tab.Size(), tab.Is64), nil
}

// tableGrow is WebAssembly.Table.prototype.grow(delta, value).
func (a *api) tableGrow(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Table.grow()")
	tab, err := a.tableOf(t, this)
	if err != nil {
		return vm.Undefined, err
	}
	delta, err := a.addressToU64(t, arg(args, 0), "Argument 0", tab.Is64)
	if err != nil {
		return vm.Undefined, err
	}
	typ := canon(tab.Type, tab.Module)
	var init engine.Ref
	switch {
	case len(args) >= 2:
		ref, msg := a.toWasmRef(args[1], typ)
		if msg != "" {
			return vm.Undefined, t.typeError("Argument 1 is invalid: %s", msg)
		}
		init = ref
	case !typ.Nullable:
		return vm.Undefined, t.typeError("Argument 1 must be specified for non-nullable element type")
	default:
		init = a.hostRef(defaultRef(typ))
	}
	old := int64(-1)
	if delta <= engine.MaxTableSize {
		old = tab.Grow(delta, init)
	}
	if old < 0 {
		return vm.Undefined, t.rangeError("failed to grow table by %d", delta)
	}
	return addressValue(uint64(old), tab.Is64), nil
}

// tableIndex reads and bounds-checks a table index.
func (a *api) tableIndex(t thrower, tab *engine.Table, v vm.Value) (uint64, error) {
	i, err := a.addressToU64(t, v, "Argument 0", tab.Is64)
	if err != nil {
		return 0, err
	}
	if i >= tab.Size() {
		return 0, t.rangeError("invalid address %d in %s table of size %d", i, tab.Type.Name(), tab.Size())
	}
	return i, nil
}

// tableGet is WebAssembly.Table.prototype.get(index).
func (a *api) tableGet(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Table.get()")
	tab, err := a.tableOf(t, this)
	if err != nil {
		return vm.Undefined, err
	}
	i, err := a.tableIndex(t, tab, arg(args, 0))
	if err != nil {
		return vm.Undefined, err
	}
	return a.refResult(t, tab.Elems[i], tab.Type)
}

// refResult is a reference read from a table, a global or an exception, as
// script sees it; an exception reference has no JavaScript value.
func (a *api) refResult(t thrower, r engine.Ref, typ module.ValueType) (vm.Value, error) {
	if typ.Heap == module.HeapExn || typ.Heap == module.HeapNoExn {
		return vm.Undefined, t.typeError("invalid type %s", typ.Name())
	}
	return a.refToJS(r), nil
}

// tableSet is WebAssembly.Table.prototype.set(index, value).
func (a *api) tableSet(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Table.set()")
	tab, err := a.tableOf(t, this)
	if err != nil {
		return vm.Undefined, err
	}
	i, err := a.tableIndex(t, tab, arg(args, 0))
	if err != nil {
		return vm.Undefined, err
	}
	typ := canon(tab.Type, tab.Module)
	var ref engine.Ref
	switch {
	case len(args) >= 2:
		r, msg := a.toWasmRef(args[1], typ)
		if msg != "" {
			return vm.Undefined, t.typeError("Argument 1 is invalid for table: %s", msg)
		}
		ref = r
	case typ.Nullable:
		ref = a.hostRef(defaultRef(typ))
	default:
		return vm.Undefined, t.typeError("Table of non-defaultable type %s needs explicit element", tab.Type.Name())
	}
	tab.Elems[i] = ref
	return vm.Undefined, nil
}
