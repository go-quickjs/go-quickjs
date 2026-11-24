package engine

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// A function body is compiled, at its first call, to register code
// (reg_compile.go): fixed-width instructions over the frame's slots, run by
// native handlers where they are built and by the Go table's loop elsewhere
// (README.md).
//
// A frame's numeric slots hold, in order, its numeric locals (parameters
// first, a v128 taking two) and two slots for each operand stack position;
// its reference slots its reference locals and one for each position. A
// function's results are left in the slots its parameters would take if they
// were of the result types, so that a caller finds them knowing only the
// signature.

// The signals a body's run ends with besides 0: a tail call, the exec's
// tail call set (sigTail); sigReturn is the native code's for a return.
const (
	sigReturn int32 = -1
	sigTail   int32 = -2
)

// Code is a module compiled for the engine: its functions' records -- each
// one's frame, its register code once the interpreter has needed it, and
// its native code's state -- made on their first call and shared by every
// instance of the module.
type Code struct {
	Module *module.Module
	fns    []atomic.Pointer[compiled]
	mu     sync.Mutex
	// jit is the code's native code, if it has asked for it (EnableJIT).
	jit atomic.Pointer[codeJIT]
	// rtts are the runtime information of the module's types, by type
	// index, as Go's work for native code asks for them (typeRTT).
	rtts atomic.Pointer[[]atomic.Pointer[rtt]]
	// native runs the code's register code by the native handlers
	// (regNativeDefault), which its functions' code takes when it is
	// compiled (regCode's native).
	native bool
	// test is what a test compiles and runs the code with, nil but in a
	// test's (codeTest).
	test *codeTest
}

// codeTest is what a test compiles and runs a Code with in place of the
// process's, set on the code (and its native code's state) before it
// runs.
type codeTest struct {
	// maxRegCode, if not 0, is the most words a body's register code may
	// take, in place of maxRegCode.
	maxRegCode int
	// epoch, if set, stands in for the JIT's clock (jitEpoch).
	epoch func() uint64
	// optWorker, if set, runs before each of the code's compiles in the
	// background.
	optWorker func()
	// counts, if set, counts the exits the code's native code takes.
	counts *jitExitCounts
	// jit is the JIT's own (codeJITTest).
	jit codeJITTest
}

// jitExitCounts counts native code's exits, and of them its ExitAllocs,
// and its calls of the allocation stub (jit_alloc.go), which are not exits.
type jitExitCounts struct {
	exits, allocs, stubs atomic.Int64
}

// NewCode returns the engine's code of module m.
func NewCode(m *module.Module) *Code {
	c := &Code{Module: m, fns: make([]atomic.Pointer[compiled], len(m.Functions)), native: regNativeDefault}
	if jitBuilt {
		c.jitFromEnv()
	}
	return c
}

// compiled is a function's record: its frame, its register code if built
// says so, and the state of its native code. A record
// is not changed once stored, but for that state, which the records of one
// function share: building the body stores a new record.
type compiled struct {
	nslots, rslots int
	// nlocals and rlocals are the numeric and reference slots before the
	// operand stack's; refs says the body holds a reference anywhere, and
	// so reads or writes its reference slots. Native code keeps the
	// exception a legacy try's catch caught in reference slot caught plus
	// the try's depth among the legacy trys around it, slots past the
	// interpreter's own.
	nlocals, rlocals int
	refs             bool
	caught           int
	// built says reg is the body's register code: a record made for a
	// function native code runs from its first call has none until the
	// interpreter needs it (Code.interp).
	built bool
	reg   *regCode
	// params and results are where the arguments go and the results are
	// found.
	params, results []slotRef

	*fnJIT
}

// fnJIT is the state of a function's native code, which its records share.
type fnJIT struct {
	// native is the function's native code, once compiled; hot counts its
	// calls until then, and refused says the JIT will not compile it.
	native  atomic.Pointer[nativeFn]
	hot     atomic.Int32
	refused atomic.Bool
	// optDone says the optimizing tier compiled the function or refused
	// it: its baseline code no longer counts calls.
	optDone atomic.Bool
	// used is the JIT clock's epoch (jitEpoch) at which a call last
	// reached the function's native code through Go, which a call of it
	// does once its link has been cleared to see whether it is still
	// called; noRoom is the epoch at which its compile found no room in
	// the Code's JIT budget, until which it is not compiled again.
	used, noRoom atomic.Uint64
	// roomNeed is what the baseline's code and the optimizing tier's, with
	// their records, needed when they found no room, which the next
	// compile of each makes room for before it is made (codeJIT.mayFit);
	// it changes under the JIT's compile lock (codeJIT.mu).
	roomNeed [2]int64
}

// newCompiled returns an empty record with a native code's state of its
// own, allocated with it.
func newCompiled() *compiled {
	p := new(struct {
		c compiled
		j fnJIT
	})
	p.c.fnJIT = &p.j
	return &p.c
}

// get returns function index's record, making it if there is none.
func (c *Code) get(index uint32) *compiled {
	if p := c.fns[index].Load(); p != nil {
		return p
	}
	return c.first(index)
}

// first makes function index's record at its first use: where native code
// runs a function from its first call (nativeFirst) and validation bounded
// its frame, the record of that frame alone, its body left to the
// interpreter's first need of it; else the record of the body the
// interpreter compiles.
//
//go:noinline
func (c *Code) first(index uint32) *compiled {
	if c.nativeFirst() {
		if p := c.boundRecord(index); p != nil {
			c.fns[index].CompareAndSwap(nil, p)
			return c.fns[index].Load()
		}
	}
	return c.interp(index)
}

// interp returns function index's record with its body built, compiling
// the body if it is not. A record made before keeps its frame -- native
// code may run in it -- and its native code's state.
func (c *Code) interp(index uint32) *compiled {
	if p := c.fns[index].Load(); p != nil && p.built {
		return p
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.fns[index].Load()
	if old != nil && old.built {
		return old
	}
	start := time.Now()
	p := compileFunction(c, index)
	countTime(&jitStats.interpTime, start)
	jitStats.interpreted.Add(1)
	if old != nil {
		if p.nslots > old.nslots || p.rslots > old.rslots {
			panic("wasm: a function body's frame larger than validation's bound")
		}
		q := *old
		q.built, q.reg = true, p.reg
		p = &q
	}
	c.fns[index].Store(p)
	return p
}

// boundRecord returns the record of function index's frame as validation
// bounds it (module.FrameBound), which holds what the interpreter's compile
// of the body would make the frame hold, or nil for a body not validated.
func (c *Code) boundRecord(index uint32) *compiled {
	nlocals, rlocals, b, ok := c.boundLocals(index)
	if !ok {
		return nil
	}
	sig := c.Module.Types[c.Module.Functions[index].TypeIndex].Func
	p := newCompiled()
	stack := int(b.Stack)
	p.nslots = nlocals + 2*stack
	p.rslots = rlocals + stack + int(b.Tries)
	p.nlocals, p.rlocals = nlocals, rlocals
	p.refs = b.Refs || rlocals > 0 || b.Tries > 0
	p.caught = rlocals + stack
	p.params, _, _ = layout(sig.Params)
	p.results, _, _ = layout(sig.Results)
	return p
}

// boundLocals returns function f's numeric and reference slots before its
// operand stack's -- its locals', or more where its results need them, as
// compileFunction counts them -- and validation's bound of its stack; ok is
// false for an import, or a body not validated.
func (c *Code) boundLocals(f uint32) (nlocals, rlocals int, b module.FrameBound, ok bool) {
	m := c.Module
	fn := &m.Functions[f]
	b = fn.Frame
	if fn.Imported || b.Stack == 0 {
		return 0, 0, b, false
	}
	sig := m.Types[fn.TypeIndex].Func
	_, nlocals, rlocals = layout(sig.Params)
	r := reader{b: m.Bytes, pc: int(fn.Code.Offset)}
	for n := r.u32(); n > 0; n-- {
		count := int(r.u32())
		switch t := r.valueType(); {
		case t.IsRef():
			rlocals += count
		case t.Kind == module.KindV128:
			nlocals += 2 * count
		default:
			nlocals += count
		}
	}
	_, rn, rr := layout(sig.Results)
	return max(nlocals, rn), max(rlocals, rr), b, true
}

// slotRef locates a value in a frame: reference slot i, or numeric slot i
// (and i+1 for a v128).
type slotRef struct {
	ref  bool
	v128 bool
	i    int
}

// layout returns where values of types ts go when they are a function's
// parameters, or its results.
func layout(ts []module.ValueType) (refs []slotRef, nslots, rslots int) {
	refs = make([]slotRef, len(ts))
	for i, t := range ts {
		switch {
		case t.IsRef():
			refs[i] = slotRef{ref: true, i: rslots}
			rslots++
		case t.Kind == module.KindV128:
			refs[i] = slotRef{v128: true, i: nslots}
			nslots += 2
		default:
			refs[i] = slotRef{i: nslots}
			nslots++
		}
	}
	return refs, nslots, rslots
}

// compileFunction compiles function index's body to register code. A body
// whose register code would pass what a pc holds stops the call that needs
// it with ErrFunctionTooLarge; no limit of the decoder's keeps such a body
// out, a body's size bounding its code only loosely (a branch carries its
// values' moves). Any other refusal is a compiler bug.
func compileFunction(code *Code, index uint32) *compiled {
	p, why := regCompile(code, index)
	if p == nil {
		if why == regTooLarge {
			panic(hostError{fmt.Errorf("%w: function %d", ErrFunctionTooLarge, index)})
		}
		panic("wasm: a body register code does not hold: " + why)
	}
	return p
}

// ErrFunctionTooLarge stops a call of a function whose body compiles to more
// register code than the interpreter addresses.
var ErrFunctionTooLarge = errors.New("wasm: function too large for the interpreter")
