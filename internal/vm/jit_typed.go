//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Typed code in a runtime with the JIT (internal/jit/README.md, "Typed
// code"): a validated asm.js module's functions compiled natively from
// their typed code at its link, as V8 compiled validated asm.js at its
// instantiation, rather than through warm-up and feedback.
//
// The JIT compiles a closure's register code, keyed by its function; a
// function of a module has two codes, its ordinary code and its typed
// code. So a linked closure has a twin (asmTwin): a closure of the same
// upvalues, realm and call caches, whose function is a copy of the
// closure's whose register code is the typed code, and whose template is
// its own -- its native code, its counts, its key in the JIT's cache.
// Typed call sites enter the twin (regCallKind), the typed interpreter
// runs its frames where it has no native code, and the JIT compiles it as
// it compiles any register code, its lowering typed code's
// (compile.LowerTyped). A frame of the twin is the function's to anything
// that reads it -- its function's name, script, positions, which its own
// register code, the typed code, gives.

// The VM's heap of typed code begins with an abi.HeapCell, which native
// code reads (ssa.HeapFeedback).
var _ [unsafe.Offsetof(asmHeap{}.base) - uintptr(abi.OffHeapBase)]struct{}
var _ [unsafe.Offsetof(asmHeap{}.len) - uintptr(abi.OffHeapLen)]struct{}
var _ [uintptr(abi.OffHeapBase) - unsafe.Offsetof(asmHeap{}.base)]struct{}
var _ [uintptr(abi.OffHeapLen) - unsafe.Offsetof(asmHeap{}.len)]struct{}

// asmTwin is the twin of linked closure cl, whose typed code is t.
func asmTwin(cl *closure, t *regCode) *closure {
	fn := new(bytecode.Function)
	*fn = *cl.fn
	fn.Reg = bytecode.RegReady(t.rc)
	fn.VMCode = unsafe.Pointer(t)
	cc := &closureCode{env: cl.env, names: cl.names, consts: cl.consts, ic: cl.ic, reg: t, regCalls: cl.regCalls, regEpochs: cl.regEpochs}
	return &closure{fn: fn, upvalues: cl.upvalues, closureCode: cc, realm: cl.realm}
}

// jitTypedLinked gives cl, just linked with its typed code t, its twin,
// keeps its heap current (asmHeapKeep), links the module's other
// functions cl calls, and compiles the twin's code natively: at the
// link, before it runs, in the background where the runtime compiles
// there, the typed interpreter running it until its code installs. A
// compile that the JIT refuses or has no room for leaves the function to
// the typed interpreter.
func (r *Runtime) jitTypedLinked(cl *closure, t *regCode) {
	x := t.typedExt()
	if x.twin != nil {
		return
	}
	tw := asmTwin(cl, t)
	x.twin = tw
	if h := x.heap; h != nil {
		r.asmHeapKeep(h)
	}
	link := func(v Value) {
		if !v.IsObject() {
			return
		}
		fd := v.Object().fn()
		if fd == nil || fd.closure == nil || fd.bound {
			return
		}
		if sc := fd.closure; !sc.asmTried {
			if sc.reg == nil {
				r.regOf(sc)
			}
			if sc.reg != nil {
				r.regTypedOf(sc)
			}
		}
	}
	for k, up := range t.rc.Asm {
		if k >= len(cl.upvalues) {
			continue
		}
		switch up.Kind {
		case bytecode.AsmUpFunc:
			link(cl.upvalues[k].get())
		case bytecode.AsmUpTable:
			// A function table's functions, each called through it
			// natively from the first compile (jitTypedTableTargets).
			if v := cl.upvalues[k].get(); v.IsObject() {
				for _, e := range v.object().elemsOf() {
					if !isHole(e) {
						link(e)
					}
				}
			}
		}
	}
	s := r.jitState()
	if !s.unavailable {
		s.regLearnedOf(r.jitRegKeyOf(tw)).typed = true
	}
	e := r.jitRegFor(tw, t)
	if e == nil && !s.unavailable && s.jobsFull() && s.regPendingJobs[r.jitRegKeyOf(tw)] == nil {
		// The background has no room for it (jitJobsMax), and nothing
		// asks a typed function to be compiled again: compiled on the
		// spot. A module's functions, linked at once, had filled it at
		// its fifth, and the rest ran in the typed interpreter for good --
		// zlib 2.6 times as long in the background as on the spot.
		s.onSpot = true
		e = r.jitRegFor(tw, t)
		s.onSpot = false
	}
	if e != nil && e.code != nil && !e.invalid {
		// Its calls enter it from now on, as tier-up's (setJITReg); one
		// compiled in the background installs itself so.
		tw.closureCode.setJITReg(e)
	}
}

// asmHeapKeep keeps h current as its buffer's bytes move or go
// (bufferChanged): native code reads its cell with no check of the epoch.
// The runtime holds h weakly: what native code reads h for is its typed
// code's, which holds it (typedExt's heap), as the code compiled from it
// holds the twin. Held strongly, every module a runtime had linked kept
// its heap for the runtime's life -- Octane's zlib its 128 MB buffer,
// long after the program had let it go and its code had been flushed.
func (r *Runtime) asmHeapKeep(h *asmHeap) {
	h.at(r)
	if len(r.asmHeaps) == cap(r.asmHeaps) {
		// The heaps collected let go of before the list grows.
		r.asmHeapsAt()
	}
	r.asmHeaps = append(r.asmHeaps, weak.Make(h))
}

// Heap is typed code's heap (ssa.HeapFeedback): its abi.HeapCell's
// address, or 0 for none.
func (f *jitRegFeedback) Heap() uintptr {
	if f.rc == nil || !f.rc.Typed {
		return 0
	}
	c := f.fb.cl.reg
	if c == nil || !c.typed || c.typedExt().heap == nil {
		return 0
	}
	return uintptr(unsafe.Pointer(c.typedExt().heap))
}

// RawMath is the computation of typed code's Math function at program pc
// (ssa.RawMathFeedback): the intrinsic of a call of that function of the
// realm's Math.
func (f *jitRegFeedback) RawMath(pc int) (ssa.Intrinsic, bool) {
	return jitRawMathOf(f.rc, f.p, pc)
}

// jitRawMathOf is RawMath's answer at pc of p, rc's program: of the code
// alone, which a snapshot's worker derives as the feedback does.
func jitRawMathOf(rc *bytecode.RegCode, p *ir.Program, pc int) (ssa.Intrinsic, bool) {
	if rc == nil || !rc.Typed || pc < 0 || pc >= len(p.Code) {
		return ssa.Intrinsic{}, false
	}
	in := &p.Code[pc]
	if in.Op != ir.Raw || in.Raw != ir.RawFMath || int(in.Key) >= len(bytecode.TMathNames) {
		return ssa.Intrinsic{}, false
	}
	switch name := bytecode.TMathNames[in.Key]; name {
	case "pow":
		return ssa.Intrinsic{Op: ssa.OpPowF64}, true
	case "atan2":
		return ssa.Intrinsic{Op: ssa.OpMathCall, Go: ssa.GoAtan2}, true
	default:
		op := asmMathOps[name]
		if op == 0 || int(op) >= len(unaryMath) {
			return ssa.Intrinsic{}, false
		}
		return ssa.Intrinsic{Op: ssa.OpMathCall, Go: ssa.GoMath, Fn: int(op)}, true
	}
}

// regWholeTyped is regWhole of typed code c's instruction w at rpc, by
// the typed table.
func (r *Runtime) regWholeTyped(f *frame, c *regCode, wb, rpc int, w bytecode.RInstr) (next int, ok bool, err error) {
	switch w.Op() {
	case bytecode.RRet, bytecode.RRetUndef:
		return 0, false, nil
	}
	st := r.regEnterTyped(f, c)
	if st.wb != wb {
		panic("vm: register frame's window moved")
	}
	next = regTypedTable[w.Op()](st.regs, w, rpc, st)
	if next < 0 && next != regDone {
		next = regTypedSlowTable[w.Op()](st.regs, w, ^next, st)
	}
	err = st.err
	r.regLeaveTyped(st)
	if next < 0 {
		if err == nil {
			panic("vm: typed instruction ended its frame without an error")
		}
		return 0, true, err
	}
	return next, true, nil
}

// jitTypedLower is typed code rc's lowering, of whatever kind is asked:
// its native callers' and Go's entries are one code.
func jitTypedLower(rc *bytecode.RegCode, fn *bytecode.Function) (*ir.Program, error) {
	p, err := jitcompile.LowerTyped(rc, fn)
	if err == nil {
		p.Checked = true
	}
	return p, err
}

// regTableTargets gives the call w at pc, of typed code, whose function is
// a function table's element (ttab_get, the instruction that last wrote
// the call's function register before it), the table's functions as its
// targets: called natively from the first compile, each checked by its
// object, as targets a call learned are. The table's functions are fixed
// by validation and linked with the function (jitTypedLinked). Learned a
// call of Go at a time, each a compile again, the calls through a table
// of four made by the micro-benchmarks' asm_table were Go's for most of a
// run: 45 ns a call, against 9 of the ordinary code's native calls.
func (f *jitRegFeedback) regTableTargets(t *jitRegTargets, pc int, w bytecode.RInstr, rpc int32, argc int, method bool) {
	rc, l := f.rc, f.fb.e
	if rc == nil || !rc.Typed || method || l == nil {
		return
	}
	fa := w.A()
	for q := int(rpc) - 1; q >= 0; q-- {
		x := rc.Code[q]
		if x.A() != fa {
			continue
		}
		c := int(x.C())
		if x.Op() != bytecode.TTabGet || c >= len(rc.Asm) || c >= len(f.fb.cl.upvalues) {
			return
		}
		tab := f.fb.cl.upvalues[c].get()
		if !tab.IsObject() {
			return
		}
		for _, e := range tab.object().elemsOf() {
			if isHole(e) || !e.IsObject() {
				continue
			}
			o := e.Object()
			fd := o.fn()
			if fd == nil || fd.closure == nil || fd.bound {
				continue
			}
			cl, crc := jitTypedCallee(fd.closure)
			if crc == nil {
				continue
			}
			n := t.n
			f.regAddTarget(t, pc, argc, method, o, cl, crc, nil, nil, false, false)
			if t.n > n && !jitRegCallKnown(l, rpc, o) {
				l.nativeCalls = append(l.nativeCalls, jitInline{pc: rpc, cl: cl, obj: o})
			}
		}
		return
	}
}

// regTableEntries is, for the call w at pc of typed code whose function is
// a function table's element (ttab_get) of more functions than a call
// checks one by one (jitCallTargets), the table's functions as native
// code calls them by the call's index (ssa.CallSite's Table): the address
// of an abi.TableEntry for each, kept by the compile, and the table's
// length; 0 for any other call. An entry of a function native callers do
// not call -- an import's, one of no typed code -- has no link, and Go
// makes the call. Checked one by one, a table's first four functions
// were native calls and the rest Go's.
func (f *jitRegFeedback) regTableEntries(pc int, w bytecode.RInstr, argc int) (uintptr, int) {
	rc := f.rc
	if rc == nil || !rc.Typed || w.Op() != bytecode.RCallW {
		return 0, 0
	}
	rpc := int(f.p.Origin[pc])
	fa := w.A()
	for q := rpc - 1; q >= 0; q-- {
		x := rc.Code[q]
		if x.A() != fa {
			continue
		}
		c := int(x.C())
		if x.Op() != bytecode.TTabGet || c >= len(rc.Asm) || c >= len(f.fb.cl.upvalues) {
			return 0, 0
		}
		tab := f.fb.cl.upvalues[c].get()
		if !tab.IsObject() {
			return 0, 0
		}
		elems := tab.object().elemsOf()
		if len(elems) <= jitCallTargets || len(elems) != rc.Asm[c].Len {
			return 0, 0
		}
		r := f.fb.r
		k := f.fb.keep()
		entries := make([]abi.TableEntry, len(elems))
		for i, e := range elems {
			if isHole(e) || !e.IsObject() {
				continue
			}
			o := e.Object()
			entries[i].Callee = uintptr(unsafe.Pointer(o))
			k.holders = append(k.holders, o)
			fd := o.fn()
			if fd == nil || fd.closure == nil || fd.bound {
				continue
			}
			cl, crc := jitTypedCallee(fd.closure)
			if crc == nil || !r.jitRegCallable(o, cl, crc, argc) {
				continue
			}
			l := r.jitState().regLinkOf(cl, crc)
			k.links = append(k.links, l)
			k.inlined = append(k.inlined, cl)
			entries[i].Closure = uintptr(unsafe.Pointer(cl))
			entries[i].Upvalues = uintptr(unsafe.Pointer(unsafe.SliceData(cl.upvalues)))
			entries[i].Link = uintptr(unsafe.Pointer(l))
		}
		k.tables = append(k.tables, entries)
		return uintptr(unsafe.Pointer(&entries[0])), len(entries)
	}
	return 0, 0
}

// jitTypedCallee is the closure and code a native call of typed code
// enters for cl, a function the call's binding holds: its twin and typed
// code where it is linked to them, else nil -- what is no function of the
// module, an import, Go calls.
func jitTypedCallee(cl *closure) (*closure, *regCode) {
	t := cl.regTyped
	if t == nil || t.typedExt().asmCl != nil && t.typedExt().asmCl != cl || t.typedExt().twin == nil {
		return nil, nil
	}
	return t.typedExt().twin, t
}

// jitTypedCross reports whether e, a call site's cache marked
// regCallCross, holds a twin and its typed code (asmTwin), which native
// code calls natively whatever its caller's kind: the interpreter enters
// it in a code entry of its own, since its context holds one table, but
// native code reads no table and its heap from the callee's own cell
// (ssa.HeapFeedback), and an exit of the callee's goes on in the typed
// interpreter from the frame Go makes of its level (runRegFor). Left to
// Go, each call of a module's function from ordinary native code had
// been a call of Go and a code entry: asm_fib's fib(10), 177 calls, paid
// 185 ns for it.
func jitTypedCross(e *regCallCache) bool {
	if e.rc == nil || !e.rc.typed || e.cl == nil || e.cl.reg != e.rc || e.callee == nil {
		return false
	}
	fd := e.callee.fn()
	return fd != nil && fd.closure != nil && jitTypedTwinOf(fd.closure, e.cl)
}

// jitTypedTwinOf reports whether tw is the twin of cl (asmTwin), whose
// function object is cl's: a native call of typed code checks cl's object
// and enters tw's code.
func jitTypedTwinOf(cl, tw *closure) bool {
	t := cl.regTyped
	return t != nil && t.typedExt().twin == tw
}
