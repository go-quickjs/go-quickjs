package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// A function's caller and arguments.
//
// Every engine still lets a sloppy function ask which function called it and
// with what, which the web's older libraries depend on: f.caller and
// f.arguments. V8, SpiderMonkey and JavaScriptCore agree on the answers, and
// keep them in accessors on Function.prototype. The standard has no such
// thing: Function.prototype's caller and arguments are its %ThrowTypeError%,
// which test262 checks, so standards mode keeps that, and only a runtime
// WithNodeQuirks answers, as V8 does.

// errLegacyReflection is V8's message for a function that has neither.
const errLegacyReflection = "'caller', 'callee', and 'arguments' properties may not be accessed on strict mode functions or the arguments objects for calls to them"

// isLegacyFunc reports whether a function answers caller and arguments: one a
// function declaration or expression, or the Function constructor, made in
// sloppy code. Nothing else does, as the standard's forbidden extensions
// require: not strict code, an arrow, a method, an accessor, a class, a
// generator, an async function, a bound function or a built-in.
func isLegacyFunc(fd *funcData) bool {
	if fd == nil || fd.closure == nil || fd.boundTarget != nil || fd.arrow {
		return false
	}
	fn := fd.closure.fn
	return fn.Kind == bytecode.KindNormal && !fn.Strict && !fn.Generator && !fn.Async
}

// legacyFunc is the function a caller or arguments accessor was called on, or
// the TypeError V8 throws for anything else.
func (r *Runtime) legacyFunc(this Value) (*Object, error) {
	if this.IsObject() {
		if o := this.Object(); o.class == ClassFunction && isLegacyFunc(o.fn()) {
			return o, nil
		}
	}
	return nil, r.throwTypeError(errLegacyReflection)
}

// initLegacyReflection defines Function.prototype's caller and arguments:
// the standard's %ThrowTypeError%, or V8's accessors under WithNodeQuirks.
func (r *Runtime) initLegacyReflection() {
	p := r.proto.function
	if !r.nodeQuirks {
		// The two properties that once let a function walk the call stack
		// are still there, and reading or writing either throws: the same
		// function an unmapped arguments object's callee uses, which is what
		// makes them indistinguishable from one another.
		r.defineAccessor(p, atomCaller, r.throwTypeErrorFn, r.throwTypeErrorFn, propConfigurable)
		r.defineAccessor(p, atomArguments, r.throwTypeErrorFn, r.throwTypeErrorFn, propConfigurable)
		return
	}
	for _, prop := range []struct {
		key    Atom
		name   string
		answer func(*Runtime, *Object) Value
	}{
		{atomCaller, "caller", (*Runtime).legacyCaller},
		{atomArguments, "arguments", (*Runtime).legacyArguments},
	} {
		answer := prop.answer
		get := r.newNativeFunc("get "+prop.name, 0, func(rt *Runtime, this Value, _ []Value) (Value, error) {
			o, err := rt.legacyFunc(this)
			if err != nil {
				return Undefined, err
			}
			return answer(rt, o), nil
		})
		// Setting either does nothing, but on a function that has neither,
		// where it throws.
		set := r.newNativeFunc("set "+prop.name, 1, func(rt *Runtime, this Value, _ []Value) (Value, error) {
			_, err := rt.legacyFunc(this)
			return Undefined, err
		})
		r.defineAccessor(p, prop.key, get, set, propConfigurable)
	}
}

// activationOf is the depth of a function's innermost activation on the
// stack -- its own frame, not that of an eval inside it -- or -1 when it is
// not running.
func (r *Runtime) activationOf(o *Object) int {
	fd := o.fn()
	if fd == nil || fd.closure == nil {
		return -1
	}
	for i := r.frameDepth - 1; i >= 0; i-- {
		if f := r.frameAt(i); f.callee == o && f.cl != nil && f.cl.fn == fd.closure.fn && !f.cl.fn.TopLevel {
			return i
		}
	}
	return -1
}

// legacyCaller is what f.caller answers, as V8's FindCaller has it: the
// function whose code called f's innermost activation, or null.
//
// Script, module and eval code in between is passed over, and so are the
// built-ins that only pass a call on -- Function.prototype.call and apply,
// Reflect.apply and construct, and eval -- which V8 runs without a frame. A caller that
// is strict code is null, as the standard requires, and so is one that is a
// built-in that calls back, such as Array.prototype.map, one across a
// ShadowRealm's boundary, and a job or a host with no function at all.
func (r *Runtime) legacyCaller(o *Object) Value {
	i := r.activationOf(o)
	if i < 0 {
		return Null
	}
	here := frameRealm(r.frameAt(i))
	for j := i - 1; j >= 0; j-- {
		f := r.frameAt(j)
		if re := frameRealm(f); re != nil && here != nil && re != here && (re.shadow || here.shadow) {
			return Null
		}
		if f.cl == nil {
			// eval called indirectly passes the call on too, to its code.
			if f.native != "" && (r.isFrameless(f.callee) || isEvalFn(f.callee)) {
				continue
			}
			return Null
		}
		if f.callee == nil || f.cl.fn.TopLevel {
			// A script's, a module's or an eval's code.
			continue
		}
		fd := f.callee.fn()
		if fd == nil || fd.closure == nil || fd.closure.fn != f.cl.fn {
			continue
		}
		if f.cl.fn.Strict {
			return Null
		}
		return Obj(f.callee)
	}
	return Null
}

// legacyArguments is what f.arguments answers: a new arguments object for its
// innermost activation, or null. It is a copy, which nothing written to it
// or to the parameters afterwards reaches, with the arguments as the call
// passed them -- but for a parameter kept in the frame alone, which V8 reads
// as it is now: one of a plain parameter list, in a function that uses
// neither arguments nor a direct eval, and that no closure captures.
func (r *Runtime) legacyArguments(o *Object) Value {
	i := r.activationOf(o)
	if i < 0 {
		return Null
	}
	f := r.frameAt(i)
	fn := f.cl.fn
	args := append([]Value(nil), f.args...)
	if fn.HasSimpleParams && !fn.UsesArguments && !fn.HasDirectEval {
		for k := 0; k < fn.ParamCount && k < len(args) && k < len(f.locals); k++ {
			if k < len(fn.Locals) && fn.Locals[k].Captured {
				continue
			}
			args[k] = f.locals[k]
		}
	}
	a := newObject(r.proto.object, ClassArguments)
	a.elems = args
	a.setOwnRaw(atomLength, Int(len(args)), propWritable|propConfigurable)
	a.setOwnRaw(atomCallee, Obj(o), propWritable|propConfigurable)
	a.setOwnRaw(r.atoms.internSymbol(r.wellKnown.iterator), Obj(r.arrayValuesFn), propWritable|propConfigurable)
	return Obj(a)
}

// isEvalFn reports whether a function is some realm's %eval%.
func isEvalFn(o *Object) bool {
	if o == nil {
		return false
	}
	fd := o.fn()
	return fd != nil && fd.realm != nil && fd.realm.evalFn == o
}
