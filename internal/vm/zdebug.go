package vm

import (
	"errors"
	"slices"
	"sort"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Debugging.
//
// A runtime made with Config.Debug runs code compiled for a debugger: every
// statement it can stop at begins with an OpDebugStmt, and its function
// records what is in scope there. Such code runs in the interpreter -- the
// tree tier builds no function with an instruction it does not know -- and
// a statement costs one test of debugState.armed until a debugger asks to
// stop somewhere. A runtime made without it compiles no OpDebugStmt, and
// runs exactly as it would were there no debugger at all.
//
// What a debugger is told, and what it asks, happens on the runtime's
// goroutine: a DebugHandler's Paused blocks the code where it stopped until
// it returns, and meanwhile may look at the frames and evaluate code in them.
// Only RequestPause may be called from another goroutine.
//
// This file sorts last in the package, with the other code kept out of the
// interpreter's way, so that adding to it moves none of the hot functions.

// debugState is a runtime's debugger.
type debugState struct {
	// armed says whether an OpDebugStmt has anything to ask: a breakpoint
	// set, a step under way, or a pause asked for, with a handler to tell.
	// With none, a statement costs the test of it.
	armed bool
	// paused is set while a handler is told something, when nothing stops:
	// code it evaluates runs through.
	paused bool

	handler DebugHandler
	// compile compiles the code a debugger evaluates, which it may though
	// the runtime generates no code from strings.
	compile Evaluator

	step      StepAction
	stepDepth int
	pauseNext bool
	// requested is set by RequestPause, from any goroutine, and turned into
	// pauseNext by the next interrupt check.
	requested atomic.Bool

	exceptions ExceptionPause

	scripts  []*DebugScript
	byScript map[*bytecode.Script]*DebugScript

	breakpoints map[int]*breakpoint
	nextID      int
	// locations counts the statements breakpoints are set at.
	locations int
}

// fnDebug is the VM's record of a function compiled for a debugger, in its
// Debug.State: how many breakpoints each statement has.
type fnDebug struct {
	breaks []int32
}

func debugOf(fn *bytecode.Function) *fnDebug {
	if fn.Debug == nil {
		return nil
	}
	return (*fnDebug)(fn.Debug.State)
}

// breakpoint is one SetBreakpoint asked for.
type breakpoint struct {
	id int
	// url is the script name it was set by, which code loaded later is
	// matched against; empty for one set in a script.
	url       string
	line, col int
	at        []bpLocation
}

type bpLocation struct {
	fn   *bytecode.Function
	stmt int
}

// DebugHandler is a debugger's side of a runtime. Its methods are called on
// the runtime's goroutine.
type DebugHandler interface {
	// ScriptParsed reports code compiled for the debugger, before it runs:
	// a script, a module, or the code of an eval or a Function call.
	ScriptParsed(s *DebugScript)
	// BreakpointResolved reports where a breakpoint set by a script's name
	// took effect, in code loaded after it was set.
	BreakpointResolved(id int, at DebugLocation)
	// Paused reports that the code has stopped, which it stays until Paused
	// returns, and goes on as the StepAction it returns says. Meanwhile the
	// pause's frames may be looked at and code evaluated in them.
	Paused(p *DebugPause) StepAction
}

// StepAction is how the code goes on after a pause.
type StepAction int

const (
	// Continue runs on until something stops it again.
	Continue StepAction = iota
	// StepInto stops at the next statement, in a function the current one
	// calls if it calls one.
	StepInto
	// StepOver stops at the next statement of the current function, or of
	// its caller once it returns.
	StepOver
	// StepOut stops at the next statement of the current function's caller.
	StepOut
)

// ExceptionPause says which exceptions a debugger stops at.
type ExceptionPause int

const (
	PauseOnNoExceptions ExceptionPause = iota
	// PauseOnUncaughtExceptions stops at an exception that no catch clause
	// on the stack will catch.
	PauseOnUncaughtExceptions
	PauseOnAllExceptions
)

// PauseReason is why the code stopped.
type PauseReason int

const (
	PauseBreakpoint PauseReason = iota
	PauseStep
	PauseDebuggerStatement
	PauseException
	PauseRequested
)

// DebugPause is the code stopped. Its frames are valid until the handler's
// Paused returns.
type DebugPause struct {
	Reason PauseReason
	// Breakpoints are the IDs of the breakpoints at the statement it
	// stopped at.
	Breakpoints []int
	// Exception is what was thrown, for PauseException, and Caught whether
	// a catch clause on the stack will catch it.
	Exception Value
	Caught    bool
	// Frames are the frames of compiled code on the stack, innermost first.
	Frames []*DebugFrame

	live bool
}

// DebugScript is code loaded for a debugger.
type DebugScript struct {
	// ID numbers the scripts of a runtime from 1, in the order they loaded.
	ID int
	// Name is the file name or specifier it was loaded as, or empty for the
	// code of an eval or a Function call.
	Name string
	// EvalOrigin says where an eval's or a Function call's code was
	// compiled, as a stack trace does.
	EvalOrigin string
	script     *bytecode.Script
	fns        []*bytecode.Function
}

// Source is the script's text.
func (s *DebugScript) Source() string { return s.script.Text() }

// DebugLocation is a place in a script, its line and column counted from 1
// as a stack trace's are, the column in UTF-16 code units.
type DebugLocation struct {
	Script       *DebugScript
	Line, Column int
}

// DebugFrame is a frame of compiled code a pause found on the stack.
type DebugFrame struct {
	r     *Runtime
	pause *DebugPause
	f     *frame
	fn    *bytecode.Function
	pc    uint32
}

// DebugScope is a scope a frame sees, innermost first.
type DebugScope struct {
	Kind ScopeKind
	// Bindings are a local or a closure scope's names.
	Bindings []DebugBinding
	// Object is a with, a script or the global scope's object, whose
	// properties are its bindings.
	Object *Object
}

// ScopeKind says what a DebugScope is.
type ScopeKind int

const (
	ScopeLocal ScopeKind = iota
	ScopeClosure
	ScopeWith
	// ScopeScript is the top-level let, const and class declarations of the
	// runtime's scripts.
	ScopeScript
	ScopeGlobal
)

// DebugBinding is a name in a scope and what it holds.
type DebugBinding struct {
	Name  string
	Value Value
	// Uninitialized marks a let or const not yet reached, whose Value means
	// nothing.
	Uninitialized bool
	Mutable       bool
}

// errNotPaused is what a frame answers once its pause is over.
var errNotPaused = errors.New("the runtime is no longer paused there")

// Debugging reports whether the runtime was made for a debugger.
func (r *Runtime) Debugging() bool { return r.debug != nil }

// SetDebugHandler attaches a debugger, or with nil detaches it. Detaching
// keeps the breakpoints, which stop nothing until one is attached again.
func (r *Runtime) SetDebugHandler(h DebugHandler) {
	if d := r.debug; d != nil {
		d.handler = h
		if h == nil {
			d.step, d.pauseNext = Continue, false
		}
		d.rearm()
	}
}

// SetDebugEvaluator gives the debugger what it compiles code to evaluate
// with.
func (r *Runtime) SetDebugEvaluator(e Evaluator) {
	if d := r.debug; d != nil {
		d.compile = e
	}
}

// DebugScripts lists the scripts loaded so far.
func (r *Runtime) DebugScripts() []*DebugScript {
	if r.debug == nil {
		return nil
	}
	return slices.Clone(r.debug.scripts)
}

// SetPauseOnExceptions sets which exceptions stop the code.
func (r *Runtime) SetPauseOnExceptions(p ExceptionPause) {
	if d := r.debug; d != nil {
		d.exceptions = p
	}
}

// RequestPause asks the code to stop at the next statement it runs. It may
// be called from any goroutine: code running now stops within the
// interrupt check's interval, and a runtime running nothing stops at the
// first statement it next runs.
func (r *Runtime) RequestPause() {
	if d := r.debug; d != nil {
		d.requested.Store(true)
	}
}

// SetBreakpoint sets a breakpoint at a line and column of every script
// named url, and of every one loaded later: at the first statement there or
// after it. It reports the breakpoint's ID and where it took effect so far.
func (r *Runtime) SetBreakpoint(url string, line, col int) (int, []DebugLocation) {
	d := r.debug
	if d == nil {
		return 0, nil
	}
	b := d.newBreakpoint(url, line, col)
	var at []DebugLocation
	for _, s := range d.scripts {
		if s.Name == url {
			if loc, ok := d.resolve(b, s); ok {
				at = append(at, loc)
			}
		}
	}
	d.rearm()
	return b.id, at
}

// SetScriptBreakpoint sets a breakpoint in one script, at the first
// statement at or after a line and column, and reports where.
func (r *Runtime) SetScriptBreakpoint(s *DebugScript, line, col int) (int, DebugLocation, bool) {
	d := r.debug
	if d == nil || s == nil {
		return 0, DebugLocation{}, false
	}
	b := d.newBreakpoint("", line, col)
	loc, ok := d.resolve(b, s)
	if !ok {
		delete(d.breakpoints, b.id)
		return 0, DebugLocation{}, false
	}
	d.rearm()
	return b.id, loc, true
}

// RemoveBreakpoint removes a breakpoint.
func (r *Runtime) RemoveBreakpoint(id int) {
	d := r.debug
	if d == nil {
		return
	}
	if b := d.breakpoints[id]; b != nil {
		for _, l := range b.at {
			debugOf(l.fn).breaks[l.stmt]--
			d.locations--
		}
		delete(d.breakpoints, id)
		d.rearm()
	}
}

func (d *debugState) newBreakpoint(url string, line, col int) *breakpoint {
	d.nextID++
	b := &breakpoint{id: d.nextID, url: url, line: line, col: col}
	if d.breakpoints == nil {
		d.breakpoints = make(map[int]*breakpoint)
	}
	d.breakpoints[b.id] = b
	return b
}

// resolve sets a breakpoint at the first statement of a script at or after
// its line and column.
func (d *debugState) resolve(b *breakpoint, s *DebugScript) (DebugLocation, bool) {
	var best bpLocation
	bestLine, bestCol := int32(-1), int32(-1)
	for _, fn := range s.fns {
		for i, st := range fn.Debug.Statements {
			line, col := s.script.Position(st.Pos)
			if int(line) < b.line || int(line) == b.line && int(col) < b.col {
				continue
			}
			if bestLine < 0 || line < bestLine || line == bestLine && col < bestCol {
				best, bestLine, bestCol = bpLocation{fn, i}, line, col
			}
		}
	}
	if bestLine < 0 {
		return DebugLocation{}, false
	}
	for _, l := range b.at {
		if l == best {
			return DebugLocation{s, int(bestLine), int(bestCol)}, true
		}
	}
	b.at = append(b.at, best)
	debugOf(best.fn).breaks[best.stmt]++
	d.locations++
	return DebugLocation{s, int(bestLine), int(bestCol)}, true
}

// rearm works out whether a statement has anything to ask.
func (d *debugState) rearm() {
	d.armed = d.handler != nil && (d.locations > 0 || d.step != Continue || d.pauseNext)
}

// debugLoad records code compiled for the debugger as it is first run, and
// tells the handler: the script it is from, every function in it, and the
// breakpoints set by its name.
func (r *Runtime) debugLoad(fn *bytecode.Function) {
	d := r.debug
	s := fn.Script
	if s == nil || !s.HasText() || debugOf(fn) != nil {
		return
	}
	ds := d.byScript[s]
	loaded := ds == nil
	if loaded {
		ds = &DebugScript{ID: len(d.scripts) + 1, Name: s.Name, EvalOrigin: s.EvalOrigin, script: s}
		if s.EvalOrigin != "" {
			ds.Name = ""
		}
		if d.byScript == nil {
			d.byScript = make(map[*bytecode.Script]*DebugScript)
		}
		d.byScript[s] = ds
		d.scripts = append(d.scripts, ds)
	}
	var walk func(*bytecode.Function)
	walk = func(f *bytecode.Function) {
		if f.Debug == nil || f.Debug.State != nil {
			return
		}
		f.Debug.State = unsafe.Pointer(&fnDebug{breaks: make([]int32, len(f.Debug.Statements))})
		ds.fns = append(ds.fns, f)
		for _, c := range f.Constants {
			if c.Kind == bytecode.ConstFunction && c.Fn != nil {
				walk(c.Fn)
			}
		}
	}
	walk(fn)
	if loaded && d.handler != nil {
		d.handler.ScriptParsed(ds)
	}
	if ds.Name == "" {
		return
	}
	ids := make([]int, 0, len(d.breakpoints))
	for id, b := range d.breakpoints {
		if b.url == ds.Name {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	for _, id := range ids {
		if loc, ok := d.resolve(d.breakpoints[id], ds); ok && d.handler != nil {
			d.handler.BreakpointResolved(id, loc)
		}
	}
	d.rearm()
}

// debugInterrupt turns a pause asked for from another goroutine into one at
// the next statement. The interrupt check calls it.
func (r *Runtime) debugInterrupt() {
	if d := r.debug; d.requested.Load() {
		d.requested.Store(false)
		d.pauseNext = true
		d.rearm()
	}
}

// debugStatement is an OpDebugStmt the debugger has something to ask of:
// whether the code stops there.
func (r *Runtime) debugStatement(f *frame, in bytecode.Instr) error {
	d := r.debug
	if d.paused || d.handler == nil {
		return nil
	}
	reason := PauseReason(-1)
	switch {
	case in.B != 0:
		reason = PauseDebuggerStatement
	case d.pauseNext:
		reason = PauseRequested
	case d.step == StepInto,
		d.step == StepOver && r.frameDepth <= d.stepDepth,
		d.step == StepOut && r.frameDepth < d.stepDepth:
		reason = PauseStep
	}
	var hit []int
	if fd := debugOf(f.cl.fn); fd != nil && int(in.A) < len(fd.breaks) && fd.breaks[in.A] > 0 {
		at := bpLocation{f.cl.fn, int(in.A)}
		for id, b := range d.breakpoints {
			if slices.Contains(b.at, at) {
				hit = append(hit, id)
			}
		}
		sort.Ints(hit)
		if reason < 0 {
			reason = PauseBreakpoint
		}
	}
	if reason < 0 {
		return nil
	}
	return r.debugPause(&DebugPause{Reason: reason, Breakpoints: hit, Exception: Undefined})
}

// debugThrow is an exception unwinding from a frame: whether the code stops
// there, the first time it is seen.
func (r *Runtime) debugThrow(err error) {
	d := r.debug
	t, ok := err.(*Thrown)
	if !ok || t.debugSeen || d.paused || d.handler == nil || d.exceptions == PauseOnNoExceptions {
		return
	}
	t.debugSeen = true
	caught := r.exceptionCaught()
	if caught && d.exceptions == PauseOnUncaughtExceptions {
		return
	}
	r.debugPause(&DebugPause{Reason: PauseException, Exception: t.Value, Caught: caught})
}

// exceptionCaught reports whether a catch clause on the stack will catch
// what is being thrown. A promise's rejection handlers are not asked: an
// exception in an async function is caught only by a catch clause on the
// stack as it is thrown.
func (r *Runtime) exceptionCaught() bool {
	for i := r.frameDepth - 1; i >= 0; i-- {
		for _, h := range r.frameAt(i).handlers {
			if !h.isFinally {
				return true
			}
		}
	}
	return false
}

// debugPause stops the code and tells the handler, and takes up the step it
// says to go on with.
func (r *Runtime) debugPause(p *DebugPause) error {
	d := r.debug
	p.live = true
	p.Frames = r.debugFrames(p)
	d.paused = true
	act := d.handler.Paused(p)
	d.paused = false
	p.live = false
	d.pauseNext = false
	d.step, d.stepDepth = act, r.frameDepth
	d.rearm()
	if r.stopped != nil {
		return r.stopped
	}
	return nil
}

// debugFrames are the frames of compiled code on the stack, innermost first.
func (r *Runtime) debugFrames(p *DebugPause) []*DebugFrame {
	var out []*DebugFrame
	for i := r.frameDepth - 1; i >= 0; i-- {
		f := r.frameAt(i)
		if f.cl == nil {
			continue
		}
		pc := f.pc
		if pc > 0 {
			// The frame's pc is past the instruction it is at.
			pc--
		}
		out = append(out, &DebugFrame{r: r, pause: p, f: f, fn: f.cl.fn, pc: pc})
	}
	return out
}

// FunctionName is the frame's function's name, or empty for an anonymous
// function and for the top level of a script or a module.
func (df *DebugFrame) FunctionName() string {
	if df.fn.TopLevel || df.fn.Name == "<anonymous>" {
		return ""
	}
	return df.fn.Name
}

// Location is where in its script the frame is.
func (df *DebugFrame) Location() DebugLocation {
	line, col := df.fn.PositionAt(df.pc)
	var s *DebugScript
	if d := df.r.debug; d != nil {
		s = d.byScript[df.fn.Script]
	}
	return DebugLocation{Script: s, Line: int(line), Column: int(col)}
}

// This is the frame's this, and Undefined in a derived constructor before
// super() has bound it.
func (df *DebugFrame) This() Value {
	if !df.pause.live {
		return Undefined
	}
	v, _ := df.f.thisValue()
	return v
}

// statement is the statement of the frame's function it is in, or nil for
// code not compiled for a debugger.
func (df *DebugFrame) statement() *bytecode.Statement {
	if df.fn.Debug == nil {
		return nil
	}
	st := df.fn.Debug.Statements
	i := sort.Search(len(st), func(i int) bool { return st[i].PC > df.pc })
	if i == 0 {
		return nil
	}
	return &st[i-1]
}

// Scopes are what the frame sees at the statement it is in, innermost
// first: its own bindings, the ones it shares with the functions it is in,
// any with statements' objects, and the top level's.
func (df *DebugFrame) Scopes() []DebugScope {
	if !df.pause.live {
		return nil
	}
	r, f := df.r, df.f
	var local, closure DebugScope
	local.Kind, closure.Kind = ScopeLocal, ScopeClosure
	if st := df.statement(); st != nil {
		for _, b := range df.fn.EvalScopes[st.Scope].Bindings {
			var v Value
			switch {
			case b.FromLocal && int(b.Index) < len(f.locals):
				v = f.locals[b.Index]
			case !b.FromLocal && int(b.Index) < len(f.cl.upvalues):
				v = f.cl.upvalues[b.Index].get()
			default:
				continue
			}
			db := DebugBinding{Name: b.Name, Value: v, Mutable: b.Mutable}
			if v.IsUninitialized() || b.FromLocal && b.TDZ {
				// A let or const the code has not reached, which the
				// compiler may not have marked if nothing could read it.
				// An enclosing function's says only that it may not be.
				db.Value, db.Uninitialized = Undefined, true
			}
			if b.FromLocal {
				local.Bindings = append(local.Bindings, db)
			} else {
				closure.Bindings = append(closure.Bindings, db)
			}
		}
	}
	out := []DebugScope{local}
	for i := len(f.withScopes) - 1; i >= 0; i-- {
		out = append(out, DebugScope{Kind: ScopeWith, Object: f.withScopes[i]})
	}
	if len(closure.Bindings) > 0 {
		out = append(out, closure)
	}
	if r.globalLex != nil && len(r.globalLex.props) != 0 {
		out = append(out, DebugScope{Kind: ScopeScript, Object: r.globalLex})
	}
	return append(out, DebugScope{Kind: ScopeGlobal, Object: r.global})
}

// Evaluate runs code in the frame, as a direct eval at the statement it is
// in would: it sees and may change the frame's bindings, and its this.
// Nothing it runs stops, and an exception it throws is returned.
func (df *DebugFrame) Evaluate(src string) (Value, error) {
	r := df.r
	d := r.debug
	if !df.pause.live || d == nil {
		return Undefined, errNotPaused
	}
	if d.compile == nil {
		return Undefined, errors.New("the debugger has nothing to compile with")
	}
	st := df.statement()
	if st == nil {
		return Undefined, errors.New("the frame's code was not compiled for a debugger")
	}
	fn, err := d.compile(src, EvalRequest{Direct: true, Scope: df.fn.EvalScopes[st.Scope]})
	if err != nil {
		return Undefined, err
	}
	return r.evalIn(df.f, fn)
}
