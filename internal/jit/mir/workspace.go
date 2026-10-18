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
	words     arena.Arena[uint64]
	sets      arena.Arena[valueSet]
	values    arena.Arena[*ssa.Value]
	blocks    arena.Arena[*ssa.Block]
	uses      arena.Arena[use]
	intervals arena.Arena[interval]
	ivRefs    arena.Arena[*interval]
	locs      arena.Arena[loc]
	saved     arena.Arena[saved]
	sched     schedule
	ready     bool
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
	w.words.KeepBig()
	w.sets.KeepBig()
	w.values.KeepBig()
	w.blocks.KeepBig()
	w.uses.KeepBig()
	w.intervals.KeepBig()
	w.ivRefs.KeepBig()
	w.locs.KeepBig()
	w.saved.KeepBig()
}

// Bytes is the memory the workspace keeps between compiles: its arenas'
// chunks, its move schedule's tables and its code generators' assemblers
// and tables.
func (w *Workspace) Bytes() int {
	n := w.bools.Bytes() + w.ints.Bytes() + w.words.Bytes() + w.sets.Bytes() + w.values.Bytes() + w.blocks.Bytes() +
		w.uses.Bytes() + w.intervals.Bytes() + w.ivRefs.Bytes() + w.locs.Bytes() + w.saved.Bytes()
	n += capBytes(w.sched.moves) + capBytes(w.sched.steps) + capBytes(w.sched.parked) + capBytes(w.sched.readers)
	n += w.amd64.a.Bytes() + capBytes(w.amd64.labels) + capBytes(w.amd64.stubFor) + capBytes(w.amd64.cold)
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
	w.words.Rewind()
	w.sets.Rewind()
	w.values.Rewind()
	w.blocks.Rewind()
	w.uses.Rewind()
	w.intervals.Rewind()
	w.ivRefs.Rewind()
	w.locs.Rewind()
	w.saved.Rewind()
	// As ssa.Workspace's Rewind does its scratch tables: the move
	// schedule's, the exit descriptions' and the assemblers' tables go
	// once they hold more than the arenas keep, which is what the recent
	// compiles needed.
	if arenas := w.arenaBytes(); w.Bytes()-arenas > arenas {
		w.sched = schedule{}
		w.exitWords = nil
		w.exitSlotsBuf, w.exitIndexBuf, w.exitWordsBuf, w.exitHeadBuf, w.exitNextBuf = nil, nil, nil, nil, nil
		w.amd64.a, w.amd64.labels, w.amd64.stubs, w.amd64.stubFor, w.amd64.cold = amd64.Asm{}, nil, nil, nil, nil
		w.arm64.a, w.arm64.labels, w.arm64.stubs, w.arm64.stubFor, w.arm64.cold = arm64.Asm{}, nil, nil, nil, nil
	}
}

// arenaBytes is what the workspace's arenas keep.
func (w *Workspace) arenaBytes() int {
	return w.bools.Bytes() + w.ints.Bytes() + w.words.Bytes() + w.sets.Bytes() + w.values.Bytes() + w.blocks.Bytes() +
		w.uses.Bytes() + w.intervals.Bytes() + w.ivRefs.Bytes() + w.locs.Bytes() + w.saved.Bytes()
}

// CompileIn is Compile, in w.
func CompileIn(w *Workspace, f *ssa.Func, enc abi.Encoding) (*Code, error) {
	w.init()
	if runtime.GOARCH == "arm64" {
		return compileARM64(w, f, enc)
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

func (c *core) useList(n int) []use {
	if c.ws != nil {
		return c.ws.uses.Make(n)
	}
	return make([]use, n)
}

func (c *core) intervalList(n int) []interval {
	if c.ws != nil {
		return c.ws.intervals.Make(n)
	}
	return make([]interval, n)
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
