package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// A function's caller and arguments.
//
// Every engine still lets a sloppy function ask which function called it and
// with what, which the web's older libraries depend on: f.caller and
// f.arguments. V8, SpiderMonkey and JavaScriptCore agree on the answers, and
// keep them in accessors on Function.prototype. The standard has no such
// thing on Function.prototype: its caller and arguments are its
// %ThrowTypeError%, which test262 checks. It does permit a sloppy ordinary
// function an own caller that never answers a strict function, which
// test262's caller tests read, so standards mode gives each such function
// one, answering as V8's accessor does (materializeCaller), and no
// arguments. WithNodeQuirks keeps both on Function.prototype, as V8 does.

// errLegacyReflection is V8's message for a function that has neither.
const errLegacyReflection = "'caller', 'callee', and 'arguments' properties may not be accessed on strict mode functions or the arguments objects for calls to them"

// isLegacyFunc reports whether a function answers caller and arguments: one a
// function declaration or expression, or the Function constructor, made in
// sloppy code. Nothing else does, as the standard's forbidden extensions
// require: not strict code, an arrow, a method, an accessor, a class, a
// generator, an async function, a bound function or a built-in.
func isLegacyFunc(fd *funcData) bool {
	if fd == nil || fd.closure == nil || fd.bound || fd.arrow {
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

// materializeCaller gives a sloppy ordinary function, in standards mode, the
// own caller accessor the standard permits it, as a real property: the
// function has it from its creation, but it is put in the table only when
// something asks about caller or lists the function's keys, which few
// programs do of few functions. callerMade records that it was put there,
// after which it is the table's like any other property, so that deleting or
// redefining it sticks.
//
// It goes after length, name and prototype, wherever those are yet, and
// before anything a script has added: a prototype built later goes before
// it, as length and name, made later, go before both.
func (r *Runtime) materializeCaller(o *Object) {
	fd := o.fn()
	if fd == nil || fd.callerMade || r.nodeQuirks || !isLegacyFunc(fd) {
		return
	}
	fd.callerMade = true
	at := 0
	for at < int(o.valn) {
		if k := o.shape.keys[at].key; k != atomLength && k != atomName && k != atomPrototype {
			break
		}
		at++
	}
	realm := fd.closure.realm
	if realm == nil {
		realm = r.Realm
	}
	get, set := r.legacyCallerAccessors(realm)
	o.insertProp(at, Property{key: atomCaller, value: accessorValue(&accessor{getter: get, setter: set}),
		flags: propAccessor | propConfigurable})
}

// legacyCallerAccessors are a realm's getter and setter of its functions' own
// caller, made when the first of them is put in a table. Every function of
// the realm shares them, as they answer for whatever function they are
// called on; the TypeError of one that has no caller is the realm's.
func (r *Runtime) legacyCallerAccessors(realm *Realm) (get, set *Object) {
	if realm.callerAccessors[0] == nil {
		here := r.Realm
		r.Realm = realm
		realm.callerAccessors[0] = r.newNativeFunc("get caller", 0, func(rt *Runtime, this Value, _ []Value) (Value, error) {
			fn, err := rt.legacyFunc(this)
			if err != nil {
				return Undefined, err
			}
			return rt.legacyCaller(fn), nil
		})
		realm.callerAccessors[1] = r.newNativeFunc("set caller", 1, func(rt *Runtime, this Value, _ []Value) (Value, error) {
			_, err := rt.legacyFunc(this)
			return Undefined, err
		})
		r.Realm = here
	}
	return realm.callerAccessors[0], realm.callerAccessors[1]
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
	if f.args == nil && (f.regKind == regFrameCall || f.regKind == regFrameEnter || f.regKind == regFrameConstruct || f.regKind == regFrameSuper) {
		// A register frame, whose arguments are its window's and its
		// caller's instruction's (regLegacyArgs).
		if a, ok := r.regLegacyArgs(i, f); ok {
			args = a
		}
	} else if fn.HasSimpleParams && !fn.UsesArguments && !fn.HasDirectEval {
		for k := 0; k < fn.ParamCount && k < len(args) && k < len(f.locals); k++ {
			if k < len(fn.Locals) && fn.Locals[k].Captured {
				continue
			}
			args[k] = f.locals[k]
		}
	}
	a := newObject(r.proto.object, ClassArguments)
	a.extOf().elems = args
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
