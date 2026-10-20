package vm

import (
	"encoding/binary"
	"math"
	"os"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The tree tier.
//
// The interpreter's loop pays for every instruction it dispatches: an
// indirect jump the processor mostly mispredicts, and the state of the loop
// stored and loaded around it. A function whose code the tier can build is
// run instead as trees of Go closures, one tree for each expression, each
// node computing its value from its children's and returning it: no operand
// stack, and a node for every few instructions rather than a dispatch for
// each.
//
// The trees are built from the function's bytecode, not its source, so that
// what an instruction does is decided in one place. Within a basic block the
// operand stack is followed symbolically: what an instruction pushes is a
// tree, and what consumes it takes the tree as a child. Children are
// evaluated in the order their instructions ran. Anything still on the stack
// where that order could otherwise change -- at the end of a block, before a
// statement, before a call -- is evaluated into the frame's own stack slot
// for it, where the interpreter would have had it, and read back from there.
//
// A node does what the instruction does: the interpreter's fast path where it
// has one, and otherwise the same helper it calls. A node that can throw or
// run code sets the frame's pc to its instruction first, so that a stack
// trace, and anything else that asks where the frame is, sees what it would
// have. An exception is a Go panic of a treeThrow. A function with handlers
// has a wrapper block that catches one and resumes at the handler; other
// trees let it propagate to the caller or the function's runTree boundary.
//
// A function with an instruction the tier does not build -- with, anything
// rarer than a loop's -- runs in the interpreter as before. A generator's or an async function's tree is run by its
// resumptions (see ztree_zyield.go), never by runTree.

// tctx is a running tree's state, which every node is given.
type tctx struct {
	r  *Runtime
	f  *frame
	cl *closure
	// closureCode is cl's, which a node reads its names, constants and
	// caches from as c.names, c.consts and c.ic: through cl they would be
	// a load more, as the closures of a template share them.
	*closureCode
	locals []Value
	// stack is the frame's operand window, where what a block leaves on the
	// stack, and a call's arguments, are written.
	stack []Value
	ret   Value
}

type (
	tval  func(c *tctx) Value
	tstmt func(c *tctx)
	// tnext ends a block: the next block's index, or -1 to return c.ret.
	tnext func(c *tctx) int
)

type tblock struct {
	body []tstmt
	next tnext
	// jump is 1 + the block an unconditional jump ends this one with, for
	// threading the jump into a block with no body of its own; back says it
	// jumps back, at pc, from a stack depth of depth.
	jump      int
	back      bool
	pc, depth int
}

// tree is a function's code as the tier runs it. It holds nothing of a
// runtime's -- every node reads the closure, its caches and its constants
// through the context -- so one tree serves every closure of the function,
// in every runtime.
type tree struct {
	blocks []tblock
	// gen is a generator's or an async function's, for its resumptions
	// (see ztree_zyield.go), and nil for any other function's.
	gen *tgen
	// seen is what its slow paths took that native code would not, for
	// the JIT (see ztree_zzzzseen.go), or nil for nothing yet.
	seen atomic.Pointer[treeSeen]
	// starts is, for a plain tree of more than one block, each block's
	// first instruction, and entries the blocks, for going on in it from
	// one (see ztree_goon.go): blocks may be only the first, the whole
	// function one structure, the rest absorbed in it but kept.
	starts  []int32
	entries []tblock
	// bigSeen is the arithmetic its slow paths made on BigInts, for the
	// JIT (see ztree_zzzzseen.go), or nil for none yet.
	bigSeen atomic.Pointer[treeBigSeen]
	// handled is, for the one block handlerTree wraps a tree of handlers
	// or for...of loops in, that tree and how runTreeHandlers runs it, for
	// going on in it from one of its blocks (see ztree_goon.go); starts
	// and entries are then its.
	handled *treeHandled
}

// treeThrow carries an exception out of a tree, to runTree.
type treeThrow struct{ err error }

// noTree marks a function the tier does not build.
var noTree = &tree{}

// treeTier turns the tier on; QJS_NOTREE in the environment turns it off,
// for comparing the two. It is read when a function is first run, so a
// function keeps the form it was first given.
var treeTier atomic.Bool

// treesBuilt counts the functions the tier has built.
var treesBuilt atomic.Int64

func init() { treeTier.Store(os.Getenv("QJS_NOTREE") == "") }

// SetTreeTier turns the tree tier on or off for functions not yet run. It is
// for tests that compare the two.
func SetTreeTier(on bool) { treeTier.Store(on) }

// TreesBuilt is how many functions the tree tier has built, for tests that
// check it ran.
func TreesBuilt() int64 { return treesBuilt.Load() }

// firstTree builds a function's tree, or settles that it has none, and
// records which in its VMCode: what runFD reads, at every call, to choose
// between the tree, noTree's interpreter, and building one.
//
//go:noinline
func firstTree(fn *bytecode.Function) *tree {
	p := buildTree(fn)
	if p == nil {
		p = noTree
	} else {
		treesBuilt.Add(1)
	}
	atomic.StorePointer(&fn.VMCode, unsafe.Pointer(p))
	return p
}

// runTree runs a frame's function as its tree, as execute runs it as
// bytecode. An exception leaving the tree, its own or one thrown in a tree it
// called through runTreeNested, is caught here and returned.
func (r *Runtime) runTree(f *frame, t *tree) (v Value, err error) {
	if r.stopped != nil {
		return Undefined, r.stopped
	}
	c := &f.tc
	c.r, c.f, c.cl, c.locals = r, f, f.cl, f.locals
	c.closureCode = f.cl.closureCode
	c.stack = r.stack[f.base : f.base+f.cl.fn.MaxStack]
	depth := r.frameDepth
	defer func() {
		if p := recover(); p != nil {
			th, ok := p.(treeThrow)
			if !ok {
				if jitBuilt {
					if jv, jerr, native := jitTreeResult(p); native {
						v, err = jv, jerr
						return
					}
				}
				panic(p)
			}
			// The trees this one called without a recover of their own
			// (runTreeNested) left their frames to it.
			r.unwindTreeFrames(depth)
			v, err = Undefined, th.err
		}
	}()
	blocks := t.blocks
	if len(blocks) == 1 {
		// Straight-line code, as most small functions are.
		blk := &blocks[0]
		for _, s := range blk.body {
			s(c)
		}
		blk.next(c)
		if r.treeTail {
			r.treeTail = false
			return Undefined, errTailCall
		}
		return c.ret, nil
	}
	b := 0
	for {
		blk := &blocks[b]
		for _, s := range blk.body {
			s(c)
		}
		if b = blk.next(c); b < 0 {
			if r.treeTail {
				r.treeTail = false
				return Undefined, errTailCall
			}
			return c.ret, nil
		}
	}
}

// getLength is the length of o read as a property, at pc: what get_length
// does for anything but a string or an array.
func (c *tctx) getLength(o Value, pc int) Value {
	c.at(pc)
	v, err := c.r.getValueProp(o, atomLength)
	if err != nil {
		c.throw(err)
	}
	return v
}

// throw leaves the tree with an exception.
func (c *tctx) throw(err error) {
	panic(treeThrow{err})
}

// at sets the frame's pc to the instruction at pc, for what is about to run
// or throw there.
func (c *tctx) at(pc int) {
	c.f.pc = uint32(pc + 1)
}

// backEdge counts a backward jump, as the interpreter's do, and runs the
// interrupt check when the budget is spent.
func (c *tctx) backEdge(pc, depth int) {
	if r := c.r; r.backEdges > 1 {
		r.backEdges--
		return
	}
	c.backEdgeCheck(pc, depth)
}

// backEdgeCheck is backEdge's interrupt check, out of line so that the count
// is inlined where a loop jumps back.
func (c *tctx) backEdgeCheck(pc, depth int) {
	r := c.r
	fullBudget := r.backEdges == 1
	r.backEdges = backEdgeCheckInterval
	c.at(pc)
	if err := r.checkInterruptNow(); err != nil {
		c.throw(err)
	}
	r.sweepStaleSlots(c.f.base + depth)
	r.tryJITTreeLoop(c, pc, depth, fullBudget)
}

func truthy(v Value) bool {
	return math.Float64bits(v.num) == trueBits || math.Float64bits(v.num) != falseBits && v.Truthy()
}

// elemAt is an element in an array's dense storage, as get_index reads one
// first thing, or a typed array's in its buffer, or false.
//
// The key is not asked whether it is a number: every other value's num is
// a NaN, which the round trip through uint32 never gives back, so the test
// that the key is an index is that test too. Each test leaves on failure,
// rather than joining the next with &&, which Go compiles to a flag kept
// in a register and tested again; and the element's bits are read as an
// integer, not as a float moved through the stack to be compared.
//
//go:noinline
func elemAt(obj, key Value) (Value, bool) {
	if !obj.IsObject() {
		return Undefined, false
	}
	o := obj.object()
	i, e := uint32(key.num), o.elemsOf()
	if float64(i) != key.num || uint(i) >= uint(len(e)) || o.flags&objMappedArguments != 0 {
		if o.class != ClassTypedArray || float64(i) != key.num {
			return Undefined, false
		}
		// A typed array's element, read from the storage its view keeps
		// once typedRead has filled it in, as typedRead reads it: written
		// here, not called, as a call would give elemAt, which every
		// array's read makes, a check of the stack's room.
		t, ok := o.dataOf().(*typedArrayData)
		if !ok || uint(i) >= uint(t.fixedLength) {
			return Undefined, false
		}
		st := t.st
		if st == nil || t.end > len(st.bytes) {
			return Undefined, false
		}
		b, j := st.bytes[t.byteOffset:t.end], int(i)
		switch t.kind {
		case elemInt8:
			return Int(int(int8(b[j]))), true
		case elemUint8, elemUint8Clamped:
			return Int(int(b[j])), true
		case elemInt16:
			return Int(int(int16(binary.LittleEndian.Uint16(b[j<<1:])))), true
		case elemUint16:
			return Int(int(binary.LittleEndian.Uint16(b[j<<1:]))), true
		case elemInt32:
			return Int32(int32(binary.LittleEndian.Uint32(b[j<<2:]))), true
		case elemUint32:
			return Uint32(binary.LittleEndian.Uint32(b[j<<2:])), true
		case elemFloat32:
			return Float(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[j<<2:])))), true
		case elemFloat64:
			return Float(math.Float64frombits(binary.LittleEndian.Uint64(b[j<<3:]))), true
		}
		return Undefined, false
	}
	p := &e[i]
	if holeAt(p) {
		return Undefined, false
	}
	return *p, true
}

// setElem stores v in an array's dense storage, as set_index does first
// thing, or a number in a typed array's buffer, or reports false. The key is a number when it is an index, as in
// elemAt.
//
//go:noinline
func setElem(o, k, v Value) bool {
	if !o.IsObject() {
		return false
	}
	a := o.object()
	i, e := uint32(k.num), a.elemsOf()
	if float64(i) != k.num || uint(i) >= uint(len(e)) || a.flags&objMappedArguments != 0 {
		if a.class != ClassTypedArray || float64(i) != k.num || !v.IsNumber() {
			return false
		}
		// A number stored in a typed array's buffer, as typedStore stores
		// it, through the storage its view keeps once typedStore has filled
		// it in, and written here for the reason elemAt's read is. A
		// Uint8Clamped element, rounded by a call, is typedStore's.
		t, ok := a.dataOf().(*typedArrayData)
		if !ok || uint(i) >= uint(t.fixedLength) {
			return false
		}
		st := t.st
		if st == nil || t.end > len(st.bytes) || st.immutable {
			return false
		}
		// An integer element takes a number that is an int32, as toInt32
		// does first; another is typedStore's, as toInt32's slow path is a
		// call.
		b, n, j := st.bytes[t.byteOffset:t.end], v.num, int(i)
		x := int32(n)
		switch t.kind {
		case elemInt8, elemUint8:
			if float64(x) != n {
				return false
			}
			b[j] = byte(x)
		case elemInt16, elemUint16:
			if float64(x) != n {
				return false
			}
			binary.LittleEndian.PutUint16(b[j<<1:], uint16(x))
		case elemInt32, elemUint32:
			if float64(x) != n {
				return false
			}
			binary.LittleEndian.PutUint32(b[j<<2:], uint32(x))
		case elemFloat32:
			binary.LittleEndian.PutUint32(b[j<<2:], math.Float32bits(float32(n)))
		case elemFloat64:
			binary.LittleEndian.PutUint64(b[j<<3:], math.Float64bits(n))
		default:
			return false
		}
		return true
	}
	p := &e[i]
	if holeAt(p) {
		return false
	}
	*p = v
	return true
}

// holeAt is isHole of the element at p, its bits read straight into an
// integer register.
func holeAt(p *Value) bool { return *(*uint64)(unsafe.Pointer(&p.num)) == holeBits }
