package engine

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// A function body is compiled into trees of closures. Within a construct the
// operand stack is followed symbolically: what an instruction pushes is an
// entry -- a constant, a read of a local or of a stack slot, or a tree that
// computes the value -- and what consumes it takes the entry as a child.
// Entries are evaluated in the order their instructions ran: a statement
// first stores every pending tree beneath what it consumes into that stack
// position's slot, and a write to a local first stores the pending reads of
// it.
//
// Statements return a control signal: 0 to go on, n > 0 to branch out of n
// constructs (a loop's branch restarts it), sigReturn to return.
//
// A frame's numeric slots hold, in order, its numeric locals (parameters
// first, a v128 taking two) and two slots for each operand stack position;
// its reference slots its reference locals and one for each position. A
// function's results are left in the slots its parameters would take if they
// were of the result types, so that a caller finds them knowing only the
// signature.

type stmt func(f *frame) int32

const (
	sigReturn int32 = -1
	sigTail   int32 = -2
)

type (
	ti32  func(*frame) uint32
	ti64  func(*frame) uint64
	tf32  func(*frame) float32
	tf64  func(*frame) float64
	tv128 func(*frame) V128
	tref  func(*frame) Ref
)

// Code is a module compiled for the engine: its functions' bodies, compiled
// on their first call and shared by every instance of the module.
type Code struct {
	Module *module.Module
	fns    []atomic.Pointer[compiled]
	mu     sync.Mutex
	// jit is the code's native code, if it has asked for it (EnableJIT).
	jit atomic.Pointer[codeJIT]
}

// NewCode returns the engine's code of module m.
func NewCode(m *module.Module) *Code {
	c := &Code{Module: m, fns: make([]atomic.Pointer[compiled], len(m.Functions))}
	if jitBuilt {
		c.jitFromEnv()
	}
	return c
}

// compiled is a function body ready to run.
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
	body             []stmt
	// epi moves the results from their stack positions to where the caller
	// finds them, after the body ends or branches to the function's label.
	epi []stmt
	// params and results are where the arguments go and the results are
	// found.
	params, results []slotRef

	// native is the function's native code, once compiled; hot counts its
	// calls until then, and refused says the JIT will not compile it.
	native  atomic.Pointer[nativeFn]
	hot     atomic.Int32
	refused atomic.Bool
	// optDone says the optimizing tier compiled the function or refused
	// it: its baseline code no longer counts calls.
	optDone atomic.Bool
}

// get returns function index's compiled body, compiling it if it is not.
func (c *Code) get(index uint32) *compiled {
	if p := c.fns[index].Load(); p != nil {
		return p
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.fns[index].Load(); p != nil {
		return p
	}
	start := time.Now()
	p := compileFunction(c, index)
	countTime(&jitStats.interpTime, start)
	jitStats.interpreted.Add(1)
	c.fns[index].Store(p)
	return p
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

// ekind is what an entry of the symbolic operand stack is.
type ekind uint8

const (
	eTree  ekind = iota
	eConst       // a constant: c, and hi for a v128
	eSlot        // a frame slot: slot, the local's index in local or -1
)

type entry struct {
	t     module.ValueType
	k     ekind
	slot  int
	local int
	c, hi uint64
	tree  any
}

// ctl is an enclosing construct being compiled.
type ctl struct {
	op     opcode.Opcode
	base   int
	params []module.ValueType
	result []module.ValueType
	// at is the construct's offset in the module, which names a loop to
	// the JIT.
	at   uint32
	body []stmt
	// then holds an if's then branch once its else began; hasElse says so.
	then    []stmt
	hasElse bool
	cond    ti32
	// dead says the rest of the construct cannot be reached.
	dead bool
	// first is the group of the block that began the construct's body, if
	// a block did, which a block ending merges with itself.
	first *blockGroup
	// index is a try_table's or try's index in the control stack.
	index int
	// clauses are a try_table's catch clauses.
	clauses []catchClause
	// tryBody and handlers are a legacy try's body and its catches, once
	// its first catch began.
	tryBody  []stmt
	handlers []handler
}

// labelTypes returns the types a branch to the construct carries.
func (c *ctl) labelTypes() []module.ValueType {
	if c.op == opcode.Loop {
		return c.params
	}
	return c.result
}

type compiler struct {
	code *Code
	m    *module.Module
	fn   uint32
	r    reader

	locals     []slotRef
	localTypes []module.ValueType
	nlocals    int
	rlocals    int
	maxHeight  int

	stack []entry
	ctrl  []*ctl
	epi   []stmt
	// refs says a reference was pushed.
	refs bool
	// tries is the deepest nesting of legacy try constructs.
	tries int
	// ctls are the constructs made so far, nctl of them by this compile,
	// kept for the next (newCtl).
	ctls []*ctl
	nctl int
}

// compilers keeps compilers from one compile to the next, with the memory
// of their stacks and their constructs.
var compilers sync.Pool

// release gives c back to compilers, dropping what it holds of the
// compile: the stack's trees, the constructs' statements.
func (c *compiler) release() {
	clear(c.stack[:cap(c.stack)])
	clear(c.ctrl[:cap(c.ctrl)])
	for _, t := range c.ctls[:c.nctl] {
		*t = ctl{}
	}
	*c = compiler{stack: c.stack[:0], ctrl: c.ctrl[:0], localTypes: c.localTypes[:0], ctls: c.ctls}
	compilers.Put(c)
}

// newCtl returns a construct of the compiler's, to be filled: one made by
// an earlier compile, or a new one. None is used twice in a compile.
func (c *compiler) newCtl() *ctl {
	if c.nctl == len(c.ctls) {
		c.ctls = append(c.ctls, new(ctl))
	}
	t := c.ctls[c.nctl]
	c.nctl++
	return t
}

func compileFunction(code *Code, index uint32) *compiled {
	m := code.Module
	f := &m.Functions[index]
	sig := m.Types[f.TypeIndex].Func
	c, _ := compilers.Get().(*compiler)
	if c == nil {
		c = new(compiler)
	}
	defer c.release()
	c.code, c.m, c.fn = code, m, index
	c.r = reader{b: m.Bytes, pc: int(f.Code.Offset)}
	end := int(f.Code.End())

	// Locals: the parameters, then the declared ones.
	c.localTypes = append(c.localTypes, sig.Params...)
	for n := c.r.u32(); n > 0; n-- {
		count := c.r.u32()
		t := c.r.valueType()
		for j := uint32(0); j < count; j++ {
			c.localTypes = append(c.localTypes, t)
		}
	}
	c.locals, c.nlocals, c.rlocals = layout(c.localTypes)
	// The results are left in slots a parameter of their type would have;
	// make sure there are enough.
	_, rn, rr := layout(sig.Results)
	c.nlocals = max(c.nlocals, rn)
	c.rlocals = max(c.rlocals, rr)

	fnCtl := c.newCtl()
	*fnCtl = ctl{base: 0, result: sig.Results}
	c.ctrl = append(c.ctrl, fnCtl)
	for c.r.pc < end && len(c.ctrl) > 0 {
		c.instr()
	}
	params, _, _ := layout(sig.Params)
	results, _, _ := layout(sig.Results)
	return &compiled{
		nslots:  c.nlocals + 2*(c.maxHeight+1),
		rslots:  c.rlocals + c.maxHeight + 1 + c.tries,
		nlocals: c.nlocals,
		rlocals: c.rlocals,
		refs:    c.refs || c.rlocals > 0 || c.tries > 0,
		caught:  c.rlocals + c.maxHeight + 1,
		body:    fnCtl.body,
		epi:     c.epi,
		params:  params,
		results: results,
	}
}

// numSlot and refSlot return the slots of stack position h.
func (c *compiler) numSlot(h int) int { return c.nlocals + 2*h }
func (c *compiler) refSlot(h int) int { return c.rlocals + h }

func (c *compiler) top() *ctl { return c.ctrl[len(c.ctrl)-1] }

func (c *compiler) emit(s stmt) {
	t := c.top()
	t.body = append(t.body, s)
}

func (c *compiler) push(e entry) {
	if e.t.IsRef() {
		c.refs = true
	}
	c.stack = append(c.stack, e)
	if h := len(c.stack); h > c.maxHeight {
		c.maxHeight = h
	}
}

func (c *compiler) pop() entry {
	e := c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
	return e
}

// popN pops the top n entries, returning them bottom first.
func (c *compiler) popN(n int) []entry {
	es := make([]entry, n)
	copy(es, c.stack[len(c.stack)-n:])
	c.stack = c.stack[:len(c.stack)-n]
	return es
}

func (c *compiler) pushTree(t module.ValueType, tree any) {
	c.push(entry{t: t, k: eTree, tree: tree, local: -1})
}

// slotEntry returns the entry reading stack position h's slot.
func (c *compiler) slotEntry(t module.ValueType, h int) entry {
	if t.IsRef() {
		return entry{t: t, k: eSlot, slot: c.refSlot(h), local: -1}
	}
	return entry{t: t, k: eSlot, slot: c.numSlot(h), local: -1}
}

// store returns a statement storing e's value into slot s (a reference slot
// if e is a reference).
func (c *compiler) store(e entry, s int) stmt {
	switch e.t.Kind {
	case module.KindI32:
		v := c.i32(e)
		return func(f *frame) int32 { f.l[s] = uint64(v(f)); return 0 }
	case module.KindI64:
		v := c.i64(e)
		return func(f *frame) int32 { f.l[s] = v(f); return 0 }
	case module.KindF32:
		v := c.f32(e)
		return func(f *frame) int32 { f.l[s] = uint64(math.Float32bits(v(f))); return 0 }
	case module.KindF64:
		v := c.f64(e)
		return func(f *frame) int32 { f.l[s] = math.Float64bits(v(f)); return 0 }
	case module.KindV128:
		v := c.v128(e)
		return func(f *frame) int32 { x := v(f); f.l[s], f.l[s+1] = x.Lo, x.Hi; return 0 }
	}
	v := c.ref(e)
	return func(f *frame) int32 { f.r[s] = v(f); return 0 }
}

// copySlot returns a statement copying the value of type t from slot src to
// slot dst, or nil if they are the same.
func copySlot(t module.ValueType, src, dst int) stmt {
	if src == dst {
		return nil
	}
	switch {
	case t.IsRef():
		return func(f *frame) int32 { f.r[dst] = f.r[src]; return 0 }
	case t.Kind == module.KindV128:
		return func(f *frame) int32 { f.l[dst], f.l[dst+1] = f.l[src], f.l[src+1]; return 0 }
	}
	return func(f *frame) int32 { f.l[dst] = f.l[src]; return 0 }
}

// materialize stores stack position h's entry into its slot, unless it is a
// constant or already there, and replaces it with a read of the slot.
func (c *compiler) materialize(h int) {
	e := c.stack[h]
	want := c.slotEntry(e.t, h)
	if e.k == eSlot && e.slot == want.slot {
		return
	}
	c.emit(c.store(e, want.slot))
	c.stack[h] = want
}

// flushBelow stores every pending tree beneath the top n entries, in order,
// so that a statement consuming those n runs after them.
func (c *compiler) flushBelow(n int) {
	for h := 0; h < len(c.stack)-n; h++ {
		if c.stack[h].k == eTree {
			c.materialize(h)
		}
	}
}

// flushLocal stores the pending reads of local k, before a write to it.
func (c *compiler) flushLocal(k int) {
	for h := range c.stack {
		if c.stack[h].k == eSlot && c.stack[h].local == k {
			c.materialize(h)
		}
	}
}

// flushAll stores every entry into its slot.
func (c *compiler) flushAll() {
	for h := range c.stack {
		c.materialize(h)
	}
}

// The typed readers of an entry.

func (c *compiler) i32(e entry) ti32 {
	switch e.k {
	case eConst:
		v := uint32(e.c)
		return func(*frame) uint32 { return v }
	case eSlot:
		s := e.slot
		return func(f *frame) uint32 { return uint32(f.l[s]) }
	}
	return e.tree.(ti32)
}

func (c *compiler) i64(e entry) ti64 {
	switch e.k {
	case eConst:
		v := e.c
		return func(*frame) uint64 { return v }
	case eSlot:
		s := e.slot
		return func(f *frame) uint64 { return f.l[s] }
	}
	return e.tree.(ti64)
}

func (c *compiler) f32(e entry) tf32 {
	switch e.k {
	case eConst:
		v := math.Float32frombits(uint32(e.c))
		return func(*frame) float32 { return v }
	case eSlot:
		s := e.slot
		return func(f *frame) float32 { return math.Float32frombits(uint32(f.l[s])) }
	}
	return e.tree.(tf32)
}

func (c *compiler) f64(e entry) tf64 {
	switch e.k {
	case eConst:
		v := math.Float64frombits(e.c)
		return func(*frame) float64 { return v }
	case eSlot:
		s := e.slot
		return func(f *frame) float64 { return math.Float64frombits(f.l[s]) }
	}
	return e.tree.(tf64)
}

func (c *compiler) v128(e entry) tv128 {
	switch e.k {
	case eConst:
		v := V128{e.c, e.hi}
		return func(*frame) V128 { return v }
	case eSlot:
		s := e.slot
		return func(f *frame) V128 { return V128{f.l[s], f.l[s+1]} }
	}
	return e.tree.(tv128)
}

func (c *compiler) ref(e entry) tref {
	switch e.k {
	case eConst:
		return func(*frame) Ref { return Null }
	case eSlot:
		s := e.slot
		return func(f *frame) Ref { return f.r[s] }
	}
	return e.tree.(tref)
}

// value returns e's value as a Val, for the generic paths.
func (c *compiler) val(e entry) func(*frame) Val {
	switch e.t.Kind {
	case module.KindI32:
		v := c.i32(e)
		return func(f *frame) Val { return Val{N: uint64(v(f))} }
	case module.KindI64:
		v := c.i64(e)
		return func(f *frame) Val { return Val{N: v(f)} }
	case module.KindF32:
		v := c.f32(e)
		return func(f *frame) Val { return Val{N: uint64(math.Float32bits(v(f)))} }
	case module.KindF64:
		v := c.f64(e)
		return func(f *frame) Val { return Val{N: math.Float64bits(v(f))} }
	case module.KindV128:
		v := c.v128(e)
		return func(f *frame) Val { x := v(f); return Val{N: x.Lo, Hi: x.Hi} }
	}
	v := c.ref(e)
	return func(f *frame) Val { return Val{R: v(f)} }
}

// The control constructs.

// enter begins a construct whose parameters are on the stack.
func (c *compiler) enter(op opcode.Opcode, params, results []module.ValueType) *ctl {
	c.flushAll()
	t := c.newCtl()
	*t = ctl{op: op, base: len(c.stack) - len(params), params: params, result: results}
	c.ctrl = append(c.ctrl, t)
	return t
}

// fallthroughResults stores the construct's results, on top of the stack,
// into their positions' slots, and empties the stack to the construct's base.
func (c *compiler) fallthroughResults(t *ctl) {
	if !t.dead {
		es := c.popN(len(t.result))
		c.flushBelow(0)
		for i, e := range es {
			dst := c.slotEntry(e.t, t.base+i).slot
			if e.k == eSlot && e.slot == dst {
				continue
			}
			c.emit(c.store(e, dst))
		}
	}
	c.stack = c.stack[:t.base]
}

// pushResults pushes reads of a construct's results from their slots.
func (c *compiler) pushResults(t *ctl) {
	for i, rt := range t.result {
		c.push(c.slotEntry(rt, t.base+i))
	}
}

// end ends the innermost construct.
func (c *compiler) end() {
	t := c.top()
	if t.op == opcode.If && !t.hasElse {
		// An if without an else passes its parameters through.
		c.elseBegin()
	}
	c.fallthroughResults(t)
	c.ctrl = c.ctrl[:len(c.ctrl)-1]
	if len(c.ctrl) == 0 {
		c.functionEpilogue(t)
		return
	}
	body := t.body
	var s stmt
	var group *blockGroup
	switch t.op {
	case opcode.Block:
		// A block whose body begins with a block, as a switch's chain of
		// blocks does, is run with it as one group. The inner group's
		// segments are extended in place: its statement, body[0], is
		// dropped for the group's, and the statement read only the
		// segments it had, which an append past them leaves as they are;
		// a chain of n blocks so takes O(n), not a copy of the segments at
		// each block's end.
		if t.first != nil {
			group = &blockGroup{segs: append(t.first.segs, body[1:])}
		} else {
			group = &blockGroup{segs: [][]stmt{body}}
		}
		s = group.stmt()
	case opcode.Loop:
		s = c.loopStmt(body, t.at)
	case opcode.If:
		s = ifStmt(t.cond, t.then, body)
	default:
		s = c.endOther(t, body)
	}
	if s != nil {
		if parent := c.top(); len(parent.body) == 0 && group != nil {
			parent.first = group
		}
		c.emit(s)
	}
	if c.top().dead {
		return
	}
	c.pushResults(t)
}

// blockGroup is a chain of nested blocks run as one statement: segs[0] is
// the innermost block's body, and segs[i] the statements of block i that
// follow block i-1's end. A branch out of k blocks from segs[i] goes on at
// segs[i+k], however deep the chain, rather than returning through each
// block between.
type blockGroup struct {
	segs [][]stmt
}

func (g *blockGroup) stmt() stmt {
	if len(g.segs) == 1 {
		return blockStmt(g.segs[0])
	}
	segs := g.segs
	n := int32(len(segs))
	return func(f *frame) int32 {
		for i := int32(0); i < n; {
			c := runSeq(f, segs[i])
			if c < 0 {
				return c
			}
			if c == 0 {
				i++
				continue
			}
			// Out of c blocks from block i: past the group if that reaches
			// beyond its outermost block.
			if i+c >= n {
				return i + c - n
			}
			i += c
		}
		return 0
	}
}

// functionEpilogue moves a function's results from their stack positions to
// where its caller finds them.
func (c *compiler) functionEpilogue(t *ctl) {
	dst, _, _ := layout(t.result)
	for i, rt := range t.result {
		src := c.slotEntry(rt, i).slot
		if s := copySlot(rt, src, dst[i].i); s != nil {
			c.epi = append(c.epi, s)
		}
	}
}

func runSeq(f *frame, body []stmt) int32 {
	for _, s := range body {
		if c := s(f); c != 0 {
			return c
		}
	}
	return 0
}

func blockStmt(body []stmt) stmt {
	switch len(body) {
	case 0:
		return nil
	case 1:
		s := body[0]
		return func(f *frame) int32 {
			if c := s(f); c > 0 {
				return c - 1
			} else {
				return c
			}
		}
	}
	return func(f *frame) int32 {
		for _, s := range body {
			if c := s(f); c != 0 {
				if c > 0 {
					return c - 1
				}
				return c
			}
		}
		return 0
	}
}

// loopStmt returns the statement of a loop whose body is body, at offset at:
// the loop of a function the JIT may compile asks for its native code once
// the call has run enough back edges (osrLoopStmt).
func (c *compiler) loopStmt(body []stmt, at uint32) stmt {
	if jitBuilt && c.code.jit.Load() != nil {
		return osrLoopStmt(body, c.fn, at)
	}
	return loopStmt(body)
}

// osrLoopStmt is loopStmt for a loop of function fn that may go on in the
// function's native code. Each back edge counts down the frame's osr; when
// it runs out, the frame's state is the loop head's -- the loop's
// parameters and every entry beneath them in their slots, the locals in
// theirs -- and the call goes on from the head of the loop in the native
// code, which runs it to the function's return (exec.osr). The loop then
// returns sigReturn, the results in place, or sigTail; where there is no
// native code the frame stops counting.
func osrLoopStmt(body []stmt, fn, at uint32) stmt {
	return func(f *frame) int32 {
		for {
			c := runSeq(f, body)
			if c == 1 {
				if f.x.poll--; f.x.poll <= 0 {
					f.x.pollContext()
				}
				if f.osr--; f.osr <= 0 {
					if sig, ok := f.x.osr(f, fn, at); ok {
						return sig
					}
					f.osr = math.MaxInt32
				}
				continue
			}
			if c > 0 {
				return c - 1
			}
			return c
		}
	}
}

func loopStmt(body []stmt) stmt {
	return func(f *frame) int32 {
		for {
			c := runSeq(f, body)
			if c == 1 {
				if f.x.poll--; f.x.poll <= 0 {
					f.x.pollContext()
				}
				continue
			}
			if c > 0 {
				return c - 1
			}
			return c
		}
	}
}

func ifStmt(cond ti32, then, els []stmt) stmt {
	return func(f *frame) int32 {
		var c int32
		if cond(f) != 0 {
			c = runSeq(f, then)
		} else {
			c = runSeq(f, els)
		}
		if c > 0 {
			return c - 1
		}
		return c
	}
}

// elseBegin switches an if from its then branch to its else branch.
func (c *compiler) elseBegin() {
	t := c.top()
	c.fallthroughResults(t)
	t.then = t.body
	t.body = nil
	t.hasElse = true
	t.dead = false
	// The else branch starts from the if's parameters, in their slots.
	for i, pt := range t.params {
		c.push(c.slotEntry(pt, t.base+i))
	}
}

// branch returns a statement storing the values a branch to construct
// depth carries, from the top of the stack, into the construct's slots. The
// entries are consumed.
func (c *compiler) branchValues(depth int) []stmt {
	t := c.ctrl[len(c.ctrl)-1-depth]
	types := t.labelTypes()
	es := c.popN(len(types))
	c.flushBelow(0)
	var ss []stmt
	for i, e := range es {
		dst := c.slotEntry(e.t, t.base+i).slot
		if e.k == eSlot && e.slot == dst {
			continue
		}
		ss = append(ss, c.store(e, dst))
	}
	return ss
}

// markDead says the rest of the innermost construct cannot be reached, and
// skips its instructions.
func (c *compiler) markDead() {
	t := c.top()
	t.dead = true
	c.stack = c.stack[:t.base]
	c.r.skipDead()
}

// seqStmt returns a statement running ss and then returning sig.
func seqStmt(ss []stmt, sig int32) stmt {
	switch len(ss) {
	case 0:
		return func(*frame) int32 { return sig }
	case 1:
		s := ss[0]
		return func(f *frame) int32 { s(f); return sig }
	}
	return func(f *frame) int32 {
		for _, s := range ss {
			s(f)
		}
		return sig
	}
}

// returnValues returns the statements of a return: the results, from the
// top of the stack, stored where the caller finds them. They are stored into
// their positions' slots first, as storing one into the slots of the locals
// could change a local another reads.
func (c *compiler) returnValues() []stmt {
	fn := c.ctrl[0]
	n := len(fn.result)
	c.flushBelow(n)
	base := len(c.stack) - n
	for h := base; h < len(c.stack); h++ {
		c.materialize(h)
	}
	dst, _, _ := layout(fn.result)
	var ss []stmt
	for i, rt := range fn.result {
		if s := copySlot(rt, c.slotEntry(rt, base+i).slot, dst[i].i); s != nil {
			ss = append(ss, s)
		}
	}
	c.stack = c.stack[:base]
	return ss
}
