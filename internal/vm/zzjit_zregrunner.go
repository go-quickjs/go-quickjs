//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"errors"
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// The VM side of register code's exits, as the slot IR's evaluator runs a
// program LowerReg made (docs/register-interpreter.md, Stage 3): what the
// evaluator does not make -- a host operation, a guard that failed, a
// Generic instruction's slow path -- the VM makes, the whole register
// instruction run on the frame's real registers by the interpreter's own
// handlers (regTable), as a host exit of Stage 3a-3's native code is to;
// and where the evaluator leaves for good,
// runReg goes on with the frame from that register pc, as a deoptimization
// does. Only tests run it (TestRegLowerEvaluate, by RegLowerDiff).

// regRunner is ir.Runner over a register frame: f, running c, its window
// at wb, and the program p evaluated for it. handles are the values the
// evaluator's slots name by handle: whatever is no number, boolean, null,
// undefined or the dead zone's marker.
type regRunner struct {
	r       *Runtime
	f       *frame
	c       *regCode
	p       *ir.Program
	wb      int
	handles []Value
	// thrown is the exception the frame threw and no handler of its own
	// took; steps how many register instructions the VM ran whole.
	thrown error
	steps  int
}

// errRegThrown is what Run reports where the frame's own handlers do not
// take the exception an instruction threw (regRunner.thrown).
var errRegThrown = errors.New("vm: register frame threw")

// toIR is v as a slot of the evaluator's.
func (x *regRunner) toIR(v Value) ir.Value {
	switch {
	case v.IsNumber():
		return ir.Float(v.Number())
	case v.IsUndefined():
		return ir.Value{Kind: ir.Undefined}
	case v.IsNull():
		return ir.Value{Kind: ir.Null}
	case v.IsUninitialized():
		return ir.Value{Kind: ir.Uninitialized}
	case v.sameBits(True):
		return ir.Bool(true)
	case v.sameBits(False):
		return ir.Bool(false)
	}
	x.handles = append(x.handles, v)
	return ir.Value{Kind: ir.Opaque, Bits: uint64(len(x.handles) - 1)}
}

// fromIR is slot v as a value of the VM's.
func (x *regRunner) fromIR(v ir.Value) Value {
	switch v.Kind {
	case ir.Number:
		return regNum(math.Float64frombits(v.Bits))
	case ir.Boolean:
		return Bool(v.Bits != 0)
	case ir.Null:
		return Null
	case ir.Uninitialized:
		return uninitialized
	case ir.Opaque, ir.String:
		return x.handles[v.Bits]
	}
	return Undefined
}

// load and store copy the frame's registers into the evaluator's slots,
// and back.
func (x *regRunner) load(slots []ir.Value) {
	for i := range x.p.Regs {
		slots[i] = x.toIR(x.r.stack[x.wb+i])
	}
}

func (x *regRunner) store(slots []ir.Value) {
	for i := range x.p.Regs {
		x.r.stack[x.wb+i] = x.fromIR(slots[i])
	}
}

// Run is ir.Runner's: it runs the register instruction of p's instruction
// pc whole on the frame, as the interpreter does, and goes on at the
// instruction the interpreter would. Inside an expansion it does so only
// where nothing of it has committed, so that the registers are what the
// instruction starts from. A return, and what the frame suspends at, it
// leaves to runReg (regLowerRun).
func (x *regRunner) Run(p *ir.Program, pc int, in *ir.Instruction, slots []ir.Value) (int, bool, error) {
	rpc := int(p.Origin[pc])
	for q := pc - 1; q >= 0 && p.Origin[q] == p.Origin[pc]; q-- {
		if p.CommitsAt(q) {
			return 0, false, nil
		}
	}
	r, f, c := x.r, x.f, x.c
	w := regAt(c.code, rpc)
	switch w.Op() {
	case bytecode.RRet, bytecode.RRetUndef, bytecode.RYield, bytecode.RCallWT, bytecode.RCallMWT:
		return 0, false, nil
	}
	x.store(slots)
	// The instruction's handler, and its slow function where the handler
	// leaves it that, as runReg runs them.
	st := r.regEnter(f, c)
	next := regTable[w.Op()](st.regs, w, rpc, st)
	if next < 0 && next != regDone {
		next = regSlowTable[w.Op()](st.regs, w, ^next, st)
	}
	thrown := st.err
	r.regLeave(st)
	x.steps++
	if next == regDone {
		x.thrown = thrown
		return 0, false, errRegThrown
	}
	x.load(slots)
	at := p.At(uint32(next))
	if at < 0 {
		return 0, false, nil
	}
	return at, true, nil
}

// regLowerEnter is regEnterW, the frame of a call of o made, but run by
// run rather than runReg: the window at base holds the function, this and
// the argc arguments.
func (r *Runtime) regLowerEnter(o *Object, cl *closure, rc *regCode, base, argc int, args []Value, run func(f *frame) (Value, error)) (Value, error) {
	top := base + int(rc.numRegs)
	if top > len(r.stack) || r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	saved := r.stackTop
	if top > saved {
		r.stackTop = top
		if top > r.stackHigh {
			r.stackHigh = top
		}
	}
	var kept []Value
	if rc.args {
		ka := len(r.argStack)
		r.argStack = append(r.argStack, args...)
		kept = r.argStack[ka:len(r.argStack):len(r.argStack)]
	}
	for i := base + 2 + min(argc, int(rc.params)); i < base+2+int(rc.params)+int(rc.inits); i++ {
		r.stack[i] = Undefined
	}
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	f := &r.cur[off]
	f.cl = cl
	f.callee = o
	f.base = base
	f.pc = 0
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil || f.withScopes != nil || f.evalVars != nil || f.native != "" || f.args != nil {
		regClearFrame(f)
	}
	f.newTarget = Undefined
	f.regKind = regFrameEnter
	if rc.args {
		f.args = kept
		p := int(rc.params)
		f.locals = r.stack[base+2 : base+2+p : base+2+p]
	}
	v, err := run(f)
	if err == errRegRestart {
		v, err = r.regRestartRun(f)
	}
	f.regKind = 0
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	r.frameDepth--
	r.stackTop = saved
	if a := f.args; a != nil {
		r.argStack = r.argStack[:len(r.argStack)-len(a)]
	}
	return v, err
}

// RegLowerResult is how a call's evaluation went (RegLowerDiff).
type RegLowerResult struct {
	// Value and Err are what the call answered.
	Value Value
	Err   error
	// Skipped says why the call was made as an ordinary call, not
	// evaluated: no register code, a program LowerReg refused.
	Skipped string
	// Resumed is the register instruction the evaluator left the frame at
	// for good, which runReg went on from; "" where it ran to the end.
	Resumed string
	// Steps is how many register instructions the VM ran whole for it.
	Steps int
}

// RegLowerCall calls fn, a function of r's, with args, as a call from Go
// does, but its frame run by the slot IR's evaluator over the program
// LowerReg makes of its register code, the VM making what the evaluator
// does not (regRunner), and runReg going on where the evaluator leaves for
// good. For tests: the differential test of the register lowering
// (TestRegLowerEvaluate) compares it with an ordinary call.
func (r *Runtime) RegLowerCall(fnv Value, args []Value, budget uint64) RegLowerResult {
	var o *Object
	var fd *funcData
	if fnv.IsObject() {
		o = fnv.Object()
		fd = o.fn()
	}
	var c *regCode
	if fd != nil && fd.closure != nil && !fd.bound && fd.extra == nil && fd.treeCall != nil && fd.closure.realm == r.Realm {
		c = fd.treeCall.reg
	}
	plain := func(why string) RegLowerResult {
		v, err := r.Call(fnv, Undefined, args)
		return RegLowerResult{Value: v, Err: err, Skipped: why}
	}
	if c == nil || !c.wide || c.suspends || fd.arrow || c.leaf != 0 {
		return plain("no register frame")
	}
	cl := fd.closure
	p, err := jitcompile.LowerRegInline(c.rc, cl.fn)
	if err != nil {
		return plain("refused: " + err.Error())
	}
	this := Undefined
	if c.coerceThis {
		this = r.globalThis
	}
	base := r.stackTop
	if base+int(c.numRegs) > len(r.stack) {
		return plain("no room")
	}
	m := min(int(c.params), len(args))
	window := r.stack[base : base+2+m : base+2+m]
	window[0] = Obj(o)
	window[1] = this
	copy(window[2:], args[:m])
	var res RegLowerResult
	res.Value, res.Err = r.regLowerEnter(o, cl, c, base, m, args, func(f *frame) (Value, error) {
		x := &regRunner{r: r, f: f, c: c, p: p, wb: base}
		slots := make([]ir.Value, p.Locals)
		x.load(slots)
		exit, err := p.EvaluateHost(slots, nil, x, 0, budget)
		res.Steps = x.steps
		switch {
		case err == errRegThrown:
			return Undefined, x.thrown
		case err != nil:
			return Undefined, err
		case exit.Kind == ir.Returned:
			return x.fromIR(exit.Value), nil
		}
		// Left for good: the frame as the state has it, runReg from its pc.
		w := regAt(c.code, int(exit.State.PC))
		res.Resumed = w.Op().String()
		if exit.Kind == ir.BudgetExit {
			res.Resumed = "budget"
		}
		x.store(slots)
		return r.runReg(f, c, int(exit.State.PC))
	})
	return res
}
