package vm

import (
	"math"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter (docs/register-interpreter.md), stage 1: a
// function the register form covers runs as its register code, every other
// as before. It is on where bytecode.RegDefault says, in a runtime without
// the JIT (Runtime.RegTier).
//
// A frame's registers are a window of the runtime's value stack laid out as
// the stack form's frame is -- the locals, then the temporaries -- so that a
// closure captures a local, a stack trace reads a pc and the frame is pushed
// and popped as ever. The loop indexes the window with 16-bit register
// numbers through a pointer to an array of 65,536 values, which needs no
// bounds check: the register backend's verifier (regcomp.Verify)
// proves every register an instruction names is below the frame's count,
// and a frame is given that many slots before it runs. The code is read the
// same way, every jump proven to land in it.
//
// The files sort after every other of the package's, so that the functions
// they add come after every function there was: the interpreter's hot
// functions keep their addresses, and only the closures move.

// regTier turns the register interpreter on for functions not yet run.
var regTier atomic.Bool

// regsBuilt counts the functions given register code.
var regsBuilt atomic.Int64

// regCode is a function's register code as the VM runs it.
type regCode struct {
	// code is the first instruction, read with no bounds check.
	code unsafe.Pointer
	src  []uint32
	// siteName is each property site's name, by its cache's number.
	siteName []uint32
	// numRegs is how many registers a frame uses; locals and params are
	// the function's counts.
	numRegs, locals, params int32
	coerceThis              bool
	// wide marks code of the register form's own frame layout: register 0
	// the function, 1 its this, the parameters from 2, and inits bindings
	// after them that a call makes undefined. Only call_w enters it.
	wide bool
	// keep is the first parameter whose argument f.arguments reads as
	// the call passed it, which the call keeps if the frame may overwrite
	// it (regKeepWindow): params, but in a sloppy function whose
	// parameter list is not plain (0) or one of whose parameters a
	// closure captures (the first such). It fills the padding after wide.
	keep  uint16
	inits int32
	// ncap is how many of the function's stack slots a closure captures,
	// the cells a closure it makes is given (regClosure).
	ncap int32
	// newTarget says the function reads new.target, which its frame keeps.
	newTarget bool
	// args says a frame keeps its arguments as passed, for RArguments.
	args bool
	// suspends marks a generator's or an async function's code, which
	// only a generator's resumption runs (zzreg_zgen.go).
	suspends bool
	// cellThis marks a derived constructor whose this binding is the
	// runtime's cell for its frame's depth (thisCells): its body makes no
	// closure, no direct eval and no debugger's stop, the things that can
	// keep a binding past its frame, as planTreeNew finds for a tree.
	cellThis bool
	// leaf is what a call may answer without a frame: a pure body that
	// calls nothing, or one that only reads or writes its this's
	// properties; read here rather than in the function, a line away.
	leaf uint8
	// leafArg is the parameter a regLeafArg body returns, and leafVal the
	// value a regLeafConst body does.
	leafArg int32
	leafVal Value
	// leafOps is a regLeafCode body's code as regLeafRun runs it.
	leafOps []regLeafOp
	// switches is each switch instruction's table, made from the code's.
	switches []*regSwitchTable
	rc       *bytecode.RegCode
	fn       *bytecode.Function
	// reuse is the sites of the code's calls of exec marked RMarkReuse
	// whose arrays may be made again, by call site (regReuseSites).
	reuse []*execReuseSite
	// genFrom is the first register a generator's or an async function's
	// vals keep (newRegGenerator): 2 where nothing captures the callee's
	// register or this's and nothing reads a local by its register
	// (LocalReg), which a resumption makes again from the generator; 0
	// otherwise.
	genFrom int32
}

// runReg runs frame f as register code c, from instruction start, and
// returns what it returns. Only a generator's resumption, and a frame that
// native code leaves, start past the first instruction.
//
// The loop is the same few instructions for every operation: the
// instruction read, its handler called through regTable, and the pc it
// returns gone on at (zzreg_table.go). A handler's ^pc runs the
// operation's slow function from regSlowTable; regDone leaves: with the
// state's error, or with what the return it ended at returns, which the
// loop reads itself, so that ret's handler stores nothing. Nothing of the
// loop is kept across a handler but the registers, the code and the
// state, which the call saves.
func (r *Runtime) runReg(f *frame, c *regCode, start int) (Value, error) {
	if r.stopped != nil {
		return Undefined, r.stopped
	}
	if regNativeOn {
		// amd64's native handlers, which leave to this table's handlers
		// what they do not do (zzzyn_native_amd64.go).
		return r.nativeRun(f, c, start)
	}
	st := r.regEnter(f, c)
	regs, code, pc := st.regs, c.code, start
	var w bytecode.RInstr
	for {
		w = regAt(code, pc)
		if pc = regTable[w.Op()](regs, w, pc, st); pc < 0 {
			if pc == regDone {
				break
			}
			pc = ^pc
			w = regAt(code, pc)
			if pc = regSlowTable[w.Op()](regs, w, pc, st); pc < 0 {
				break
			}
		}
	}
	err := st.err
	r.regLeave(st)
	if err != nil {
		return Undefined, err
	}
	if w.Op() == bytecode.RRet {
		return regs[w.A()], nil
	}
	return Undefined, nil
}

// regDone is the pc a handler returns once its frame has returned, by ret
// or ret_undef, or has thrown what none of its handlers catches, the
// state's err.
const regDone = math.MinInt

// regState is what a register frame's handlers are given besides its
// registers: the frame, its code, and what the frame ends with. A frame
// takes one from the runtime's list as it starts and gives it back as it
// ends (regEnter, regLeave): a frame that a panic leaves keeps its own,
// which the collector takes.
type regState struct {
	r    *Runtime
	f    *frame
	c    *regCode
	cl   *closure
	regs *[1 << 16]Value
	// wb is where the frame's window starts in the runtime's stack.
	wb int
	// kept is the pc past the instruction that calls out, kept across the
	// call: what regCatch finds the instruction that threw by, and what a
	// call that goes on elsewhere -- a switch, an iterator's step, a
	// generator's resumption -- sets.
	kept regKept
	err  error
	next *regState
	// native is the native handlers' context of the frames this state
	// runs (zzzyn_native_amd64.go), made at its first native run and
	// kept with the state.
	native *nativeCtx
}

// regEnter is a state for frame f running c.
func (r *Runtime) regEnter(f *frame, c *regCode) *regState {
	st := r.regFree
	if st == nil {
		st = &regState{r: r}
	} else {
		r.regFree = st.next
	}
	st.f, st.c, st.cl = f, c, f.cl
	st.wb = f.base - int(c.locals)
	st.regs = regWindow(r.stack, st.wb)
	return st
}

// regLeave gives st back, holding nothing of its frame's that the
// collector would keep: its registers' window is the runtime's stack.
func (r *Runtime) regLeave(st *regState) {
	st.f, st.c, st.cl, st.err = nil, nil, nil, nil
	st.next = r.regFree
	r.regFree = st
}

// at sets the frame's pc to the stack form's instruction of pc, which a
// stack trace or an error's position reads, and keeps the pc past it, for
// an instruction that may throw or run code; it returns the frame.
func (st *regState) at(pc int) *frame {
	st.kept.pc = pc + 1
	f := st.f
	f.pc = st.c.src[pc] + 1
	return f
}

// done goes on at the kept pc, or fails with err.
func (st *regState) done(err error) int {
	if err != nil {
		return st.fail(err)
	}
	return st.kept.pc
}

// put stores v in R[A] and goes on past the instruction, or fails with err.
func (st *regState) put(regs *[1 << 16]Value, w bytecode.RInstr, v Value, err error) int {
	if err != nil {
		return st.fail(err)
	}
	regs[w.A()] = v
	return st.kept.pc
}

// fail is the fail tail: the instruction at st.kept.pc-1 threw err, which
// a handler of the frame's may catch (regCatch); where none does, the
// frame ends with it.
//
//go:noinline
func (st *regState) fail(err error) int {
	if pc := st.r.regCatch(st.f, st.c, &st.kept, st.regs, err); pc >= 0 {
		return pc
	}
	st.err = err
	return regDone
}
