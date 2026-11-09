package wasmjs

import (
	"math"
	"strconv"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// tagData is what a WebAssembly.Tag carries.
type tagData struct {
	tag *engine.Tag
}

// exceptionData is what a WebAssembly.Exception carries.
type exceptionData struct {
	e *engine.Exception
}

// maxParams is the most parameters a tag's type may have.
const maxParams = 1000

// makeJSTag makes WebAssembly.JSTag, the tag of type [externref] JavaScript
// exceptions crossing wasm are thrown with.
func (a *api) makeJSTag() {
	c := module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.ExternRef}})
	a.jsTag = &engine.Tag{Type: c, Sig: module.Canonical(c).Func}
	a.jsTagObj = a.tagObject(a.jsTag, a.tagProto)
	vm.HostDefineOwn(a.ns, a.rt.Intern("JSTag"), vm.Obj(a.jsTagObj), false, false, true)
}

// iterableLength reads the length of a list a descriptor gives, which must
// be an array index; false if it is not one.
func (a *api) iterableLength(o vm.Value) (uint32, bool, error) {
	lv, err := a.rt.GetProp(o, a.rt.Intern("length"))
	if err != nil {
		return 0, false, err
	}
	if lv.IsNumber() {
		f := lv.Number()
		if f >= 0 && f < math.MaxUint32 && f == math.Trunc(f) && !(f == 0 && math.Signbit(f)) {
			return uint32(f), true, nil
		}
	}
	s, err := a.rt.ToString(lv)
	if err != nil {
		return 0, false, err
	}
	n, err := strconv.ParseUint(s.Go(), 10, 32)
	if err != nil || n == math.MaxUint32 || strconv.FormatUint(n, 10) != s.Go() {
		return 0, false, nil
	}
	return uint32(n), true, nil
}

// newTag is new WebAssembly.Tag(type).
func (a *api) newTag(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Tag()")
	if !rt.Constructing() {
		return vm.Undefined, t.typeError("WebAssembly.Tag must be invoked with 'new'")
	}
	typ := arg(args, 0)
	if !typ.IsObject() {
		return vm.Undefined, t.typeError("Argument 0 must be a tag type")
	}
	params, err := rt.GetProp(typ, rt.Intern("parameters"))
	if err != nil {
		return vm.Undefined, err
	}
	if !params.IsObject() {
		return vm.Undefined, t.typeError("Argument 0 must be a tag type with 'parameters'")
	}
	n, ok, err := a.iterableLength(params)
	if err != nil {
		return vm.Undefined, err
	}
	if !ok {
		return vm.Undefined, t.typeError("Argument 0 contains parameters without 'length'")
	}
	if n > maxParams {
		return vm.Undefined, t.typeError("Argument 0 contains too many parameters")
	}
	types := make([]module.ValueType, n)
	for i := range types {
		v, err := rt.GetProp(params, rt.Intern(strconv.Itoa(i)))
		if err != nil {
			return vm.Undefined, err
		}
		vt, ok, err := a.valueType(v)
		if err != nil {
			return vm.Undefined, err
		}
		if !ok {
			return vm.Undefined, t.typeError("Argument 0 parameter type at index #%d must be a value type", i)
		}
		types[i] = vt
	}
	c := module.CanonicalFunc(&module.FuncType{Params: types})
	return vm.Obj(a.newTagObject(&engine.Tag{Type: c, Sig: module.Canonical(c).Func})), nil
}

// newTagObject makes a WebAssembly.Tag of a tag of its own.
func (a *api) newTagObject(tag *engine.Tag) *vm.Object {
	return a.tagObject(tag, a.tagProto)
}

// tagObject returns the WebAssembly.Tag of tag, made the first time it is
// asked for.
func (a *api) tagObject(tag *engine.Tag, proto *vm.Object) *vm.Object {
	if o := a.wrappers.get(unsafe.Pointer(tag)); o != nil {
		return o
	}
	o := vm.HostObject(proto, &tagData{tag: tag})
	a.wrappers.put(unsafe.Pointer(tag), o)
	return o
}

// tagOf returns the tag v is the WebAssembly.Tag of, or nil.
func tagOf(v vm.Value) *engine.Tag {
	if d, ok := vm.HostData(v).(*tagData); ok {
		return d.tag
	}
	return nil
}

// exceptionObject returns the WebAssembly.Exception of e, made the first
// time it is asked for.
func (a *api) exceptionObject(e *engine.Exception) *vm.Object {
	if o := a.wrappers.get(unsafe.Pointer(e)); o != nil {
		return o
	}
	o := vm.HostObject(a.exceptionProto, &exceptionData{e: e})
	a.wrappers.put(unsafe.Pointer(e), o)
	return o
}

// exceptionOf returns the exception v is the WebAssembly.Exception of, or
// nil.
func (a *api) exceptionOf(v vm.Value) *engine.Exception {
	if d, ok := vm.HostData(v).(*exceptionData); ok {
		return d.e
	}
	return nil
}

// newException is new WebAssembly.Exception(tag, payload, options).
func (a *api) newException(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Exception()")
	if !rt.Constructing() {
		return vm.Undefined, t.typeError("WebAssembly.Exception must be invoked with 'new'")
	}
	tag := tagOf(arg(args, 0))
	if tag == nil {
		return vm.Undefined, t.typeError("Argument 0 must be a WebAssembly tag")
	}
	if tag == a.jsTag {
		return vm.Undefined, t.typeError("Argument 0 cannot be WebAssembly.JSTag")
	}
	sig := tag.Sig
	if len(sig.Results) != 0 {
		return vm.Undefined, t.typeError("Invalid WebAssembly tag (return values not permitted in Exception tag)")
	}
	payload := arg(args, 1)
	if !payload.IsObject() {
		return vm.Undefined, t.typeError("Exception values must be an iterable object")
	}
	n, ok, err := a.iterableLength(payload)
	if err != nil {
		return vm.Undefined, err
	}
	if !ok {
		return vm.Undefined, t.typeError("Exception values argument has no length")
	}
	if int(n) != len(sig.Params) {
		return vm.Undefined, t.typeError("Number of exception values does not match signature length")
	}
	vals := make([]engine.Val, len(sig.Params))
	for i, pt := range sig.Params {
		v, err := rt.GetProp(payload, rt.Intern(strconv.Itoa(i)))
		if err != nil {
			return vm.Undefined, err
		}
		switch {
		case pt.Kind == module.KindV128:
			return vm.Undefined, t.typeError("Invalid type v128")
		case pt.IsRef():
			ref, msg := a.toWasmRef(v, pt)
			if msg != "" {
				return vm.Undefined, t.typeError("%s", msg)
			}
			vals[i] = engine.Val{R: ref}
		case v.IsUndefined():
		default:
			n, err := a.toWasmNum(v, pt)
			if err != nil {
				return vm.Undefined, err
			}
			vals[i] = n
		}
	}
	opts := arg(args, 2)
	if !opts.IsNullish() && !opts.IsObject() {
		return vm.Undefined, t.typeError("Argument 2 is not an object")
	}
	o := a.exceptionObject(engine.NewException(tag, vals, nil))
	if opts.IsObject() {
		ts, err := rt.GetProp(opts, rt.Intern("traceStack"))
		if err != nil {
			return vm.Undefined, err
		}
		if ts.Truthy() {
			vm.HostCaptureStack(rt, o)
		}
	}
	return vm.Obj(o), nil
}

// exceptionGetArg is WebAssembly.Exception.prototype.getArg(tag, index).
func (a *api) exceptionGetArg(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Exception.getArg()")
	e := a.exceptionOf(this)
	if e == nil {
		return vm.Undefined, t.typeError("Receiver is not a WebAssembly.Exception")
	}
	tag := tagOf(arg(args, 0))
	if tag == nil {
		return vm.Undefined, t.typeError("Argument 0 must be a WebAssembly.Tag")
	}
	i, err := a.addressToU64(t, arg(args, 1), "Index", false)
	if err != nil {
		return vm.Undefined, err
	}
	if tag != e.Tag {
		return vm.Undefined, t.typeError("First argument does not match the exception tag")
	}
	params := tag.Sig.Params
	if i >= uint64(len(params)) {
		return vm.Undefined, t.rangeError("Index out of range")
	}
	pt := params[i]
	switch {
	case pt.Kind == module.KindV128:
		return vm.Undefined, t.typeError("Invalid type v128")
	case pt.IsRef():
		return a.refResult(t, e.Vals[i].R, pt)
	}
	return a.toJS(e.Vals[i], pt)
}

// exceptionIs is WebAssembly.Exception.prototype.is(tag).
func (a *api) exceptionIs(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Exception.is()")
	e := a.exceptionOf(this)
	if e == nil {
		return vm.Undefined, t.typeError("Receiver is not a WebAssembly.Exception")
	}
	tag := tagOf(arg(args, 0))
	if tag == nil {
		return vm.Undefined, t.typeError("Argument 0 must be a WebAssembly.Tag")
	}
	return vm.Bool(tag == e.Tag), nil
}

// exceptionStack is the getter of WebAssembly.Exception.prototype.stack; an
// exception made with traceStack has a stack of its own, which hides it.
func (a *api) exceptionStack(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	if a.exceptionOf(this) == nil {
		return vm.Undefined, a.thrower("WebAssembly.Exception.stack()").typeError("Receiver is not a WebAssembly.Exception")
	}
	return vm.Undefined, nil
}
