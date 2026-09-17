package vm

import (
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Materializing a compiled template.
//
// A bytecode.Function refers to names and constants by index into tables of
// plain strings and numbers, because the compiler must not depend on the value
// representation. The first time a template is used those tables are converted
// once into atoms and values, and the result is cached on the closure, so that
// a property access in a loop costs an array index rather than a hash lookup.

// prepare converts a compiled template into a closure ready to run.
func (r *Runtime) prepare(fn *bytecode.Function) *closure {
	cl := &closure{fn: fn, realm: r}

	cl.names = make([]Atom, len(fn.Names))
	for i, n := range fn.Names {
		cl.names[i] = r.atoms.intern(n)
	}

	cl.consts = make([]Value, len(fn.Constants))
	for i, c := range fn.Constants {
		cl.consts[i] = r.materialize(c)
	}
	return cl
}

// materialize converts one constant-pool entry into a value.
func (r *Runtime) materialize(c bytecode.Constant) Value {
	switch c.Kind {
	case bytecode.ConstNumber:
		return Float(c.Num)
	case bytecode.ConstString:
		return Str(NewString(c.Str))
	case bytecode.ConstBigInt:
		if b, ok := ParseBigInt(c.Str); ok {
			return Big(b)
		}
		return Big(NewBigInt(0))
	case bytecode.ConstFunction:
		// A nested template is prepared eagerly and stored as a closure with no
		// upvalues bound; OpClosure copies it and binds them per instantiation.
		tmpl := r.prepare(c.Fn)
		return Value{num: mkTag(KindObject, 0), ref: tmpl}
	case bytecode.ConstRegExp:
		// A regexp literal is built fresh each time it is evaluated, so the
		// pool entry stays unmaterialized and OpNewRegExp reads it directly.
		return Undefined
	}
	return Undefined
}

// Run executes a compiled top-level program and returns its completion value.
func (r *Runtime) Run(fn *bytecode.Function) (Value, error) {
	cl := r.prepare(fn)
	// `this` at the top level of a script is the global object regardless of
	// strictness. Only a strict function invoked without a receiver, and module
	// code, see undefined.
	this := Obj(r.global)
	if fn.IsModule {
		this = Undefined
	}
	v, err := r.run(cl, this, nil, Undefined, nil)
	r.endTurn()
	return v, err
}

// Call invokes a callable value from Go.
func (r *Runtime) Call(fn Value, this Value, args []Value) (Value, error) {
	return r.call(fn, this, args)
}

// Construct invokes a constructor from Go.
func (r *Runtime) Construct(fn Value, args []Value) (Value, error) {
	return r.construct(fn, args)
}

// NewFunction wraps a Go function as a callable JavaScript value.
func (r *Runtime) NewFunction(name string, length int, fn NativeFunc) Value {
	return Obj(r.newNativeFunc(name, length, fn))
}

// NewObject returns an empty object with the standard prototype.
func (r *Runtime) NewObject() *Object {
	return newObject(r.proto.object, ClassObject)
}

// NewArray returns an array holding the given values.
func (r *Runtime) NewArray(vals []Value) *Object { return r.newArrayFrom(vals) }

// GetProp reads a property from a value.
func (r *Runtime) GetProp(v Value, key Atom) (Value, error) { return r.getValueProp(v, key) }

// SetProp writes a property on a value.
func (r *Runtime) SetProp(v Value, key Atom, val Value) error {
	return r.setValueProp(v, key, val, true)
}

// DefineProp installs an own data property with the default attributes.
func (r *Runtime) DefineProp(o *Object, key Atom, val Value) error {
	return r.defineOwnProp(o, key, val, propDefault)
}

// OwnKeys returns an object's own enumerable string keys.
func (r *Runtime) OwnKeys(o *Object) []Atom {
	r.materializeFunctionProto(o)
	keys := o.ownKeys(false, r.atoms)
	out := keys[:0]
	for _, k := range keys {
		// A key whose enumerability cannot be decided without running code
		// that fails is left out: this is the Go-facing view of an object, and
		// it reports what it can rather than an error.
		if yes, err := r.isEnumerable(o, k); err == nil && yes {
			out = append(out, k)
		}
	}
	return out
}

// ToString converts a value to a string, which may call user code.
func (r *Runtime) ToString(v Value) (*String, error) { return r.toString(v) }

// ToNumber converts a value to a number, which may call user code.
func (r *Runtime) ToNumber(v Value) (float64, error) { return r.toNumber(v) }

// Iterate drives the iteration protocol over a value.
func (r *Runtime) Iterate(v Value, fn func(Value) error) error { return r.iterate(v, fn) }

// IsCallable reports whether a value can be called.
func IsCallable(v Value) bool { return isCallable(v) }

// IsArray reports whether a value is an array.
func IsArray(v Value) bool { return v.IsObject() && v.Object().IsArray() }

// ArrayElements exposes an array's dense storage for the marshalling layer.
func ArrayElements(o *Object) []Value { return o.elems }

// IsHole reports whether an array element is absent.
func IsHole(v Value) bool { return isHole(v) }

// ThrowTypeError builds a TypeError for a native function to return.
func (r *Runtime) ThrowTypeError(format string, args ...any) error {
	return r.throwTypeError(format, args...)
}

// ThrowRangeError builds a RangeError for a native function to return.
func (r *Runtime) ThrowRangeError(format string, args ...any) error {
	return r.throwRangeError(format, args...)
}

// NewError builds an error object of the standard kind named by name.
func (r *Runtime) NewError(name, msg string) Value {
	for k := errorKind(0); k < errorKindCount; k++ {
		if errorKindNames[k] == name {
			return Obj(r.newError(k, msg))
		}
	}
	return Obj(r.newError(errError, msg))
}

// ThrowError converts a Go error into a JavaScript exception.
//
// A *Thrown passes through unchanged, so a JavaScript value that was thrown,
// caught by Go code and returned again keeps its identity rather than being
// reduced to its message.
func (r *Runtime) ThrowError(err error) error {
	if t, ok := err.(*Thrown); ok {
		return t
	}
	return r.throw(Obj(r.newError(errError, err.Error())))
}

// ThrowValue raises a value that is already a JavaScript value, which is what
// a host function passing on an error it caught from script needs.
func (r *Runtime) ThrowValue(v Value) error { return r.throw(v) }

// ThrowSyntaxError raises a SyntaxError, which is what a host function that
// compiled source on the script's behalf reports.
func (r *Runtime) ThrowSyntaxError(format string, args ...any) error {
	return r.throwSyntaxError(format, args...)
}

// LooseEquals applies the == operator, which may call user code.
func (r *Runtime) LooseEquals(a, b Value) (Value, error) {
	eq, err := r.looseEquals(a, b)
	if err != nil {
		return Undefined, err
	}
	return Bool(eq), nil
}

// Constructing reports whether the native function currently running was
// invoked with `new`.
//
// The wrapper constructors need it: Boolean(x) produces a primitive while
// new Boolean(x) produces an object, and nothing else distinguishes the two
// calls from inside the implementation.
func (r *Runtime) Constructing() bool {
	f := r.topFrame()
	if f == nil {
		return false
	}
	return !f.newTarget.IsUndefined()
}

// newTarget returns the new.target of the running native call, which a
// constructor needs when its behaviour depends on whether it was subclassed.
func (r *Runtime) newTarget() Value {
	f := r.topFrame()
	if f == nil {
		return Undefined
	}
	return f.newTarget
}

// SetEvaluator installs eval and the Function constructor.
func (r *Runtime) SetEvaluator(fn Evaluator) {
	r.evaluator = fn
	r.installEval()
}

// installEval defines the global eval and the Function constructor.
func (r *Runtime) installEval() {
	r.evalFn = r.defMethod(r.global, "eval", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		src := arg(args, 0)
		// eval returns anything that is not a string unchanged, which is what
		// makes eval(42) safe.
		if !src.IsString() {
			return src, nil
		}
		if rt.evaluator == nil {
			return Undefined, rt.throwTypeError("code generation from strings is disabled")
		}
		// Reaching this function through anything but a plain call is an
		// indirect eval, which runs in global scope. A direct call never gets
		// here: the interpreter recognizes it and evaluates in the caller's
		// scope instead.
		return rt.evalIndirect(src.String().Go())
	})

	fnProto := r.proto.function
	ctorVal, err := r.getProp(r.global, r.atoms.intern("Function"), Obj(r.global))
	if err != nil || !ctorVal.IsObject() {
		return
	}
	_ = fnProto
	ctor := ctorVal.Object()
	ctor.data = &funcData{
		name:     "Function",
		length:   1,
		ctorKind: ctorBase,
		native: func(rt *Runtime, this Value, args []Value) (Value, error) {
			if rt.evaluator == nil {
				return Undefined, rt.throwTypeError("code generation from strings is disabled")
			}
			// The last argument is the body; the rest are parameter lists,
			// which are joined with commas exactly as written. The parameters
			// are converted first, in order, and the body last -- which is
			// what a toString with a side effect sees.
			var params []string
			for _, a := range args[:max(0, len(args)-1)] {
				s, err := rt.toString(a)
				if err != nil {
					return Undefined, err
				}
				params = append(params, s.Go())
			}
			body := ""
			if len(args) > 0 {
				s, err := rt.toString(args[len(args)-1])
				if err != nil {
					return Undefined, err
				}
				body = s.Go()
			}
			src := "(function anonymous(" + joinComma(params) + "\n) {\n" + body + "\n})"
			return rt.evalIndirect(src)
		},
	}

	// The three function kinds that are not ordinary each have a constructor of
	// their own. They are not global: the only way to name one is
	// Object.getPrototypeOf(function* () {}).constructor, which is precisely
	// what makes them worth defining -- code that reaches for one is
	// introspecting, and finding Function there is wrong.
	r.defDerivedFunctionCtor("GeneratorFunction", r.genFuncProto, ctor,
		func(params, body string) string {
			return "(function* anonymous(" + params + "\n) {\n" + body + "\n})"
		})
	r.defDerivedFunctionCtor("AsyncFunction", r.asyncFuncProto, ctor,
		func(params, body string) string {
			return "(async function anonymous(" + params + "\n) {\n" + body + "\n})"
		})
	r.defDerivedFunctionCtor("AsyncGeneratorFunction", r.asyncGenFuncProto, ctor,
		func(params, body string) string {
			return "(async function* anonymous(" + params + "\n) {\n" + body + "\n})"
		})
}

// defDerivedFunctionCtor builds one of the intrinsic constructors that sit
// alongside Function.
func (r *Runtime) defDerivedFunctionCtor(name string, proto *Object, base *Object,
	source func(params, body string) string) {
	if proto == nil {
		return
	}
	c := newObject(base, ClassFunction)
	c.data = &funcData{
		name: name, length: 1, ctorKind: ctorBase,
		native: func(rt *Runtime, this Value, args []Value) (Value, error) {
			if rt.evaluator == nil {
				return Undefined, rt.throwTypeError("code generation from strings is disabled")
			}
			// The parameters are converted first and the body last, as in
			// the Function constructor these stand in for.
			var params []string
			for _, a := range args[:max(0, len(args)-1)] {
				s, err := rt.toString(a)
				if err != nil {
					return Undefined, err
				}
				params = append(params, s.Go())
			}
			body := ""
			if len(args) > 0 {
				s, err := rt.toString(args[len(args)-1])
				if err != nil {
					return Undefined, err
				}
				body = s.Go()
			}
			return rt.evalIndirect(source(joinComma(params), body))
		},
	}
	c.setOwnRaw(atomPrototype, Obj(proto), 0)
	proto.setOwnRaw(atomConstructor, Obj(c), propConfigurable)
}

// wrapEvalError turns a compile failure into a thrown SyntaxError.
//
// The Go error says which engine it came from and what kind of error it is,
// both of which the thrown object says for itself -- so the message it carries
// is what is left after those are taken off.
func (r *Runtime) wrapEvalError(err error) error {
	if _, ok := err.(*Thrown); ok {
		return err
	}
	msg := strings.TrimPrefix(err.Error(), "quickjs: ")
	msg = strings.TrimPrefix(msg, "SyntaxError: ")
	return r.throwError(errSyntax, "%s", msg)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
