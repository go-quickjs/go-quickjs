package vm

import (
	"runtime"
	"sync"
	"weak"
)

// Weak references to JavaScript values.
//
// A weak reference is one the collector is allowed to ignore: it names a value
// without keeping it alive. Go gained the two pieces needed for that in 1.24 --
// weak.Pointer, which refers without retaining, and runtime.AddCleanup, which
// runs a function after an object becomes unreachable -- so WeakRef,
// FinalizationRegistry, WeakMap and WeakSet here are the real thing rather than
// strong references wearing the name.
//
// Two properties of the Go machinery shape everything below.
//
// A cleanup runs on its own goroutine, at a moment the collector chooses, and a
// Runtime is not safe for concurrent use. So a cleanup never touches the
// runtime: it appends to a queue behind a mutex, and the runtime drains that
// queue between jobs, where running JavaScript is safe. That is also what the
// specification asks for -- a finalizer is a job, not an interruption.
//
// A weak.Pointer is comparable and two of them are equal exactly when they name
// the same object, which is what lets a WeakMap use one as a hash key without
// the map retaining what it is keyed by.

// weakTarget is a weak reference to a value that may be weakly held: an object,
// or a symbol that is not in the global registry.
//
// The two cases are separate pointers rather than one interface because
// weak.Pointer is generic in what it points at, and an interface holding one
// would be a strong reference to the boxed pointer rather than to the object --
// comparable, but for the wrong reason.
type weakTarget struct {
	obj weak.Pointer[Object]
	sym weak.Pointer[Symbol]
	// isSymbol selects which of the two is in use, since a zero weak.Pointer is
	// indistinguishable from one whose target has been collected.
	isSymbol bool
	// set distinguishes a target that was never assigned from one that has
	// been collected.
	set bool
}

// makeWeak takes a weak reference to a value.
func makeWeak(v Value) weakTarget {
	switch {
	case v.IsObject():
		return weakTarget{obj: weak.Make(v.Object()), set: true}
	case v.IsSymbol():
		return weakTarget{sym: weak.Make(v.Symbol()), isSymbol: true, set: true}
	}
	return weakTarget{}
}

// get returns the value, or reports that it has been collected.
func (w weakTarget) get() (Value, bool) {
	if !w.set {
		return Undefined, false
	}
	if w.isSymbol {
		if s := w.sym.Value(); s != nil {
			return Sym(s), true
		}
		return Undefined, false
	}
	if o := w.obj.Value(); o != nil {
		return Obj(o), true
	}
	return Undefined, false
}

// kind reports which sort of value the target names, which the map index needs
// so that an object and a symbol cannot collide.
func (w weakTarget) kind() Kind {
	if w.isSymbol {
		return KindSymbol
	}
	return KindObject
}

// alive reports whether the target is still there.
func (w weakTarget) alive() bool {
	_, ok := w.get()
	return ok
}

// keepDuringJob holds a value strongly until the next time the job queue
// drains.
//
// Two calls to deref in one turn have to answer the same way: a program that
// checks a reference and then uses it cannot have the value vanish in between.
// The specification says so directly, and it is the only thing that makes a
// WeakRef usable at all.
func (r *Runtime) keepDuringJob(v Value) {
	r.keptAlive = append(r.keptAlive, v)
}

// releaseKeptValues drops the values held for the current job, which is what
// makes a target collectable again once nothing else refers to it.
func (r *Runtime) releaseKeptValues() {
	if len(r.keptAlive) > 0 {
		clear(r.keptAlive)
		r.keptAlive = r.keptAlive[:0]
	}
}

// cleanupQueue collects the finalization callbacks the collector has released,
// so that the runtime can run them between jobs.
//
// It is the boundary between the collector's goroutine and the interpreter's:
// everything on this side of it is guarded by the mutex and holds no runtime
// state at all.
type cleanupQueue struct {
	mu      sync.Mutex
	pending []pendingCleanup
}

// pendingCleanup is one registration whose target has been collected.
type pendingCleanup struct {
	registry *Object
	held     Value
	// cell is the registration, so that draining can drop it and so that an
	// unregister that happened first can cancel it.
	cell *finalizationCell
}

func (q *cleanupQueue) push(c pendingCleanup) {
	q.mu.Lock()
	q.pending = append(q.pending, c)
	q.mu.Unlock()
}

// take removes everything queued so far.
func (q *cleanupQueue) take() []pendingCleanup {
	q.mu.Lock()
	out := q.pending
	q.pending = nil
	q.mu.Unlock()
	return out
}

// runCleanups calls the finalization callbacks whose targets have gone.
//
// It runs between jobs rather than when the collector says so, because the
// collector says so on another goroutine and a Runtime is not safe for
// concurrent use. An error from a callback is reported to the host the same way
// an unhandled rejection is: it belongs to no script, so there is nothing to
// throw it at.
func (r *Runtime) runCleanups() {
	pending := r.cleanups.take()
	for _, p := range pending {
		if p.cell.unregistered {
			continue
		}
		p.cell.done = true
		d, ok := p.registry.data.(*finalizationData)
		if !ok || !isCallable(d.cleanup) {
			continue
		}
		if _, err := r.call(d.cleanup, Undefined, []Value{p.held}); err != nil {
			r.reportCleanupError(err)
		}
	}
	if len(pending) > 0 {
		r.pruneRegistries()
	}
}

// reportCleanupError hands a callback's failure to the host.
func (r *Runtime) reportCleanupError(err error) {
	if r.onCleanupError != nil {
		r.onCleanupError(err)
	}
}

// watchForCollection arranges for a registration's held value to be queued once
// its target is collected.
//
// The cleanup closure must not be able to reach the target, or the target would
// never be collected and the cleanup would never run. It captures the registry,
// the held value and the cell -- none of which the target is reachable from,
// unless the program arranged it, which is its own affair.
func (r *Runtime) watchForCollection(reg *Object, cell *finalizationCell, v Value) {
	// Everything the cleanup needs is captured now rather than read from the
	// cell later: the cleanup runs on another goroutine, and the only field it
	// would be reading is one this one is free to change.
	q, held := r.cleanups, cell.held
	done := func() {
		q.push(pendingCleanup{registry: reg, held: held, cell: cell})
	}
	switch {
	case v.IsObject():
		runtime.AddCleanup(v.Object(), func(struct{}) { done() }, struct{}{})
	case v.IsSymbol():
		runtime.AddCleanup(v.Symbol(), func(struct{}) { done() }, struct{}{})
	}
}
