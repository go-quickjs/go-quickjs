package opt

import "sync"

// workspace is what a compile works in -- the IR's values and blocks, the
// passes' tables, the allocator's and the lowering's -- kept for the next
// compile, so that a compile allocates little once the workspace has grown
// to its function's size. A compile takes one from workspaces and puts it
// back when its code is made: nothing the code holds is the workspace's.
type workspace struct {
	f  Func
	bd builder

	values chunks[Value]
	blocks chunks[Block]
	ctls   chunks[ctl]
	vals   arena[*Value]
	blks   arena[*Block]
	i32    arena[int32]
	types  arena[Type]
	// incomplete are the blocks' incomplete phis.
	incomplete arena[incompletePhi]
	// defs are the locals' values at the blocks' ends, by defKey.
	defs u64map[*Value]

	// The control flow graph's: marks by block ID (blockMarks), a work
	// list, a walk's frames, the dominator tree, the natural loops, and
	// loopDepths' cut edges, components and their lists.
	marks    []int32
	work     []*Block
	frames   []orderFrame
	dom      domTree
	loops    []natLoop
	cut      u64map[bool]
	inSCC    []int32
	scc      sccs
	sccLists arena[[]*Block]

	// The passes': gvn's table, and dce's dead values by ID and work list.
	gvn     gvnTable
	dead    []bool
	dceWork []*Value

	// Jump threading's: the predecessors threaded, the threaded edges,
	// the uses within a block of its values (marked with stamp), and
	// repairPhis' variables, values at blocks' starts and phis made.
	gone      []int32
	ends      threadEnds
	stamp     uint32
	localMark []uint32
	localUses []int32
	vars      []int32
	start     u64map[*Value]
	made      []madePhi

	// forwardGlobals' state, and the loops the builder made.
	fw         fwState
	loopRanges []loopRange

	gen genScratch
}

var workspaces = sync.Pool{New: func() any { return new(workspace) }}

// reset empties ws for a function to be built in it.
func (ws *workspace) reset() {
	ws.f = Func{}
	ws.bd.reset()
	ws.values.reset()
	ws.blocks.reset()
	ws.ctls.reset()
	ws.vals.reset()
	ws.blks.reset()
	ws.i32.reset()
	ws.types.reset()
	ws.incomplete.reset()
	ws.sccLists.reset()
	clear(ws.loopRanges)
	ws.loopRanges = ws.loopRanges[:0]
}

// blockMarks returns a mark for each of f's blocks, by ID, all 0. Its
// user has it until the next call.
func (ws *workspace) blockMarks(f *Func) []int32 {
	ws.marks = grown(ws.marks, int(f.nextBlock))
	clear(ws.marks)
	return ws.marks
}
