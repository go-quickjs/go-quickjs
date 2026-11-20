//go:build amd64 || arm64

package vm

import (
	"math"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The native handlers' code entry of typed code (reg_typed.go): a
// context whose table is the typed dispatcher's (nativeTyped), and whose
// exits run the typed table's Go handlers. nativeLoop and nativeCallMiss
// name the ordinary table, and making them take it would change the
// ordinary entry's code: their copies here name the typed one.

// nativeTypedExitTable and nativeTypedSlowTable are a typed context's
// Go table and slow table (goTable, slowTable): the typed table's handler
// and slow function of the instruction, after which the heap is read
// again (nativeTypedExit), since what Go ran may have detached its buffer
// -- a call out, valueOf, an interrupt's hook -- and the code goes on
// without coming back to nativeLoopTyped.
var nativeTypedExitTable, nativeTypedSlowTable [256]regHandler

func init() {
	for op := range nativeTypedExitTable {
		nativeTypedExitTable[op], nativeTypedSlowTable[op] = nativeTypedExit, nativeTypedSlow
	}
	nativeCallMissTypedFn = nativeCallMissTyped
}

// nativeTypedExit runs the typed table's handler of w, an instruction a
// typed context's code left, and reads the entry's heap again.
func nativeTypedExit(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	pc = regTypedTable[w.Op()](regs, w, pc, st)
	if ctx := st.r.typedNative; ctx != nil && ctx.typedCtx().heap != nil {
		nativeHeapAt(st.r, ctx)
	}
	return pc
}

// nativeTypedSlow is nativeTypedExit's of the slow function.
func nativeTypedSlow(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	pc = regTypedSlowTable[w.Op()](regs, w, pc, st)
	if ctx := st.r.typedNative; ctx != nil && ctx.typedCtx().heap != nil {
		nativeHeapAt(st.r, ctx)
	}
	return pc
}

// nativeHeapAt gives ctx its heap's base and length as of r's typedEpoch.
func nativeHeapAt(r *Runtime, ctx *nativeCtx) {
	t := ctx.typedCtx()
	h := t.heap
	h.at(r)
	t.heapBase, t.heapLen = uintptr(h.base), h.len
}

// nativeRunTyped is nativeRun for typed code: a state of the typed list,
// whose context is the typed table's from its first run on.
func (r *Runtime) nativeRunTyped(f *frame, c *regCode, start int) (Value, error) {
	st := r.regEnterTyped(f, c)
	ctx := st.native
	if ctx == nil {
		ctx = r.newNativeCtxTyped(st)
	}
	ctx.epoch = uintptr(unsafe.Pointer(&r.shapes.globalEpoch))
	// The entry's heap, its code's: every typed frame the code enters
	// itself has it or none (regTypedCross).
	t := ctx.typedCtx()
	t.heap = c.typedExt().heap
	t.heapBase, t.heapLen = uintptr(unsafe.Pointer(&asmNoHeap)), 0
	// The innermost typed entry's context, whose heap Go makes current
	// after each exit (nativeTypedExit): the outer one's again after it.
	prev := r.typedNative
	r.typedNative = ctx
	// The frames the context enters past f's are typed too (typedFrame).
	r.typedRuns[len(r.typedRuns)-1].ctx = ctx
	v, err := r.nativeLoopTyped(st, ctx, f, c, start)
	r.typedNative = prev
	nativeReset(ctx)
	t.heap = nil
	r.regLeaveTyped(st)
	return v, err
}

// newNativeCtxTyped is newNativeCtx's context for a typed state: the
// typed dispatcher's table, and the typed table's Go handlers and slow
// functions for its exits.
func (r *Runtime) newNativeCtxTyped(st *regState) *nativeCtx {
	t := new(nativeTypedCtx)
	ctx := &t.nativeCtx
	r.initNativeCtx(ctx, st)
	ctx.disp = nativeTyped
	ctx.table = nativeTyped.addr
	ctx.goTable = uintptr(unsafe.Pointer(&nativeTypedExitTable[0]))
	ctx.slowTable = uintptr(unsafe.Pointer(&nativeTypedSlowTable[0]))
	ctx.callMissFn = *(*uintptr)(unsafe.Pointer(&nativeCallMissTypedFn))
	return ctx
}

// nativeCallMissTypedFn is nativeCallMissTyped's func value, set by init.
var nativeCallMissTypedFn func(*[1 << 16]Value, bytecode.RInstr, int, *regState) int

// nativeLoopTyped is nativeLoop over the typed table.
func (r *Runtime) nativeLoopTyped(st *regState, ctx *nativeCtx, f *frame, c *regCode, start int) (Value, error) {
	ctx.depth, ctx.filled = 0, 0
	nativeSetTop(ctx, uintptr(unsafe.Pointer(st.regs)), f, c)
	ctx.ip = uintptr(c.code) + uintptr(start)*8
	ctx.top, ctx.topOK = st, 1
	// restarted says a tail call has given the frame to its callee in
	// place and the code goes on with the callee's (nativeRestart).
	restarted := false
	for {
		if ctx.typedCtx().heap != nil {
			// The heap as of now, which Go may have detached.
			nativeHeapAt(r, ctx)
		}
		ctx.budget = int64(r.backEdges)
		if n := r.nativeCounts; n != nil {
			atomic.AddInt64(&n.enters, 1)
		}
		nativeEnter(nativeBase, unsafe.Pointer(ctx))
		r.backEdges = int(ctx.budget)
		if d := ctx.retDone; d != 0 {
			// The base frame returned, or yielded (2): its value in the
			// scratch.
			ctx.retDone = 0
			v := Value{num: math.Float64frombits(ctx.scratch[sV0]), ref: *(*unsafe.Pointer)(unsafe.Pointer(&ctx.scratch[sV1]))}
			ctx.scratch[sV1] = 0
			if d == 2 {
				// rhYield's suspension, which no handler takes.
				pc := int(ctx.pc)
				st.kept.pc = pc + 1
				st.err = r.regYield(st.f, v, pc+1)
				v = Undefined
			}
			return r.nativeFinish(st, ctx, v, restarted)
		}
		s := ctx.top
		if ctx.topOK == 0 {
			s = r.nativeTop(st, ctx)
		}
		if ctx.ended != 0 {
			// A Go handler the code called ended the top frame (regDone).
			ctx.ended = 0
			if v, done := r.nativeEnd(s, ctx, regDone, regAt(s.c.code, int(ctx.pc))); done {
				if st.err == errRegRestart && r.nativeRestart(st, ctx) {
					restarted = true
					continue
				}
				return r.nativeFinish(st, ctx, v, restarted)
			}
			continue
		}
		// The instruction the code left, run by its Go handler in the top
		// frame: only a frame's end goes further.
		pc, w := int(ctx.pc), bytecode.RInstr(ctx.w)
		op := w.Op()
		miss := ctx.miss != 0
		ctx.miss = 0
		if n := r.nativeCounts; n != nil {
			atomic.AddInt64(&n.exits[op], 1)
		}
		if ctx.depth == ctx.cap && ctx.cap < nativeMaxDepth && (op == bytecode.RCallW || op == bytecode.RCallMW || op == bytecode.RCallWT || op == bytecode.RCallMWT || op == bytecode.RIterNext) {
			// A call, or a generator's step, the frame stack has no room
			// for: the stack grown, the call tried again, nothing of it
			// done.
			ctx.grow()
			continue
		}
		if miss {
			pc = nativeCallMissTyped(s.regs, w, pc, s)
		} else {
			pc = regTypedTable[op](s.regs, w, pc, s)
		}
		// The instructions after it that have no native handler are run
		// here too, rather than each entering the code to exit at once.
		code := s.c.code
		for pc >= 0 {
			w = regAt(code, pc)
			op = w.Op()
			if ctx.disp.has[op] {
				break
			}
			if n := r.nativeCounts; n != nil {
				atomic.AddInt64(&n.exits[op], 1)
			}
			pc = regTypedTable[op](s.regs, w, pc, s)
		}
		if pc >= 0 {
			ctx.ip = uintptr(code) + uintptr(pc)*8
			continue
		}
		if v, done := r.nativeEnd(s, ctx, pc, w); done {
			if st.err == errRegRestart && r.nativeRestart(st, ctx) {
				restarted = true
				continue
			}
			return r.nativeFinish(st, ctx, v, restarted)
		}
	}
}

// nativeCallMissTyped is nativeCallMiss at a site of typed code, which
// enters a typed callee alone.
func nativeCallMissTyped(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, f, c := st.r, st.f, st.c
	win := regWindow(r.stack, st.wb+int(w.A()))
	calls := f.cl.regCalls
	if uint(w.C()) >= uint(len(calls)) || !regIsObject(&win[0]) {
		return regTypedTable[w.Op()](regs, w, pc, st)
	}
	e, o := &calls[w.C()], (*Object)(win[0].ref)
	method := w.Op().MethodCall()
	if e.callee == o || e.kind == regCallGen || e.kind == regCallMarked || method && pc < len(c.rc.Marks) && c.rc.Marks[pc] != 0 {
		return regTypedTable[w.Op()](regs, w, pc, st)
	}
	if fd := o.fn(); fd != nil && fd.closure != nil && !fd.bound {
		// Only a callee the handler enters with a frame: a leaf rhCallW's
		// miss answers without one (pureCall, leafCall), and a frame that
		// keeps its arguments is entered by regEnterW, each cheaper than
		// the exit the call would come back to. One with native code the
		// handler has Go enter (nativeJITCall), as rhCallW's hit does.
		if cl, rc, kind, this, enter := r.regCallKind(fd, method, c); enter && kind != 0 && rc.leaf == regLeafNone && !rc.args && !regTypedCross(rc, c) && (kind != regCallObjThis || win[1].IsObject()) {
			if !f.cl.regCallsHeld {
				r.regCallsHold(f.cl.closureCode)
			}
			regCacheCallee(e, o, cl, rc, this, kind, c)
			return pc
		}
	}
	return regTypedTable[w.Op()](regs, w, pc, st)
}
