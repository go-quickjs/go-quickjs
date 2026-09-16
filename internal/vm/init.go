package vm

import (
	"context"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Config configures a new Runtime.
type Config struct {
	// MemoryLimit caps the bytes the runtime may account for, or 0 for no
	// limit.
	MemoryLimit int64
	// StackSize is the number of value slots shared by all frames. Zero selects
	// the default.
	StackSize int
	// MaxCallDepth bounds recursion. Zero selects the default.
	MaxCallDepth int
}

// New creates a Runtime with the standard globals installed.
func New(cfg Config) *Runtime {
	stackSize := cfg.StackSize
	if stackSize <= 0 {
		stackSize = defaultStackSize
	}
	maxFrames := cfg.MaxCallDepth
	if maxFrames <= 0 {
		maxFrames = defaultCallDepthLimit
	}

	r := &Runtime{
		atoms: newAtomTable(),
		stack: make([]Value, stackSize),
		// The frame stack is allocated at its full size and never grown, for
		// the same reason the value stack is: the interpreter holds a *frame
		// across nested calls, and a reallocation would leave those pointers
		// aimed at the old array. Exhausting it is the call depth limit.
		frames:           make([]frame, 0, maxFrames),
		maxFrames:        maxFrames,
		memoryLimit:      cfg.MemoryLimit,
		interruptCounter: interruptCheckInterval,
		symbolRegistry:   make(map[string]*Symbol),
		templateCache:    make(map[*bytecode.Function][]*Object),
	}

	r.initWellKnownSymbols()
	r.initIntrinsics()
	r.initGlobals()
	return r
}

// SetClock installs the source of the current time, which Date and Date.now
// read. A nil clock means the process clock.
func (r *Runtime) SetClock(fn func() time.Time) { r.clock = fn }

// SetTimeZone installs the zone local time is expressed in. A nil zone means
// the process zone.
func (r *Runtime) SetTimeZone(loc *time.Location) { r.timeZone = loc }

// SetContext installs the context the interpreter checks for cancellation.
func (r *Runtime) SetContext(ctx context.Context) { r.ctx = ctx }

// Global returns the global object.
func (r *Runtime) Global() *Object { return r.global }

// Atoms exposes the intern table so that the public API can build keys.
func (r *Runtime) Intern(name string) Atom { return r.atoms.intern(name) }

// AtomName returns the string form of an atom.
func (r *Runtime) AtomName(a Atom) string { return r.atoms.name(a) }

func (r *Runtime) initWellKnownSymbols() {
	mk := func(name string) *Symbol {
		return &Symbol{Description: "Symbol." + name, HasDescription: true}
	}
	r.wellKnown = wellKnownSymbols{
		iterator:           mk("iterator"),
		asyncIterator:      mk("asyncIterator"),
		hasInstance:        mk("hasInstance"),
		toPrimitive:        mk("toPrimitive"),
		toStringTag:        mk("toStringTag"),
		species:            mk("species"),
		isConcatSpreadable: mk("isConcatSpreadable"),
		unscopables:        mk("unscopables"),
		match:              mk("match"),
		matchAll:           mk("matchAll"),
		replace:            mk("replace"),
		search:             mk("search"),
		split:              mk("split"),
	}
}

// initIntrinsics creates the prototype objects.
//
// The order matters: Object.prototype has no prototype of its own and must
// exist first, and Function.prototype must exist before any native function can
// be created.
func (r *Runtime) initIntrinsics() {
	r.proto.object = newObject(nil, ClassObject)

	// Function.prototype is itself callable and returns undefined.
	r.proto.function = newObject(r.proto.object, ClassFunction)
	r.proto.function.data = &funcData{
		name: "",
		native: func(*Runtime, Value, []Value) (Value, error) {
			return Undefined, nil
		},
	}

	r.proto.array = newObject(r.proto.object, ClassArray)
	r.proto.str = newObject(r.proto.object, ClassStringWrapper)
	r.proto.str.data = emptyString
	r.proto.number = newObject(r.proto.object, ClassNumberWrapper)
	r.proto.number.data = float64(0)
	r.proto.boolean = newObject(r.proto.object, ClassBooleanWrapper)
	r.proto.boolean.data = false
	r.proto.symbol = newObject(r.proto.object, ClassObject)
	r.proto.bigint = newObject(r.proto.object, ClassObject)
	r.proto.iterator = newObject(r.proto.object, ClassObject)
	r.proto.arrayIter = newObject(r.proto.iterator, ClassObject)
	r.proto.stringIter = newObject(r.proto.iterator, ClassObject)

	// Error.prototype and the native error prototypes chained from it.
	r.proto.err = newObject(r.proto.object, ClassError)
	for k := errorKind(0); k < errorKindCount; k++ {
		if k == errError {
			r.proto.nativeErrors[k] = r.proto.err
			continue
		}
		r.proto.nativeErrors[k] = newObject(r.proto.err, ClassError)
	}
}

// ---------------------------------------------------------------------------
// Helpers for defining built-ins
// ---------------------------------------------------------------------------

// newNativeFunc creates a callable object wrapping a Go function.
func (r *Runtime) newNativeFunc(name string, length int, fn NativeFunc) *Object {
	o := newObject(r.proto.function, ClassFunction)
	o.data = &funcData{native: fn, name: name, length: length, ctorKind: ctorNone}
	return o
}

// defMethod defines a built-in method, which is writable and configurable but
// not enumerable, as every specification-defined method is.
func (r *Runtime) defMethod(target *Object, name string, length int, fn NativeFunc) *Object {
	f := r.newNativeFunc(name, length, fn)
	target.setOwnRaw(r.atoms.intern(name), Obj(f), propWritable|propConfigurable)
	return f
}

// defSymbolMethod defines a method keyed by a well-known symbol.
func (r *Runtime) defSymbolMethod(target *Object, sym *Symbol, name string, length int, fn NativeFunc) {
	f := r.newNativeFunc(name, length, fn)
	target.setOwnRaw(r.atoms.internSymbol(sym), Obj(f), propWritable|propConfigurable)
}

// defValue defines a non-enumerable data property.
func (r *Runtime) defValue(target *Object, name string, v Value) {
	target.setOwnRaw(r.atoms.intern(name), v, propWritable|propConfigurable)
}

// defConst defines a read-only, non-enumerable data property.
func (r *Runtime) defConst(target *Object, name string, v Value) {
	target.setOwnRaw(r.atoms.intern(name), v, 0)
}

// defToStringTag sets Symbol.toStringTag, which is what
// Object.prototype.toString reports for the object.
//
// The key must be the symbol itself; a property literally named
// "[Symbol.toStringTag]" is an ordinary string key that nothing consults.
func (r *Runtime) defToStringTag(target *Object, name string) {
	target.setOwnRaw(r.atoms.internSymbol(r.wellKnown.toStringTag),
		Str(NewString(name)), propConfigurable)
}

// defGetter defines a non-enumerable accessor with only a getter.
func (r *Runtime) defGetter(target *Object, name string, fn NativeFunc) {
	g := r.newNativeFunc("get "+name, 0, fn)
	r.defineAccessor(target, r.atoms.intern(name), g, nil, propConfigurable)
}

// newCtor creates a constructor function with its prototype link established
// in both directions.
func (r *Runtime) newCtor(name string, length int, proto *Object, fn NativeFunc) *Object {
	c := newObject(r.proto.function, ClassFunction)
	c.data = &funcData{native: fn, name: name, length: length, ctorKind: ctorBase}
	c.setOwnRaw(atomPrototype, Obj(proto), 0)
	proto.setOwnRaw(atomConstructor, Obj(c), propWritable|propConfigurable)
	r.defValue(r.global, name, Obj(c))
	return c
}

// arg returns the i'th argument, or undefined when it was not supplied.
func arg(args []Value, i int) Value {
	if i < len(args) {
		return args[i]
	}
	return Undefined
}

// initGlobals installs the global object and the standard library.
func (r *Runtime) initGlobals() {
	r.global = newObject(r.proto.object, ClassObject)

	// The value properties of the global object.
	r.global.setOwnRaw(atomUndefined, Undefined, 0)
	r.defConst(r.global, "NaN", Float(nan()))
	r.defConst(r.global, "Infinity", Float(inf(1)))
	r.defValue(r.global, "globalThis", Obj(r.global))

	r.initObjectBuiltins()
	r.initFunctionBuiltins()
	r.initArrayBuiltins()
	r.initArrayExtras()
	r.initStringBuiltins()
	r.initNumberBuiltins()
	r.initBooleanBuiltins()
	r.initSymbolBuiltins()
	r.initErrorBuiltins()
	r.initMathBuiltins()
	r.initArrayBufferBuiltins()
	r.initTypedArrayBuiltins()
	r.initDataViewBuiltins()
	r.initWeakRefBuiltins()
	r.initProxyBuiltins()
	r.initReflectBuiltins()
	r.initGeneratorBuiltins()
	r.initAsyncGeneratorBuiltins()
	r.initPromiseBuiltins()
	r.initRegExpBuiltins()
	r.initStringRegExpMethods()
	r.initDateBuiltins()
	r.initMapBuiltins()
	r.initSetBuiltins()
	r.initWeakCollections()
	r.initJSONBuiltins()
	r.initGlobalFunctions()
	// Runs last, because each of its additions hangs off a constructor an
	// earlier step installed.
	r.initIteratorHelpers()
	r.initRecentBuiltins()
	r.initExtraBuiltins()
	r.initDynamicImport()
}
