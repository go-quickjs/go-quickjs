package engine

import (
	"context"
	"errors"
	"sync"
)

// The interpreter runs a function as a tree of Go closures, compiled from its
// body (see compile.go). A frame is what every node is given.

// frame is a running function's state.
type frame struct {
	x    *exec
	l    []uint64
	r    []Ref
	inst *Instance
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
}

type callSite struct {
	fn     *Function
	offset uint32
}

// MaxCallDepth is how deep wasm calls may nest before the call stack is
// exhausted.
var MaxCallDepth = 30_000

const (
	chunkSlots = 1 << 14
	pollEvery  = 1 << 12
)

var execPool = sync.Pool{New: func() any { return new(exec) }}

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
	x := execPool.Get().(*exec)
	x.ctx = ctx
	x.poll = pollEvery
	defer func() {
		if r := recover(); r != nil {
			err = recovered(x, r)
			results = nil
		}
		x.depth = 0
		x.calls = x.calls[:0]
		x.caught = x.caught[:0]
		x.tail = nil
		x.nc, x.nsp, x.rc, x.rsp = 0, 0, 0, 0
		x.ctx = nil
		execPool.Put(x)
	}()
	results = make([]Val, len(fn.Sig.Results))
	x.callGeneric(fn, args, results)
	return results, nil
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
