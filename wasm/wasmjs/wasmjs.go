//go:build quickjs_wasm

package wasmjs

import (
	"errors"
	"fmt"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
)

// Install defines the global WebAssembly on rt. The namespace is made the
// first time a script reads the global, so a runtime that never uses it pays
// nothing for it.
func Install(rt *quickjs.Runtime, opts ...Option) error {
	if rt == nil {
		return errors.New("wasmjs: no runtime")
	}
	r := hostaccess.VM(rt)
	if r == nil {
		return errors.New("wasmjs: the runtime is closed")
	}
	var c config
	for _, o := range opts {
		o(&c)
	}
	// What the namespace builds on is taken now, before a script can
	// replace it.
	proxy, err := r.GetProp(vm.Obj(r.Global()), r.Intern("Proxy"))
	if err != nil {
		return err
	}
	objectCtor, err := r.GetProp(vm.Obj(r.Global()), r.Intern("Object"))
	if err != nil {
		return err
	}
	objectProto, err := r.GetProp(objectCtor, r.Intern("prototype"))
	if err != nil || !objectProto.IsObject() {
		return errors.New("wasmjs: no Object.prototype")
	}
	var built *vm.Object
	vm.HostDefineLazyGlobal(r, "WebAssembly", func(r *vm.Runtime) *vm.Object {
		if built == nil {
			built = newAPI(r, rt, c, proxy, objectProto.Object()).ns
		}
		return built
	})
	return nil
}

// api is a runtime's WebAssembly namespace: its classes and the state that
// ties wasm's objects to the JavaScript objects that stand for them.
type api struct {
	rt     *vm.Runtime
	quirks bool
	limits *decoder.Limits

	ns *vm.Object

	moduleProto, instanceProto, memoryProto, tableProto    *vm.Object
	globalProto, tagProto, exceptionProto, suspendingProto *vm.Object
	compileErrorProto, linkErrorProto, runtimeErrorProto   *vm.Object
	suspendErrorProto                                      *vm.Object

	// jsTag is WebAssembly.JSTag's tag, which a JavaScript exception
	// crossing wasm is thrown with.
	jsTag    *engine.Tag
	jsTagObj *vm.Object

	// proxy is the Proxy constructor, which wasm's GC objects are made
	// opaque with, and opaque the handler they are made with.
	proxy vm.Value
	// objectProto is Object.prototype, as Install found it.
	objectProto *vm.Object
	opaque      *vm.Object

	// wrappers maps wasm's objects -- functions, memories, tables, globals,
	// tags, exceptions, structs and arrays -- to the JavaScript objects that
	// stand for them, so that one stands for each.
	wrappers wrapperCache

	// jspi is the state of the promising calls that are running, nil
	// without the JS Promise Integration.
	jspi *jspiState

	// qrt is the public runtime, which internal scripts are evaluated in;
	// streamBytes and streamThen are the functions streamingJS makes.
	qrt                     any
	streamBytes, streamThen vm.Value

	// budget charges what wasm holds to the runtime's memory limit, nil for
	// a runtime without one (see memlimit.go).
	budget *engine.Budget
	// jitBudget bounds the native code of the runtime's modules, made at
	// the first compile that has any (see memlimit.go).
	jitBudget *engine.JITBudget
}

// newAPI builds the WebAssembly namespace of r.
func newAPI(r *vm.Runtime, qrt any, c config, proxy vm.Value, objectProto *vm.Object) *api {
	a := &api{rt: r, qrt: qrt, quirks: vm.HostNodeQuirks(r), proxy: proxy, objectProto: objectProto,
		streamBytes: vm.Undefined, streamThen: vm.Undefined}
	if a.quirks {
		a.limits = decoder.V8Limits()
	} else {
		a.limits = decoder.JSAPILimits()
	}
	if !c.noJSPI {
		a.jspi = &jspiState{}
	}
	a.wrappers.m = map[uintptr]wrapperEntry{}
	if vm.HostMemoryLimited(r) {
		a.budget = engine.NewOuterBudget(jsMeter{r})
	}
	a.build()
	return a
}

// build makes the namespace and its classes, in the order their properties
// are listed.
func (a *api) build() {
	r := a.rt
	// The namespace object is an ordinary object, not a function.
	ns := r.NewPlainObject()
	a.ns = ns
	a.toStringTag(ns, "WebAssembly")
	a.method(ns, "compile", 1, a.compileFn)
	a.method(ns, "validate", 1, a.validate)
	a.method(ns, "instantiate", 1, a.instantiate)

	a.moduleProto = a.class(ns, "Module", "WebAssembly.Module", a.newModule)
	a.moduleStatics(ns)
	a.instanceProto = a.class(ns, "Instance", "WebAssembly.Instance", a.newInstance)
	a.getter(a.instanceProto, "exports", a.instanceExports)
	a.tableProto = a.class(ns, "Table", "WebAssembly.Table", a.newTable)
	a.getter(a.tableProto, "length", a.tableLength)
	a.method(a.tableProto, "grow", 1, a.tableGrow)
	a.method(a.tableProto, "set", 1, a.tableSet)
	a.method(a.tableProto, "get", 1, a.tableGet)
	a.memoryProto = a.class(ns, "Memory", "WebAssembly.Memory", a.newMemory)
	a.method(a.memoryProto, "grow", 1, a.memoryGrow)
	a.getter(a.memoryProto, "buffer", a.memoryBuffer)
	a.globalProto = a.class(ns, "Global", "WebAssembly.Global", a.newGlobal)
	a.method(a.globalProto, "valueOf", 0, a.globalValueOf)
	a.accessor(a.globalProto, "value", a.globalValue, a.setGlobalValue)
	a.tagProto = a.class(ns, "Tag", "WebAssembly.Tag", a.newTag)
	a.makeJSTag()
	a.exceptionProto = a.classOf(ns, "Exception", "WebAssembly.Exception", 2, a.newException)
	a.method(a.exceptionProto, "getArg", 2, a.exceptionGetArg)
	a.method(a.exceptionProto, "is", 1, a.exceptionIs)
	a.getter(a.exceptionProto, "stack", a.exceptionStack)
	a.compileErrorProto = a.errorClass(ns, "CompileError")
	a.linkErrorProto = a.errorClass(ns, "LinkError")
	a.runtimeErrorProto = a.errorClass(ns, "RuntimeError")
	a.method(ns, "compileStreaming", 1, a.compileStreaming)
	a.method(ns, "instantiateStreaming", 1, a.instantiateStreaming)
	if a.jspi != nil {
		a.suspendingProto = a.class(ns, "Suspending", "WebAssembly.Suspending", a.newSuspending)
		a.method(ns, "promising", 1, a.promising)
		a.suspendErrorProto = a.errorClass(ns, "SuspendError")
	}
	a.method(a.memoryProto, "toFixedLengthBuffer", 0, a.memoryToFixedLengthBuffer)
	a.method(a.memoryProto, "toResizableBuffer", 0, a.memoryToResizableBuffer)
	a.makeOpaqueHandler()
}

// method defines an operation of an interface: a function that is
// writable, enumerable and configurable.
func (a *api) method(o *vm.Object, name string, length int, fn vm.NativeFunc) *vm.Object {
	f := a.rt.NewFunc(name, length, fn)
	vm.HostDefineOwn(o, a.rt.Intern(name), vm.Obj(f), true, true, true)
	return f
}

// getter defines a read-only attribute, enumerable and configurable.
func (a *api) getter(o *vm.Object, name string, fn vm.NativeFunc) {
	g := a.rt.NewFunc("get "+name, 0, fn)
	vm.HostDefineAccessor(a.rt, o, a.rt.Intern(name), g, nil, true, true)
}

// accessor defines a writable attribute.
func (a *api) accessor(o *vm.Object, name string, get, set vm.NativeFunc) {
	g := a.rt.NewFunc("get "+name, 0, get)
	s := a.rt.NewFunc("set "+name, 1, set)
	vm.HostDefineAccessor(a.rt, o, a.rt.Intern(name), g, s, true, true)
}

// toStringTag gives o the Symbol.toStringTag tag.
func (a *api) toStringTag(o *vm.Object, tag string) {
	vm.HostDefineOwn(o, vm.HostToStringTag(a.rt), vm.Str(vm.NewString(tag)), false, false, true)
}

// class defines the constructor name on ns, of length 1, whose prototype is
// tagged tag, and returns the prototype.
func (a *api) class(ns *vm.Object, name, tag string, fn vm.NativeFunc) *vm.Object {
	return a.classOf(ns, name, tag, 1, fn)
}

// classOf is class with a constructor of length length.
func (a *api) classOf(ns *vm.Object, name, tag string, length int, fn vm.NativeFunc) *vm.Object {
	proto := a.rt.NewPlainObject()
	a.toStringTag(proto, tag)
	ctor := vm.HostConstructor(a.rt, name, length, proto, fn)
	vm.HostDefineOwn(ns, a.rt.Intern(name), vm.Obj(ctor), true, false, true)
	return proto
}

// errorClass defines one of the namespace's NativeError-like constructors
// and returns its prototype.
func (a *api) errorClass(ns *vm.Object, name string) *vm.Object {
	ctor, proto := vm.HostErrorClass(a.rt, name)
	vm.HostDefineOwn(ns, a.rt.Intern(name), vm.Obj(ctor), true, false, true)
	return proto
}

// arg returns args[i], or undefined.
func arg(args []vm.Value, i int) vm.Value {
	if i < len(args) {
		return args[i]
	}
	return vm.Undefined
}

// thrower makes the errors an operation of the interface throws, their
// messages prefixed with the operation's name: "WebAssembly.Module(): ".
type thrower struct {
	a    *api
	name string
}

func (a *api) thrower(name string) thrower { return thrower{a, name} }

func (t thrower) msg(format string, args ...any) string {
	return t.name + ": " + fmt.Sprintf(format, args...)
}

func (t thrower) typeError(format string, args ...any) error {
	return t.a.rt.ThrowTypeError("%s", t.msg(format, args...))
}

func (t thrower) rangeError(format string, args ...any) error {
	return t.a.rt.ThrowRangeError("%s", t.msg(format, args...))
}

func (t thrower) compileError(format string, args ...any) error {
	return t.a.throwError(t.a.compileErrorProto, t.msg(format, args...))
}

func (t thrower) linkError(format string, args ...any) error {
	return t.a.throwError(t.a.linkErrorProto, t.msg(format, args...))
}

func (t thrower) runtimeError(format string, args ...any) error {
	return t.a.throwError(t.a.runtimeErrorProto, t.msg(format, args...))
}

// newError makes an error of the class of proto.
func (a *api) newError(proto *vm.Object, msg string) vm.Value {
	return vm.Obj(vm.HostNewError(a.rt, proto, msg))
}

// throwError throws an error of the class of proto.
func (a *api) throwError(proto *vm.Object, msg string) error {
	return a.rt.ThrowValue(a.newError(proto, msg))
}

// thrownValue returns the value err throws, and whether it throws one: a
// failure that is not a JavaScript exception -- the host stopping the
// script -- has none.
func thrownValue(err error) (vm.Value, bool) {
	var t *vm.Thrown
	if errors.As(err, &t) {
		return t.Value, true
	}
	return vm.Undefined, false
}

// newTargetProto returns the prototype an object a constructor makes is
// given: new.target's prototype property, or Object.prototype where that is
// not an object.
func (a *api) newTargetProto(rt *vm.Runtime) (*vm.Object, error) {
	return vm.HostProtoFromNewTarget(rt, a.objectProto)
}
