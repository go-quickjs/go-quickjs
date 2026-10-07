package vm

import (
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"sync"
)

// What a debugger working from other goroutines needs: a way to have work
// run on the runtime's goroutine, and what the Chrome DevTools Protocol
// asks of a runtime -- values described and listed without running a
// script, code evaluated in global scope, the places a script can stop.
// The protocol itself is the inspector package's.

// debugInbox is the work other goroutines posted for the runtime's.
type debugInbox struct {
	mu   sync.Mutex
	work []func()
	// ready has a value when work waits.
	ready chan struct{}
}

func (d *debugState) inbox() *debugInbox {
	return &d.posted
}

// newDebugState is a runtime's debugger, as Config.Debug makes it.
func newDebugState() *debugState {
	return &debugState{posted: debugInbox{ready: make(chan struct{}, 1)}, closed: make(chan struct{})}
}

// DebugPost runs fn on the runtime's goroutine, from any goroutine: during a
// pause, by whoever is waiting on DebugReady there; while code runs, at the
// next interrupt check; and while the runtime waits for work of its host's,
// as a job of the host's loop.
func (r *Runtime) DebugPost(fn func()) {
	d := r.debug
	if d == nil {
		return
	}
	in := d.inbox()
	in.mu.Lock()
	if d.isClosed.Load() {
		// The runtime has closed, and runs nothing more.
		in.mu.Unlock()
		return
	}
	in.work = append(in.work, fn)
	in.mu.Unlock()
	d.hasWork.Store(true)
	select {
	case in.ready <- struct{}{}:
	default:
	}
	r.postFromElsewhere(func() { r.DebugDrain() })
}

// DebugReady has a value when work has been posted, for a goroutine waiting
// on the runtime's behalf -- a paused debugger -- to call DebugDrain.
func (r *Runtime) DebugReady() <-chan struct{} {
	if r.debug == nil {
		return nil
	}
	return r.debug.posted.ready
}

// DebugDrain runs the work posted so far, on the runtime's goroutine.
func (r *Runtime) DebugDrain() {
	d := r.debug
	if d == nil {
		return
	}
	in := d.inbox()
	for {
		in.mu.Lock()
		work := in.work
		in.work = nil
		d.hasWork.Store(false)
		in.mu.Unlock()
		if len(work) == 0 {
			return
		}
		for _, fn := range work {
			fn()
		}
	}
}

// PauseAtNextStatement stops the code at the next statement it runs, on the
// runtime's goroutine: a debugger's pause command, or a program stopped
// before its first statement.
func (r *Runtime) PauseAtNextStatement() {
	if d := r.debug; d != nil {
		d.pauseNext = true
		d.rearm()
	}
}

// DebugCompile compiles code in global scope without running it, and
// reports what is wrong with it.
func (r *Runtime) DebugCompile(src string) error {
	d := r.debug
	if d == nil || d.compile == nil {
		return errors.New("the runtime has no debugger")
	}
	_, err := d.compile(src, EvalRequest{})
	return err
}

// DebugGlobalLexicalNames are the names the runtime's scripts declared at
// their top level with let, const or class.
func (r *Runtime) DebugGlobalLexicalNames() []string {
	if r.globalLex == nil {
		return nil
	}
	var names []string
	for _, k := range r.globalLex.ownKeys(false, r.atoms) {
		names = append(names, r.atoms.name(k))
	}
	return names
}

// SetBreakpointsActive turns every breakpoint on or off at once, which
// keeps them where they are.
func (r *Runtime) SetBreakpointsActive(on bool) {
	if d := r.debug; d != nil {
		d.inactive = !on
		d.rearm()
	}
}

// SetBreakpointMatching sets a breakpoint at a line and column of every
// script match accepts, and of every one loaded later, as SetBreakpoint
// does for one name.
func (r *Runtime) SetBreakpointMatching(match func(*DebugScript) bool, line, col int) (int, []DebugLocation) {
	d := r.debug
	if d == nil {
		return 0, nil
	}
	b := d.newBreakpoint("", line, col)
	b.match = match
	var at []DebugLocation
	for _, s := range d.scripts {
		if s.Name != "" && match(s) {
			if loc, ok := d.resolve(b, s); ok {
				at = append(at, loc)
			}
		}
	}
	d.rearm()
	return b.id, at
}

// URLMatcher is a breakpoint's match for a script name equal to url, or
// matching a regular expression where regex is set.
func URLMatcher(url, regex string) (func(string) bool, error) {
	if regex == "" {
		return func(name string) bool { return name == url }, nil
	}
	re, err := regexp.Compile(regex)
	if err != nil {
		return nil, err
	}
	return re.MatchString, nil
}

// Locations are the places in the script the code can stop, in order.
func (s *DebugScript) Locations() []DebugLocation {
	var out []DebugLocation
	for _, fn := range s.fns {
		for i, st := range fn.Debug.Statements {
			if !debugOf(fn).visible(i) {
				continue
			}
			line, col := s.script.Position(st.Pos)
			out = append(out, DebugLocation{Script: s, Line: int(line), Column: int(col)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Line < b.Line || a.Line == b.Line && a.Column < b.Column
	})
	return out
}

// Offset is where in a larger file the script's Source is placed: how many
// lines down it starts, and how many columns in.
func (s *DebugScript) Offset() (line, col int) {
	l, c := s.script.Offsets()
	if l < 0 {
		// The lines above the file are left out of the source.
		return 0, 0
	}
	return int(l), int(c)
}

// DebugEvaluate runs code in global scope, as an indirect eval does, though
// the runtime generates no code from strings. Nothing it runs stops.
func (r *Runtime) DebugEvaluate(src string) (Value, error) {
	d := r.debug
	if d == nil || d.compile == nil {
		return Undefined, errors.New("the runtime has no debugger")
	}
	if err := r.debugStopped(); err != nil {
		return Undefined, err
	}
	fn, err := d.compile(src, EvalRequest{})
	if err != nil {
		return Undefined, err
	}
	paused := d.paused
	d.paused = true
	defer func() { d.paused = paused }()
	return r.Run(fn)
}

// DebugCall calls a function as a debugger does, with nothing it runs
// stopping.
func (r *Runtime) DebugCall(fn, this Value, args []Value) (Value, error) {
	if err := r.debugStopped(); err != nil {
		return Undefined, err
	}
	if d := r.debug; d != nil {
		paused := d.paused
		d.paused = true
		defer func() { d.paused = paused }()
	}
	return r.Call(fn, this, args)
}

// SetVariable changes a binding the frame sees at its statement, as an
// assignment there would but for a const, which it refuses.
func (df *DebugFrame) SetVariable(name string, v Value) error {
	if !df.pause.live {
		return errNotPaused
	}
	if err := df.r.debugStopped(); err != nil {
		return err
	}
	st := df.statement()
	if st == nil {
		return errors.New("the frame's code was not compiled for a debugger")
	}
	f := df.f
	for _, b := range df.fn.EvalScopes[st.Scope].Bindings {
		if b.Name != name {
			continue
		}
		if !b.Mutable {
			return errors.New("assignment to constant variable " + name)
		}
		switch {
		case b.FromLocal && int(b.Index) < len(f.locals):
			f.locals[b.Index] = v
		case !b.FromLocal && int(b.Index) < len(f.cl.upvalues):
			*f.cl.upvalues[b.Index].slot = v
		default:
			continue
		}
		return nil
	}
	return errors.New("no variable " + name + " in scope")
}

// DebugStackEntry is a frame of compiled code on the stack.
type DebugStackEntry struct {
	FunctionName string
	Location     DebugLocation
}

// DebugStack is the frames of compiled code on the stack, innermost first,
// at most limit of them.
func (r *Runtime) DebugStack(limit int) []DebugStackEntry {
	var out []DebugStackEntry
	p := &DebugPause{live: true}
	for _, f := range r.debugFrames(p) {
		if len(out) == limit {
			break
		}
		out = append(out, DebugStackEntry{FunctionName: f.FunctionName(), Location: f.Location()})
	}
	p.live = false
	return out
}

// DebugValue is a value as a debugger describes it, as the protocol's
// RemoteObject does: its type and subtype, the name of its class, and a
// short description.
type DebugValue struct {
	// Type is what typeof says, but "object" for a function a class makes
	// too: undefined, object, function, string, number, boolean, symbol or
	// bigint.
	Type string
	// Subtype says more of an object: null, array, error, date, regexp,
	// map, set, weakmap, weakset, promise, proxy, typedarray, arraybuffer,
	// dataview, generator, iterator, or empty.
	Subtype   string
	ClassName string
	// Description is how a debugger shows it.
	Description string
	// Unserializable is the text of a number JSON cannot write -- NaN,
	// Infinity, -Infinity, -0 -- or a BigInt's, "1n".
	Unserializable string
}

// DebugDescribe describes a value without running any of a script's code.
func (r *Runtime) DebugDescribe(v Value) DebugValue {
	switch {
	case v.IsUndefined():
		return DebugValue{Type: "undefined", Description: "undefined"}
	case v.IsNull():
		return DebugValue{Type: "object", Subtype: "null", Description: "null"}
	case v.IsBool():
		return DebugValue{Type: "boolean", Description: strconv.FormatBool(v.BoolValue())}
	case v.IsNumber():
		d := DebugValue{Type: "number"}
		if s, err := r.toString(v); err == nil {
			d.Description = s.Go()
		}
		x := v.Number()
		switch {
		case math.IsNaN(x), math.IsInf(x, 0):
			d.Unserializable = d.Description
		case x == 0 && math.Signbit(x):
			d.Unserializable, d.Description = "-0", "-0"
		}
		return d
	case v.IsString():
		return DebugValue{Type: "string", Description: v.String().Go()}
	case v.IsSymbol():
		return DebugValue{Type: "symbol", Description: v.Symbol().String()}
	case v.IsBigInt():
		s := v.BigInt().V.String() + "n"
		return DebugValue{Type: "bigint", Description: s, Unserializable: s}
	}
	o := v.Object()
	d := DebugValue{Type: "object", ClassName: r.className(o)}
	d.Description = d.ClassName
	switch o.class {
	case ClassFunction:
		d.Type, d.ClassName, d.Description = "function", "Function", functionText(o)
	case ClassArray:
		d.Subtype = "array"
		d.Description = d.ClassName + "(" + strconv.FormatInt(r.debugLength(o), 10) + ")"
	case ClassError:
		d.Subtype = "error"
		d.Description = r.describeVia(o, "stack", d.ClassName)
	case ClassDate:
		d.Subtype = "date"
		d.Description = r.describeCall(r.proto.date, "toString", o, d.ClassName)
	case ClassRegExp:
		d.Subtype = "regexp"
		d.Description = r.describeCall(r.proto.regexp, "toString", o, d.ClassName)
	case ClassMap, ClassSet:
		d.Subtype = map[Class]string{ClassMap: "map", ClassSet: "set"}[o.class]
		proto := map[Class]*Object{ClassMap: r.proto.mapProto, ClassSet: r.proto.setProto}[o.class]
		d.Description = d.ClassName + "(" + r.describeGetter(proto, "size", o, "?") + ")"
	case ClassWeakMap:
		d.Subtype = "weakmap"
	case ClassWeakSet:
		d.Subtype = "weakset"
	case ClassPromise:
		d.Subtype = "promise"
	case ClassProxy:
		d.Subtype, d.Description = "proxy", "Proxy"
	case ClassTypedArray:
		d.Subtype = "typedarray"
		d.Description = d.ClassName + "(" + strconv.FormatInt(r.debugLength(o), 10) + ")"
	case ClassArrayBuffer:
		d.Subtype = "arraybuffer"
	case ClassDataView:
		d.Subtype = "dataview"
	case ClassGenerator, ClassAsyncGenerator:
		d.Subtype = "generator"
	case ClassIterator, ClassIteratorHelper, ClassIteratorWrap:
		d.Subtype = "iterator"
	}
	return d
}

// className is the name of an object's constructor, as a debugger calls its
// class: what its prototype's constructor property holds, read without
// running a getter.
func (r *Runtime) className(o *Object) string {
	if o.class == ClassProxy {
		return "Proxy"
	}
	for p := o; p != nil; p = p.proto {
		if proxyOf(p) != nil {
			break
		}
		pd, err := r.currentDescriptor(p, r.atoms.intern("constructor"))
		if err != nil || pd == nil || pd.isAccessor() || !pd.value.IsObject() {
			continue
		}
		if fd := pd.value.Object().fn(); fd != nil {
			if name := fd.nameOr(""); name != "" {
				return name
			}
		}
	}
	return "Object"
}

// debugLength is an array's or a typed array's length, as its own property
// or its prototype's getter says it.
func (r *Runtime) debugLength(o *Object) int64 {
	l, err := r.quietGet(o, "length")
	if err != nil || !l.IsNumber() {
		return 0
	}
	return int64(l.Number())
}

// describeVia is a property of an object as a string -- an error's stack --
// or else what a debugger calls it otherwise.
func (r *Runtime) describeVia(o *Object, name, otherwise string) string {
	s, err := r.quietGet(o, name)
	if err != nil || !s.IsString() {
		return otherwise
	}
	return s.String().Go()
}

// quietGet reads a property as a debugger does, with nothing a getter runs
// stopping.
func (r *Runtime) quietGet(o *Object, name string) (Value, error) {
	if d := r.debug; d != nil {
		paused := d.paused
		d.paused = true
		defer func() { d.paused = paused }()
	}
	return r.getValueProp(Obj(o), r.atoms.intern(name))
}

// describeCall is what a built-in method of an intrinsic prototype makes
// of an object, which a script cannot have replaced.
func (r *Runtime) describeCall(proto *Object, method string, o *Object, otherwise string) string {
	if proto == nil {
		return otherwise
	}
	pd, err := r.currentDescriptor(proto, r.atoms.intern(method))
	if err != nil || pd == nil || pd.isAccessor() {
		return otherwise
	}
	s, err := r.DebugCall(pd.value, Obj(o), nil)
	if err != nil || !s.IsString() {
		return otherwise
	}
	return s.String().Go()
}

// describeGetter is what a built-in getter of an intrinsic prototype says
// of an object.
func (r *Runtime) describeGetter(proto *Object, name string, o *Object, otherwise string) string {
	if proto == nil {
		return otherwise
	}
	pd, err := r.currentDescriptor(proto, r.atoms.intern(name))
	if err != nil || pd == nil || pd.getter == nil {
		return otherwise
	}
	v, err := r.DebugCall(Obj(pd.getter), Obj(o), nil)
	if err != nil {
		return otherwise
	}
	s, err := r.toString(v)
	if err != nil {
		return otherwise
	}
	return s.Go()
}

// DebugProperty is an object's own property as a debugger lists it.
type DebugProperty struct {
	Name string
	// Symbol is the property's key where it is a symbol, and Name then its
	// description, as Symbol.prototype.toString writes it.
	Symbol *Symbol
	// Value is a data property's; Getter and Setter an accessor's, either
	// nil.
	Value          Value
	Getter, Setter *Object
	Accessor       bool
	Writable       bool
	Enumerable     bool
	Configurable   bool
}

// DebugInternal is a slot of an object no property shows, as a debugger
// lists it: [[Prototype]], a wrapper's [[PrimitiveValue]], a promise's
// state and result, a proxy's target and handler.
type DebugInternal struct {
	Name  string
	Value Value
}

// DebugProperties lists an object's own properties and its internal slots,
// running none of a script's code: a proxy's traps are not asked, and an
// accessor is listed rather than called.
func (r *Runtime) DebugProperties(o *Object) ([]DebugProperty, []DebugInternal) {
	var internal []DebugInternal
	if p := proxyOf(o); p != nil {
		target, handler := Null, Null
		if p.target != nil {
			target = Obj(p.target)
		}
		if p.handler != nil {
			handler = Obj(p.handler)
		}
		return nil, []DebugInternal{{"[[Handler]]", handler}, {"[[Target]]", target}}
	}
	var props []DebugProperty
	keys, _ := r.ownKeysOf(o, true)
	for _, k := range keys {
		pd, err := r.currentDescriptor(o, k)
		if err != nil || pd == nil {
			continue
		}
		dp := DebugProperty{
			Accessor: pd.isAccessor(), Writable: pd.writable,
			Enumerable: pd.enumerable, Configurable: pd.configurable,
		}
		if r.atoms.IsSymbol(k) {
			dp.Symbol = r.atoms.symbol(k)
			dp.Name = dp.Symbol.String()
		} else {
			dp.Name = r.atoms.name(k)
		}
		if dp.Accessor {
			dp.Getter, dp.Setter = pd.getter, pd.setter
		} else {
			dp.Value = pd.value
		}
		props = append(props, dp)
	}
	if pd, ok := o.data.(*promiseData); ok {
		state := map[promiseState]string{promisePending: "pending", promiseFulfilled: "fulfilled", promiseRejected: "rejected"}[pd.state]
		internal = append(internal, DebugInternal{"[[PromiseState]]", Str(NewString(state))})
		if pd.state != promisePending {
			internal = append(internal, DebugInternal{"[[PromiseResult]]", pd.value})
		}
	}
	switch o.class {
	case ClassBooleanWrapper, ClassNumberWrapper, ClassStringWrapper, ClassSymbolWrapper, ClassBigIntWrapper:
		if v, ok := o.data.(Value); ok {
			internal = append(internal, DebugInternal{"[[PrimitiveValue]]", v})
		}
	}
	proto := Null
	if o.proto != nil {
		proto = Obj(o.proto)
	}
	internal = append(internal, DebugInternal{"[[Prototype]]", proto})
	return props, internal
}
