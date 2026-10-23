package mir

import (
	"runtime"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/arena"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A Workspace is the memory a runtime's compiles share, as ssa.Workspace is
// for building: a compile in one (CompileIn) takes its analysis and
// allocation tables from the workspace's arenas, and Rewind takes them all
// back at once, keeping the chunks. The Code's Bytes and Entries are its
// own; its Locations, for tests, may not be asked for after the Rewind.
type Workspace struct {
	bools     arena.Arena[bool]
	ints      arena.Arena[int]
	int32s    arena.Arena[int32]
	words     arena.Arena[uint64]
	sets      arena.Arena[valueSet]
	values    arena.Arena[*ssa.Value]
	blocks    arena.Arena[*ssa.Block]
	intervals arena.Arena[interval]
	recs      arena.Arena[useRec]
	gms       arena.Arena[gmember]
	ivRefs    arena.Arena[*interval]
	locs      arena.Arena[loc]
	saved     arena.Arena[saved]
	sched     schedule
	// xuses is allocate's list of uses in other blocks, kept from compile
	// to compile, and gpool its room for the phis' groups' members; of
	// each, what the last compile used.
	xuses                []xuse
	states               []*ssa.FrameState
	kv                   []*ssa.Value
	gpool                []uint64
	xusesUsed, gpoolUsed int
	// needs are what each of the recent compiles used of the scratch
	// tables (scratchUsed), the next to replace at needs[next]: the
	// tables keep what those needed, as the arenas keep theirs.
	needs       [16]int
	next, known int
	ready       bool
	// What a compile's exit descriptions share (abi.ExitFrame) is built
	// here, then copied out once at its length (core's finishExits).
	exitWords    map[uint64]int32
	exitHeadBuf  []int32
	exitNextBuf  []int32
	exitSlotsBuf []abi.ExitSlot
	exitIndexBuf []uint16
	exitWordsBuf []uint64
	// Each code generator's assembler and tables. A Code's Bytes from
	// CompileIn are the assembler's: they hold until the workspace's next
	// compile.
	amd64 struct {
		a       amd64.Asm
		labels  []amd64.Label
		stubs   map[stubKey]amd64.Label
		stubFor []stub
		cold    []func()
		pool    []poolConst
		poolTab []int32
	}
	arm64 struct {
		a       arm64.Asm
		labels  []arm64.Label
		stubs   map[stubKey]arm64.Label
		stubFor []a64Stub
		cold    []func()
	}
}

// clearFuncs empties fs, dropping the closures it held, for reuse.
func clearFuncs(fs []func()) []func() {
	clear(fs)
	return fs[:0]
}

// init has the workspace's arenas keep the big tables a compile makes.
func (w *Workspace) init() {
	if w.ready {
		return
	}
	w.ready = true
	w.bools.KeepBig()
	w.ints.KeepBig()
	w.int32s.KeepBig()
	w.words.KeepBig()
	w.sets.KeepBig()
	w.values.KeepBig()
	w.blocks.KeepBig()
	w.intervals.KeepBig()
	w.recs.KeepBig()
	w.gms.KeepBig()
	w.ivRefs.KeepBig()
	w.locs.KeepBig()
	w.saved.KeepBig()
}

// Bytes is the memory the workspace keeps between compiles: its arenas'
// chunks, its move schedule's tables and its code generators' assemblers
// and tables.
func (w *Workspace) Bytes() int {
	n := w.bools.Bytes() + w.ints.Bytes() + w.int32s.Bytes() + w.words.Bytes() + w.sets.Bytes() + w.values.Bytes() + w.blocks.Bytes() +
		w.intervals.Bytes() + w.recs.Bytes() + w.gms.Bytes() + w.ivRefs.Bytes() + w.locs.Bytes() + w.saved.Bytes()
	n += capBytes(w.sched.moves) + capBytes(w.sched.steps) + capBytes(w.sched.parked) + capBytes(w.sched.readers) + capBytes(w.xuses) + capBytes(w.states) + capBytes(w.kv) + capBytes(w.gpool)
	n += w.amd64.a.Bytes() + capBytes(w.amd64.labels) + capBytes(w.amd64.stubFor) + capBytes(w.amd64.cold) + capBytes(w.amd64.pool) + capBytes(w.amd64.poolTab)
	n += w.arm64.a.Bytes() + capBytes(w.arm64.labels) + capBytes(w.arm64.stubFor) + capBytes(w.arm64.cold)
	// A map's entries, as their keys and values, at least.
	n += (len(w.amd64.stubs) + len(w.arm64.stubs)) * int(unsafe.Sizeof(stubKey{})+unsafe.Sizeof(amd64.Label(0)))
	n += len(w.exitWords) * 12
	n += capBytes(w.exitSlotsBuf) + capBytes(w.exitIndexBuf) + capBytes(w.exitWordsBuf) + capBytes(w.exitHeadBuf) + capBytes(w.exitNextBuf)
	return n
}

// capBytes is the memory s's backing array holds.
func capBytes[T any](s []T) int {
	var t T
	return cap(s) * int(unsafe.Sizeof(t))
}

// Rewind takes back everything a compile in the workspace used.
func (w *Workspace) Rewind() {
	w.bools.Rewind()
	w.ints.Rewind()
	w.int32s.Rewind()
	w.words.Rewind()
	w.sets.Rewind()
	w.values.Rewind()
	w.blocks.Rewind()
	w.intervals.Rewind()
	w.recs.Rewind()
	w.gms.Rewind()
	w.ivRefs.Rewind()
	w.locs.Rewind()
	w.saved.Rewind()
	// The scratch tables -- the move schedule's, the uses', the groups',
	// the exit descriptions' and the assemblers' -- go once they hold more
	// than twice what any of the recent compiles used of them, as the
	// arenas keep what those compiles used: a compile bigger than they
	// grew them. (Compared with what the arenas keep, they went with every
	// compile whose tables had grown as the arenas' shrank.)
	used := w.scratchUsed()
	keep := used
	for _, n := range w.needs[:w.known] {
		keep = max(keep, n)
	}
	w.needs[w.next], w.next, w.known = used, (w.next+1)%len(w.needs), min(w.known+1, len(w.needs))
	if w.Bytes()-w.arenaBytes() > 2*keep {
		w.sched, w.xuses, w.states, w.kv, w.gpool, w.xusesUsed, w.gpoolUsed = schedule{}, nil, nil, nil, nil, 0, 0
		w.exitWords = nil
		w.exitSlotsBuf, w.exitIndexBuf, w.exitWordsBuf, w.exitHeadBuf, w.exitNextBuf = nil, nil, nil, nil, nil
		w.amd64.a, w.amd64.labels, w.amd64.stubs, w.amd64.stubFor, w.amd64.cold, w.amd64.pool, w.amd64.poolTab = amd64.Asm{}, nil, nil, nil, nil, nil, nil
		w.arm64.a, w.arm64.labels, w.arm64.stubs, w.arm64.stubFor, w.arm64.cold = arm64.Asm{}, nil, nil, nil, nil
	}
}

// scratchUsed is what the last compile used of the scratch tables: of the
// assemblers' and the uses' and groups' lists, their lengths, of the
// others, as they are emptied for reuse, all they hold.
func (w *Workspace) scratchUsed() int {
	n := w.Bytes() - w.arenaBytes()
	n -= w.amd64.a.Bytes() + w.arm64.a.Bytes() + capBytes(w.xuses) + capBytes(w.gpool)
	n += w.amd64.a.Used() + w.arm64.a.Used()
	n += w.xusesUsed*int(unsafe.Sizeof(xuse{})) + w.gpoolUsed*8
	return n
}

// arenaBytes is what the workspace's arenas keep.
func (w *Workspace) arenaBytes() int {
	return w.bools.Bytes() + w.ints.Bytes() + w.int32s.Bytes() + w.words.Bytes() + w.sets.Bytes() + w.values.Bytes() + w.blocks.Bytes() +
		w.intervals.Bytes() + w.recs.Bytes() + w.gms.Bytes() + w.ivRefs.Bytes() + w.locs.Bytes() + w.saved.Bytes()
}

// CompileIn is Compile, in w.
func CompileIn(w *Workspace, f *ssa.Func, enc abi.Encoding) (*Code, error) {
	w.init()
	if runtime.GOARCH == "arm64" {
		c, err := compileARM64(w, f, enc)
		if c != nil && Annotate {
			// The SSA compiled, as amd64's code has it; arm64's code has
			// no marks.
			c.Listing = f.String()
		}
		return c, err
	}
	return compileAMD64(w, f, enc)
}

// The core's tables, from its workspace if it has one.

func (c *core) bools(n int) []bool {
	if c.ws != nil {
		return c.ws.bools.Make(n)
	}
	return make([]bool, n)
}

func (c *core) ints(n int) []int {
	if c.ws != nil {
		return c.ws.ints.Make(n)
	}
	return make([]int, n)
}

func (c *core) int32s(n int) []int32 {
	if c.ws != nil {
		return c.ws.int32s.Make(n)
	}
	return make([]int32, n)
}

// groupPool is n words for allocate's groups, which it may grow: the
// workspace's, if there is one, which keepGroupPool gives back, as they
// are, not cleared.
func (c *core) groupPool(n int) []uint64 {
	if c.ws != nil {
		if cap(c.ws.gpool) < n {
			c.ws.gpool = make([]uint64, n, 2*n)
		}
		return c.ws.gpool[:n]
	}
	return make([]uint64, n)
}

func (c *core) keepGroupPool(s []uint64) {
	if c.ws != nil {
		c.ws.gpool, c.ws.gpoolUsed = s[:0], len(s)
	}
}

func (c *core) words(n int) valueSet {
	if c.ws != nil {
		return c.ws.words.Make(n)
	}
	return make(valueSet, n)
}

func (c *core) setList(n int) []valueSet {
	if c.ws != nil {
		return c.ws.sets.Make(n)
	}
	return make([]valueSet, n)
}

func (c *core) valueList(n int) []*ssa.Value {
	if c.ws != nil {
		return c.ws.values.Make(n)
	}
	return make([]*ssa.Value, n)
}

func (c *core) blockList(n int) []*ssa.Block {
	if c.ws != nil {
		return c.ws.blocks.Make(n)
	}
	return make([]*ssa.Block, n)
}

// xuseList is an empty list for allocate's uses in other blocks: the
// workspace's, if there is one, which keepXuses gives back.
func (c *core) xuseList() []xuse {
	if c.ws != nil {
		return c.ws.xuses[:0]
	}
	return nil
}

// stateList is an empty list for findLazy's states: the workspace's, if
// there is one, which keepStates gives back, emptied.
func (c *core) stateList() []*ssa.FrameState {
	if c.ws != nil {
		return c.ws.states[:0]
	}
	return nil
}

func (c *core) keepStates(s []*ssa.FrameState) {
	if c.ws != nil {
		clear(s)
		c.ws.states = s[:0]
	}
}

// keepList is an empty list for allocate's values of keeps and calls
// (kv): the workspace's, if there is one, which keepKeepList gives back,
// emptied.
func (c *core) keepList() []*ssa.Value {
	if c.ws != nil {
		return c.ws.kv[:0]
	}
	return nil
}

func (c *core) keepKeepList(s []*ssa.Value) {
	if c.ws != nil {
		clear(s)
		c.ws.kv = s[:0]
	}
}

func (c *core) keepXuses(s []xuse) {
	if c.ws != nil {
		c.ws.xuses, c.ws.xusesUsed = s[:0], len(s)
	}
}

func (c *core) intervalList(n int) []interval {
	if c.ws != nil {
		return c.ws.intervals.Make(n)
	}
	return make([]interval, n)
}

func (c *core) useRecs(n int) []useRec {
	if c.ws != nil {
		return c.ws.recs.Make(n)
	}
	return make([]useRec, n)
}

func (c *core) groupMembers(n int) []gmember {
	if c.ws != nil {
		return c.ws.gms.Make(n)
	}
	return make([]gmember, n)
}

func (c *core) intervalRefs(n int) []*interval {
	if c.ws != nil {
		return c.ws.ivRefs.Make(n)
	}
	return make([]*interval, n)
}

// appendSaved is append(s, v), growing s in the workspace if there is one.
func (c *core) appendSaved(s []saved, v saved) []saved {
	if c.ws != nil {
		return c.ws.saved.Append(s, v)
	}
	return append(s, v)
}

func (c *core) locList(n int) []loc {
	if c.ws != nil {
		return c.ws.locs.Make(n)
	}
	return make([]loc, n)
}
