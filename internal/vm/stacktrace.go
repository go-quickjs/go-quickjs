package vm

import (
	"math"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Stack traces, as V8 has them.
//
// An error's stack is captured when the error is made, as frames rather than
// text: the frames are gone by the time anything reads it, but most errors are
// caught and dropped without anyone doing so, and formatting a trace nobody
// reads is the whole cost of a throw. The text is made the first time the
// stack is read, by an accessor every error shares -- which is also when
// Error.prepareStackTrace, if a script has set one, is asked for it, and when
// the header takes the error's name and message, so that a subclass that names
// itself in its constructor is named in its trace.
//
// Error.stackTraceLimit says how many frames are kept, and
// Error.captureStackTrace gives any object a stack of the frames above a
// function of its choosing.

// stackFrame is one frame of a captured stack trace. An error keeps ten of
// them until its stack is read, so they are kept small.
type stackFrame struct {
	// fn is the frame's code, and pc the instruction it is at; fn is nil for a
	// function implemented in Go, which callee is.
	fn     *bytecode.Function
	callee *Object
	this   Value
	pc     uint32
	flags  frameFlags
}

// frameFlags says what kind of frame a stackFrame is.
type frameFlags uint8

const (
	// frameArrow marks a frame with no receiver of its own: an arrow
	// function's, or the code of a direct eval.
	frameArrow frameFlags = 1 << iota
	// frameConstruct marks a frame running a function as a constructor.
	frameConstruct
	// frameHidden marks a frame that is strict, or is called from one that
	// is, whose receiver and function a CallSite does not give out.
	frameHidden
	// frameToplevel marks a frame whose receiver is its realm's global
	// object, which is decided when the frame is captured: by the time the
	// stack is read, another realm may be the running one.
	frameToplevel
)

func (fr *stackFrame) arrow() bool     { return fr.flags&frameArrow != 0 }
func (fr *stackFrame) construct() bool { return fr.flags&frameConstruct != 0 }
func (fr *stackFrame) hidden() bool    { return fr.flags&frameHidden != 0 }

// stackTrace is what an object's stack is: frames until it is first read,
// and then what reading it made of them.
type stackTrace struct {
	frames []stackFrame
	value  Value
	// set marks a trace an object has, and done one that has been read.
	set, done bool
}

// stackTraceLimit reads Error.stackTraceLimit, reporting false when it is
// not a number -- and then an error is made with no stack at all, as V8 makes
// one.
func (r *Runtime) stackTraceLimit() (int, bool) {
	p := r.proto.errorCtors[errError].getOwn(r.atoms.intern("stackTraceLimit"))
	if p == nil || p.isAccessor() || !p.value.IsNumber() {
		return 0, false
	}
	n := p.value.Number()
	switch {
	case math.IsNaN(n) || n <= 0:
		return 0, true
	case n >= float64(math.MaxInt32):
		return math.MaxInt32, true
	}
	return int(n), true
}

// captureTrace records the frames of the call stack, innermost first.
//
// skipTop leaves out the innermost frame, which is the native function doing
// the capturing. Frames are then left out until one whose function is until,
// and that one too, when until is set; a function that is not on the stack
// leaves no frames at all.
func (r *Runtime) captureTrace(until *Object, skipTop bool) stackTrace {
	limit, ok := r.stackTraceLimit()
	if !ok {
		return stackTrace{set: true, done: true, value: Undefined}
	}
	st := stackTrace{set: true}
	if limit == 0 {
		return st
	}
	hidden := false
	// A trace made on one side of a ShadowRealm's boundary hides what is on
	// the other: a CallSite's getThis and getFunction would otherwise hand
	// code inside the realm objects of the realm outside, and back.
	here := r.Realm
	for i := r.frameDepth - 1; i >= 0 && len(st.frames) < limit; i-- {
		f := r.frameAt(i)
		if re := frameRealm(f); re != nil && re != here && (re.shadow || here.shadow) {
			hidden = true
		}
		if f.cl == nil && (f.native == "" || r.isFrameless(f.callee)) {
			// A built-in left out of the trace is strict all the same, and
			// hides its callers as one that is shown does.
			hidden = true
			continue
		}
		if skipTop {
			skipTop = false
			continue
		}
		if until != nil {
			if f.callee == until {
				until = nil
			}
			continue
		}
		fr := stackFrame{this: f.this, callee: f.callee}
		if f.cl != nil {
			fr.fn = f.cl.fn
			if f.pc > 0 {
				// The saved pc is past the instruction the frame is at.
				fr.pc = f.pc - 1
			}
			hidden = hidden || fr.fn.Strict
		} else {
			// A built-in is strict, and so hides its callers as one does.
			hidden = true
		}
		if f.callee != nil {
			if fd := f.callee.fn(); fd != nil && fd.arrow {
				fr.flags |= frameArrow
			}
		}
		if !fr.arrow() && !f.newTarget.IsUndefined() {
			fr.flags |= frameConstruct
		}
		if hidden {
			fr.flags |= frameHidden
		}
		if re := frameRealm(f); re != nil && f.this.IsObject() &&
			(f.this.Object() == re.global || f.this.Object() == re.scope) {
			fr.flags |= frameToplevel
		}
		if st.frames == nil {
			st.frames = make([]stackFrame, 0, min(limit, i+1))
		}
		st.frames = append(st.frames, fr)
	}
	return st
}

// isFrameless reports whether a native function is one a trace passes over.
func (r *Runtime) isFrameless(fn *Object) bool {
	for _, f := range r.frameless {
		if f == fn {
			return true
		}
	}
	return false
}

// attachStack gives an object its stack: the frames, and the accessor that
// makes them text when they are read.
func (r *Runtime) attachStack(o *Object, st stackTrace) {
	o.setOwnRaw(atomStack, r.stackAccessor, propAccessor|propConfigurable)
	r.setTrace(o, st)
}

// setTrace stores an object's frames: in the error itself for an error, and
// under a key no script can name for anything else.
func (r *Runtime) setTrace(o *Object, st stackTrace) {
	if t, ok := o.data.(*Thrown); ok && o.class == ClassError {
		t.trace = st
		return
	}
	holder := newObject(nil, ClassObject)
	holder.data = &st
	o.setOwnRaw(r.stackSlot, Obj(holder), propPrivate)
}

// traceOf returns an object's frames, or nil when it has none.
func (r *Runtime) traceOf(o *Object) *stackTrace {
	if t, ok := o.data.(*Thrown); ok && o.class == ClassError && t.trace.set {
		return &t.trace
	}
	if p := o.getOwn(r.stackSlot); p != nil && p.value.IsObject() {
		st, _ := p.value.Object().data.(*stackTrace)
		return st
	}
	return nil
}

// initStackTraces installs the accessor, Error.captureStackTrace and
// Error.stackTraceLimit. CallSite waits until a script needs one.
func (r *Runtime) initStackTraces() {
	r.stackSlot = r.atoms.internSymbol(NewSymbol("stack trace", true))
	method := func(o *Object, name string) *Object {
		if p := o.getOwn(r.atoms.intern(name)); p != nil && p.value.IsObject() {
			return p.value.Object()
		}
		return nil
	}
	if reflect := r.global.getOwn(r.atoms.intern("Reflect")); reflect != nil && reflect.value.IsObject() {
		r.frameless = [4]*Object{
			method(r.proto.function, "call"), method(r.proto.function, "apply"),
			method(reflect.value.Object(), "apply"), method(reflect.value.Object(), "construct"),
		}
	}

	// The getter and the setter are shared by every error, and are
	// anonymous, as V8's are.
	get := r.newNativeFunc("", 0, func(rt *Runtime, this Value, _ []Value) (Value, error) {
		if !this.IsObject() {
			return Undefined, nil
		}
		st := rt.traceOf(this.Object())
		if st == nil {
			return Undefined, nil
		}
		if !st.done {
			v, err := rt.formatTrace(this.Object(), st.frames)
			if err != nil {
				// Nothing is kept of a failed attempt, so the next read
				// tries again.
				return Undefined, err
			}
			st.done, st.value, st.frames = true, v, nil
		}
		return st.value, nil
	})
	set := r.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() {
			return Undefined, nil
		}
		if st := rt.traceOf(this.Object()); st != nil {
			st.done, st.value, st.frames = true, arg(args, 0), nil
		}
		return Undefined, nil
	})
	r.stackAccessor = accessorValue(&accessor{getter: get, setter: set})
	r.stackGetter, r.stackSetter = get, set

	errorCtor := r.proto.errorCtors[errError]
	defBuiltin(errorCtor, r.atoms.intern("stackTraceLimit"), Float(10),
		propWritable|propEnumerable|propConfigurable)
	r.defMethod(errorCtor, "captureStackTrace", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() || target.Object().class == ClassProxy {
			return Undefined, rt.throwTypeError("invalid_argument")
		}
		o := target.Object()
		var until *Object
		if fn := arg(args, 1); isCallable(fn) {
			until = fn.Object()
		}
		st := rt.captureTrace(until, true)
		ok, err := rt.defineProperty(o, atomStack, &propDesc{
			getter: rt.stackGetter, hasGet: true,
			setter: rt.stackSetter, hasSet: true,
			hasEnumerable: true, configurable: true, hasConfigurable: true,
		})
		if err != nil {
			return Undefined, err
		}
		if !ok {
			if p := o.getOwn(atomStack); p != nil {
				return Undefined, rt.throwTypeError("Cannot redefine property: stack")
			}
			return Undefined, rt.throwTypeError("Cannot define property stack, object is not extensible")
		}
		rt.setTrace(o, st)
		return Undefined, nil
	})
}

// formatTrace makes an object's stack out of its frames: what
// Error.prepareStackTrace returns, if a script has set one and is not already
// in it, and otherwise the header and one line a frame.
func (r *Runtime) formatTrace(o *Object, frames []stackFrame) (Value, error) {
	errorCtor := Obj(r.proto.errorCtors[errError])
	prepare, err := r.getProp(errorCtor.Object(), r.atoms.intern("prepareStackTrace"), errorCtor)
	if err != nil {
		return Undefined, err
	}
	if isCallable(prepare) && !r.preparingStack {
		sites := make([]Value, len(frames))
		for i := range frames {
			sites[i] = r.newCallSite(frames[i])
		}
		// An error made while preparing a stack is formatted the ordinary
		// way, or preparing it would prepare another without end.
		r.preparingStack = true
		defer func() { r.preparingStack = false }()
		return r.call(prepare, errorCtor, []Value{Obj(o), Obj(r.newArrayFrom(sites))})
	}
	header, err := r.errorToString(o)
	if err != nil {
		return Undefined, err
	}
	var b strings.Builder
	b.Grow(len(header) + 48*len(frames))
	b.WriteString(header)
	for i := range frames {
		b.WriteString("\n    at ")
		if r.sourceMaps != nil {
			var next *stackFrame
			if i+1 < len(frames) {
				next = &frames[i+1]
			}
			if r.writeMappedCallSite(&b, &frames[i], next) {
				continue
			}
		}
		r.writeCallSite(&b, &frames[i])
	}
	return Str(NewString(b.String())), nil
}

// errorToString is Error.prototype.toString, which is also a trace's header.
func (r *Runtime) errorToString(o *Object) (string, error) {
	this := Obj(o)
	nameVal, err := r.getProp(o, atomName, this)
	if err != nil {
		return "", err
	}
	msgVal, err := r.getProp(o, atomMessage, this)
	if err != nil {
		return "", err
	}
	name := "Error"
	if !nameVal.IsUndefined() {
		s, err := r.toString(nameVal)
		if err != nil {
			return "", err
		}
		name = s.Go()
	}
	msg := ""
	if !msgVal.IsUndefined() {
		s, err := r.toString(msgVal)
		if err != nil {
			return "", err
		}
		msg = s.Go()
	}
	switch {
	case msg == "":
		return name, nil
	case name == "":
		return msg, nil
	}
	return name + ": " + msg, nil
}

// --- What a frame says about itself, as a CallSite answers ------------------

// functionName is the frame's function's name, or "" for one that has none:
// top-level code, or an anonymous function. Eval code is "eval".
func (fr *stackFrame) functionName() string {
	if fr.fn == nil {
		if fr.callee != nil {
			if fd := fr.callee.fn(); fd != nil {
				return fd.name
			}
		}
		return ""
	}
	if !fr.fn.Anonymous {
		if fr.callee != nil {
			if fd := fr.callee.fn(); fd != nil && fd.name != "" {
				return fd.name
			}
		}
		if name := fr.fn.Name; name != "" && !strings.HasPrefix(name, "<") {
			return name
		}
	}
	if fr.isEval() {
		return "eval"
	}
	return ""
}

// isEval reports whether the frame is running code an eval or the Function
// constructor compiled.
func (fr *stackFrame) isEval() bool {
	return fr.fn != nil && fr.fn.Script != nil && fr.fn.Script.EvalOrigin != ""
}

// isToplevel reports whether the frame has no receiver worth naming: none at
// all, or the global object.
func (r *Runtime) isToplevel(fr *stackFrame) bool {
	return fr.arrow() || fr.this.IsNullish() || fr.flags&frameToplevel != 0
}

// frameRealm is the realm a frame's function belongs to.
func frameRealm(f *frame) *Realm {
	if f.cl != nil {
		return f.cl.realm
	}
	if f.callee != nil {
		if fd := f.callee.fn(); fd != nil {
			return fd.realm
		}
	}
	return nil
}

// position is the frame's line and column, or zeros for a native frame.
func (fr *stackFrame) position() (line, col int32) {
	if fr.fn == nil {
		return 0, 0
	}
	return fr.fn.PositionAt(fr.pc)
}

// fileName is the script the frame's code came from, or "" for native code
// and for eval code, which has no file of its own.
func (fr *stackFrame) fileName() string {
	if fr.fn == nil || fr.isEval() || fr.fn.Script == nil {
		return ""
	}
	return fr.fn.Script.Name
}

// sourceName is what a trace calls the frame's script: the name a
// //# sourceURL= comment gave it, eval code's too, or else its file name,
// as V8's GetScriptNameOrSourceURL has it.
func (fr *stackFrame) sourceName() string {
	if fr.fn != nil && fr.fn.Script != nil && fr.fn.Script.SourceURL() != "" {
		return fr.fn.Script.SourceURL()
	}
	return fr.fileName()
}

// typeName is the name of the receiver's constructor, found without running
// any code: a function receiver -- a static method's class -- is named for
// itself, and anything else for the nearest constructor property on its
// prototype chain.
func (r *Runtime) typeName(fr *stackFrame) string {
	v := fr.this
	switch {
	case v.IsNullish():
		return ""
	case v.IsString():
		return "String"
	case v.IsNumber():
		return "Number"
	case v.IsBool():
		return "Boolean"
	case v.IsSymbol():
		return "Symbol"
	case v.IsBigInt():
		return "BigInt"
	case !v.IsObject():
		return ""
	}
	o := v.Object()
	if fd := o.fn(); fd != nil && fd.name != "" {
		return fd.name
	}
	for p := o; p != nil; p = p.proto {
		c := p.getOwn(atomConstructor)
		if c == nil {
			continue
		}
		if !c.isAccessor() && c.value.IsObject() {
			if fd := c.value.Object().fn(); fd != nil && fd.name != "" {
				return fd.name
			}
		}
		break
	}
	return "Object"
}

// methodName is the key the receiver reaches the frame's function under: its
// own name if that is one, and otherwise the one property on the receiver's
// prototype chain that holds it.
func (r *Runtime) methodName(fr *stackFrame) string {
	if !fr.this.IsObject() || fr.callee == nil {
		return ""
	}
	fn := fr.callee
	holds := func(p *Property) bool {
		if p.flags&propPrivate != 0 {
			return false
		}
		if p.isAccessor() {
			a := p.getterSetter()
			return a != nil && (a.getter == fn || a.setter == fn)
		}
		return p.value.IsObject() && p.value.Object() == fn
	}
	if name := fr.functionName(); name != "" {
		key := r.atoms.intern(name)
		for o := fr.this.Object(); o != nil; o = o.proto {
			if p := o.getOwn(key); p != nil {
				if holds(p) {
					return name
				}
				break
			}
		}
	}
	found := ""
	for o := fr.this.Object(); o != nil; o = o.proto {
		for i := range o.props {
			p := &o.props[i]
			if p.flags&propDeleted != 0 || r.atoms.IsSymbol(p.key) || !holds(p) {
				continue
			}
			name := r.atoms.name(p.key)
			if found != "" && found != name {
				// Two keys hold it, and neither is the one.
				return ""
			}
			found = name
		}
	}
	return found
}

// writeCallSite writes a frame as a trace shows it: the function, as a
// method of its receiver's type or as a constructor, and where it is.
func (r *Runtime) writeCallSite(b *strings.Builder, fr *stackFrame) {
	fnName := fr.functionName()
	switch {
	case fr.construct():
		b.WriteString("new ")
		if fnName == "" {
			fnName = "<anonymous>"
		}
		b.WriteString(fnName)
	case !r.isToplevel(fr):
		r.writeMethodCall(b, fr, fnName)
	case fnName != "":
		b.WriteString(fnName)
	default:
		r.writeLocation(b, fr)
		return
	}
	b.WriteString(" (")
	r.writeLocation(b, fr)
	b.WriteByte(')')
}

// writeMethodCall writes Type.function, and the key it was called by where
// that is not its name.
func (r *Runtime) writeMethodCall(b *strings.Builder, fr *stackFrame, fnName string) {
	typeName := r.typeName(fr)
	methodName := r.methodName(fr)
	if fnName == "" {
		if typeName != "" {
			b.WriteString(typeName)
			b.WriteByte('.')
		}
		if methodName == "" {
			methodName = "<anonymous>"
		}
		b.WriteString(methodName)
		return
	}
	if typeName != "" && !strings.HasPrefix(fnName, typeName+".") {
		b.WriteString(typeName)
		b.WriteByte('.')
	}
	b.WriteString(fnName)
	if methodName != "" && fnName != methodName &&
		!strings.HasSuffix(fnName, "."+methodName) && !strings.HasSuffix(fnName, " "+methodName) {
		b.WriteString(" [as ")
		b.WriteString(methodName)
		b.WriteByte(']')
	}
}

// writeLocation writes where a frame is: its file, line and column, or where
// the eval that compiled its code was.
func (r *Runtime) writeLocation(b *strings.Builder, fr *stackFrame) {
	if fr.fn == nil {
		b.WriteString("<anonymous>")
		return
	}
	name := fr.sourceName()
	if name == "" && fr.isEval() {
		// Eval code is placed where it was evaluated, unless it named
		// itself.
		b.WriteString(fr.fn.Script.EvalOrigin)
		b.WriteString(", ")
	}
	if name == "" {
		name = "<anonymous>"
	}
	b.WriteString(name)
	if line, col := fr.position(); line > 0 {
		b.WriteByte(':')
		b.WriteString(itoa32(line))
		if col > 0 {
			b.WriteByte(':')
			b.WriteString(itoa32(col))
		}
	}
}

// evalOrigin describes where code being evaluated now is evaluated from, as
// the trace of a frame in that code shows it: "eval at f (main.js:3:9)". It
// is the innermost frame running JavaScript, which the eval or Function call
// is in.
// activeReferrer is what an import() the running code makes is resolved
// against: the referrer of the script or module of the innermost frame running
// JavaScript, or empty when none is.
func (r *Runtime) activeReferrer() string {
	for i := r.frameDepth - 1; i >= 0; i-- {
		if f := r.frameAt(i); f.cl != nil {
			if s := f.cl.fn.Script; s != nil {
				return s.Referrer
			}
			return ""
		}
	}
	return ""
}

func (r *Runtime) evalOrigin() string {
	for i := r.frameDepth - 1; i >= 0; i-- {
		f := r.frameAt(i)
		if f.cl == nil {
			continue
		}
		fr := stackFrame{fn: f.cl.fn, callee: f.callee}
		if f.pc > 0 {
			fr.pc = f.pc - 1
		}
		var b strings.Builder
		b.WriteString("eval at ")
		name := fr.functionName()
		if name == "" {
			name = "<anonymous>"
		}
		b.WriteString(name)
		b.WriteString(" (")
		r.writeLocation(&b, &fr)
		b.WriteByte(')')
		return b.String()
	}
	return "eval"
}
