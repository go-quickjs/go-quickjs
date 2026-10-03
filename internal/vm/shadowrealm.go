package vm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// ShadowRealm.
//
// A ShadowRealm is a realm of its own that code can be evaluated in, and
// nothing but primitives and functions crosses between it and the realm that
// made it: an object that crosses is refused, and a function that crosses is
// wrapped in one of the receiving realm's, which wraps its arguments on the
// way in and its result on the way out. An exception does not cross either;
// it arrives as a TypeError of the realm it reached.

// shadowRealmData is a ShadowRealm's [[ShadowRealm]] slot.
type shadowRealmData struct {
	realm *Realm
}

func (r *Runtime) initShadowRealm() {
	proto := newObject(r.proto.object, ClassObject)
	r.newCtor("ShadowRealm", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !rt.Constructing() {
			return Undefined, rt.throwTypeError("Constructor ShadowRealm requires 'new'")
		}
		p, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		o := newObject(p, ClassObject)
		re := rt.NewRealm()
		re.shadow = true
		o.data = &shadowRealmData{realm: re}
		return Obj(o), nil
	})

	r.defMethod(proto, "evaluate", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		sr, err := rt.shadowRealmOf(this, "evaluate")
		if err != nil {
			return Undefined, err
		}
		src := arg(args, 0)
		if !src.IsString() {
			return Undefined, rt.throwTypeError("ShadowRealm.prototype.evaluate requires a string")
		}
		if rt.evaluator == nil {
			return Undefined, rt.throwTypeError("code generation from strings is disabled")
		}
		// The source is parsed as the evaluating realm's, so a SyntaxError is
		// its own rather than the shadow realm's.
		fn, err := rt.evaluator(src.String().Go(), EvalRequest{})
		if err != nil {
			return Undefined, rt.wrapEvalError(err)
		}
		v, err := rt.RunIn(sr.realm, fn)
		if err != nil {
			return Undefined, rt.crossedError(err)
		}
		return rt.wrappedValue(rt.Realm, v)
	})

	r.defMethod(proto, "importValue", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		sr, err := rt.shadowRealmOf(this, "importValue")
		if err != nil {
			return Undefined, err
		}
		spec, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		exportName := arg(args, 1)
		if !exportName.IsString() {
			return Undefined, rt.throwTypeError("ShadowRealm.prototype.importValue requires an export name that is a string")
		}
		return rt.shadowImportValue(sr.realm, spec.Go(), exportName.String())
	})

	r.defToStringTag(proto, "ShadowRealm")
}

// shadowRealmOf is ValidateShadowRealmObject.
func (r *Runtime) shadowRealmOf(v Value, method string) (*shadowRealmData, error) {
	if v.IsObject() {
		if sr, ok := v.Object().data.(*shadowRealmData); ok {
			return sr, nil
		}
	}
	return nil, r.throwTypeError("ShadowRealm.prototype.%s requires a ShadowRealm", method)
}

// crossedError is what an exception becomes when it reaches the realm that
// crossed into another: a TypeError of its own, which says what it was. A
// host's interruption is not an exception and goes on as it is.
func (r *Runtime) crossedError(err error) error {
	t, ok := err.(*Thrown)
	if !ok {
		return err
	}
	msg := "an error was thrown across a realm boundary"
	if s, err := r.safeString(t.Value); err == nil && s != "" {
		msg = s
	}
	return r.throwTypeError("%s", msg)
}

// safeString describes a thrown value without running any of its code: an
// error's name and message as they were set, or a primitive as it is.
func (r *Runtime) safeString(v Value) (string, error) {
	if v.IsObject() {
		o := v.Object()
		var name, msg string
		for p := o; p != nil; p = p.proto {
			if name == "" {
				if n := p.getOwn(atomName); n != nil && !n.isAccessor() && n.value.IsString() {
					name = n.value.String().Go()
				}
			}
			if msg == "" {
				if m := p.getOwn(atomMessage); m != nil && !m.isAccessor() && m.value.IsString() {
					msg = m.value.String().Go()
				}
			}
		}
		switch {
		case name != "" && msg != "":
			return name + ": " + msg, nil
		case msg != "":
			return msg, nil
		}
		return name, nil
	}
	if v.IsSymbol() {
		return "", nil
	}
	s, err := r.toString(v)
	if err != nil {
		return "", err
	}
	return s.Go(), nil
}

// wrappedValue is GetWrappedValue: a primitive crosses into realm re as it
// is, a function as a wrapper of re's, and any other object not at all.
func (r *Runtime) wrappedValue(re *Realm, v Value) (Value, error) {
	if !v.IsObject() {
		return v, nil
	}
	if !isCallable(v) {
		return Undefined, r.throwTypeError("only primitives and functions can cross a ShadowRealm boundary")
	}
	return r.wrapFunction(re, v.Object())
}

// wrapFunction is WrappedFunctionCreate: a function of realm re that calls
// target, wrapping what crosses on the way.
func (r *Runtime) wrapFunction(re *Realm, target *Object) (Value, error) {
	var f *Object
	r.InRealm(re, func() {
		f = r.newNativeFunc("", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			// This runs in the wrapper's realm, which is the caller's.
			callerRealm := rt.Realm
			targetRealm, err := rt.functionRealm(target)
			if err != nil {
				return Undefined, err
			}
			wrappedArgs := make([]Value, len(args))
			for i, a := range args {
				if wrappedArgs[i], err = rt.wrappedValue(targetRealm, a); err != nil {
					return Undefined, err
				}
			}
			wrappedThis, err := rt.wrappedValue(targetRealm, this)
			if err != nil {
				return Undefined, err
			}
			res, err := rt.call(Obj(target), wrappedThis, wrappedArgs)
			if err != nil {
				return Undefined, rt.crossedError(err)
			}
			return rt.wrappedValue(callerRealm, res)
		})
	})
	if err := r.copyNameAndLength(f, target); err != nil {
		return Undefined, r.crossedError(err)
	}
	return Obj(f), nil
}

// copyNameAndLength gives a wrapper its target's length and name, read as a
// script would read them.
func (r *Runtime) copyNameAndLength(f, target *Object) error {
	length := 0.0
	has, err := r.hasOwnPropOf(target, atomLength)
	if err != nil {
		return err
	}
	if has {
		l, err := r.getProp(target, atomLength, Obj(target))
		if err != nil {
			return err
		}
		if l.IsNumber() {
			switch n := l.Number(); {
			case math.IsInf(n, 1):
				length = n
			case math.IsInf(n, -1), math.IsNaN(n):
				length = 0
			default:
				length = max(math.Trunc(n), 0)
			}
		}
	}
	n, err := r.getProp(target, atomName, Obj(target))
	if err != nil {
		return err
	}
	name := emptyString
	if n.IsString() {
		name = n.String()
	}
	// Both are real properties from the start, since a length of infinity
	// is not something the synthesized form can hold.
	f.setOwnRaw(atomLength, Float(length), propConfigurable)
	f.setOwnRaw(atomName, Str(name), propConfigurable)
	if fd := f.fn(); fd != nil {
		fd.propsMaterialized = true
	}
	return nil
}

// shadowImportValue is ShadowRealmImportValue: the named export of a module
// imported into the shadow realm, as a promise of the caller's realm that a
// failure rejects with one of its TypeErrors.
func (r *Runtime) shadowImportValue(re *Realm, spec string, exportName *String) (Value, error) {
	callerRealm := r.Realm
	result := r.newPromise()
	// The import is the shadow realm's own import(), which runs in it -- and
	// so do the jobs it queues, which load and evaluate the module there.
	var inner Value
	r.InRealm(re, func() {
		inner = r.importCall(bytecode.ImportEvaluate, Str(NewString(spec)), Undefined)
	})
	name := exportName.Go()
	onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, args []Value) (Value, error) {
		ns := arg(args, 0)
		if !ns.IsObject() {
			return Undefined, rt.throwTypeError("the module has no exports")
		}
		key := rt.atoms.intern(name)
		has, err := rt.hasOwnPropOf(ns.Object(), key)
		if err != nil {
			return Undefined, err
		}
		if !has {
			return Undefined, rt.throwTypeError("the module has no export named %s", name)
		}
		v, err := rt.getProp(ns.Object(), key, ns)
		if err != nil {
			return Undefined, err
		}
		return rt.wrappedValue(callerRealm, v)
	})
	// A module that fails to load or to evaluate rejects the result with a
	// TypeError of the caller's, as %ThrowTypeError% would.
	onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, args []Value) (Value, error) {
		return Undefined, rt.crossedError(&Thrown{Value: arg(args, 0)})
	})
	r.promiseThenInto(r.toPromise(inner), Obj(onFulfilled), Obj(onRejected), result)
	return Obj(result), nil
}
