package vm

import (
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
// have. An exception is a Go panic of a treeThrow, caught where the function
// began; nothing in the tier's functions catches one, so it leaves the
// function, as an exception with no handler in it does.
//
// A function with an instruction the tier does not build -- an exception
// handler, an iterator, a generator's, anything rarer than a loop's -- runs
// in the interpreter as before.

// tctx is a running tree's state, which every node is given.
type tctx struct {
	r      *Runtime
	f      *frame
	cl     *closure
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
}

// tree is a function's code as the tier runs it. It holds nothing of a
// runtime's -- every node reads the closure, its caches and its constants
// through the context -- so one tree serves every closure of the function,
// in every runtime.
type tree struct {
	blocks []tblock
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

// QJS_TREEALL in the environment has the tier build every function it can,
// for running a test suite through it.
func init() {
	treeTier.Store(os.Getenv("QJS_NOTREE") == "")
	treeEverything.Store(os.Getenv("QJS_TREEALL") != "")
}

// treeEverything has the tier build every function it can, whether or not
// it has a loop to win back the cost and few enough calls: for tests, which
// want every instruction it builds run in it.
var treeEverything atomic.Bool

// SetTreeTier turns the tree tier on or off for functions not yet run, and
// with everything has it build every function it can rather than those it
// expects to run faster. It is for tests that compare the two.
func SetTreeTier(on, everything bool) {
	treeTier.Store(on)
	treeEverything.Store(everything)
}

// TreesBuilt is how many functions the tree tier has built, for tests that
// check it ran.
func TreesBuilt() int64 { return treesBuilt.Load() }

// treeOf is the tree the function runs as, building it the first time, or
// nil for one the tier does not build.
func treeOf(fn *bytecode.Function) *tree {
	p := (*tree)(atomic.LoadPointer(&fn.VMCode))
	if p == nil {
		p = buildTree(fn)
		if p == nil {
			p = noTree
		} else {
			treesBuilt.Add(1)
		}
		atomic.StorePointer(&fn.VMCode, unsafe.Pointer(p))
	}
	if p == noTree {
		return nil
	}
	return p
}

// runTree runs a frame's function as its tree, as execute runs it as
// bytecode.
func (r *Runtime) runTree(f *frame, t *tree) (v Value, err error) {
	if r.stopped != nil {
		return Undefined, r.stopped
	}
	c := &f.tc
	c.r, c.f, c.cl, c.locals = r, f, f.cl, f.locals
	c.stack = r.stack[f.base : f.base+f.cl.fn.MaxStack]
	defer func() {
		if p := recover(); p != nil {
			th, ok := p.(treeThrow)
			if !ok {
				panic(p)
			}
			v, err = Undefined, th.err
		}
		c.r, c.cl, c.locals, c.stack = nil, nil, nil, nil
	}()
	blocks := t.blocks
	b := 0
	for {
		blk := &blocks[b]
		for _, s := range blk.body {
			s(c)
		}
		if b = blk.next(c); b < 0 {
			return c.ret, nil
		}
	}
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
	r := c.r
	if r.backEdges--; r.backEdges <= 0 {
		r.backEdges = backEdgeCheckInterval
		c.at(pc)
		if err := r.checkInterruptNow(); err != nil {
			c.throw(err)
		}
		r.sweepStaleSlots(c.f.base + depth)
	}
}

func truthy(v Value) bool {
	return math.Float64bits(v.num) == trueBits || math.Float64bits(v.num) != falseBits && v.Truthy()
}
