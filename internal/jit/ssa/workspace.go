package ssa

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/arena"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// A Workspace is the memory a runtime's compiles share, as V8 compiles in a
// zone: a Func built in one (BuildIn) takes its values, their argument
// lists, its frame states and blocks, and its passes' tables from the
// workspace's arenas, and Rewind takes them all back at once, keeping the
// chunks, so that a runtime that compiles often allocates little. Nothing
// built in a workspace may be used after its Rewind. A Func built without
// one (BuildWith) allocates its own, and may be kept.
type Workspace struct {
	values    arena.Arena[Value]
	refs      arena.Arena[*Value]
	states    arena.Arena[FrameState]
	blocks    arena.Arena[Block]
	blockRefs arena.Arena[*Block]
	refLists  arena.Arena[[]*Value]
	bools     arena.Arena[bool]
	bytes     arena.Arena[uint8]
	ints      arena.Arena[int]
	caches    arena.Arena[InlineCache]
	accessors arena.Arena[AccessorSite]
	answers   arena.Arena[siteAnswer]
	scratch   scratch
	ready     bool
}

// init has the workspace's arenas keep the big tables a compile makes.
func (w *Workspace) init() {
	if w.ready {
		return
	}
	w.ready = true
	w.values.KeepBig()
	w.refs.KeepBig()
	w.states.KeepBig()
	w.blocks.KeepBig()
	w.blockRefs.KeepBig()
	w.refLists.KeepBig()
	w.bools.KeepBig()
	w.bytes.KeepBig()
	w.ints.KeepBig()
	w.caches.KeepBig()
	w.accessors.KeepBig()
	w.answers.KeepBig()
}

// Bytes is the memory the workspace keeps between compiles: its arenas'
// chunks and its passes' scratch tables.
func (w *Workspace) Bytes() int { return w.arenaBytes() + w.scratchBytes() }

// arenaBytes is what the workspace's arenas keep.
func (w *Workspace) arenaBytes() int {
	return w.values.Bytes() + w.refs.Bytes() + w.states.Bytes() + w.blocks.Bytes() + w.blockRefs.Bytes() +
		w.refLists.Bytes() + w.bools.Bytes() + w.bytes.Bytes() + w.ints.Bytes() + w.caches.Bytes() + w.accessors.Bytes() +
		w.answers.Bytes()
}

// scratchBytes is what the passes' scratch tables keep.
func (w *Workspace) scratchBytes() int {
	s := &w.scratch
	n := capBytes(s.flags) + capBytes(s.vals) + capBytes(s.phis) + capBytes(s.ordered) + capBytes(s.front) +
		capBytes(s.boxes) + capBytes(s.rest) + capBytes(s.work) + capBytes(s.first) + capBytes(s.last) +
		capBytes(s.guards) + capBytes(s.liveStores) + capBytes(s.liveCands) + capBytes(s.liveFlat) +
		capBytes(s.live) + capBytes(s.liveFrom) + capBytes(s.liveWords) + capBytes(s.subst) + capBytes(s.originPhis)
	for _, l := range s.live {
		n += capBytes(l)
	}
	return n
}

// capBytes is the memory s's backing array holds.
func capBytes[T any](s []T) int {
	var t T
	return cap(s) * int(unsafe.Sizeof(t))
}

// Rewind takes back everything built in the workspace.
func (w *Workspace) Rewind() {
	w.values.Rewind()
	w.refs.Rewind()
	w.states.Rewind()
	w.blocks.Rewind()
	w.blockRefs.Rewind()
	w.refLists.Rewind()
	w.bools.Rewind()
	w.bytes.Rewind()
	w.ints.Rewind()
	w.caches.Rewind()
	w.accessors.Rewind()
	w.answers.Rewind()
	// The arenas keep what the recent compiles needed (arena's Rewind);
	// the scratch tables, grown by value ID, the most any compile has. Once
	// they hold more than the arenas, a compile bigger than the recent
	// ones grew them, and they go with its chunks: what the workspace
	// keeps is what the recent compiles needed, apart from any budget.
	if w.scratchBytes() > w.arenaBytes() {
		w.scratch = scratch{}
	}
}

// BuildIn is BuildWith, building in w.
func BuildIn(w *Workspace, p *ir.Program, fb Feedback) (*Func, error) {
	w.init()
	return build(w, p, fb)
}

// appendAnswer is append(s, a), growing s in the workspace if there is one.
func (f *Func) appendAnswer(s []siteAnswer, a siteAnswer) []siteAnswer {
	if f.ws != nil {
		return f.ws.answers.Append(s, a)
	}
	return append(s, a)
}

// bools is n false flags: from the workspace, if the Func has one.
func (f *Func) bools(n int) []bool {
	if f.ws != nil {
		return f.ws.bools.Make(n)
	}
	return make([]bool, n)
}

// bytes is n zero bytes: from the workspace, if the Func has one.
func (f *Func) bytes(n int) []uint8 {
	if f.ws != nil {
		return f.ws.bytes.Make(n)
	}
	return make([]uint8, n)
}

// ints is n zeros: from the workspace, if the Func has one.
func (f *Func) ints(n int) []int {
	if f.ws != nil {
		return f.ws.ints.Make(n)
	}
	return make([]int, n)
}

// appendValue is append(s, v), growing s in the workspace if there is one.
func (f *Func) appendValue(s []*Value, v *Value) []*Value {
	if f.ws != nil {
		return f.ws.refs.Append(s, v)
	}
	return append(s, v)
}

// blockList is n nil blocks: from the workspace, if the Func has one.
func (f *Func) blockList(n int) []*Block {
	if f.ws != nil {
		return f.ws.blockRefs.Make(n)
	}
	return make([]*Block, n)
}

// refLists is n nil lists of values: from the workspace, if the Func has
// one.
func (f *Func) refLists(n int) [][]*Value {
	if f.ws != nil {
		return f.ws.refLists.Make(n)
	}
	return make([][]*Value, n)
}

// appendBlock is append(s, b), growing s in the workspace if there is one.
func (f *Func) appendBlock(s []*Block, b *Block) []*Block {
	if f.ws != nil {
		return f.ws.blockRefs.Append(s, b)
	}
	return append(s, b)
}
