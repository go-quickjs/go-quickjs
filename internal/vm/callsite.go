package vm

import "strings"

// CallSite is what Error.prepareStackTrace is given a frame as: an object with
// no properties of its own whose prototype's methods answer questions about
// the frame. There is no global CallSite; the constructor is reached only as
// the prototype's constructor, and refuses to make one.

// callSite is a CallSite's frame.
type callSite struct {
	frame stackFrame
}

// newCallSite wraps a frame for Error.prepareStackTrace.
func (r *Runtime) newCallSite(fr stackFrame) Value {
	// The prototype is made when a script first asks for a CallSite, which
	// most never do.
	if r.proto.callSite == nil {
		r.initCallSite()
	}
	o := newObject(r.proto.callSite, ClassObject)
	o.data = &callSite{frame: fr}
	return Obj(o)
}

func (r *Runtime) initCallSite() {
	proto := newObject(r.proto.object, ClassObject)
	r.proto.callSite = proto
	ctor := r.newNativeFunc("CallSite", 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
		return Undefined, rt.throwTypeError("Illegal constructor")
	})
	ctor.setOwnRaw(atomPrototype, Obj(proto), 0)
	proto.setOwnRaw(atomConstructor, Obj(ctor), propWritable|propConfigurable)

	// method defines one of the prototype's methods, each of which works only
	// on a CallSite.
	method := func(name string, fn func(rt *Runtime, fr *stackFrame) Value) {
		r.defMethod(proto, name, 0, func(rt *Runtime, this Value, _ []Value) (Value, error) {
			var cs *callSite
			if this.IsObject() {
				cs, _ = this.Object().data.(*callSite)
			}
			if cs == nil {
				return Undefined, rt.throwTypeError("CallSite method %s expects CallSite as receiver", name)
			}
			return fn(rt, &cs.frame), nil
		})
	}
	str := func(s string) Value {
		if s == "" {
			return Null
		}
		return Str(NewString(s))
	}
	num := func(n int32) Value {
		if n <= 0 {
			return Null
		}
		return Float(float64(n))
	}

	method("getThis", func(rt *Runtime, fr *stackFrame) Value {
		if fr.hidden() {
			return Undefined
		}
		return fr.this
	})
	// Only a method call has a receiver type and a key worth naming.
	method("getTypeName", func(rt *Runtime, fr *stackFrame) Value {
		if fr.construct() || rt.isToplevel(fr) {
			return Null
		}
		return str(rt.typeName(fr))
	})
	method("getFunction", func(rt *Runtime, fr *stackFrame) Value {
		if fr.hidden() || fr.callee == nil {
			return Undefined
		}
		return Obj(fr.callee)
	})
	method("getFunctionName", func(rt *Runtime, fr *stackFrame) Value {
		return str(fr.functionName())
	})
	method("getMethodName", func(rt *Runtime, fr *stackFrame) Value {
		if fr.construct() || rt.isToplevel(fr) {
			return Null
		}
		return str(rt.methodName(fr))
	})
	fileName := func(rt *Runtime, fr *stackFrame) Value {
		if name := fr.fileName(); name != "" {
			return Str(NewString(name))
		}
		return Undefined
	}
	method("getFileName", fileName)
	method("getScriptNameOrSourceURL", func(rt *Runtime, fr *stackFrame) Value {
		if name := fr.sourceName(); name != "" {
			return Str(NewString(name))
		}
		return Undefined
	})
	method("getScriptHash", func(*Runtime, *stackFrame) Value { return Str(emptyString) })
	method("getLineNumber", func(rt *Runtime, fr *stackFrame) Value {
		line, _ := fr.position()
		return num(line)
	})
	method("getColumnNumber", func(rt *Runtime, fr *stackFrame) Value {
		_, col := fr.position()
		return num(col)
	})
	method("getEnclosingLineNumber", func(rt *Runtime, fr *stackFrame) Value {
		if fr.fn == nil {
			return Null
		}
		line, _ := fr.enclosing()
		return num(line)
	})
	method("getEnclosingColumnNumber", func(rt *Runtime, fr *stackFrame) Value {
		if fr.fn == nil {
			return Null
		}
		_, col := fr.enclosing()
		return num(col)
	})
	method("getPosition", func(rt *Runtime, fr *stackFrame) Value {
		if fr.fn == nil {
			return Float(0)
		}
		pos, _ := fr.fn.PosAt(fr.pc)
		return Float(float64(fr.fn.Script.Offset(pos)))
	})
	method("getEvalOrigin", func(rt *Runtime, fr *stackFrame) Value {
		if !fr.isEval() {
			return Undefined
		}
		return Str(NewString(fr.fn.Script.EvalOrigin))
	})
	method("getPromiseIndex", func(*Runtime, *stackFrame) Value { return Null })
	method("isToplevel", func(rt *Runtime, fr *stackFrame) Value { return Bool(rt.isToplevel(fr)) })
	method("isEval", func(rt *Runtime, fr *stackFrame) Value { return Bool(fr.isEval()) })
	method("isNative", func(*Runtime, *stackFrame) Value { return False })
	method("isConstructor", func(rt *Runtime, fr *stackFrame) Value { return Bool(fr.construct()) })
	method("isAsync", func(*Runtime, *stackFrame) Value { return False })
	method("isPromiseAll", func(*Runtime, *stackFrame) Value { return False })
	method("toString", func(rt *Runtime, fr *stackFrame) Value {
		var b strings.Builder
		rt.writeCallSite(&b, fr)
		return Str(NewString(b.String()))
	})
}
