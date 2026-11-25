package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// The interpreter runs a function's register code (README.md) in a frame,
// which every handler is given.

// frame is a running function's state.
type frame struct {
	x    *exec
	l    []uint64
	r    []Ref
	inst *Instance
	// osr counts down the back edges the call runs in the interpreter
	// before its loops ask for the function's native code (backEdge).
	osr int32

	// rc is the register code the frame runs, if it runs any (runReg);
	// tick counts down its back edges to the next check of the context or
	// of native code, from tickSet (backEdge); sig is how it ended: 0, or
	// sigTail for a tail call.
	rc            *regCode
	tick, tickSet int32
	sig           int32
	// cbase is the exec's caught exceptions below the frame's, where the
	// frame's code has try regions (runRegCatching).
	cbase int
	// farN and farR are the scratch slots of an instruction behind a far
	// prefix (rhFar).
	farN [8]uint64
	farR [4]Ref
	// nctx is the frame's context for native code (nativeRun).
	nctx nativeCtx
}

// exec is the state of a chain of wasm calls entered from the host: the
// stacks frames are taken from, the call depth, and what a trap's stack trace
// is made of.
type exec struct {
	ctx context.Context
	// nstack and rstack are the stacks frames are taken from, in chunks that
	// never move, so that a frame's slices stay valid.
	nchunks [][]uint64
	rchunks [][]Ref
	nc, rc  int // the current chunks
	nsp     int // the next free slot of the current numeric chunk
	rsp     int // the next free slot of the current reference chunk

	depth int
	// calls holds, for each depth, the function running there and the
	// offset of the call it is making, for stack traces.
	calls []callSite
	// poll counts down to the next check of ctx.
	poll int32

	// tail is the function a return_call asks the caller to call next, its
	// arguments in tailArgs.
	tail     *Function
	tailArgs []Val

	// hostResults is scratch for calls of host functions.
	hostResults []Val
	// frames are the frame objects of each depth, reused.
	frames []*frame
	// caught holds the exceptions the legacy catches being run handle, for
	// rethrow.
	caught []*Exception
	// vals is a stack of the arguments of the host functions being called,
	// each call's taken and given back around it (takeVals).
	vals []Val
	// natPools is the address of the exec's table of pools of new objects
	// for the instance whose code the native handlers run (native_gc.go),
	// or 0; natClasses the classes of arrays' blocks by their elements'
	// bytes (arrayClassOf); natAlloc the allocation stub.
	natPools, natClasses, natAlloc uintptr

	// jit is the state of the native code running on the exec.
	jit jitExec
	// pools are the tables of pools of new objects the native handlers
	// take from, one an instance (native_gc.go), given back as the exec's
	// call ends.
	pools []execPools

	// Past the fields native code reads at fixed offsets (regdef):
	// maxDepth is how deep the exec's calls may nest: MaxCallDepth, less
	// the depth of the call whose host function made this one (hostCtx).
	maxDepth int
	// hctx is the context the exec's host functions are given, made for
	// the first of them.
	hctx *hostCtx
	// budget is the budget of the instance whose function the exec's call
	// entered, if any.
	budget *Budget
}

// execPools is an exec's table of pools for an instance.
type execPools struct {
	inst *Instance
	p    *gcPools
}

type callSite struct {
	fn     *Function
	offset uint32
}

// MaxCallDepth is how deep wasm calls may nest before the call stack is
// exhausted.
var MaxCallDepth = 30_000

// reentryDepth is what a call back into wasm from a host function counts
// for beyond the host function's own level (hostCtx). Crossing from wasm
// to Go and back takes 4 to 6 KB of Go stack on 64-bit hosts and an exec
// of its own, many times a wasm call's, so a chain of them counted as two
// levels a round trip reached 15,000 round trips, some 90 MB of stack and
// 100 MB of heap, more than a 32-bit host can give. At 16 levels a round
// trip a chain ends after about 1,875, some 11 MB of stack.
const reentryDepth = 14

const (
	chunkSlots = 1 << 14
	pollEvery  = 1 << 12
)

var execPool = sync.Pool{New: func() any {
	x := new(exec)
	jitTrack(x)
	return x
}}

// takeNum returns n zeroed numeric slots for a frame, and the position to
// release them to.
func (x *exec) takeNum(n int) ([]uint64, int, int) {
	nc, nsp := x.nc, x.nsp
	if x.nc < len(x.nchunks) && nsp+n <= len(x.nchunks[x.nc]) {
		s := x.nchunks[x.nc][nsp : nsp+n : nsp+n]
		clear(s)
		x.nsp = nsp + n
		return s, nc, nsp
	}
	// A new chunk: the next one, or a fresh one large enough.
	next := x.nc + 1
	if x.nc >= len(x.nchunks) {
		next = 0
	}
	if next >= len(x.nchunks) || len(x.nchunks[next]) < n {
		c := make([]uint64, max(chunkSlots, n))
		if next >= len(x.nchunks) {
			x.nchunks = append(x.nchunks, c)
			next = len(x.nchunks) - 1
		} else {
			x.nchunks[next] = c
		}
	}
	x.nc, x.nsp = next, n
	s := x.nchunks[next][:n:n]
	clear(s)
	return s, nc, nsp
}

// takeRef returns n null reference slots for a frame, and the position to
// release them to.
func (x *exec) takeRef(n int) ([]Ref, int, int) {
	if n == 0 {
		return nil, x.rc, x.rsp
	}
	rc, rsp := x.rc, x.rsp
	if x.rc < len(x.rchunks) && rsp+n <= len(x.rchunks[x.rc]) {
		s := x.rchunks[x.rc][rsp : rsp+n : rsp+n]
		clear(s)
		x.rsp = rsp + n
		return s, rc, rsp
	}
	next := x.rc + 1
	if x.rc >= len(x.rchunks) {
		next = 0
	}
	if next >= len(x.rchunks) || len(x.rchunks[next]) < n {
		c := make([]Ref, max(chunkSlots, n))
		if next >= len(x.rchunks) {
			x.rchunks = append(x.rchunks, c)
			next = len(x.rchunks) - 1
		} else {
			x.rchunks[next] = c
		}
	}
	x.rc, x.rsp = next, n
	s := x.rchunks[next][:n:n]
	clear(s)
	return s, rc, rsp
}

// ErrCallDepth wraps the stack-overflow trap.
var errStackOverflow = &Trap{Code: TrapStackOverflow}

// Invoke calls fn with args, returning its results. A trap is returned as a
// *Trap, an uncaught wasm exception as an *Exception; an error a host
// function returned is returned as it is.
func Invoke(ctx context.Context, fn *Function, args []Val) (results []Val, err error) {
	return invoke(ctx, fn, args, nil)
}

// Trace gives a host the wasm frames of a call while it runs, for the host's
// own stack traces.
type Trace struct{ x *exec }

// Frames returns the frames of the call InvokeTraced is running, innermost
// first, each at the call it is making; nil once the call has returned. It
// is called on the goroutine the call runs on -- from a host function the
// call made -- or while the call waits for one.
func (t *Trace) Frames() []Frame {
	if t == nil || t.x == nil {
		return nil
	}
	return t.x.trace()
}

// InvokeTraced is Invoke, with t giving the call's frames while it runs.
func InvokeTraced(ctx context.Context, fn *Function, args []Val, t *Trace) ([]Val, error) {
	return invoke(ctx, fn, args, t)
}

func invoke(ctx context.Context, fn *Function, args []Val, t *Trace) (results []Val, err error) {
	x := execPool.Get().(*exec)
	x.ctx = ctx
	x.maxDepth = MaxCallDepth
	x.budget = nil
	if fn.Inst != nil {
		x.budget = fn.Inst.budget
	}
	turn := x.budget
	if ctx != nil {
		if h, ok := ctx.Value(hostCtxKey{}).(*hostCtx); ok {
			if room := h.room.Load(); room >= 0 {
				x.maxDepth = int(room)
				if h.budget == x.budget {
					turn = nil
				}
			}
		}
	}
	turn.Enter()
	x.poll = pollEvery
	if t != nil {
		t.x = x
	}
	var outer *Budget
	if fn.Inst != nil && fn.Inst.budget != nil && fn.Inst.budget.outer != nil {
		outer = fn.Inst.budget
		outer.enter(x)
	}
	defer func() {
		if r := recover(); r != nil {
			err = recovered(x, r)
			results = nil
		}
		if outer != nil {
			outer.leave(x)
		}
		turn.Exit()
		x.budget = nil
		if t != nil {
			t.x = nil
		}
		x.depth = 0
		if x.hctx != nil {
			x.hctx.room.Store(-1)
			x.hctx = nil
		}
		x.calls = x.calls[:0]
		x.caught = x.caught[:0]
		x.vals = x.vals[:0]
		x.tail = nil
		x.setJITLevel(0)
		x.dropPools()
		x.nc, x.nsp, x.rc, x.rsp = 0, 0, 0, 0
		x.ctx = nil
		execPool.Put(x)
	}()
	results = make([]Val, len(fn.Sig.Results))
	x.callGeneric(fn, args, results)
	return results, nil
}

// hostCtx is the context a host function is given: the call's own, and the
// room the call stack has left below the host function, which a call it
// makes back into wasm with this context, or with one made from it, starts
// with. MaxCallDepth then bounds a chain of calls that goes back and forth
// between wasm and the host as it bounds wasm's own recursion, where each
// call starting at depth 0 would let the chain run until Go's stack ran out.
//
// The room is the innermost running host function's, set as it is called
// and set back as it returns. An exception thrown through a host function
// leaves it the smaller room of that deeper call until the next host call
// sets it: a bound never looser than the depth's. It is -1, no bound of its
// own, once the call that made the context has returned. It is atomic since
// a host function may hand its context to other goroutines.
type hostCtx struct {
	context.Context
	room atomic.Int64
	// budget is the exec's: a call back into wasm of the same budget is
	// part of the call's turn (Budget.Enter).
	budget *Budget
}

type hostCtxKey struct{}

func (c *hostCtx) Value(key any) any {
	if key == (hostCtxKey{}) {
		return c
	}
	return c.Context.Value(key)
}

// hostContext returns the context the exec's host functions are given.
func (x *exec) hostContext() *hostCtx {
	if x.hctx == nil {
		ctx := x.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		x.hctx = &hostCtx{Context: ctx, budget: x.budget}
	}
	return x.hctx
}

// hostError carries a host function's error out through wasm frames.
type hostError struct{ err error }

// recovered turns what a panic out of wasm code carried into an error.
func recovered(x *exec, r any) error {
	switch v := r.(type) {
	case *Trap:
		if len(v.Frames) == 0 {
			v.Frames = x.trace()
		}
		return v
	case trapSite:
		if v.at != 0 && len(x.calls) > 0 {
			v.at = uint64(x.regOffset(v.at))
		}
		return v.trap(x.trace())
	case *Exception:
		return v
	case *delegation:
		return v.exn
	case hostError:
		return v.err
	case error:
		var re interface{ RuntimeError() }
		if errors.As(v, &re) {
			// A Go runtime error is a bug of the engine's; let it through.
			panic(v)
		}
		return v
	}
	panic(r)
}

// trace returns the wasm call stack, innermost first.
func (x *exec) trace() []Frame {
	fr := make([]Frame, 0, len(x.calls))
	for i := len(x.calls) - 1; i >= 0; i-- {
		c := x.calls[i]
		if c.fn == nil {
			continue
		}
		fr = append(fr, Frame{Module: c.fn.Inst, Func: c.fn.Index, Offset: c.offset})
	}
	return fr
}

// pollContext is called every pollEvery back edges and calls; it ends
// execution if the context is done.
func (x *exec) pollContext() {
	x.poll = pollEvery
	if x.ctx != nil {
		if err := x.ctx.Err(); err != nil {
			panic(hostError{err})
		}
	}
}

// takeVals returns n zeroed values on the exec's argument stack, and the
// height to give them back at. A call made while they are in use takes its
// own above them; growing the stack moves it, but the values a caller holds
// stay where it took them.
func (x *exec) takeVals(n int) ([]Val, int) {
	top := len(x.vals)
	need := top + n
	if need > cap(x.vals) {
		grown := make([]Val, need, 2*need)
		copy(grown, x.vals)
		x.vals = grown
	}
	x.vals = x.vals[:need]
	w := x.vals[top:need:need]
	clear(w)
	return w, top
}
